// Package config loads AI-HA configuration via viper: the backup tool from
// AIHA_BACKUP_* and the framework server from AIHA_FRAMEWORK_* environment
// variables, overriding the built-in defaults; see conf/template-setup.env.
package config

import (
	"time"

	"github.com/cloud-barista/ai-ha/pkg/backup/repository"
	"github.com/spf13/viper"
)

// Config holds runtime configuration for the backup tool.
type Config struct {
	// RepositoryURI is the default backup repository (e.g. a local path or
	// file://, sftp://, http://, s3:// URI).
	RepositoryURI string
	// Compression is the default compression codec ("zstd", "none", ...).
	Compression string
	// LogLevel is the zerolog level ("debug", "info", "warn", "error").
	LogLevel string
	// LockDir holds the lock files that serialize metadata updates between
	// concurrent backup processes on the same host.
	LockDir string
	// SFTP holds credentials for the SFTP repository backend.
	SFTP SFTPConfig
	// SMB holds credentials for the SMB/CIFS repository backend.
	SMB SMBConfig
	// S3 holds connection settings for the S3 repository backend.
	S3 S3Config
	// Resume tunes the chunked, resumable transfer for remote backends.
	Resume ResumeConfig
}

// ResumeConfig tunes the disk-backed spool used for resumable uploads, loaded
// from AIHA_BACKUP_* environment variables.
type ResumeConfig struct {
	// ChunkSizeBytes is the size each spooled chunk is rolled over at.
	ChunkSizeBytes int64
	// BufferCapBytes caps the unsent bytes held on local disk (backpressure).
	BufferCapBytes int64
	// DisconnectTimeout aborts the backup if a chunk cannot upload for this long.
	DisconnectTimeout time.Duration
}

// SMBConfig holds credentials for the SMB backend, loaded from
// AIHA_BACKUP_SMB_* environment variables.
type SMBConfig struct {
	User     string
	Password string
	Domain   string
}

// S3Config holds connection settings for the S3 (and S3-compatible) backend,
// loaded from AIHA_BACKUP_S3_* environment variables. When AccessKey/SecretKey
// are empty, the standard AWS credential chain is used.
type S3Config struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
	Region       string
	Endpoint     string
	UseSSL       bool
}

// SFTPConfig holds SSH credentials and options for the SFTP backend, loaded
// from AIHA_BACKUP_SFTP_* environment variables.
type SFTPConfig struct {
	User           string
	Password       string
	PrivateKeyPath string
	Passphrase     string
	KnownHostsPath string
	Insecure       bool
}

// Load reads configuration from environment variables (AIHA_BACKUP_*), applying
// defaults for any value not set.
func Load() (*Config, error) {
	v := viper.New()
	v.SetEnvPrefix("AIHA_BACKUP")
	v.AutomaticEnv()

	v.SetDefault("repository", "./aiha-repo")
	v.SetDefault("compression", "zstd")
	v.SetDefault("log_level", "info")
	v.SetDefault("lock_dir", repository.DefaultLockDir)
	v.SetDefault("sftp_insecure", false)
	v.SetDefault("s3_use_ssl", true)
	v.SetDefault("chunk_size", int64(64<<20))
	v.SetDefault("buffer_cap", int64(2<<30))
	v.SetDefault("disconnect_timeout", 5*time.Minute)

	return &Config{
		RepositoryURI: v.GetString("repository"),
		Compression:   v.GetString("compression"),
		LogLevel:      v.GetString("log_level"),
		LockDir:       v.GetString("lock_dir"),
		SFTP: SFTPConfig{
			User:           v.GetString("sftp_user"),
			Password:       v.GetString("sftp_password"),
			PrivateKeyPath: v.GetString("sftp_key"),
			Passphrase:     v.GetString("sftp_key_passphrase"),
			KnownHostsPath: v.GetString("sftp_known_hosts"),
			Insecure:       v.GetBool("sftp_insecure"),
		},
		SMB: SMBConfig{
			User:     v.GetString("smb_user"),
			Password: v.GetString("smb_password"),
			Domain:   v.GetString("smb_domain"),
		},
		S3: S3Config{
			AccessKey:    v.GetString("s3_access_key"),
			SecretKey:    v.GetString("s3_secret_key"),
			SessionToken: v.GetString("s3_session_token"),
			Region:       v.GetString("s3_region"),
			Endpoint:     v.GetString("s3_endpoint"),
			UseSSL:       v.GetBool("s3_use_ssl"),
		},
		Resume: ResumeConfig{
			ChunkSizeBytes:    v.GetInt64("chunk_size"),
			BufferCapBytes:    v.GetInt64("buffer_cap"),
			DisconnectTimeout: v.GetDuration("disconnect_timeout"),
		},
	}, nil
}
