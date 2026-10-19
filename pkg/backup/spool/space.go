package spool

import (
	"errors"
	"fmt"
)

// ErrInsufficientSpace reports that the spool directory's filesystem cannot
// hold what the spool may write there.
var ErrInsufficientSpace = errors.New("not enough free space for the spool")

// PeakDiskUse is the most local disk a run with cfg can occupy at once: a full
// ready queue, the chunk being uploaded, and the finished chunk the producer
// holds while it waits for room in the queue. Each is up to ChunkSize.
func PeakDiskUse(cfg Config) int64 {
	cfg = cfg.withDefaults()
	maxChunks := max(int(cfg.BufferCap/cfg.ChunkSize), 1)
	return int64(maxChunks+2) * cfg.ChunkSize
}

// CheckSpace reports ErrInsufficientSpace when dir's filesystem has less free
// space than a run with cfg may use. It is checked before anything is written:
// running out midway fails the backup only after the partition has been read
// for nothing, and fills a filesystem the system itself is using.
func CheckSpace(dir string, cfg Config) error {
	free, known, err := freeBytes(dir)
	if err != nil {
		return fmt.Errorf("check free space in %s: %w", dir, err)
	}
	if !known {
		return nil
	}
	if need := PeakDiskUse(cfg); free < need {
		return fmt.Errorf("%w: %s has %d MiB free but the spool may use up to %d MiB; "+
			"lower the spool buffer cap or move the spool directory to a larger filesystem",
			ErrInsufficientSpace, dir, free>>20, need>>20)
	}
	return nil
}
