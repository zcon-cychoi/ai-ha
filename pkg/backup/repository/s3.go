package repository

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/cloud-barista/ai-ha/pkg/backup/meta"
	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/rs/zerolog/log"
)

// s3MinPartSize is the S3 multipart minimum part size: every part except the
// last must be at least 5 MiB. The resumable spool chunk size is raised to this
// when it is smaller (see MinChunkSize).
const s3MinPartSize = 5 << 20

// defaultAWSEndpoint is used when no custom endpoint is configured.
const defaultAWSEndpoint = "s3.amazonaws.com"

// S3Auth holds the connection settings for the S3 (and S3-compatible) backend.
// Values are typically loaded from AIHA_BACKUP_S3_* environment variables.
type S3Auth struct {
	// AccessKey and SecretKey select static credentials. When empty, the
	// standard AWS credential chain is used (env, shared file, IAM role).
	AccessKey string
	SecretKey string
	// SessionToken is used with temporary (STS) static credentials.
	SessionToken string
	// Region is the bucket region (e.g. "ap-northeast-2").
	Region string
	// Endpoint overrides the host for S3-compatible storage (e.g. MinIO,
	// "minio.example.com:9000"). Empty means AWS S3.
	Endpoint string
	// UseSSL toggles HTTPS. Defaults to true.
	UseSSL bool
}

// s3Repository stores backups as objects in an S3 (or S3-compatible) bucket.
// Backup files are uploaded via resumable multipart uploads (one spool chunk =
// one part; see s3Sink); per-system SQLite metadata is synced through a local
// working copy, as with the sftp/smb backends.
type s3Repository struct {
	*metaSync
	core   *minio.Core   // low-level API for multipart uploads (see s3Sink)
	client *minio.Client // high-level API for object get/put/stat
	bucket string
	prefix string // key prefix within the bucket (no leading/trailing slash)
}

