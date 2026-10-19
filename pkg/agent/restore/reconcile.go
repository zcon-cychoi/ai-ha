package restore

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The functions here rewrite the restored system's configuration so that it
// boots on the target. Each takes a file's contents and returns the new
// contents and whether anything changed, so that they can be tested without a
// disk; rewriteFile applies one to a file.
//
// The restore recreates filesystems with the source's UUIDs, so the UUIDs in
// the restored configuration should already be right. It runs the UUID
// rewrites only as checks — a rewrite that would change something means a
// value is wrong — and leaves the files alone. The rewrites are kept for a
// restore onto a different layout, such as a migration, where the UUIDs will
// not survive.

// fsUUIDPattern matches a filesystem UUID as ext4 and xfs report it.
var fsUUIDPattern = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)

// rewriteFstab points every "UUID=" entry for a restored mount point at the
// filesystem's new UUID. newUUIDs maps a mount point to its new UUID. Other
// entries, comments and spacing are kept as they are; a LABEL= entry still
// matches because the labels were recreated.
func rewriteFstab(content string, newUUIDs map[string]string) (string, bool) {
	lines := strings.SplitAfter(content, "\n")
	changed := false
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") || !strings.HasPrefix(fields[0], "UUID=") {
			continue
		}
		uuid, ok := newUUIDs[fields[1]]
		if !ok {
			continue
		}
		replaced := strings.Replace(line, fields[0], "UUID="+uuid, 1)
		if replaced != line {
			lines[i] = replaced
			changed = true
		}
	}
	return strings.Join(lines, ""), changed
}

// macPattern matches a netplan "macaddress:" value and captures the address.
var macPattern = regexp.MustCompile(`(?i)macaddress:\s*["']?([0-9a-f]{2}(?::[0-9a-f]{2}){5})`)

// netplanMACs returns the distinct MAC addresses, lowercased, that netplan
// files match interfaces on.
func netplanMACs(files map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, content := range files {
		for _, m := range macPattern.FindAllStringSubmatch(content, -1) {
			mac := strings.ToLower(m[1])
			if !seen[mac] {
				seen[mac] = true
				out = append(out, mac)
			}
		}
	}
	return out
}

// rewriteNetplan replaces every occurrence of oldMAC with newMAC, ignoring case.
func rewriteNetplan(content, oldMAC, newMAC string) (string, bool) {
	re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(oldMAC))
	out := re.ReplaceAllLiteralString(content, newMAC)
	return out, out != content
}

