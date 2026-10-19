package rescue

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
)

const (
	requestKeyFile = "claim.key"
	planFile       = "restore-plan.json"
	rcloneFile     = "rclone.conf"
	// reportTokenFile holds the token that authenticates progress and result
	// reports. Like rclone.conf it is a secret and lives only on tmpfs.
	reportTokenFile = "report.token"
	// RcloneRemote is the rclone remote name the staged configuration defines.
	RcloneRemote = "aiha"
)

var requestKeyPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// loadOrCreateRequestKey returns the request key kept in dir, creating and
// storing one first if there is none. The key is stored before any claim is
// sent, so a rerun of the agent retries under the same key.
func loadOrCreateRequestKey(dir string) (string, error) {
	path := filepath.Join(dir, requestKeyFile)
	data, err := os.ReadFile(path)
	if err == nil {
		key := strings.TrimSpace(string(data))
		if !requestKeyPattern.MatchString(key) {
			return "", fmt.Errorf("%s is corrupt; remove it only if no claim was sent with it", path)
		}
		return key, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read request key: %w", err)
	}

	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate request key: %w", err)
	}
	key := hex.EncodeToString(b)
	if err := writeFileAtomic(path, []byte(key+"\n")); err != nil {
		return "", err
	}
	return key, nil
}

// Repository is the repository information without credentials.
type Repository struct {
	Type     string `json:"type"`
	Provider string `json:"provider"`
	Endpoint string `json:"endpoint,omitempty"`
	Region   string `json:"region,omitempty"`
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix,omitempty"`
}

// stagedPlan is written to restore-plan.json. Credentials live only in
// rclone.conf.
type stagedPlan struct {
	RestoreID    string          `json:"restoreId"`
	RecoveryID   string          `json:"recoveryId"`
	Plan         json.RawMessage `json:"plan"`
	Repository   Repository      `json:"repository"`
	RcloneRemote string          `json:"rcloneRemote"`
}

// writeResults stages a claim response in dir. Every file is built before any
// is written, so an invalid response leaves nothing behind.
func writeResults(dir string, resp model.RestoreClaimResp) error {
	if _, err := model.DecodeRestorePlan(resp.Plan); err != nil {
		return fmt.Errorf("the framework sent an unusable restore plan: %w", err)
	}
	if resp.ReportToken == "" || strings.ContainsAny(resp.ReportToken, "\r\n") {
		return errors.New("the framework sent no usable report token")
	}
	rclone, err := rcloneConfig(resp.Repository)
	if err != nil {
		return err
	}
	r := resp.Repository
	plan, err := json.MarshalIndent(stagedPlan{
		RestoreID:  resp.RestoreID,
		RecoveryID: resp.RecoveryID,
		Plan:       resp.Plan,
		Repository: Repository{
			Type: r.Type, Provider: r.Provider, Endpoint: r.Endpoint,
			Region: r.Region, Bucket: r.Bucket, Prefix: r.Prefix,
		},
		RcloneRemote: RcloneRemote,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode restore plan: %w", err)
	}

	if err := writeFileAtomic(filepath.Join(dir, rcloneFile), rclone); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, reportTokenFile), []byte(resp.ReportToken+"\n")); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, planFile), append(plan, '\n'))
}

// Staged is a claim staged by Run, as read back for the restore.
type Staged struct {
	RestoreID  string
	RecoveryID string
	Plan       model.RestorePlan
	Repository Repository
	// RcloneFile is the rclone configuration defining RcloneRemote.
	RcloneFile   string
	RcloneRemote string
	// ReportToken authenticates progress and result reports. It is a secret.
	ReportToken string
}

// LoadStaged reads the claim staged in dir, the per-recovery directory Run
// created.
func LoadStaged(dir string) (Staged, error) {
	data, err := os.ReadFile(filepath.Join(dir, planFile))
	if err != nil {
		return Staged{}, fmt.Errorf("read staged restore plan: %w", err)
	}
	var sp stagedPlan
	if err := json.Unmarshal(data, &sp); err != nil {
		return Staged{}, fmt.Errorf("parse staged restore plan: %w", err)
	}
	plan, err := model.DecodeRestorePlan(sp.Plan)
	if err != nil {
		return Staged{}, err
	}
	tok, err := os.ReadFile(filepath.Join(dir, reportTokenFile))
	if err != nil {
		return Staged{}, fmt.Errorf("read report token: %w", err)
	}
	rcloneConf := filepath.Join(dir, rcloneFile)
	if _, err := os.Stat(rcloneConf); err != nil {
		return Staged{}, fmt.Errorf("staged rclone configuration: %w", err)
	}
	return Staged{
		RestoreID:    sp.RestoreID,
		RecoveryID:   sp.RecoveryID,
		Plan:         plan,
		Repository:   sp.Repository,
		RcloneFile:   rcloneConf,
		RcloneRemote: sp.RcloneRemote,
		ReportToken:  strings.TrimSpace(string(tok)),
	}, nil
}

// rcloneProviders are the S3 providers the staged configuration supports.
var rcloneProviders = map[string]bool{"AWS": true, "Minio": true}

// rcloneConfig renders an rclone S3 remote for the repository. Values may not
// contain line breaks: one would let a value inject further settings.
func rcloneConfig(r model.RepositoryAccess) ([]byte, error) {
	if r.Type != "s3" {
		return nil, fmt.Errorf("unsupported repository type %q", r.Type)
	}
	if !rcloneProviders[r.Provider] {
		return nil, fmt.Errorf("unsupported S3 provider %q", r.Provider)
	}
	if r.Bucket == "" || r.AccessKey == "" || r.SecretKey == "" {
		return nil, errors.New("repository access is incomplete: bucket and keys are required")
	}

	settings := [][2]string{
		{"type", "s3"},
		{"provider", r.Provider},
		{"access_key_id", r.AccessKey},
		{"secret_access_key", r.SecretKey},
		{"session_token", r.SessionToken},
		{"region", r.Region},
		{"endpoint", r.Endpoint},
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\n", RcloneRemote)
	for _, kv := range settings {
		if kv[1] == "" {
			continue
		}
		if strings.ContainsAny(kv[1], "\r\n") {
			return nil, fmt.Errorf("repository %s contains a line break", kv[0])
		}
		fmt.Fprintf(&b, "%s = %s\n", kv[0], kv[1])
	}
	return []byte(b.String()), nil
}

// writeFileAtomic replaces path with data, readable only by the owner. Readers
// see either the old file or the complete new one.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once renamed

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("restrict %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
