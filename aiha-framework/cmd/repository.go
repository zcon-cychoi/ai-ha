package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/repository"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

// The credentials are read from the environment, never from flags: a flag is
// visible to every process on the host through its command line.
const (
	envAccessKey    = "AIHA_REPOSITORY_ACCESS_KEY"
	envSecretKey    = "AIHA_REPOSITORY_SECRET_KEY"
	envSessionToken = "AIHA_REPOSITORY_SESSION_TOKEN"
)

var repoDef repository.Definition

var repositoryCmd = &cobra.Command{
	Use:   "repository",
	Short: "Manage the backup repositories the framework hands to agents",
}

var repositoryAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Register an S3 backup repository",
	Long: `Register an S3 repository (AWS or MinIO) that backup jobs can write to.

The credentials come from ` + envAccessKey + ` and ` + envSecretKey + `
(and ` + envSessionToken + ` for temporary credentials), never from flags. They
are stored encrypted with the key in AIHA_FRAMEWORK_SECRET_KEY_FILE, and an
agent receives them only with a job it has claimed.

The location — provider, endpoint, region, bucket and prefix — cannot be
changed afterwards: backup sets already written there would be looked for
somewhere else. Register a new repository for a new location.

A session token is not renewed by the framework. When it expires, replace it
with "repository update-credentials".`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		creds := credentialsFromEnv()

		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		repos, err := newRepositoryService(pool, true)
		if err != nil {
			return err
		}
		created, err := repos.Create(ctx, repoDef, creds)
		if err != nil {
			return err
		}
		log.Info().Str("repositoryId", created.ID).Str("name", created.Name).Msg("Repository registered")
		printRepository(cmd.OutOrStdout(), created)
		return nil
	},
}

var repositoryUpdateCredentialsCmd = &cobra.Command{
	Use:   "update-credentials <id|name>",
	Short: "Replace a repository's credentials, keeping its ID and location",
	Long: `Replace a repository's credentials with those in ` + envAccessKey + `,
` + envSecretKey + ` and, optionally, ` + envSessionToken + `.

The old credentials are not read, so this also recovers from a lost or replaced
AIHA_FRAMEWORK_SECRET_KEY_FILE: enter each repository's credentials again under
the new key. The repository keeps its ID, so jobs and backup sets that name it
stay valid.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		creds := credentialsFromEnv()

		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		repos, err := newRepositoryService(pool, true)
		if err != nil {
			return err
		}
		updated, err := repos.UpdateCredentials(ctx, args[0], creds)
		if err != nil {
			return err
		}
		log.Info().Str("repositoryId", updated.ID).Msg("Repository credentials replaced")
		printRepository(cmd.OutOrStdout(), updated)
		return nil
	},
}

var repositoryListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the registered repositories",
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		repos, err := newRepositoryService(pool, false)
		if err != nil {
			return err
		}
		list, err := repos.List(ctx)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No repositories registered")
			return nil
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tPROVIDER\tENDPOINT\tBUCKET\tPREFIX\tCREDENTIALS UPDATED")
		for _, r := range list {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Name, r.Provider, orDash(r.Endpoint),
				r.Bucket, orDash(r.Prefix), r.CredentialsUpdatedAt.Format("2006-01-02 15:04:05"))
		}
		return w.Flush()
	},
}

var repositoryGetCmd = &cobra.Command{
	Use:   "get <id|name>",
	Short: "Show a registered repository, without its credentials",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		repos, err := newRepositoryService(pool, false)
		if err != nil {
			return err
		}
		r, err := repos.Get(ctx, args[0])
		if err != nil {
			return err
		}
		printRepository(cmd.OutOrStdout(), r)
		return nil
	},
}

// credentialsFromEnv reads the credentials to register. They are trimmed: an
// env file line with trailing spaces would otherwise carry them into the key.
func credentialsFromEnv() repository.Credentials {
	return repository.Credentials{
		AccessKey:    strings.TrimSpace(os.Getenv(envAccessKey)),
		SecretKey:    strings.TrimSpace(os.Getenv(envSecretKey)),
		SessionToken: strings.TrimSpace(os.Getenv(envSessionToken)),
	}
}

func printRepository(out io.Writer, r model.RepositoryInfo) {
	fmt.Fprintf(out, "Repository:   %s (%s)\n", r.ID, r.Name)
	fmt.Fprintf(out, "Location:     %s %s bucket=%s prefix=%s\n", r.Provider, orDash(r.Endpoint), r.Bucket, orDash(r.Prefix))
	fmt.Fprintf(out, "Region:       %s\n", orDash(r.Region))
	session := "no"
	if r.HasSessionToken {
		session = "yes (expires; replace with update-credentials)"
	}
	fmt.Fprintf(out, "Session tok.: %s\n", session)
	fmt.Fprintf(out, "Created:      %s\n", r.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(out, "Credentials:  updated %s\n", r.CredentialsUpdatedAt.Format(time.RFC3339))
}

func init() {
	f := repositoryAddCmd.Flags()
	f.StringVar(&repoDef.Name, "name", "", "name to refer to the repository by (required)")
	f.StringVar(&repoDef.Provider, "provider", repository.ProviderMinio, "AWS or Minio")
	f.StringVar(&repoDef.Endpoint, "endpoint", "", "Minio: http(s)://host[:port]; empty for AWS")
	f.StringVar(&repoDef.Region, "region", "", "bucket region (required for AWS; Minio defaults to us-east-1)")
	f.StringVar(&repoDef.Bucket, "bucket", "", "bucket name (required)")
	f.StringVar(&repoDef.Prefix, "prefix", "", "key prefix inside the bucket; empty for its root")
	_ = repositoryAddCmd.MarkFlagRequired("name")
	_ = repositoryAddCmd.MarkFlagRequired("bucket")

	repositoryCmd.AddCommand(repositoryAddCmd)
	repositoryCmd.AddCommand(repositoryUpdateCredentialsCmd)
	repositoryCmd.AddCommand(repositoryListCmd)
	repositoryCmd.AddCommand(repositoryGetCmd)
	rootCmd.AddCommand(repositoryCmd)
}
