package repository

import (
	"context"
	"fmt"
	"io"

	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/spool"
	"github.com/minio/minio-go/v7"
	"github.com/rs/zerolog/log"
)

// NewSink opens a resumable S3 sink for a new backup file, backed by a multipart
// upload: each spooled chunk becomes one part. S3 is stateless HTTP, so a
// network interruption needs no reconnect — the multipart upload id persists on
// the server and the spool simply retries the failed part.
func (r *s3Repository) NewSink(ctx context.Context, system model.SystemInfo, filename string) (spool.Sink, error) {
	key, err := r.objectKey(system, filename)
	if err != nil {
		return nil, err
	}
	uploadID, err := r.core.NewMultipartUpload(ctx, r.bucket, key, minio.PutObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("start multipart upload: %w", err)
	}
	return &s3Sink{core: r.core, bucket: r.bucket, key: key, uploadID: uploadID}, nil
}

// s3Sink assembles a backup object from ordered chunks via an S3 multipart
// upload. Chunk index N maps to part number N+1. A retried part re-uploads under
// the same part number (S3 overwrites it), so retries are idempotent.
type s3Sink struct {
	core     *minio.Core
	bucket   string
	key      string
	uploadID string

	parts          []minio.CompletePart
	committedIndex int
}

func (s *s3Sink) Put(ctx context.Context, index int, data io.Reader, size int64) error {
	if index < s.committedIndex {
		return nil // already uploaded (idempotent retry)
	}
	if index != s.committedIndex {
		return fmt.Errorf("out-of-order chunk %d, expected %d", index, s.committedIndex)
	}

	partID := index + 1
	part, err := s.core.PutObjectPart(ctx, s.bucket, s.key, s.uploadID, partID, data, size, minio.PutObjectPartOptions{})
	if err != nil {
		return err
	}

	s.parts = append(s.parts, minio.CompletePart{PartNumber: partID, ETag: part.ETag})
	s.committedIndex++
	return nil
}

// Finalize completes the multipart upload, assembling the parts into the object.
func (s *s3Sink) Finalize(ctx context.Context) error {
	if _, err := s.core.CompleteMultipartUpload(ctx, s.bucket, s.key, s.uploadID, s.parts, minio.PutObjectOptions{}); err != nil {
		return fmt.Errorf("complete multipart upload: %w", err)
	}
	return nil
}

// Abort discards the multipart upload and its uploaded parts, so incomplete
// parts do not linger and accrue storage charges.
func (s *s3Sink) Abort(ctx context.Context) error {
	if err := s.core.AbortMultipartUpload(ctx, s.bucket, s.key, s.uploadID); err != nil {
		log.Warn().Err(err).Str("key", s.key).Msg("Failed to abort S3 multipart upload")
		return err
	}
	return nil
}
