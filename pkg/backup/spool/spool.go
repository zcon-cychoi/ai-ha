// Package spool provides a disk-backed, bounded streaming buffer between a
// backup producer (tar + compression) and a resumable upload Sink.
//
// The producer writes the compressed archive into a spoolWriter, which rolls
// the byte stream into fixed-size chunk files in a temporary directory. A
// consumer uploads those chunks to the Sink in order, deleting each on success.
//
//   - Backpressure: the ready queue is bounded (BufferCap / ChunkSize). When it
//     is full the producer blocks, so local disk use stays within BufferCap
//     plus two chunks (see PeakDiskUse; CheckSpace compares it with the free
//     space before a run).
//   - Interruption tolerance: a failed chunk upload is retried with backoff. The
//     bytes already live on disk, so no re-archiving is needed to resume.
//   - Bounded wait: if a chunk cannot be uploaded for DisconnectTimeout, the run
//     fails (no infinite wait). A slow-but-working link keeps making progress
//     and is not failed.
//
// Chunks are strictly ordered: chunk N is only meaningful after 0..N-1, so the
// Sink assembles them by appending in order (see Sink).
package spool

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"time"
)

// Defaults for a resumable transfer.
const (
	DefaultChunkSize         = 64 << 20 // 64 MiB
	DefaultBufferCap         = 2 << 30  // 2 GiB
	DefaultDisconnectTimeout = 5 * time.Minute
	defaultRetryBase         = 200 * time.Millisecond
	maxRetryBackoff          = 10 * time.Second
)

// Config tunes the spool. Zero fields fall back to the defaults above.
type Config struct {
	// ChunkSize is the size each on-disk chunk is rolled over at.
	ChunkSize int64
	// BufferCap is the maximum number of unsent bytes held on local disk.
	BufferCap int64
	// DisconnectTimeout is how long a single chunk may fail to upload before
	// the run is aborted.
	DisconnectTimeout time.Duration
	// RetryBase is the base delay for exponential upload retry backoff.
	RetryBase time.Duration
}

// DefaultConfig returns the built-in defaults.
func DefaultConfig() Config {
	return Config{
		ChunkSize:         DefaultChunkSize,
		BufferCap:         DefaultBufferCap,
		DisconnectTimeout: DefaultDisconnectTimeout,
		RetryBase:         defaultRetryBase,
	}
}

func (c Config) withDefaults() Config {
	if c.ChunkSize <= 0 {
		c.ChunkSize = DefaultChunkSize
	}
	if c.BufferCap <= 0 {
		c.BufferCap = DefaultBufferCap
	}
	if c.DisconnectTimeout <= 0 {
		c.DisconnectTimeout = DefaultDisconnectTimeout
	}
	if c.RetryBase <= 0 {
		c.RetryBase = defaultRetryBase
	}
	return c
}

// Sink is a resumable, append-oriented destination assembled from ordered
// chunks. Implementations MUST be resumable within a session: if Put for a
// given index previously failed part-way, calling Put again for the same index
// must overwrite that attempt (append backends truncate the partial tail to the
// committed offset first; S3 re-uploads the part).
type Sink interface {
	// Put appends chunk index (0-based, strictly increasing) to the target.
	Put(ctx context.Context, index int, data io.Reader, size int64) error
	// Finalize completes the target once all chunks are committed.
	Finalize(ctx context.Context) error
	// Abort discards a partial/in-progress target.
	Abort(ctx context.Context) error
}

// chunkRef points at one spooled chunk file.
type chunkRef struct {
	index int
	path  string
	size  int64
}

// Run streams produce's output through a bounded on-disk buffer in tmpDir into
// sink, and returns the total number of bytes transferred. produce receives the
// writer it should write the (compressed) archive stream to.
func Run(ctx context.Context, tmpDir string, cfg Config, sink Sink, produce func(w io.Writer) error) (int64, error) {
	cfg = cfg.withDefaults()

	maxChunks := max(int(cfg.BufferCap/cfg.ChunkSize), 1)
	ready := make(chan chunkRef, maxChunks)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sw := &spoolWriter{ctx: ctx, dir: tmpDir, chunkSize: cfg.ChunkSize, ready: ready}

	errc := make(chan error, 2)

	// Producer: archive -> chunk files -> ready queue.
	go func() {
		if err := produce(sw); err != nil {
			errc <- err
			return
		}
		if err := sw.flushFinal(); err != nil {
			errc <- err
			return
		}
		close(ready) // success: signal the consumer no more chunks are coming.
		errc <- nil
	}()

	// Consumer: ready queue -> sink, in order, with retry + timeout.
	go func() {
		errc <- consume(ctx, ready, sink, cfg)
	}()

	var firstErr error
	for range 2 {
		if err := <-errc; err != nil && firstErr == nil {
			firstErr = err
			cancel() // stop the other goroutine
		}
	}

	if firstErr != nil {
		// context.WithoutCancel: cleanup must run even though ctx is cancelled.
		_ = sink.Abort(context.WithoutCancel(ctx))
		return sw.written, firstErr
	}
	return sw.written, nil
}