func newS3(ctx context.Context, uri string, auth S3Auth, lockDir string) (Repository, error) {
	bucket, prefix, err := parseS3Target(uri)
	if err != nil {
		return nil, err
	}

	endpoint := auth.Endpoint
	secure := auth.UseSSL
	if endpoint == "" {
		endpoint = defaultAWSEndpoint
		secure = true // AWS is always HTTPS
	}

	core, err := minio.NewCore(endpoint, &minio.Options{
		Creds:  s3Credentials(auth),
		Secure: secure,
		Region: auth.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 client: %w", err)
	}

	log.Info().Str("endpoint", endpoint).Str("bucket", bucket).Str("prefix", prefix).Msg("Connecting to S3 repository")

	// Fail fast if the bucket is missing or inaccessible.
	exists, err := core.Client.BucketExists(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("access bucket %q: %w", bucket, err)
	}
	if !exists {
		return nil, fmt.Errorf("bucket %q not found or not accessible", bucket)
	}

	sync, err := newMetaSync(uri, "aiha-s3-meta-", lockDir)
	if err != nil {
		return nil, err
	}

	return &s3Repository{metaSync: sync, core: core, client: core.Client, bucket: bucket, prefix: prefix}, nil
}

// s3Credentials selects static credentials when provided, otherwise the standard
// AWS credential chain (environment, shared credentials file, IAM role).
func s3Credentials(auth S3Auth) *credentials.Credentials {
	if auth.AccessKey != "" && auth.SecretKey != "" {
		return credentials.NewStaticV4(auth.AccessKey, auth.SecretKey, auth.SessionToken)
	}
	return credentials.NewChainCredentials([]credentials.Provider{
		&credentials.EnvAWS{},
		&credentials.FileAWSCredentials{},
		&credentials.IAM{},
	})
}

func (r *s3Repository) Scheme() model.TransportScheme { return model.SchemeS3 }

// MinChunkSize reports the S3 multipart minimum part size so the engine raises
// the spool chunk size when a smaller one is configured.
func (r *s3Repository) MinChunkSize() int64 { return s3MinPartSize }

// systemPrefix returns the object key prefix for a system, guarding against key
// injection via a crafted hostname.
func (r *s3Repository) systemPrefix(system model.SystemInfo) (string, error) {
	name := system.DirName()
	if name == "" || name != path.Base(name) || name == "." || name == ".." {
		return "", fmt.Errorf("invalid system name %q", name)
	}
	return path.Join(r.prefix, name), nil
}

// objectKey returns the full key for a backup file under a system.
func (r *s3Repository) objectKey(system model.SystemInfo, filename string) (string, error) {
	if filename != path.Base(filename) {
		return "", fmt.Errorf("invalid backup file name %q", filename)
	}
	prefix, err := r.systemPrefix(system)
	if err != nil {
		return "", err
	}
	return path.Join(prefix, filename), nil
}

// EnsureSystem is a no-op: S3 has no directories, keys are created on upload.
func (r *s3Repository) EnsureSystem(_ context.Context, system model.SystemInfo) error {
	_, err := r.systemPrefix(system)
	return err
}

// NewWriter streams an object to the bucket via a pipe. The resumable multipart
// path (NewSink) is preferred for backups; this satisfies the Repository
// interface and is used for any non-resumable caller.
func (r *s3Repository) NewWriter(ctx context.Context, system model.SystemInfo, filename string) (io.WriteCloser, error) {
	key, err := r.objectKey(system, filename)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	w := &s3PipeWriter{pw: pw, done: make(chan error, 1)}
	go func() {
		_, err := r.client.PutObject(ctx, r.bucket, key, pr, -1, minio.PutObjectOptions{})
		// Unblock Write on failure and record the outcome for Close.
		_ = pr.CloseWithError(err)
		w.done <- err
	}()
	return w, nil
}

// s3PipeWriter adapts an object PutObject into an io.WriteCloser.
type s3PipeWriter struct {
	pw   *io.PipeWriter
	done chan error
}

func (w *s3PipeWriter) Write(p []byte) (int, error) { return w.pw.Write(p) }

func (w *s3PipeWriter) Close() error {
	if err := w.pw.Close(); err != nil {
		return err
	}
	return <-w.done
}

func (r *s3Repository) NewReader(ctx context.Context, system model.SystemInfo, filename string) (io.ReadCloser, error) {
	key, err := r.objectKey(system, filename)
	if err != nil {
		return nil, err
	}
	// Stat first so a missing object surfaces a clean error before streaming.
	if _, err := r.client.StatObject(ctx, r.bucket, key, minio.StatObjectOptions{}); err != nil {
		if isS3NotFound(err) {
			return nil, fmt.Errorf("backup file %q not found", filename)
		}
		return nil, fmt.Errorf("open backup object: %w", err)
	}
	obj, err := r.client.GetObject(ctx, r.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("open backup object: %w", err)
	}
	return obj, nil
}

// metaLocation returns the object key of the system's metadata database.
func (r *s3Repository) metaLocation(system model.SystemInfo) (string, error) {
	prefix, err := r.systemPrefix(system)
	if err != nil {
		return "", err
	}
	return path.Join(prefix, metaFileName), nil
}

// Meta opens a read-only copy of the system's metadata object.
func (r *s3Repository) Meta(ctx context.Context, system model.SystemInfo) (meta.Store, error) {
	return r.open(ctx, r, system)
}

// UpdateMeta applies fn to the system's metadata object under an exclusive lock.
func (r *s3Repository) UpdateMeta(ctx context.Context, system model.SystemInfo, fn func(context.Context, meta.Store) error) error {
	return r.update(ctx, r, system, fn)
}

func (r *s3Repository) Close() error {
	return r.cleanup()
}

// downloadMeta copies the metadata object into localPath. A missing object is
// not an error: the local copy is left empty so a fresh database is created.
func (r *s3Repository) downloadMeta(ctx context.Context, key, localPath string) error {
	obj, err := r.client.GetObject(ctx, r.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("download metadata: %w", err)
	}
	defer obj.Close()

	lf, err := os.OpenFile(localPath, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()

	if _, err := io.Copy(lf, obj); err != nil {
		if isS3NotFound(err) {
			return nil // no metadata yet; start fresh
		}
		return fmt.Errorf("download metadata: %w", err)
	}
	return nil
}

// uploadMeta replaces the metadata object with the contents of localPath.
func (r *s3Repository) uploadMeta(ctx context.Context, localPath, key string) error {
	lf, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer lf.Close()

	info, err := lf.Stat()
	if err != nil {
		return err
	}
	if _, err := r.client.PutObject(ctx, r.bucket, key, lf, info.Size(), minio.PutObjectOptions{}); err != nil {
		return fmt.Errorf("upload metadata: %w", err)
	}
	return nil
}

// parseS3Target splits an s3:// URI into a bucket and key prefix.
// e.g. "s3://my-bucket/aiha/prod" -> ("my-bucket", "aiha/prod").
func parseS3Target(uri string) (bucket, prefix string, err error) {
	u, perr := url.Parse(uri)
	if perr != nil {
		return "", "", fmt.Errorf("parse s3 URI: %w", perr)
	}
	if u.Scheme != "s3" {
		return "", "", fmt.Errorf("not an s3 URI: %q", uri)
	}
	bucket = u.Host
	if bucket == "" {
		return "", "", fmt.Errorf("s3 URI missing bucket: %q", uri)
	}
	prefix = strings.Trim(u.Path, "/")
	return bucket, prefix, nil
}

// isS3NotFound reports whether err is an S3 "object does not exist" error.
func isS3NotFound(err error) bool {
	code := minio.ToErrorResponse(err).Code
	return code == "NoSuchKey" || code == "NoSuchObject"
}