var (
	// assignmentPattern matches a shell variable assignment in a grub
	// defaults file, capturing the name and the value as written.
	assignmentPattern = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
	// partUUIDValuePattern matches a value that is nothing but a partition or
	// filesystem identifier: a UUID, or an MBR PARTUUID such as "1a2b3c4d-01".
	partUUIDValuePattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|[0-9a-fA-F]{8}-[0-9a-fA-F]{2})$`)
	// pinnedArgPattern matches a kernel argument that names a device by a
	// fixed identifier, e.g. "root=UUID=..." or "resume=/dev/disk/by-uuid/...".
	pinnedArgPattern = regexp.MustCompile(`^\S+=(?:UUID=|PARTUUID=|/dev/disk/by-uuid/|/dev/disk/by-partuuid/)\S*$`)
)

// pinnedID is a disk identifier a grub defaults file fixes, and where.
type pinnedID struct {
	// where names the setting, e.g. "GRUB_FORCE_PARTUUID" or
	// "GRUB_CMDLINE_LINUX_DEFAULT root=UUID=...".
	where string
	value string
}

// grubPinnedIDs returns the disk identifiers a grub defaults file fixes: a
// variable whose whole value is one, and kernel arguments naming a device by
// one in GRUB_CMDLINE_LINUX*. These are what cleanGrubDefaults would remove.
func grubPinnedIDs(content string) []pinnedID {
	var out []pinnedID
	for line := range strings.Lines(content) {
		m := assignmentPattern.FindStringSubmatch(strings.TrimRight(line, "\r\n"))
		if m == nil {
			continue
		}
		name := m[1]
		_, value := splitQuoted(m[2])
		if partUUIDValuePattern.MatchString(value) {
			out = append(out, pinnedID{where: name, value: value})
			continue
		}
		if !strings.HasPrefix(name, "GRUB_CMDLINE_LINUX") {
			continue
		}
		for arg := range strings.FieldsSeq(value) {
			if !pinnedArgPattern.MatchString(arg) {
				continue
			}
			id := arg[strings.Index(arg, "=")+1:]
			for _, prefix := range []string{"UUID=", "PARTUUID=", "/dev/disk/by-uuid/", "/dev/disk/by-partuuid/"} {
				if strings.HasPrefix(id, prefix) {
					id = strings.TrimPrefix(id, prefix)
					break
				}
			}
			out = append(out, pinnedID{where: name + " " + arg, value: id})
		}
	}
	return out
}

// cleanGrubDefaults removes identifiers of the source's disks from a grub
// defaults file: a variable whose whole value is such an identifier (Ubuntu
// cloud images set GRUB_FORCE_PARTUUID this way) is deleted, and kernel
// arguments naming a device by one are dropped from GRUB_CMDLINE_LINUX*.
// It returns the removed items for the log.
//
// The restore does not call it while UUIDs are preserved; it checks them with
// grubPinnedIDs instead. It is kept for a restore whose target disks do not
// carry the source's identifiers.
func cleanGrubDefaults(content string) (string, []string) {
	lines := strings.SplitAfter(content, "\n")
	var out []string
	var removed []string
	for _, line := range lines {
		m := assignmentPattern.FindStringSubmatch(strings.TrimRight(line, "\r\n"))
		if m == nil {
			out = append(out, line)
			continue
		}
		name, raw := m[1], m[2]
		quote, value := splitQuoted(raw)

		if partUUIDValuePattern.MatchString(value) {
			removed = append(removed, name)
			continue
		}
		if !strings.HasPrefix(name, "GRUB_CMDLINE_LINUX") {
			out = append(out, line)
			continue
		}

		var kept []string
		dropped := false
		for arg := range strings.FieldsSeq(value) {
			if pinnedArgPattern.MatchString(arg) {
				removed = append(removed, name+": "+arg)
				dropped = true
				continue
			}
			kept = append(kept, arg)
		}
		if !dropped {
			out = append(out, line)
			continue
		}
		newline := ""
		if strings.HasSuffix(line, "\n") {
			newline = "\n"
		}
		prefix := line[:strings.Index(line, name+"=")+len(name)+1]
		out = append(out, prefix+quote+strings.Join(kept, " ")+quote+newline)
	}
	return strings.Join(out, ""), removed
}

// splitQuoted returns the quote character around a shell value, if any, and
// the value inside it.
func splitQuoted(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if len(raw) >= 2 && (raw[0] == '"' || raw[0] == '\'') && raw[len(raw)-1] == raw[0] {
		return raw[:1], raw[1 : len(raw)-1]
	}
	return "", raw
}

// rewriteEFIStub points a grub stub configuration on the EFI system partition
// at the boot partition's new UUID. A stub only locates the real grub.cfg —
// it has a configfile line and no menu entries — and its search line names the
// partition by UUID. A full grub.cfg is left alone; update-grub rewrites it.
func rewriteEFIStub(content, bootUUID string) (string, bool) {
	if !strings.Contains(content, "configfile") || strings.Contains(content, "menuentry") {
		return content, false
	}
	lines := strings.SplitAfter(content, "\n")
	changed := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "search") ||
			(!strings.Contains(trimmed, "fs_uuid") && !strings.Contains(trimmed, "--fs-uuid")) {
			continue
		}
		replaced := fsUUIDPattern.ReplaceAllLiteralString(line, bootUUID)
		if replaced != line {
			lines[i] = replaced
			changed = true
		}
	}
	return strings.Join(lines, ""), changed
}

// rewriteFile applies edit to the file at path and replaces it, keeping its
// permissions, if anything changed. It reports whether it did.
func rewriteFile(path string, edit func(string) (string, bool)) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	out, changed := edit(string(data))
	if !changed {
		return false, nil
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".aiha-*")
	if err != nil {
		return false, fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once renamed

	if _, err := tmp.WriteString(out); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("set mode on %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return false, fmt.Errorf("replace %s: %w", path, err)
	}
	return true, nil
}
