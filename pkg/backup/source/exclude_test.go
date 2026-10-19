package source

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func matchAny(patterns []*regexp.Regexp, s string) bool {
	for _, re := range patterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func assertMatches(t *testing.T, patterns []*regexp.Regexp, paths []string) {
	t.Helper()
	for _, p := range paths {
		if !matchAny(patterns, p) {
			t.Errorf("expected %q to match an exclude pattern", p)
		}
	}
}

func assertNoMatch(t *testing.T, patterns []*regexp.Regexp, paths []string) {
	t.Helper()
	for _, p := range paths {
		if matchAny(patterns, p) {
			t.Errorf("expected %q NOT to match any exclude pattern", p)
		}
	}
}

// makeRootTree builds a minimal OS-root-looking tree and returns its path.
func makeRootTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range osRootDirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "os-release"), []byte("ID=test\n"), 0o644); err != nil {
		t.Fatalf("write os-release: %v", err)
	}
	return root
}

func TestIsOSRoot(t *testing.T) {
	root := makeRootTree(t)
	if !IsOSRoot(root) {
		t.Fatal("expected a complete root tree to be detected as OS root")
	}

	// Missing /etc/os-release.
	noRelease := makeRootTree(t)
	if err := os.Remove(filepath.Join(noRelease, "etc", "os-release")); err != nil {
		t.Fatal(err)
	}
	if IsOSRoot(noRelease) {
		t.Fatal("tree without /etc/os-release should not be OS root")
	}

	// Missing a required directory.
	noVar := makeRootTree(t)
	if err := os.RemoveAll(filepath.Join(noVar, "var")); err != nil {
		t.Fatal(err)
	}
	if IsOSRoot(noVar) {
		t.Fatal("tree without /var should not be OS root")
	}

	// A plain data directory.
	if IsOSRoot(t.TempDir()) {
		t.Fatal("empty dir should not be OS root")
	}
}

func TestPartitionExcludes(t *testing.T) {
	p, err := NewPartition("/dev/sdX1", "sdX1")
	if err != nil {
		t.Fatalf("new partition: %v", err)
	}

	// OS root: pseudo filesystem *contents* + our mount dir (whole).
	root := makeRootTree(t)
	got := p.Excludes(root)
	// Contents under dev/proc/sys/run and our mount dir match.
	assertMatches(t, got, []string{"dev/null", "proc/1/status", "sys/kernel", "run/lock", "mnt/aiha-mount-abc"})
	// The pseudo-fs directory entries themselves are kept, as are unrelated
	// paths and same-named nested dirs.
	assertNoMatch(t, got, []string{"dev", "proc", "sys", "run", "etc/hostname", "var/log/dev", "home/user/proc"})

	// Non-root: only the mount prefix is excluded.
	plain := t.TempDir()
	got2 := p.Excludes(plain)
	assertMatches(t, got2, []string{"mnt/aiha-mount-xyz"})
	assertNoMatch(t, got2, []string{"dev", "proc", "sys", "run"})
}

// The agent's state holds this system's identity and, in time, its
// credentials. An archive carrying them would let a restored copy authenticate
// as the original, so the contents must never enter the archive.
func TestExcludesOmitsAgentStateFromAnOSRoot(t *testing.T) {
	root := makeRootTree(t)
	patterns := (&PartitionSource{}).Excludes(root)

	assertMatches(t, patterns, []string{
		"var/lib/aiha/system-id",
		"var/lib/aiha/agent-token",
		"var/lib/aiha/recovery-pending.json",
		"var/lib/aiha/nested/deeper/secret",
	})
	// The directory entry itself stays, as it does for the pseudo
	// filesystems, so the restored system still has the path.
	assertNoMatch(t, patterns, []string{
		"var/lib/aiha",
		"var/lib/aihaXother/file",
		"var/lib/other/file",
		"var/lib",
	})
}

// The agent's state directory is configurable. If the exclusion stayed pinned
// to the default while the agent wrote elsewhere, the credentials it holds
// would silently travel inside every archive.
func TestExcludesFollowsAConfiguredAgentStateDir(t *testing.T) {
	root := makeRootTree(t)

	src := &PartitionSource{}
	src.SetAgentStateDir("/opt/aiha/state")
	patterns := src.Excludes(root)

	assertMatches(t, patterns, []string{
		"opt/aiha/state/agent-token",
		"opt/aiha/state/system-id",
	})
	// The default is no longer what is protected, because it is no longer
	// where the agent writes.
	assertNoMatch(t, patterns, []string{
		"var/lib/aiha/agent-token",
		"opt/aiha/state",
	})

	// And the manifest must name the path that was actually excluded.
	declared := src.ExcludedPaths()
	if len(declared) != 1 || declared[0] != "/opt/aiha/state" {
		t.Fatalf("ExcludedPaths() = %v, want [/opt/aiha/state]", declared)
	}
}

// A source left on the default protects the default.
func TestExcludesDefaultsToTheAgentStateDir(t *testing.T) {
	root := makeRootTree(t)
	src := &PartitionSource{}

	assertMatches(t, src.Excludes(root), []string{"var/lib/aiha/agent-token"})
	if declared := src.ExcludedPaths(); len(declared) != 1 || declared[0] != DefaultAgentStateDir {
		t.Fatalf("ExcludedPaths() = %v, want [%s]", declared, DefaultAgentStateDir)
	}
}

// A backup of an arbitrary directory must not silently drop a path that merely
// shares the name. The exclusion is a property of an OS root, not of the name.
func TestExcludesLeavesAgentStateAloneOutsideAnOSRoot(t *testing.T) {
	plain := t.TempDir()
	if err := os.MkdirAll(filepath.Join(plain, "var", "lib", "aiha"), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	patterns := (&PartitionSource{}).Excludes(plain)

	assertNoMatch(t, patterns, []string{
		"var/lib/aiha/system-id",
		"dev/null",
		"proc/1/status",
	})
}