// consume uploads ready chunks in order. Each chunk is retried until it
// succeeds or DisconnectTimeout elapses with no progress on that chunk.
func consume(ctx context.Context, ready <-chan chunkRef, sink Sink, cfg Config) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ch, ok := <-ready:
			if !ok {
				return sink.Finalize(ctx)
			}
			if err := uploadChunk(ctx, sink, ch, cfg); err != nil {
				return err
			}
			_ = os.Remove(ch.path) // free local disk once committed
		}
	}
}

// uploadChunk uploads a single chunk, retrying transient failures. The stall
// clock is per-chunk and only advances while the chunk is actively failing, so
// idle waiting for the producer is never counted as a disconnect.
func uploadChunk(ctx context.Context, sink Sink, ch chunkRef, cfg Config) error {
	stallStart := time.Now()
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := putChunk(ctx, sink, ch)
		if err == nil {
			return nil
		}
		if time.Since(stallStart) > cfg.DisconnectTimeout {
			return fmt.Errorf("transfer stalled for %s at chunk %d: %w", cfg.DisconnectTimeout, ch.index, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff(cfg.RetryBase, attempt)):
		}
	}
}

// putChunk opens the chunk file and hands a fresh reader to the sink, so every
// retry re-reads the chunk from the start.
func putChunk(ctx context.Context, sink Sink, ch chunkRef) error {
	f, err := os.Open(ch.path)
	if err != nil {
		return err
	}
	defer f.Close()
	return sink.Put(ctx, ch.index, f, ch.size)
}

// backoff returns an exponential delay with jitter, capped at maxRetryBackoff.
func backoff(base time.Duration, attempt int) time.Duration {
	d := base
	for i := 0; i < attempt && d < maxRetryBackoff; i++ {
		d *= 2
	}
	d = min(d, maxRetryBackoff)
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// spoolWriter rolls the byte stream written to it into fixed-size chunk files,
// pushing each completed chunk onto the ready queue (blocking when the queue is
// full, which is the backpressure that bounds local disk use).
type spoolWriter struct {
	ctx       context.Context
	dir       string
	chunkSize int64
	ready     chan chunkRef

	cur     *os.File
	curSize int64
	index   int
	written int64
}

func (s *spoolWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		if s.cur == nil {
			if err := s.open(); err != nil {
				return total, err
			}
		}
		room := s.chunkSize - s.curSize
		n := int64(len(p))
		if n > room {
			n = room
		}
		w, err := s.cur.Write(p[:n])
		s.curSize += int64(w)
		s.written += int64(w)
		total += w
		if err != nil {
			return total, err
		}
		p = p[n:]
		if s.curSize >= s.chunkSize {
			if err := s.rollover(); err != nil {
				return total, err
			}
		}
	}
	return total, nil
}

func (s *spoolWriter) open() error {
	path := filepath.Join(s.dir, fmt.Sprintf("chunk-%08d", s.index))
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	s.cur = f
	s.curSize = 0
	return nil
}

// rollover closes the current chunk file and enqueues it (blocking on a full
// queue until the consumer drains or the context is cancelled).
func (s *spoolWriter) rollover() error {
	ref := chunkRef{index: s.index, path: s.cur.Name(), size: s.curSize}
	if err := s.cur.Close(); err != nil {
		return err
	}
	s.cur = nil
	s.index++
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case s.ready <- ref:
		return nil
	}
}

// flushFinal emits the trailing partial chunk (if any). It does not close the
// ready channel; the caller does that only on the success path.
func (s *spoolWriter) flushFinal() error {
	if s.cur == nil {
		return nil
	}
	if s.curSize == 0 {
		name := s.cur.Name()
		_ = s.cur.Close()
		_ = os.Remove(name)
		s.cur = nil
		return nil
	}
	return s.rollover()
}
