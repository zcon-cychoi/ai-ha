// Package repository keeps the backup repositories the framework knows: where
// each one is and the credentials to reach it. Jobs name a repository by ID,
// and the credentials go to an agent only when it claims a job — they are
// never copied into a job.
//
// Credentials are stored encrypted (see pkg/framework/secret). A repository's
// location cannot change once registered: backup sets already written there
// would be looked for somewhere else. Its credentials can be replaced.
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	backupmodel "github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/secret"
)

var (
	// ErrNotFound reports that no repository has the given ID or name.
	ErrNotFound = errors.New("repository not found")
	// ErrInvalid reports a repository definition or credentials that cannot
	// be accepted.
	ErrInvalid = errors.New("invalid repository")
	// ErrExists reports a name already taken.
	ErrExists = errors.New("a repository with this name already exists")
	// ErrNoSecretKey reports that the framework has no key to encrypt or
	// decrypt credentials with.
	ErrNoSecretKey = errors.New("no secret key is configured (AIHA_FRAMEWORK_SECRET_KEY_FILE)")
)

const (
	// TypeS3 is the only repository type: the rescue environment restores
	// from S3 through rclone, and the year-one scenario stores to S3.
	TypeS3 = "s3"
	// ProviderAWS is Amazon S3 itself, reached at its standard endpoint.
	ProviderAWS = "AWS"
	// ProviderMinio is an S3-compatible service at an explicit endpoint.
	ProviderMinio = "Minio"

	// defaultMinioRegion is what MinIO answers to unless configured otherwise.
	defaultMinioRegion = "us-east-1"
)

var (
	namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	// idPattern is the shape of a repository ID. A name of this shape is
	// refused, so that looking a repository up by "ID or name" is never
	// ambiguous.
	idPattern     = regexp.MustCompile(`^[0-9a-f]{16}$`)
	bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
)

// Definition is where a repository is. It is fixed at registration.
type Definition struct {
	Name     string
	Provider string
	// Endpoint is the URL of an S3-compatible service, e.g.
	// "https://minio.example.com:9000". Empty for AWS.
	Endpoint string
	Region   string
	Bucket   string
	// Prefix is the key prefix inside the bucket, without leading or trailing
	// slashes; empty for the bucket root.
	Prefix string
}

// Credentials authenticate to the repository. A session token expires and is
// not renewed here: replace the credentials when it does.
type Credentials struct {
	AccessKey    string `json:"accessKey"`
	SecretKey    string `json:"secretKey"`
	SessionToken string `json:"sessionToken,omitempty"`
}

// Record is a stored repository. Credentials holds the sealed credentials.
type Record struct {
	ID                   string
	Definition           Definition
	Type                 string
	Credentials          []byte
	HasSessionToken      bool
	CreatedAt            time.Time
	CredentialsUpdatedAt time.Time
}

// Store persists repositories.
type Store interface {
	// Insert stores a new repository. It returns ErrExists when the name is
	// taken.
	Insert(ctx context.Context, r Record) error
	// Get returns the repository whose ID or name is idOrName, or ErrNotFound.
	Get(ctx context.Context, idOrName string) (Record, error)
	// List returns every repository, by name.
	List(ctx context.Context) ([]Record, error)
	// SetCredentials replaces a repository's sealed credentials.
	SetCredentials(ctx context.Context, id string, sealed []byte, hasSessionToken bool) (Record, error)
}

// Service registers repositories and hands out access to them.
type Service struct {
	store Store
	// box encrypts credentials. Nil when no key is configured: listing and
	// inspecting still work, anything touching credentials fails.
	box *secret.Box
}

// NewService returns a Service. box may be nil.
func NewService(store Store, box *secret.Box) *Service {
	return &Service{store: store, box: box}
}

// Create registers a repository.
func (s *Service) Create(ctx context.Context, def Definition, creds Credentials) (model.RepositoryInfo, error) {
	if s.box == nil {
		return model.RepositoryInfo{}, ErrNoSecretKey
	}
	def, err := normalize(def)
	if err != nil {
		return model.RepositoryInfo{}, err
	}
	if err := validateCredentials(creds); err != nil {
		return model.RepositoryInfo{}, err
	}

	rec := Record{ID: backupmodel.NewID(), Definition: def, Type: TypeS3, HasSessionToken: creds.SessionToken != ""}
	if rec.Credentials, err = s.seal(rec.ID, creds); err != nil {
		return model.RepositoryInfo{}, err
	}
	if err := s.store.Insert(ctx, rec); err != nil {
		return model.RepositoryInfo{}, err
	}
	stored, err := s.store.Get(ctx, rec.ID)
	if err != nil {
		return model.RepositoryInfo{}, err
	}
	return info(stored), nil
}

// List returns every registered repository, without credentials.
func (s *Service) List(ctx context.Context) ([]model.RepositoryInfo, error) {
	recs, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]model.RepositoryInfo, 0, len(recs))
	for _, r := range recs {
		out = append(out, info(r))
	}
	return out, nil
}

// Get returns the repository with the given ID or name, without credentials.
func (s *Service) Get(ctx context.Context, idOrName string) (model.RepositoryInfo, error) {
	rec, err := s.store.Get(ctx, idOrName)
	if err != nil {
		return model.RepositoryInfo{}, err
	}
	return info(rec), nil
}

