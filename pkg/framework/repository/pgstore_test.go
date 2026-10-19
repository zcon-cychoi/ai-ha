package repository

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests run only when AIHA_TEST_DB_DSN points at a disposable database.
func testPGService(t *testing.T) (*Service, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("AIHA_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("AIHA_TEST_DB_DSN not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	_, _ = pool.Exec(ctx, `DELETE FROM repositories WHERE name LIKE 'pgtest-%'`)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM repositories WHERE name LIKE 'pgtest-%'`) })
	return NewService(NewPGStore(pool), testKey(1)), pool
}

func TestPGRepositoryRoundTrip(t *testing.T) {
	s, pool := testPGService(t)
	ctx := context.Background()
	def := minioDef()
	def.Name = "pgtest-minio"

	created, err := s.Create(ctx, def, testCreds)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Create(ctx, def, testCreds); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: err = %v, want ErrExists", err)
	}

	var stored []byte
	if err := pool.QueryRow(ctx, `SELECT credentials FROM repositories WHERE repository_id = $1`, created.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte(testCreds.SecretKey)) {
		t.Fatal("the database holds the secret key in plaintext")
	}

	for _, key := range []string{created.ID, "pgtest-minio"} {
		got, err := s.Get(ctx, key)
		if err != nil || got.ID != created.ID || got.Prefix != "prod" {
			t.Fatalf("get %s = %+v, %v", key, got, err)
		}
	}
	access, err := s.Access(ctx, created.ID)
	if err != nil || access.SecretKey != testCreds.SecretKey {
		t.Fatalf("access = %+v, %v", access, err)
	}

	updated, err := s.UpdateCredentials(ctx, created.ID, Credentials{AccessKey: "a2", SecretKey: "s2"})
	if err != nil || updated.ID != created.ID || !updated.CredentialsUpdatedAt.After(created.CredentialsUpdatedAt) {
		t.Fatalf("update = %+v, %v", updated, err)
	}
	if access, _ := s.Access(ctx, created.ID); access.SecretKey != "s2" {
		t.Fatalf("access after update = %+v", access)
	}
	list, err := s.List(ctx)
	if err != nil || len(list) == 0 {
		t.Fatalf("list = %v, %v", list, err)
	}
}
