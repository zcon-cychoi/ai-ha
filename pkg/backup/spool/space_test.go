package spool

import (
	"errors"
	"testing"
)

func TestPeakDiskUse(t *testing.T) {
	tests := []struct {
		cfg  Config
		want int64
	}{
		// 32 queued chunks, one uploading, one waiting for room.
		{Config{ChunkSize: 64 << 20, BufferCap: 2 << 30}, 34 * (64 << 20)},
		// A cap smaller than a chunk still queues one.
		{Config{ChunkSize: 64 << 20, BufferCap: 1 << 20}, 3 * (64 << 20)},
		{Config{}, int64(DefaultBufferCap/DefaultChunkSize+2) * DefaultChunkSize},
	}
	for _, tt := range tests {
		if got := PeakDiskUse(tt.cfg); got != tt.want {
			t.Fatalf("PeakDiskUse(%+v) = %d, want %d", tt.cfg, got, tt.want)
		}
	}
}

func TestCheckSpace(t *testing.T) {
	dir := t.TempDir()
	if err := CheckSpace(dir, Config{ChunkSize: 1 << 20, BufferCap: 1 << 20}); err != nil {
		t.Fatalf("a few MiB refused: %v", err)
	}
	// No test machine has an exabyte free.
	err := CheckSpace(dir, Config{ChunkSize: 1 << 40, BufferCap: 1 << 60})
	if !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("err = %v, want ErrInsufficientSpace", err)
	}
	if err := CheckSpace(dir+"/missing", Config{}); err == nil || errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("missing dir: err = %v, want a statfs failure", err)
	}
}
