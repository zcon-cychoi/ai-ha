package restore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync/atomic"
)

// extractFunc streams one backup file from the repository into dest. read is
// advanced by the number of bytes read from the repository, which is what
// the progress reports compare against the file's size.
type extractFunc func(ctx context.Context, src source, dest string, read *atomic.Int64) error

// source is a backup file to read and how to decode it.
type source struct {
	// rcloneArgs are the arguments to rclone that write the file to standard
	// output, configuration included.
	rcloneArgs []string
	// decompress is the command that decodes the file, nil for none.
	decompress []string
}

// extract runs "rclone cat | decompress | tar -x" with the first link read
// here, so that the bytes taken from the repository can be counted. tar
// keeps numeric owners: names map to different ids in the rescue system.
//
// Every process is waited for and every failure reported. A pipeline is only
// as good as its worst stage: tar exiting cleanly on a stream rclone cut short
// would leave a partition half restored and look like success.
func extract(ctx context.Context, src source, dest string, read *atomic.Int64) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var rcloneErr, decompressErr, tarErr tailBuffer
	rclone := exec.CommandContext(ctx, "rclone", src.rcloneArgs...)
	rclone.Stderr = &rcloneErr
	fromRepo, err := rclone.StdoutPipe()
	if err != nil {
		return fmt.Errorf("connect rclone: %w", err)
	}

	tar := exec.CommandContext(ctx, "tar", "-x", "-p", "--numeric-owner", "-f", "-", "-C", dest)
	tar.Stderr = &tarErr

	var decompress *exec.Cmd
	var sink io.WriteCloser
	if src.decompress != nil {
		decompress = exec.CommandContext(ctx, src.decompress[0], src.decompress[1:]...)
		decompress.Stderr = &decompressErr
		if sink, err = decompress.StdinPipe(); err != nil {
			return fmt.Errorf("connect %s: %w", src.decompress[0], err)
		}
		if tar.Stdin, err = decompress.StdoutPipe(); err != nil {
			return fmt.Errorf("connect %s to tar: %w", src.decompress[0], err)
		}
	} else if sink, err = tar.StdinPipe(); err != nil {
		return fmt.Errorf("connect tar: %w", err)
	}

	// Started from the end of the pipeline, so that each stage has a reader.
	started := []*exec.Cmd{}
	for _, cmd := range []*exec.Cmd{tar, decompress, rclone} {
		if cmd == nil {
			continue
		}
		if err := cmd.Start(); err != nil {
			cancel()
			for _, s := range started {
				_ = s.Wait()
			}
			return fmt.Errorf("start %s: %w", cmd.Path, err)
		}
		started = append(started, cmd)
	}

	_, copyErr := io.Copy(sink, &countingReader{r: fromRepo, n: read})
	closeErr := sink.Close()
	if copyErr != nil {
		// A later stage stopped reading. Stop rclone too rather than leave it
		// blocked on a pipe nobody drains.
		cancel()
	}

	waitRclone := rclone.Wait()
	var waitDecompress error
	if decompress != nil {
		waitDecompress = decompress.Wait()
	}
	waitTar := tar.Wait()

	// The first stage to fail explains the rest: rclone failing starves tar,
	// tar failing breaks the pipe behind it.
	switch {
	case waitRclone != nil && ctx.Err() == nil:
		return fmt.Errorf("read from the repository: %w%s", waitRclone, rcloneErr.suffix())
	case waitTar != nil:
		return fmt.Errorf("unpack: %w%s", waitTar, tarErr.suffix())
	case waitDecompress != nil:
		return fmt.Errorf("decompress: %w%s", waitDecompress, decompressErr.suffix())
	case waitRclone != nil:
		return fmt.Errorf("read from the repository: %w%s", waitRclone, rcloneErr.suffix())
	case copyErr != nil:
		return fmt.Errorf("pass the archive on: %w", copyErr)
	case closeErr != nil:
		return fmt.Errorf("finish the archive stream: %w", closeErr)
	}
	return nil
}

// countingReader adds every byte read to n.
type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// tailBuffer keeps the end of a command's standard error for its error
// message: the last lines are the ones that say why it stopped.
type tailBuffer struct {
	buf bytes.Buffer
}

const tailBytes = 4 << 10

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if extra := t.buf.Len() - tailBytes; extra > 0 {
		t.buf.Next(extra)
	}
	return len(p), nil
}

// suffix renders the last line of the captured output for an error message.
func (t *tailBuffer) suffix() string {
	s := strings.TrimSpace(t.buf.String())
	if s == "" {
		return ""
	}
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return ": " + s
}
