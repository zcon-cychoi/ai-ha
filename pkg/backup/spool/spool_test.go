package spool

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

var errFakeNet = errors.New("simulated network failure")

// memSink is an in-memory Sink for testing. It can simulate transient failures
// (failsLeft), permanent failure (alwaysFail) and slow uploads (delay), and it
// reassembles committed chunks in order.
type memSink struct {
	mu         sync.Mutex
	parts      map[int][]byte
	failsLeft  int
	alwaysFail bool
	delay      time.Duration
	finalized  bool
	aborted    bool
}

func (s *memSink) Put(ctx context.Context, index int, data io.Reader, _ int64) error {
	b, err := io.ReadAll(data) // re-read fully on every attempt
	if err != nil {
		return err
	}
	if s.delay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.delay):
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.alwaysFail {
		return errFakeNet
	}
	if s.failsLeft > 0 {
		s.failsLeft--
		return errFakeNet
	}
	if s.parts == nil {
		s.parts = map[int][]byte{}
	}
	s.parts[index] = b
	return nil
}

func (s *memSink) Finalize(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finalized = true
	return nil
}

func (s *memSink) Abort(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aborted = true
	return nil
}

func (s *memSink) assembled() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []byte
	for i := 0; ; i++ {
		p, ok := s.parts[i]
		if !ok {
			break
		}
		out = append(out, p...)
	}
	return out
}

// produce writes payload to w. Helper to build a produce func.
func writePayload(payload []byte) func(io.Writer) error {
	return func(w io.Writer) error {
		_, err := w.Write(payload)
		return err
	}
}

func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func testConfig(chunk, cap int64) Config {
	return Config{
		ChunkSize:         chunk,
		BufferCap:         cap,
		DisconnectTimeout: 2 * time.Second,
		RetryBase:         2 * time.Millisecond,
	}
}

func TestRunHappyPath(t *testing.T) {
	in := payload(10_000) // ~10 chunks at 1000 bytes
	sink := &memSink{}

	n, err := Run(context.Background(), t.TempDir(), testConfig(1000, 8000), sink, writePayload(in))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != int64(len(in)) {
		t.Fatalf("transferred %d bytes, want %d", n, len(in))
	}
	if !sink.finalized {
		t.Fatal("sink not finalized")
	}
	if got := sink.assembled(); !bytes.Equal(got, in) {
		t.Fatalf("assembled %d bytes, mismatch with input (%d)", len(got), len(in))
	}
}

func TestRunRetriesTransientFailures(t *testing.T) {
	in := payload(5_000)
	// Fail the first 4 chunk uploads, then succeed. Verifies retry + resume.
	sink := &memSink{failsLeft: 4}

	n, err := Run(context.Background(), t.TempDir(), testConfig(1000, 8000), sink, writePayload(in))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != int64(len(in)) {
		t.Fatalf("transferred %d bytes, want %d", n, len(in))
	}
	if got := sink.assembled(); !bytes.Equal(got, in) {
		t.Fatal("assembled data mismatch after retries")
	}
}

func TestRunAbortsOnDisconnectTimeout(t *testing.T) {
	in := payload(5_000)
	sink := &memSink{alwaysFail: true}

	cfg := testConfig(1000, 2000) // small buffer so the producer blocks too
	cfg.DisconnectTimeout = 100 * time.Millisecond

	start := time.Now()
	_, err := Run(context.Background(), t.TempDir(), cfg, sink, writePayload(in))
	if err == nil {
		t.Fatal("expected error on permanent upload failure")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("Run took too long to give up: %v", time.Since(start))
	}
	if !sink.aborted {
		t.Fatal("sink was not aborted on failure")
	}
}

func TestRunBackpressureWithSlowSink(t *testing.T) {
	in := payload(20_000) // 20 chunks
	// Small buffer (2 chunks) + slow sink forces the producer to block on the
	// bounded queue repeatedly. It must still complete correctly.
	sink := &memSink{delay: 5 * time.Millisecond}
	cfg := testConfig(1000, 2000)

	n, err := Run(context.Background(), t.TempDir(), cfg, sink, writePayload(in))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != int64(len(in)) {
		t.Fatalf("transferred %d bytes, want %d", n, len(in))
	}
	if got := sink.assembled(); !bytes.Equal(got, in) {
		t.Fatal("assembled data mismatch under backpressure")
	}
}

func TestRunCancelPropagates(t *testing.T) {
	in := payload(50_000)
	sink := &memSink{delay: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, err := Run(ctx, t.TempDir(), testConfig(1000, 4000), sink, writePayload(in))
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
}
