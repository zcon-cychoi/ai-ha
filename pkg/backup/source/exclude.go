package source

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// MountPrefix names the temporary bind-mount directories this package creates
// under /mnt. The distinctive prefix makes them easy to identify and to exclude
// from a root-partition backup (see PartitionSource.Excludes).
const MountPrefix = "aiha-mount-"

// ExcludeProvider is an optional Source capability: a source that supplies path
// patterns to omit from the archive. Each pattern is a regular expression
// matched against an entry's archive-relative slash path (e.g. "dev",
// "var/lib/x"); a matching directory prunes its whole subtree.
type ExcludeProvider interface {
	Excludes(root string) []*regexp.Regexp
}

// osRootDirs are the top-level directories expected on an OS root filesystem.
var osRootDirs = []string{"etc", "dev", "proc", "sys", "usr", "var"}

// IsOSRoot reports whether the tree at root looks like an OS root partition: all
// of osRootDirs exist as directories and /etc/os-release is present as a file.
func IsOSRoot(root string) bool {
	if fi, err := os.Stat(filepath.Join(root, "etc", "os-release")); err != nil || fi.IsDir() {
		return false
	}
	for _, d := range osRootDirs {
		fi, err := os.Stat(filepath.Join(root, d))
		if err != nil || !fi.IsDir() {
			return false
		}
	}
	return true
}

// DefaultAgentStateDir is where the agent keeps its local state. It mirrors
// state.Dir and is spelled here rather than imported to keep this package free
// of a dependency on the agent.
const DefaultAgentStateDir = "/var/lib/aiha"

// AgentStateDir is the directory whose contents are excluded from a root
// partition backup. It holds this system's identity and its framework
// credentials.
//
// It is settable because the agent's state directory is configurable: were
// this fixed while the agent wrote elsewhere, the exclusion would quietly stop
// covering the credentials and they would travel inside every archive. The
// agent sets it from the same configuration it stores its state under.
func (p *PartitionSource) SetAgentStateDir(dir string) {
	p.agentStateDir = dir
}

// ExcludedPaths implements ExcludedPathProvider, naming the agent's state
// directory so that a restore knows its absence from the archive is intended.
func (p *PartitionSource) ExcludedPaths() []string {
	dir := p.agentStateDir
	if dir == "" {
		dir = DefaultAgentStateDir
	}
	return []string{filepath.Clean(dir)}
}

// agentStateExclude returns the pattern excluding the agent's state, as an
// archive-relative path. An absolute path outside the partition being backed
// up cannot appear in the archive, so it yields no pattern.
func agentStateExclude(dir string) *regexp.Regexp {
	if dir == "" {
		dir = DefaultAgentStateDir
	}
	rel := strings.Trim(filepath.ToSlash(filepath.Clean(dir)), "/")
	if rel == "" || rel == "." {
		return nil
	}
	return regexp.MustCompile(`^` + regexp.QuoteMeta(rel) + `/`)
}

// Excludes returns the default exclude patterns for a partition backup. Our own
// transient mount directories under /mnt are excluded whole (avoiding
// self-inclusion and leftovers from crashed runs). For an OS root partition the
// pseudo filesystems /dev, /proc, /sys and /run have only their *contents*
// excluded — the mount-point directories themselves are kept, so the restored
// system still has them to mount onto.
//
// The agent's own state is excluded on the same terms, and for a reason worth
// stating: it holds this system's identity and, once registration exists, its
// credentials. A system restored from an archive containing them would come up
// as a second copy of the original — able to authenticate as it and to take
// the work assigned to it. Keeping them out of the archive is the cheapest
// place to prevent that; the restore procedure removes them again for archives
// taken before this.
func (p *PartitionSource) Excludes(root string) []*regexp.Regexp {
	excludes := []*regexp.Regexp{
		regexp.MustCompile(`^mnt/` + regexp.QuoteMeta(MountPrefix)),
	}
	if IsOSRoot(root) {
		for _, d := range []string{"dev", "proc", "sys", "run"} {
			// "^dev/" matches the directory's contents but not the directory
			// entry itself, keeping the empty mount point in the archive.
			excludes = append(excludes, regexp.MustCompile(`^`+d+`/`))
		}
		if agentState := agentStateExclude(p.agentStateDir); agentState != nil {
			excludes = append(excludes, agentState)
		}
	}
	return excludes
}
