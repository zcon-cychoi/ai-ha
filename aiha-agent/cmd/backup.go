//go:build !rescue

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"text/tabwriter"

	"github.com/cloud-barista/ai-ha/pkg/agent/state"
	"github.com/cloud-barista/ai-ha/pkg/backup/engine"
	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/repository"
	"github.com/cloud-barista/ai-ha/pkg/backup/source"
	"github.com/cloud-barista/ai-ha/pkg/backup/spool"
	"github.com/cloud-barista/ai-ha/pkg/filelock"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var (
	backupDevices        []string
	backupName           string
	backupCompression    string
	backupExcludes       []string
	backupIgnoreRecovery bool
	backupStateDir       string
)

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Back up Linux partitions to the repository",
	Long: `Back up one or more Linux partitions as a single backup set. Each partition is
bind-mounted read-only into a temporary directory, archived with tar,
compressed (zstd by default) and streamed into the repository. Requires root
privileges.

Pass --device once per partition that makes up the system. The partitions of a
machine are only restorable together, so capture them in one run:

  aiha-agent backup --device /dev/nvme0n1p1 --device /dev/nvme0n1p16

The set is written with a manifest recording the mount points, partition
identifiers, firmware mode and fstab that a restore needs. A partition that
fails does not abandon the rest; the manifest records which one failed.`,
	PreRunE: func(cmd *cobra.Command, args []string) error {
		if err := checkRecoveryPending(); err != nil {
			return err
		}
		return loadConfig(cmd, args)
	},
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()

		system, err := resolveSystem()
		if err != nil {
			return err
		}

		// Serialised with framework jobs, so that a backup started by hand and
		// one the framework asked for cannot run over each other.
		lock, err := filelock.Acquire(ctx, filepath.Join(backupStateDir, "work.lock"), 0)
		if err != nil {
			if errors.Is(err, filelock.ErrLocked) {
				return errors.New("another backup is running on this host")
			}
			return err
		}
		defer lock.Release()

		set, setErr := runBackupSet(ctx, backupRequest{
			devices:     backupDevices,
			name:        backupName,
			compression: backupCompression,
			excludes:    backupExcludes,
			stateDir:    backupStateDir,
			system:      system,
			// A backup run by hand writes to the repository configured on
			// this host; one the framework asks for gets its repository with
			// the job.
			openRepository: openRepository,
		})
		printBackupSet(cmd.OutOrStdout(), system, set)
		if setErr != nil {
			return fmt.Errorf("backup failed: %w", setErr)
		}
		return nil
	},
}

// backupRequest is everything one backup set needs, however it was asked for.
type backupRequest struct {
	devices     []string
	name        string
	compression string
	excludes    []string
	// stateDir is the agent's state directory, which the backup excludes.
	stateDir string
	system   model.SystemInfo
	// expectedFSUUIDs maps a device to the filesystem UUID that is supposed to
	// be on it. It is set for a backup the framework asked for, from the
	// inventory the job was built from, and empty when an operator named the
	// devices by hand.
	expectedFSUUIDs map[string]string
	// progress, when set, accumulates the bytes produced, for a reporter.
	progress *atomic.Int64
	// openRepository opens the repository the set is written to.
	openRepository func(ctx context.Context) (repository.Repository, error)
}

