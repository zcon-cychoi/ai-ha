// Package archive provides the archiving layer for backups. The default
// implementation streams a POSIX tar archive, matching the "tar + zstd"
// approach described in the backup design.
package archive

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rs/zerolog/log"
)

// Archiver serializes a directory tree to a stream and back.
type Archiver interface {
	// Archive walks root and writes a tar stream to w. Entries whose
	// archive-relative slash path matches any exclude regexp are skipped; a
	// matching directory prunes its whole subtree.
	Archive(ctx context.Context, root string, w io.Writer, excludes []*regexp.Regexp) error
	// Extract reads a tar stream from r and writes files under dest.
	Extract(ctx context.Context, r io.Reader, dest string) error
}

// TarArchiver is the default tar-based Archiver.
type TarArchiver struct{}

// NewTar returns a tar Archiver.
func NewTar() *TarArchiver { return &TarArchiver{} }

// Archive writes the tree rooted at root into w as a tar stream. Regular files,
// directories and symlinks are archived; other special files (devices, sockets,
// FIFOs) are skipped with a warning.
func (a *TarArchiver) Archive(ctx context.Context, root string, w io.Writer, excludes []*regexp.Regexp) error {
	tw := tar.NewWriter(w)
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		// Skip excluded paths; a matching directory prunes its whole subtree.
		if matchAny(excludes, filepath.ToSlash(rel)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		var link string
		// if info.Mode()&os.ModeSymlink != 0 {
		if info.Mode().Type() == os.ModeSymlink {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		} else if !info.Mode().IsRegular() && !info.IsDir() {
			log.Warn().Str("path", path).Msg("Skipping unsupported special file")
			return nil
		}

		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}

		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		// The file may change between when its header size was written and now
		// (the source is a live filesystem, no snapshot). tar requires exactly
		// hdr.Size bytes: cap a grown file to the declared size and zero-pad a
		// shrunk one, so the entry stays well-formed either way.
		size := info.Size()
		written, err := io.Copy(tw, io.LimitReader(f, size))
		if err != nil {
			return fmt.Errorf("copy %s: %w", rel, err)
		}
		if written < size {
			if _, err := io.CopyN(tw, zeroReader{}, size-written); err != nil {
				return fmt.Errorf("pad %s: %w", rel, err)
			}
		}
		return nil
	})
	if walkErr != nil {
		// Close discards buffered data; return the original walk error.
		_ = tw.Close()
		return walkErr
	}
	return tw.Close()
}

// Extract reads a tar stream from r and writes its entries under dest. Entry
// paths are sanitized to prevent path traversal outside dest.
func (a *TarArchiver) Extract(ctx context.Context, r io.Reader, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		target, err := safeJoin(dest, hdr.Name)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return fmt.Errorf("write %s: %w", hdr.Name, err)
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		default:
			log.Warn().Str("name", hdr.Name).Int("type", int(hdr.Typeflag)).Msg("Skipping unsupported tar entry")
		}
	}
}

// matchAny reports whether s matches any of the exclude patterns.
func matchAny(patterns []*regexp.Regexp, s string) bool {
	for _, re := range patterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// zeroReader is an infinite source of zero bytes, used to pad a tar entry when
// its file shrank after the header size was written.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// safeJoin joins dest and name, rejecting entries that escape dest.
func safeJoin(dest, name string) (string, error) {
	target := filepath.Join(dest, filepath.FromSlash(name))
	cleanDest := filepath.Clean(dest)
	if target != cleanDest && !strings.HasPrefix(target, cleanDest+string(os.PathSeparator)) {
		return "", fmt.Errorf("illegal path in archive: %q", name)
	}
	return target, nil
}
