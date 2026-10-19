package sysinfo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// This is the only place on the backup side that runs an external command.
// Everything else there reads /proc, /sys and /etc directly or makes a
// syscall. The restore executor (pkg/agent/restore) runs its commands through
// ExecRunner too, apart from the rclone | tar pipeline it assembles itself.
// Partition tables and the LVM configuration have no such interface, so they
// are read through the tools that own them; keeping that in one type keeps the
// timeout, the output limits and the test seam in one place too.

// commandTimeout bounds a single collection command. The commands here inspect
// local metadata and return in milliseconds; a longer run means something is
// wedged, and a backup must not wait on it.
const commandTimeout = 30 * time.Second

// maxOutputBytes caps what one command may return. "lsblk -J -O" on a large
// machine is tens of kilobytes; anything approaching this is a malfunction,
// and the output ends up in a manifest.
const maxOutputBytes = 8 << 20

// ErrCommandNotFound reports that the tool is not installed. Callers treat it
// as "this system does not have that subsystem" rather than as a failure: a
// machine without LVM has no lvm binary.
var ErrCommandNotFound = errors.New("command not found")

// Runner runs a system command and returns its standard output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner runs commands with os/exec.
type ExecRunner struct {
	// Timeout bounds one command. Zero means commandTimeout.
	Timeout time.Duration
}

// Run implements Runner. It resolves the tool on PATH, bounds the run by
// Timeout and returns standard output. Standard error is folded into the
// returned error, never into the output.
func (r ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("%s: %w", name, ErrCommandNotFound)
		}
		return nil, fmt.Errorf("locate %s: %w", name, err)
	}

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = commandTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// These tools read locale-sensitive output formats; pin the locale so a
	// parser does not meet translated field names.
	cmd.Env = append(cmd.Environ(), "LC_ALL=C")

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("run %s: %w", name, ctx.Err())
		}
		return nil, fmt.Errorf("run %s: %w%s", name, err, stderrSuffix(stderr.String()))
	}
	if stdout.Len() > maxOutputBytes {
		return nil, fmt.Errorf("run %s: output exceeds %d bytes", name, maxOutputBytes)
	}
	return stdout.Bytes(), nil
}

// stderrSuffix renders a command's standard error for an error message,
// trimmed to one line so that a failure stays readable in a log.
func stderrSuffix(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 200
	if len(s) > max {
		s = s[:max] + "..."
	}
	return ": " + s
}
