// Package engine orchestrates a backup/restore: it wires a Source, the tar
// Archiver, a compression codec and a Repository together, and records the
// result in the repository's per-system metadata store.
package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/backup/archive"
	"github.com/cloud-barista/ai-ha/pkg/backup/compress"
	"github.com/cloud-barista/ai-ha/pkg/backup/meta"
	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/repository"
	"github.com/cloud-barista/ai-ha/pkg/backup/source"
	"github.com/cloud-barista/ai-ha/pkg/backup/spool"
	"github.com/rs/zerolog/log"
)

// Engine coordinates backups against a repository.
type Engine struct {
	repo     repository.Repository
	archiver archive.Archiver
	// Resume tunes the chunked, resumable transfer used when the repository
	// supports it (repository.ResumableRepository). Defaults from spool.
	Resume spool.Config
	// Excludes are user-supplied regexps matched against archive-relative paths;
	// they are merged with any excludes the source provides.
	Excludes []*regexp.Regexp
	// Progress, when set, accumulates the bytes of compressed archive produced,
	// across every partition of a set. It is read concurrently by whoever is
	// reporting on the backup, which is why it is atomic.
	//
	// It counts what the compressor produces rather than what the repository
	// has acknowledged: on the resumable path those differ while chunks wait
	// to upload. That is the one measure the first year reports.
	Progress *atomic.Int64
}

// New returns an Engine backed by repo, using the tar archiver.
func New(repo repository.Repository) *Engine {
	return &Engine{repo: repo, archiver: archive.NewTar(), Resume: spool.DefaultConfig()}
}

// Backup archives src, streams it through the chosen codec into the repository
// and records the outcome. The returned record reflects the final state.
//
// system identifies who is being backed up. It is supplied by the caller
// rather than read from the host here: which identity this installation has is
// the agent's business, and a repository must not silently file backups under
// a hostname that a cloud provider reuses.
func (e *Engine) Backup(ctx context.Context, system model.SystemInfo, src source.Source, comp model.CompressionType) (model.BackupRecord, error) {
	if system.DirName() == "" {
		return model.BackupRecord{}, fmt.Errorf("backup: the system has no identifier")
	}
	if comp == "" {
		comp = model.DefaultCompression
	}
	compressor, err := compress.Get(comp)
	if err != nil {
		return model.BackupRecord{}, err
	}

	// The file name stays human-readable, so it uses the hostname; the
	// directory it lands in is keyed by the identifier.
	fileLabel := system.Hostname
	if fileLabel == "" {
		fileLabel = system.ID
	}

	rec := model.BackupRecord{
		ID:          model.NewID(),
		Hostname:    system.Hostname,
		Partition:   src.Name(),
		FileName:    model.BackupFileName(fileLabel, src.Name()),
		Compression: comp,
		Scheme:      e.repo.Scheme(),
		Status:      model.BackupRunning,
		CreatedAt:   time.Now().UTC(),
	}

	if err := e.repo.EnsureSystem(ctx, system); err != nil {
		return rec, err
	}

	// The metadata is opened and closed around each write rather than held for
	// the whole backup, so a long-running backup cannot finish holding a stale
	// copy and overwrite records written by another partition's backup.
	if err := e.repo.UpdateMeta(ctx, system, func(ctx context.Context, store meta.Store) error {
		if err := store.UpsertSystem(ctx, system); err != nil {
			return err
		}
		return store.InsertBackup(ctx, rec)
	}); err != nil {
		return rec, err
	}

	size, backupErr := e.runBackup(ctx, src, compressor, system, rec.FileName)

	completed := time.Now().UTC()
	rec.CompletedAt = &completed
	rec.SizeBytes = size
	if backupErr != nil {
		rec.Status = model.BackupFailed
		rec.Message = backupErr.Error()
	} else {
		rec.Status = model.BackupCompleted
	}

	// Record the outcome even when the backup's context was cancelled.
	if err := e.repo.UpdateMeta(context.WithoutCancel(ctx), system, func(ctx context.Context, store meta.Store) error {
		return store.UpdateBackup(ctx, rec)
	}); err != nil {
		log.Error().Err(err).Str("backupId", rec.ID).Msg("Failed to update backup record")
	}
	return rec, backupErr
}

