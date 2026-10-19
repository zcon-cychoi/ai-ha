package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cloud-barista/ai-ha/pkg/backup/model"
)

// systemIDFile holds the identifier under which this system's backups are
// stored. It is stable across reboots, agent upgrades and hostname changes.
const systemIDFile = "system-id"

// systemIDPattern is what a stored identifier must look like. It becomes a
// directory name in the repository, so it is restricted to characters that
// cannot escape a path or surprise a backend.
var systemIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ErrNoSystemID reports that this system has no identifier yet.
var ErrNoSystemID = errors.New("no system id")

// ReadSystemID returns the identifier recorded under dir. A dir of "" means
// Dir. It returns ErrNoSystemID when none has been recorded.
func ReadSystemID(dir string) (string, error) {
	if dir == "" {
		dir = Dir
	}
	path := filepath.Join(dir, systemIDFile)

	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNoSystemID
	}
	if err != nil {
		return "", fmt.Errorf("read system id %s: %w", path, err)
	}

	id := strings.TrimSpace(string(b))
	if !systemIDPattern.MatchString(id) {
		return "", fmt.Errorf("system id in %s is not usable as a directory name", path)
	}
	return id, nil
}

// EnsureSystemID returns this system's identifier, generating and recording
// one the first time it is called.
//
// The identifier is generated locally only because the framework does not
// issue one yet. Registration will return the framework's systemId, and this
// file is where it will be stored; until then a local value keeps backups from
// being filed under a hostname, which in a cloud is neither unique nor stable
// (AWS reuses names like "ip-10-0-1-5").
func EnsureSystemID(dir string) (string, error) {
	if dir == "" {
		dir = Dir
	}

	id, err := ReadSystemID(dir)
	switch {
	case err == nil:
		return id, nil
	case !errors.Is(err, ErrNoSystemID):
		return "", err
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create state directory %s: %w", dir, err)
	}

	id = model.NewID()
	path := filepath.Join(dir, systemIDFile)

	// Write the file complete, then link it into place. Creating the final
	// path first and filling it afterwards would let a second agent read it
	// while it is still empty; a link either publishes the finished file or
	// fails because someone else already published theirs.
	tmp, err := writeTemp(dir, systemIDFile, []byte(id+"\n"))
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)

	if err := os.Link(tmp, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			// Another agent won; adopt the identifier it published.
			return ReadSystemID(dir)
		}
		return "", fmt.Errorf("create system id %s: %w", path, err)
	}
	return id, nil
}

// writeTemp writes data to a uniquely named private file in dir and returns
// its path. The caller links it into place and removes it.
func writeTemp(dir, name string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	defer f.Close()

	if err := f.Chmod(0o600); err != nil {
		return "", fmt.Errorf("set mode on %s: %w", f.Name(), err)
	}
	if _, err := f.Write(data); err != nil {
		return "", fmt.Errorf("write %s: %w", f.Name(), err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("sync %s: %w", f.Name(), err)
	}
	return f.Name(), nil
}

// publish links a completed temporary file into place without overwriting an
// existing one. It reports fs.ErrExist when something is already there, which
// callers read as "someone else got here first" rather than as a failure.
func publish(tmp, path string) error {
	if err := os.Link(tmp, path); err != nil {
		return err
	}
	return nil
}