// runBackupSet captures one backup set. It is shared by the backup command and
// by framework jobs, so that the two cannot drift apart: a fix to how a backup
// is taken applies to both.
func runBackupSet(ctx context.Context, req backupRequest) (model.BackupSet, error) {
	if len(req.devices) == 0 {
		return model.BackupSet{}, errors.New("no devices to back up")
	}
	if len(req.devices) > 1 && req.name != "" {
		return model.BackupSet{}, errors.New("a partition name applies to a single device")
	}

	sources := make([]source.Source, 0, len(req.devices))
	for _, device := range req.devices {
		src, err := source.NewPartition(device, req.name)
		if err != nil {
			return model.BackupSet{}, err
		}
		// The exclusion has to follow wherever the agent actually keeps its
		// state, or a moved directory would ship its credentials inside the
		// archive.
		src.SetAgentStateDir(req.stateDir)
		sources = append(sources, src)
	}

	// Checked before anything is opened or written. A device that has gone
	// would otherwise fail when the engine reached it, after the partitions
	// before it had already been uploaded — leaving a failed set that cost a
	// full transfer. The engine captures every source it is given and only
	// then reports the set as failed, so the place to refuse is here.
	if err := checkDevices(ctx, sources, req.expectedFSUUIDs); err != nil {
		return model.BackupSet{}, err
	}

	repo, err := req.openRepository(ctx)
	if err != nil {
		return model.BackupSet{}, err
	}
	defer repo.Close()

	eng := engine.New(repo)
	eng.Progress = req.progress
	eng.Resume = spool.Config{
		ChunkSize:         cfg.Resume.ChunkSizeBytes,
		BufferCap:         cfg.Resume.BufferCapBytes,
		DisconnectTimeout: cfg.Resume.DisconnectTimeout,
	}
	for _, pat := range req.excludes {
		re, err := regexp.Compile(pat)
		if err != nil {
			return model.BackupSet{}, fmt.Errorf("invalid exclude pattern %q: %w", pat, err)
		}
		eng.Excludes = append(eng.Excludes, re)
	}

	return eng.BackupSet(ctx, req.system, sources, model.CompressionType(req.compression))
}

// checkRecoveryPending refuses to back up from a system that was restored and
// has not had its boot verified.
//
// Such a system booted from a backup image, so it holds a copy of the original
// system's identity. Backing up under that identity would write into the
// original system's history. The framework clears the marker once it has
// verified the boot and issued new credentials.
func checkRecoveryPending() error {
	pending, err := state.ReadRecoveryPending(backupStateDir)
	if err != nil {
		return err
	}
	if pending == nil {
		return nil
	}
	if backupIgnoreRecovery {
		// Never log the marker's boot-verify token.
		log.Warn().
			Str("recoveryId", pending.RecoveryID).
			Msg("Proceeding on a restored system whose boot is not verified")
		return nil
	}
	return fmt.Errorf(
		"this system was restored (recovery %s) and its boot is not verified yet, "+
			"so it still holds the original system's identity: complete registration "+
			"first, or pass --ignore-recovery-pending to override",
		pending.RecoveryID)
}

func init() {
	backupCmd.Flags().StringArrayVar(&backupDevices, "device", nil, "partition device to back up, e.g. /dev/sda1 (required; repeat for each partition of the system)")
	backupCmd.Flags().StringVar(&backupName, "name", "", "partition label used in the file name (derived from device when empty; only with a single --device)")
	backupCmd.Flags().StringVar(&backupCompression, "compression", "", "compression codec: zstd (default), none")
	backupCmd.Flags().StringArrayVar(&backupExcludes, "exclude", nil, "regex of archive-relative paths to exclude (repeatable); merged with source defaults")
	backupCmd.Flags().BoolVar(&backupIgnoreRecovery, "ignore-recovery-pending", false, "back up even when this system was restored and its boot is not verified")
	backupCmd.Flags().StringVar(&backupStateDir, "state-dir", state.Dir, "directory holding the agent's local state")
	_ = backupCmd.MarkFlagRequired("device")
	addRepoFlag(backupCmd)
	rootCmd.AddCommand(backupCmd)
}

// printBackupSet reports what the set captured, including any item that failed.
func printBackupSet(w io.Writer, system model.SystemInfo, set model.BackupSet) {
	if set.ID == "" {
		return
	}
	fmt.Fprintf(w, "Backup set %s (%s)\n", set.ID, set.Status)
	fmt.Fprintf(w, "System:     %s\n", system.ID)
	fmt.Fprintf(w, "Manifest:   %s\n", model.ManifestName(set.ID))

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PARTITION\tMOUNT\tFILE\tSIZE\tSTATUS")
	for _, item := range set.Items {
		mount := item.MountPoint
		if mount == "" {
			mount = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n",
			item.Partition, mount, item.FileName, item.SizeBytes, item.Status)
		if item.Message != "" {
			fmt.Fprintf(tw, "\t\t%s\t\t\n", item.Message)
		}
	}
	_ = tw.Flush()
}