// runBackup performs the streaming pipeline: source -> tar -> codec -> repo,
// returning the number of (compressed) bytes written to the repository.
func (e *Engine) runBackup(ctx context.Context, src source.Source, compressor compress.Compressor, system model.SystemInfo, filename string) (int64, error) {
	root, cleanup, err := src.Prepare(ctx)
	if err != nil {
		return 0, fmt.Errorf("prepare source: %w", err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			log.Error().Err(err).Msg("Failed to clean up backup source")
		}
	}()

	excludes := e.buildExcludes(src, root)

	// Prefer the chunked, resumable path when the repository supports it, so a
	// transient network interruption does not force restarting the backup.
	if rr, ok := e.repo.(repository.ResumableRepository); ok {
		mountPoint := ""
		if d, ok := src.(source.Describer); ok {
			mountPoint = d.MountPoint(ctx)
		}
		return e.runResumable(ctx, rr, root, mountPoint, compressor, system, filename, excludes)
	}

	w, err := e.repo.NewWriter(ctx, system, filename)
	if err != nil {
		return 0, err
	}
	counter := &countingWriter{w: w, total: e.Progress}

	cw, err := compressor.NewWriter(counter)
	if err != nil {
		_ = w.Close()
		return 0, err
	}

	log.Info().Str("partition", src.Name()).Str("file", filename).Str("compression", string(compressor.Type())).Msg("Starting backup")

	if err := e.archiver.Archive(ctx, root, cw, excludes); err != nil {
		_ = cw.Close()
		_ = w.Close()
		return counter.n, fmt.Errorf("archive: %w", err)
	}
	if err := cw.Close(); err != nil {
		_ = w.Close()
		return counter.n, fmt.Errorf("finalize compression: %w", err)
	}
	if err := w.Close(); err != nil {
		return counter.n, fmt.Errorf("finalize transfer: %w", err)
	}
	return counter.n, nil
}

// buildExcludes merges the source's own exclude patterns (e.g. a root partition
// omitting /proc, /sys, /dev, /run) with the user-configured excludes.
func (e *Engine) buildExcludes(src source.Source, root string) []*regexp.Regexp {
	var excludes []*regexp.Regexp
	if ep, ok := src.(source.ExcludeProvider); ok {
		excludes = append(excludes, ep.Excludes(root)...)
	}
	excludes = append(excludes, e.Excludes...)
	if len(excludes) > 0 {
		log.Info().Int("count", len(excludes)).Msg("Applying archive exclude patterns")
	}
	return excludes
}

