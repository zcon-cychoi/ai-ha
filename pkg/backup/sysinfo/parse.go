package sysinfo

import (
	"bufio"
	"encoding/json"
	"io"
	"slices"
	"strings"
)

// The parsers here take an io.Reader and return a value. Keeping them free of
// syscalls lets them be tested on any platform against captured output, the
// same split source.findMountPoint uses.

// parseOSRelease reads the shell-fragment format of /etc/os-release: KEY=value
// lines where the value may be quoted. Unparsable lines are skipped rather
// than reported, since the file is informational.
func parseOSRelease(r io.Reader) (OSRelease, error) {
	var (
		out     = OSRelease{Fields: map[string]string{}}
		raw     strings.Builder
		scanner = bufio.NewScanner(r)
	)
	for scanner.Scan() {
		line := scanner.Text()
		raw.WriteString(line)
		raw.WriteByte('\n')

		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		out.Fields[strings.TrimSpace(key)] = unquote(strings.TrimSpace(value))
	}
	if err := scanner.Err(); err != nil {
		return OSRelease{}, err
	}
	out.Raw = raw.String()
	out.ID = out.Fields["ID"]
	out.VersionID = out.Fields["VERSION_ID"]
	return out, nil
}

// unquote strips one layer of matching single or double quotes.
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// parseFstab reads /etc/fstab. Fields are whitespace-separated; a line needs
// at least spec, mount point, type and options to be usable.
func parseFstab(r io.Reader) (Fstab, error) {
	var (
		out     Fstab
		raw     strings.Builder
		scanner = bufio.NewScanner(r)
	)
	for scanner.Scan() {
		line := scanner.Text()
		raw.WriteString(line)
		raw.WriteByte('\n')

		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		f := strings.Fields(trimmed)
		if len(f) < 4 {
			continue
		}
		entry := FstabEntry{
			// fstab escapes spaces in paths as \040; undo that so the value
			// is a real path.
			Spec:       unescapeMount(f[0]),
			MountPoint: unescapeMount(f[1]),
			FSType:     f[2],
			Options:    f[3],
		}
		if len(f) > 4 {
			entry.Dump = f[4]
		}
		if len(f) > 5 {
			entry.Pass = f[5]
		}
		out.Entries = append(out.Entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return Fstab{}, err
	}
	out.Raw = raw.String()
	return out, nil
}

// parseMountinfo reads /proc/self/mountinfo.
//
// The format is: mount ID, parent ID, major:minor, root, mount point, mount
// options, then zero or more optional fields, a "-" separator, and finally the
// filesystem type and mount source. The optional fields are why the tail has
// to be located by the separator rather than by a fixed index.
func parseMountinfo(r io.Reader) ([]Mount, error) {
	var (
		out     []Mount
		scanner = bufio.NewScanner(r)
	)
	// Mount points can be long; give the scanner room beyond its 64K default.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		sep := slices.Index(fields, "-")
		if sep < 0 || sep+2 >= len(fields) || sep < 6 {
			continue
		}
		out = append(out, Mount{
			DevNo:      fields[2],
			Root:       unescapeMount(fields[3]),
			MountPoint: unescapeMount(fields[4]),
			Options:    fields[5],
			FSType:     fields[sep+1],
			Source:     unescapeMount(fields[sep+2]),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// diskFSTypes are the filesystems that live on a block device.
//
// A mount of anything else describes the running kernel rather than the
// system's storage. A restore has no use for tmpfs, cgroup2, or the squashfs
// of a snap, and a mount table full of them buries the handful of lines that
// matter: on a plain Ubuntu server the pseudo filesystems outnumber the real
// ones by more than ten to one.
//
// The list is deliberately wider than backupableFSTypes. A partition this
// agent cannot capture still has to reach the inventory so that judge can
// explain it — an operator has to be able to tell an exclusion from an
// absence. It is an allowlist rather than a list of things to drop because
// the set of pseudo filesystems grows with every kernel release, while the
// set of on-disk ones barely moves.
var diskFSTypes = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true,
	"xfs": true, "btrfs": true, "jfs": true, "reiserfs": true,
	"vfat": true, "exfat": true, "ntfs": true, "ntfs3": true,
	"f2fs": true,
}

// diskMounts keeps the mounts of a real filesystem on a real device.
//
// Both halves of the test are needed. The device check alone would keep the
// squashfs of every installed snap, which mounts a /dev/loop device; the
// filesystem check alone would keep a bind mount of an ext4 tree whose source
// is a directory rather than a device.
func diskMounts(mounts []Mount) []Mount {
	out := make([]Mount, 0, len(mounts))
	for _, m := range mounts {
		if strings.HasPrefix(m.Source, "/dev/") && diskFSTypes[m.FSType] {
			out = append(out, m)
		}
	}
	return out
}

// unescapeMount decodes the octal escapes the kernel and fstab use for
// characters that would otherwise split a field: space, tab, newline and
// backslash.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+3 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		if c, ok := octalByte(s[i+1 : i+4]); ok {
			b.WriteByte(c)
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// octalByte converts three octal digits to a byte.
func octalByte(s string) (byte, bool) {
	var v byte
	for i := 0; i < 3; i++ {
		if s[i] < '0' || s[i] > '7' {
			return 0, false
		}
		v = v*8 + (s[i] - '0')
	}
	return v, true
}

// diskDevices returns the whole-disk devices backing the given mount sources,
// so that a partition table dump is taken once per disk.
//
// It trims a trailing partition suffix: "/dev/sda1" and "/dev/nvme0n1p2"
// become "/dev/sda" and "/dev/nvme0n1". Sources that are not /dev paths are
// ignored, as are device-mapper and loop devices, whose partition table (if
// any) belongs to whatever backs them.
func diskDevices(mounts []Mount) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range mounts {
		disk, ok := diskOf(m.Source)
		if !ok || seen[disk] {
			continue
		}
		seen[disk] = true
		out = append(out, disk)
	}
	slices.Sort(out)
	return out
}

// diskOf reduces a partition device path to its disk.
func diskOf(source string) (string, bool) {
	const devPrefix = "/dev/"
	if !strings.HasPrefix(source, devPrefix) {
		return "", false
	}
	name := source[len(devPrefix):]
	if name == "" || strings.ContainsRune(name, '/') {
		// Nested paths are device-mapper and similar; not a plain disk.
		return "", false
	}
	switch {
	case strings.HasPrefix(name, "dm-"), strings.HasPrefix(name, "loop"),
		strings.HasPrefix(name, "md"), strings.HasPrefix(name, "zram"):
		return "", false
	}

	// How a partition is spelled depends on the device family, and the two
	// families cannot share one rule: an NVMe disk's own name ends in a digit
	// ("nvme0n1"), so stripping trailing digits would turn a whole disk into
	// a device that does not exist.
	if strings.HasPrefix(name, "nvme") || strings.HasPrefix(name, "mmcblk") {
		// Partitions carry a "p" suffix: "nvme0n1p2" -> "nvme0n1". Without
		// one the name is already a whole disk.
		base, ok := trimPartitionSuffix(name, true)
		if !ok {
			return devPrefix + name, true
		}
		return devPrefix + base, true
	}
	// sd/vd/hd/xvd and friends: "sda1" -> "sda", "sda" unchanged.
	base, ok := trimPartitionSuffix(name, false)
	if !ok {
		return devPrefix + name, true
	}
	return devPrefix + base, true
}

// trimPartitionSuffix removes a trailing partition number from a device name,
// reporting whether the name had one. With requireP the number must be
// introduced by a "p", as on NVMe and MMC devices.
func trimPartitionSuffix(name string, requireP bool) (string, bool) {
	end := len(name)
	for end > 0 && name[end-1] >= '0' && name[end-1] <= '9' {
		end--
	}
	if end == len(name) || end == 0 {
		return name, false // no trailing digits, or digits only
	}
	if requireP {
		if name[end-1] != 'p' || end-1 == 0 {
			return name, false
		}
		end--
	}
	return name[:end], true
}

// volumeGroupNames extracts the volume group names from "lvm fullreport
// --reportformat json" output.
//
// The document nests as {"report":[{"vg":[{"vg_name":"..."}], ...}]}. Only the
// vg_name fields are needed, so the rest is left undecoded: the full report is
// stored verbatim anyway, and a schema change elsewhere in it must not break
// this. A document that does not match yields no names and no error — the
// caller then simply records no per-group configuration.
func volumeGroupNames(report []byte) []string {
	var doc struct {
		Report []struct {
			VG []struct {
				Name string `json:"vg_name"`
			} `json:"vg"`
		} `json:"report"`
	}
	if err := json.Unmarshal(report, &doc); err != nil {
		return nil
	}

	seen := map[string]bool{}
	var out []string
	for _, section := range doc.Report {
		for _, vg := range section.VG {
			if vg.Name == "" || seen[vg.Name] {
				continue
			}
			seen[vg.Name] = true
			out = append(out, vg.Name)
		}
	}
	slices.Sort(out)
	return out
}
