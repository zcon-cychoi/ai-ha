//go:build rescue

package cmd

// This is the rescue build. The rescue environment downloads it over the
// network into a RAM-backed filesystem, so it leaves out the backup engine and
// every repository backend: restoring reads the repository through rclone,
// which the rescue image already carries.
const (
	binaryName = "aiha-rescue"
	rootShort  = "AI-HA rescue agent"
	rootLong   = `aiha-rescue runs inside the rescue environment booted for a restore. It claims
the instance's restore job from the framework, then restores the backup set onto
this machine's disks and reports progress and the result.

It is the same program as aiha-agent, built without the backup engine so that
it stays small enough to download into RAM.`
)