// runResumable streams source -> tar -> codec into a disk-backed spool, which
// uploads the archive to the repository in resumable chunks. Returns the number
// of (compressed) bytes transferred.
//
// mountPoint is where the partition being archived is mounted, "" if unknown.
// The spool directory is excluded from the archive when it lies inside it.
func (e *Engine) runResumable(ctx context.Context, rr repository.ResumableRepository, root, mountPoint string, compressor compress.Compressor, system model.SystemInfo, filename string, excludes []*regexp.Regexp) (int64, error) {
	tmpDir, err := os.MkdirTemp("", "aiha-spool-")
	if err != nil {
		return 0, fmt.Errorf("create spool dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			log.Error().Err(err).Str("dir", tmpDir).Msg("Failed to clean up spool dir")
		}
	}()

	// Backing up the partition that holds the spool — the root, with the
	// default /tmp — would archive the spool's own chunks as they are written.
	if ex := spoolExclude(mountPoint, tmpDir); ex != nil {
		excludes = append(slices.Clip(excludes), ex)
		log.Info().Str("dir", tmpDir).Msg("Excluding the spool directory from the archive")
	}

	// Some backends require a minimum chunk size (e.g. S3 multipart parts must
	// be >= 5 MiB); raise the configured size when it is smaller.
	resume := e.Resume
	if c, ok := rr.(repository.ChunkSizeConstrainer); ok {
		if m := c.MinChunkSize(); resume.ChunkSize < m {
			log.Warn().Int64("requested", resume.ChunkSize).Int64("min", m).Msg("Raising chunk size to backend minimum")
			resume.ChunkSize = m
		}
	}

	// Checked before the sink exists, so a refusal leaves no upload begun in
	// the repository. When the repository is slower than archiving, the spool
	// fills up to its cap, and a cap beyond the free space fills the disk.
	if err := spool.CheckSpace(tmpDir, resume); err != nil {
		return 0, err
	}

	sink, err := rr.NewSink(ctx, system, filename)
	if err != nil {
		return 0, err
	}

	log.Info().Str("file", filename).Str("compression", string(compressor.Type())).
		Int64("chunkSize", resume.ChunkSize).Msg("Starting resumable backup")

	return spool.Run(ctx, tmpDir, resume, sink, func(w io.Writer) error {
		cw, err := compressor.NewWriter(&countingWriter{w: w, total: e.Progress})
		if err != nil {
			return err
		}
		if err := e.archiver.Archive(ctx, root, cw, excludes); err != nil {
			_ = cw.Close()
			return fmt.Errorf("archive: %w", err)
		}
		if err := cw.Close(); err != nil {
			return fmt.Errorf("finalize compression: %w", err)
		}
		return nil
	})
}

// Restore extracts a backup file into req.Dest.
func (e *Engine) Restore(ctx context.Context, req model.RestoreRequest) error {
	if req.Dest == "" {
		return fmt.Errorf("restore destination is required")
	}
	compressor, err := compress.Get(req.Compression)
	if err != nil {
		return err
	}
	system := model.SystemInfo{ID: req.SystemID}

	r, err := e.repo.NewReader(ctx, system, req.FileName)
	if err != nil {
		return err
	}
	defer r.Close()

	cr, err := compressor.NewReader(r)
	if err != nil {
		return err
	}
	defer cr.Close()

	log.Info().Str("file", req.FileName).Str("dest", req.Dest).Msg("Starting restore")
	if err := e.archiver.Extract(ctx, cr, req.Dest); err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	return nil
}

// ListSets returns the system's backup sets, newest first. It reads the index
// rather than the manifests, which is why it is cheap; the manifests remain
// the authority on what a set contains.
func (e *Engine) ListSets(ctx context.Context, systemID string) ([]model.BackupSet, error) {
	store, err := e.repo.Meta(ctx, model.SystemInfo{ID: systemID})
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return store.ListBackupSets(ctx)
}

// List returns the backup history of a system, identified by its systemId.
func (e *Engine) List(ctx context.Context, systemID string) ([]model.BackupRecord, error) {
	system := model.SystemInfo{ID: systemID}
	store, err := e.repo.Meta(ctx, system)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return store.ListBackups(ctx)
}

// spoolExclude returns the pattern that keeps spoolDir out of an archive of
// the partition mounted at mountPoint, or nil when spoolDir lies outside it.
// The directory itself is excluded too, so a restore does not bring back an
// empty leftover. A spool on a filesystem mounted below mountPoint (a tmpfs
// /tmp) is matched as well, which costs nothing: a bind mount of the partition
// does not show that filesystem's contents anyway.
func spoolExclude(mountPoint, spoolDir string) *regexp.Regexp {
	if mountPoint == "" {
		return nil
	}
	rel, err := filepath.Rel(mountPoint, spoolDir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return nil
	}
	return regexp.MustCompile(`^` + regexp.QuoteMeta(filepath.ToSlash(rel)) + `(/|$)`)
}

// countingWriter counts bytes written to the underlying writer, and optionally
// adds them to a total shared with a concurrent reader.
type countingWriter struct {
	w     io.Writer
	n     int64
	total *atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	if c.total != nil {
		c.total.Add(int64(n))
	}
	return n, err
}