// UpdateCredentials replaces a repository's credentials, keeping its ID and
// location. The old credentials are not read, so this also recovers a
// repository whose credentials were sealed under a key that has been lost.
func (s *Service) UpdateCredentials(ctx context.Context, idOrName string, creds Credentials) (model.RepositoryInfo, error) {
	if s.box == nil {
		return model.RepositoryInfo{}, ErrNoSecretKey
	}
	if err := validateCredentials(creds); err != nil {
		return model.RepositoryInfo{}, err
	}
	rec, err := s.store.Get(ctx, idOrName)
	if err != nil {
		return model.RepositoryInfo{}, err
	}
	sealed, err := s.seal(rec.ID, creds)
	if err != nil {
		return model.RepositoryInfo{}, err
	}
	rec, err = s.store.SetCredentials(ctx, rec.ID, sealed, creds.SessionToken != "")
	if err != nil {
		return model.RepositoryInfo{}, err
	}
	return info(rec), nil
}

// Access returns the repository with its credentials decrypted, for an agent
// that has been given a job against it. The result is a secret: never log it.
func (s *Service) Access(ctx context.Context, id string) (model.RepositoryAccess, error) {
	if s.box == nil {
		return model.RepositoryAccess{}, ErrNoSecretKey
	}
	rec, err := s.store.Get(ctx, id)
	if err != nil {
		return model.RepositoryAccess{}, err
	}
	if rec.ID != id {
		// Access is by ID only: a job records the ID, and a name could be
		// another repository's by the time the job runs.
		return model.RepositoryAccess{}, ErrNotFound
	}
	plain, err := s.box.Open(rec.Credentials, []byte(rec.ID))
	if err != nil {
		return model.RepositoryAccess{}, fmt.Errorf("credentials of repository %s: %w", rec.ID, err)
	}
	var creds Credentials
	if err := json.Unmarshal(plain, &creds); err != nil {
		return model.RepositoryAccess{}, fmt.Errorf("decode credentials of repository %s: %w", rec.ID, err)
	}
	d := rec.Definition
	return model.RepositoryAccess{
		Type: rec.Type, Provider: d.Provider, Endpoint: d.Endpoint, Region: d.Region,
		Bucket: d.Bucket, Prefix: d.Prefix,
		AccessKey: creds.AccessKey, SecretKey: creds.SecretKey, SessionToken: creds.SessionToken,
	}, nil
}

// seal encrypts creds bound to the repository's ID.
func (s *Service) seal(id string, creds Credentials) ([]byte, error) {
	plain, err := json.Marshal(creds)
	if err != nil {
		return nil, fmt.Errorf("encode credentials: %w", err)
	}
	return s.box.Seal(plain, []byte(id))
}

func info(r Record) model.RepositoryInfo {
	d := r.Definition
	return model.RepositoryInfo{
		ID: r.ID, Name: d.Name, Type: r.Type, Provider: d.Provider, Endpoint: d.Endpoint,
		Region: d.Region, Bucket: d.Bucket, Prefix: d.Prefix, HasSessionToken: r.HasSessionToken,
		CreatedAt: r.CreatedAt, CredentialsUpdatedAt: r.CredentialsUpdatedAt,
	}
}

// normalize checks a definition and returns it in stored form.
func normalize(d Definition) (Definition, error) {
	invalid := func(format string, args ...any) (Definition, error) {
		return Definition{}, fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
	}
	if !namePattern.MatchString(d.Name) || idPattern.MatchString(d.Name) {
		return invalid("name must be 1-63 letters, digits, '.', '_' or '-', and not look like a repository ID")
	}
	switch d.Provider {
	case ProviderAWS:
		if d.Endpoint != "" {
			return invalid("an AWS repository takes no endpoint")
		}
		if d.Region == "" {
			return invalid("an AWS repository requires a region")
		}
	case ProviderMinio:
		endpoint, err := normalizeEndpoint(d.Endpoint)
		if err != nil {
			return invalid("%s", err)
		}
		d.Endpoint = endpoint
		if d.Region == "" {
			d.Region = defaultMinioRegion
		}
	default:
		return invalid("provider must be %s or %s", ProviderAWS, ProviderMinio)
	}
	if !bucketPattern.MatchString(d.Bucket) {
		return invalid("bucket %q is not a valid S3 bucket name", d.Bucket)
	}
	prefix, err := normalizePrefix(d.Prefix)
	if err != nil {
		return invalid("%s", err)
	}
	d.Prefix = prefix
	return d, nil
}

// normalizeEndpoint accepts the URL of an S3-compatible service: a scheme and
// a host, optionally a port, and nothing else. A path would be dropped by the
// S3 client without a word, so it is refused rather than accepted.
func normalizeEndpoint(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("a Minio repository requires an endpoint")
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil, u.Scheme != "http" && u.Scheme != "https", u.Host == "", u.Hostname() == "":
		return "", errors.New("endpoint must be an http(s) URL with a host, e.g. https://minio.example.com:9000")
	case u.User != nil:
		return "", errors.New("endpoint must not carry credentials")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "", errors.New("endpoint must not have a query or fragment")
	case u.Path != "" && u.Path != "/":
		return "", errors.New("endpoint must not have a path")
	}
	return u.Scheme + "://" + u.Host, nil
}

// normalizePrefix trims surrounding slashes and refuses segments that would
// make the prefix mean something other than it says.
func normalizePrefix(raw string) (string, error) {
	p := strings.Trim(raw, "/")
	if p == "" {
		return "", nil
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("prefix %q has an empty, '.' or '..' segment", raw)
		}
	}
	return p, nil
}

func validateCredentials(c Credentials) error {
	if c.AccessKey == "" || c.SecretKey == "" {
		return fmt.Errorf("%w: an access key and a secret key are required", ErrInvalid)
	}
	return nil
}
