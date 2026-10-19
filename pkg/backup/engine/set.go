package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/backup/meta"
	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/source"
	"github.com/cloud-barista/ai-ha/pkg/backup/sysinfo"
	"github.com/rs/zerolog/log"
)

// AgentVersion is recorded in every manifest so that a capture which turns out
// to be wrong can be traced to the build that produced it. The CLI sets it.
var AgentVersion string

// BackupSet archives several partitions as one capture and writes the
// manifest that a restore reads.
//
// The partitions of a machine are only restorable together: a system whose
// /boot and / are separate is not recoverable from either alone. Capturing
// them as a set records that relationship, and the manifest records the system
// facts — partition identifiers, mount points, firmware, fstab — that the
// archives themselves do not carry and that cannot be recovered later, since
// by restore time the source machine may be gone.
//
// A failed item does not abandon the set. The remaining partitions are still
// captured and the manifest records which item failed and why, because a
// partial capture that says so is worth more than no capture at all.
func (e *Engine) BackupSet(ctx context.Context, system model.SystemInfo, sources []source.Source, comp model.CompressionType) (model.BackupSet, error) {
	if len(sources) == 0 {
		return model.BackupSet{}, fmt.Errorf("backup set: no sources given")
	}
	if system.DirName() == "" {
		return model.BackupSet{}, fmt.Errorf("backup set: the system has no identifier")
	}

	excluded := excludedPaths(sources)

	set := model.BackupSet{
		ID:        model.NewID(),
		SystemID:  system.ID,
		Hostname:  system.Hostname,
		Status:    model.BackupRunning,
		CreatedAt: time.Now().UTC(),
	}
	log.Info().Str("setId", set.ID).Str("systemId", system.ID).
		Int("items", len(sources)).Msg("Starting backup set")

	// Collected before the archives so that the manifest describes the system
	// as it was when the capture began.
	sys := collectSystemInfo(ctx)

	var firstErr error
	for _, src := range sources {
		item, err := e.backupItem(ctx, system, src, comp)
		set.Items = append(set.Items, item)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	completed := time.Now().UTC()
	set.CompletedAt = &completed
	if firstErr != nil {
		set.Status = model.BackupFailed
		set.Message = firstErr.Error()
	} else {
		set.Status = model.BackupCompleted
	}

	// The manifest is written even for a failed set: it is the only record of
	// what was attempted and what succeeded. Written with WithoutCancel so
	// that a cancelled backup still leaves one behind.
	if err := e.writeManifest(context.WithoutCancel(ctx), system, set, sys, excluded); err != nil {
		log.Error().Err(err).Str("setId", set.ID).Msg("Failed to write the backup set manifest")
		if firstErr == nil {
			firstErr = err
		}
	}
	return set, firstErr
}

// backupItem captures one partition and describes it for the manifest.
func (e *Engine) backupItem(ctx context.Context, system model.SystemInfo, src source.Source, comp model.CompressionType) (model.BackupItem, error) {
	item := model.BackupItem{
		ID:        model.NewID(),
		Partition: src.Name(),
	}
	// Where the partition sits is read before the backup, while it is still
	// mounted in its own right rather than through our bind mount.
	if d, ok := src.(source.Describer); ok {
		item.Device = d.Device()
		item.MountPoint = d.MountPoint(ctx)
	}

	rec, err := e.Backup(ctx, system, src, comp)
	item.FileName = rec.FileName
	item.Compression = rec.Compression
	item.SizeBytes = rec.SizeBytes
	item.Status = rec.Status
	item.Message = rec.Message
	item.CreatedAt = rec.CreatedAt
	item.CompletedAt = rec.CompletedAt
	if err != nil {
		return item, fmt.Errorf("back up %s: %w", src.Name(), err)
	}
	return item, nil
}

// collectSystemInfo gathers the system facts for the manifest. A failure here
// costs the manifest a section, never the backup.
func collectSystemInfo(ctx context.Context) json.RawMessage {
	info, err := sysinfo.Collect(ctx, nil)
	if err != nil {
		log.Warn().Err(err).Msg("Could not collect system information for the manifest")
		return nil
	}
	for _, e := range info.Errors {
		log.Warn().Str("item", e.Item).Str("reason", e.Err).
			Msg("A system information item could not be collected")
	}
	raw, err := json.Marshal(info)
	if err != nil {
		log.Warn().Err(err).Msg("Could not encode system information for the manifest")
		return nil
	}
	return raw
}

// writeManifest stores the set's manifest in the repository next to its
// archives, under a name derived from the set id.
func (e *Engine) writeManifest(ctx context.Context, system model.SystemInfo, set model.BackupSet, sys json.RawMessage, excluded []string) error {
	manifest := model.Manifest{
		Version:      model.ManifestVersion,
		AgentVersion: AgentVersion,
		Set:          set,
		System:       sys,
		// Recorded so that a restore knows the absence is deliberate: the
		// agent's identity and credentials are kept out of every archive.
		ExcludedPaths: excluded,
	}

	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}

	w, err := e.repo.NewWriter(ctx, system, model.ManifestName(set.ID))
	if err != nil {
		return fmt.Errorf("create manifest: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		w.Close()
		return fmt.Errorf("write manifest: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finalize manifest: %w", err)
	}

	// The set is recorded in the index too, so that listing does not have to
	// read every manifest. The manifest remains the source of truth.
	if err := e.repo.UpdateMeta(ctx, system, func(ctx context.Context, store meta.Store) error {
		return store.UpsertBackupSet(ctx, set)
	}); err != nil {
		return fmt.Errorf("record backup set: %w", err)
	}
	return nil
}

// excludedPaths collects what the sources deliberately left out, so that the
// manifest names the paths actually excluded rather than assuming them.
func excludedPaths(sources []source.Source) []string {
	seen := map[string]bool{}
	var out []string
	for _, src := range sources {
		p, ok := src.(source.ExcludedPathProvider)
		if !ok {
			continue
		}
		for _, path := range p.ExcludedPaths() {
			if path == "" || seen[path] {
				continue
			}
			seen[path] = true
			out = append(out, path)
		}
	}
	slices.Sort(out)
	return out
}
