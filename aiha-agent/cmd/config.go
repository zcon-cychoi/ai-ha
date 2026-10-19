//go:build !rescue

package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/cloud-barista/ai-ha/config"
	"github.com/cloud-barista/ai-ha/pkg/agent/state"
	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/repository"
	"github.com/spf13/cobra"
)

var (
	flagRepo string
	cfg      *config.Config
)

// loadConfig reads AIHA_BACKUP_* into cfg, applying the --repo override. It is
// the PreRunE of every command that reaches the repository. It is deliberately
// not a persistent hook on the root command: "rescue" must not read repository
// credentials it has no use for.
func loadConfig(*cobra.Command, []string) error {
	loaded, err := config.Load()
	if err != nil {
		return err
	}
	cfg = loaded
	if flagRepo != "" {
		cfg.RepositoryURI = flagRepo
	}
	return nil
}

// addRepoFlag registers --repo on a command whose PreRunE is loadConfig.
func addRepoFlag(cmd *cobra.Command) {
	cmd.PersistentFlags().StringVar(&flagRepo, "repo", "",
		"backup repository URI (default from AIHA_BACKUP_REPOSITORY or ./aiha-repo)")
}

// openRepository opens the configured repository, supplying remote-backend
// credentials from configuration so sftp/smb/s3 backends work transparently.
func openRepository(ctx context.Context) (repository.Repository, error) {
	return repository.Open(ctx, cfg.RepositoryURI,
		repository.WithSFTPAuth(repository.SFTPAuth{
			User:           cfg.SFTP.User,
			Password:       cfg.SFTP.Password,
			PrivateKeyPath: cfg.SFTP.PrivateKeyPath,
			Passphrase:     cfg.SFTP.Passphrase,
			KnownHostsPath: cfg.SFTP.KnownHostsPath,
			Insecure:       cfg.SFTP.Insecure,
		}),
		repository.WithSMBAuth(repository.SMBAuth{
			User:     cfg.SMB.User,
			Password: cfg.SMB.Password,
			Domain:   cfg.SMB.Domain,
		}),
		repository.WithS3Auth(repository.S3Auth{
			AccessKey:    cfg.S3.AccessKey,
			SecretKey:    cfg.S3.SecretKey,
			SessionToken: cfg.S3.SessionToken,
			Region:       cfg.S3.Region,
			Endpoint:     cfg.S3.Endpoint,
			UseSSL:       cfg.S3.UseSSL,
		}),
		repository.WithLockDir(cfg.LockDir),
	)
}

// resolveSystem identifies the system being backed up: the stable identifier
// the repository files it under, plus the hostname for display.
//
// The identifier is created on first use and kept in the state directory. Once
// the agent registers with the framework, the framework issues it instead and
// this is where it will be stored.
func resolveSystem() (model.SystemInfo, error) {
	id, err := state.EnsureSystemID(backupStateDir)
	if err != nil {
		return model.SystemInfo{}, fmt.Errorf("resolve system id: %w", err)
	}

	// A hostname that cannot be read costs a display name, not the backup.
	hostname, err := os.Hostname()
	if err != nil {
		hostname = ""
	}
	return model.SystemInfo{ID: id, Hostname: hostname}, nil
}

// resolveSystemID returns the system a command should act on: the one named on
// the command line, or this agent's own when none was. Asking an operator to
// type an identifier the agent already holds is how a repository ends up being
// listed for the wrong system.
//
// It reads the identifier rather than creating one, unlike resolveSystem. A
// command that only inspects must not bring a system into being as a side
// effect of being run.
func resolveSystemID(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}

	id, err := state.ReadSystemID(backupStateDir)
	if errors.Is(err, state.ErrNoSystemID) {
		return "", fmt.Errorf(
			"this system has no identifier yet: name one with --system, or enrol the agent with %q",
			binaryName+" register")
	}
	if err != nil {
		return "", err
	}
	return id, nil
}
