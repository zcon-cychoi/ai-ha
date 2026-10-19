package repository

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/framework/secret"
)

// memStore is an in-memory Store.
type memStore struct {
	mu   sync.Mutex
	recs map[string]Record
}

func newMemStore() *memStore { return &memStore{recs: map[string]Record{}} }

func (m *memStore) Insert(_ context.Context, r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.recs {
		if e.Definition.Name == r.Definition.Name {
			return ErrExists
		}
	}
	r.CreatedAt, r.CredentialsUpdatedAt = time.Now(), time.Now()
	m.recs[r.ID] = r
	return nil
}

func (m *memStore) Get(_ context.Context, idOrName string) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.recs[idOrName]; ok {
		return r, nil
	}
	for _, r := range m.recs {
		if r.Definition.Name == idOrName {
			return r, nil
		}
	}
	return Record{}, ErrNotFound
}

func (m *memStore) List(context.Context) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Record
	for _, r := range m.recs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Definition.Name < out[j].Definition.Name })
	return out, nil
}

func (m *memStore) SetCredentials(_ context.Context, id string, sealed []byte, hasToken bool) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[id]
	if !ok {
		return Record{}, ErrNotFound
	}
	r.Credentials, r.HasSessionToken, r.CredentialsUpdatedAt = sealed, hasToken, time.Now()
	m.recs[id] = r
	return r, nil
}

func testKey(b byte) *secret.Box {
	box, err := secret.NewBox(bytes.Repeat([]byte{b}, secret.KeySize))
	if err != nil {
		panic(err)
	}
	return box
}

var testCreds = Credentials{AccessKey: "test-access", SecretKey: "test-secret-value"}

func minioDef() Definition {
	return Definition{Name: "minio-main", Provider: ProviderMinio, Endpoint: "http://minio.example.com:9000",
		Bucket: "aiha-backup", Prefix: "/prod/"}
}

func TestCreateNormalizesAndHidesCredentials(t *testing.T) {
	store := newMemStore()
	s := NewService(store, testKey(1))
	got, err := s.Create(context.Background(), minioDef(), testCreds)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got.Prefix != "prod" || got.Region != "us-east-1" || got.Endpoint != "http://minio.example.com:9000" || got.Type != "s3" {
		t.Fatalf("stored definition = %+v", got)
	}
	rec := store.recs[got.ID]
	if bytes.Contains(rec.Credentials, []byte(testCreds.SecretKey)) || bytes.Contains(rec.Credentials, []byte(testCreds.AccessKey)) {
		t.Fatal("credentials are stored in plaintext")
	}

	access, err := s.Access(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("access: %v", err)
	}
	if access.SecretKey != testCreds.SecretKey || access.Bucket != "aiha-backup" || access.Prefix != "prod" {
		t.Fatalf("access = %+v", access)
	}
	if _, err := s.Access(context.Background(), "minio-main"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("access by name: err = %v, want ErrNotFound", err)
	}
	if _, err := s.Create(context.Background(), minioDef(), testCreds); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate name: err = %v, want ErrExists", err)
	}
}

func TestCreateRejectsInvalidDefinitions(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Definition)
	}{
		{"name that looks like an ID", func(d *Definition) { d.Name = "0123456789abcdef" }},
		{"name with a slash", func(d *Definition) { d.Name = "a/b" }},
		{"unknown provider", func(d *Definition) { d.Provider = "Ceph" }},
		{"minio without endpoint", func(d *Definition) { d.Endpoint = "" }},
		{"endpoint without scheme", func(d *Definition) { d.Endpoint = "minio.example.com:9000" }},
		{"endpoint with ftp", func(d *Definition) { d.Endpoint = "ftp://minio.example.com" }},
		{"endpoint with a path", func(d *Definition) { d.Endpoint = "https://minio.example.com/s3" }},
		{"endpoint with credentials", func(d *Definition) { d.Endpoint = "https://user:pw@minio.example.com" }},
		{"endpoint with a query", func(d *Definition) { d.Endpoint = "https://minio.example.com?x=1" }},
		{"endpoint with a fragment", func(d *Definition) { d.Endpoint = "https://minio.example.com#a" }},
		{"aws with endpoint", func(d *Definition) { d.Provider = ProviderAWS; d.Region = "us-east-1" }},
		{"aws without region", func(d *Definition) { d.Provider = ProviderAWS; d.Endpoint = "" }},
		{"bad bucket", func(d *Definition) { d.Bucket = "Bad_Bucket" }},
		{"prefix with ..", func(d *Definition) { d.Prefix = "a/../b" }},
		{"prefix with empty segment", func(d *Definition) { d.Prefix = "a//b" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := minioDef()
			tt.mutate(&def)
			s := NewService(newMemStore(), testKey(1))
			if _, err := s.Create(context.Background(), def, testCreds); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}

	s := NewService(newMemStore(), testKey(1))
	if _, err := s.Create(context.Background(), minioDef(), Credentials{AccessKey: "a"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing secret key: err = %v, want ErrInvalid", err)
	}
	aws := Definition{Name: "aws", Provider: ProviderAWS, Region: "ap-northeast-2", Bucket: "aiha-backup"}
	if _, err := s.Create(context.Background(), aws, testCreds); err != nil {
		t.Fatalf("valid AWS repository refused: %v", err)
	}
}

func TestUpdateCredentialsRecoversFromALostKey(t *testing.T) {
	store := newMemStore()
	created, err := NewService(store, testKey(1)).Create(context.Background(), minioDef(), testCreds)
	if err != nil {
		t.Fatal(err)
	}

	// The key is lost: a new one cannot open the old credentials...
	s := NewService(store, testKey(2))
	if _, err := s.Access(context.Background(), created.ID); !errors.Is(err, secret.ErrDecrypt) {
		t.Fatalf("old credentials under a new key: err = %v, want ErrDecrypt", err)
	}
	// ...but entering them again keeps the repository and its ID.
	renewed := Credentials{AccessKey: "new-access", SecretKey: "new-secret", SessionToken: "tok"}
	updated, err := s.UpdateCredentials(context.Background(), "minio-main", renewed)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.ID != created.ID || !updated.HasSessionToken || updated.Prefix != "prod" {
		t.Fatalf("updated = %+v", updated)
	}
	access, err := s.Access(context.Background(), created.ID)
	if err != nil || access.SecretKey != "new-secret" || access.SessionToken != "tok" {
		t.Fatalf("access after update = %+v, %v", access, err)
	}
}

func TestWithoutAKeyOnlyListingWorks(t *testing.T) {
	store := newMemStore()
	created, _ := NewService(store, testKey(1)).Create(context.Background(), minioDef(), testCreds)
	s := NewService(store, nil)
	if _, err := s.Get(context.Background(), created.ID); err != nil {
		t.Fatalf("get without a key: %v", err)
	}
	if _, err := s.Access(context.Background(), created.ID); !errors.Is(err, ErrNoSecretKey) {
		t.Fatalf("access without a key: err = %v, want ErrNoSecretKey", err)
	}
	if _, err := s.Create(context.Background(), minioDef(), testCreds); !errors.Is(err, ErrNoSecretKey) {
		t.Fatalf("create without a key: err = %v, want ErrNoSecretKey", err)
	}
}
