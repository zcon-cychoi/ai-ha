package repository

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/spool"
)

// openTestSFTPRepo starts an in-process SFTP server and opens a repository
// pointing at it, rooted at a fresh temp directory.
func openTestSFTPRepo(t *testing.T) (Repository, context.Context) {
	t.Helper()
	return openSFTPRepoAt(t, startTestSFTPServer(t), t.TempDir(), t.TempDir()), context.Background()
}

func sinkPayload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// TestSFTPSinkResumableRoundTrip drives a multi-chunk backup through spool.Run
// into the SFTP sink, then reads the assembled file back and verifies it.
func TestSFTPSinkResumableRoundTrip(t *testing.T) {
	repo, ctx := openTestSFTPRepo(t)

	rr, ok := repo.(ResumableRepository)
	if !ok {
		t.Fatal("sftp repository does not implement ResumableRepository")
	}

	system := model.SystemInfo{ID: "web01", Hostname: "web01"}
	if err := repo.EnsureSystem(ctx, system); err != nil {
		t.Fatalf("ensure system: %v", err)
	}

	const filename = "web01-sda1-deadbeef.aha"
	in := sinkPayload(10_000) // ~10 chunks at 1000 bytes

	sink, err := rr.NewSink(ctx, system, filename)
	if err != nil {
		t.Fatalf("new sink: %v", err)
	}
	cfg := spool.Config{
		ChunkSize:         1000,
		BufferCap:         4000,
		DisconnectTimeout: 2 * time.Second,
		RetryBase:         2 * time.Millisecond,
	}
	n, err := spool.Run(ctx, t.TempDir(), cfg, sink, func(w io.Writer) error {
		_, werr := w.Write(in)
		return werr
	})
	if err != nil {
		t.Fatalf("spool.Run: %v", err)
	}
	if n != int64(len(in)) {
		t.Fatalf("transferred %d bytes, want %d", n, len(in))
	}

	r, err := repo.NewReader(ctx, system, filename)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, in) {
		t.Fatalf("assembled %d bytes, mismatch with input (%d)", len(got), len(in))
	}
}

// TestSFTPSinkResumesAfterPartialTail simulates a chunk upload that died
// mid-write (leaving a partial tail on the server) followed by a reconnect, and
// verifies the sink truncates the tail and re-appends to produce correct bytes.
func TestSFTPSinkResumesAfterPartialTail(t *testing.T) {
	repo, ctx := openTestSFTPRepo(t)
	rr := repo.(ResumableRepository)

	system := model.SystemInfo{ID: "web01", Hostname: "web01"}
	if err := repo.EnsureSystem(ctx, system); err != nil {
		t.Fatalf("ensure system: %v", err)
	}

	sinkIface, err := rr.NewSink(ctx, system, "web01-sda1-cafef00d.aha")
	if err != nil {
		t.Fatalf("new sink: %v", err)
	}
	s := sinkIface.(*sftpSink)

	chunkA := bytes.Repeat([]byte{0xAA}, 1000)
	chunkB := bytes.Repeat([]byte{0xBB}, 1000)

	// Commit chunk 0 normally.
	if err := s.Put(ctx, 0, bytes.NewReader(chunkA), int64(len(chunkA))); err != nil {
		t.Fatalf("put chunk 0: %v", err)
	}

	// Simulate a failed attempt at chunk 1: a partial tail is written to the
	// target on the server, but the sink's committed state is NOT advanced
	// (mirroring a connection that died mid-write). Use a separate connection.
	sshClient, client, err := s.dialer.dial(ctx)
	if err != nil {
		t.Fatalf("side dial: %v", err)
	}
	f, err := client.OpenFile(s.path, os.O_WRONLY)
	if err != nil {
		t.Fatalf("open target for garbage tail: %v", err)
	}
	if _, err := f.Seek(int64(len(chunkA)), io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	if _, err := f.Write(bytes.Repeat([]byte{0xFF}, 400)); err != nil {
		t.Fatalf("write garbage tail: %v", err)
	}
	_ = f.Close()
	_ = client.Close()
	_ = sshClient.Close()

	// Force the sink to reconnect on its next operation.
	s.reset()

	// Re-attempt chunk 1: it must truncate the 400-byte garbage tail and append.
	if err := s.Put(ctx, 1, bytes.NewReader(chunkB), int64(len(chunkB))); err != nil {
		t.Fatalf("put chunk 1 after reconnect: %v", err)
	}
	if err := s.Finalize(ctx); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	r, err := repo.NewReader(ctx, system, "web01-sda1-cafef00d.aha")
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	want := append(append([]byte{}, chunkA...), chunkB...)
	if !bytes.Equal(got, want) {
		t.Fatalf("assembled %d bytes, want %d (tail not repaired?)", len(got), len(want))
	}
}
