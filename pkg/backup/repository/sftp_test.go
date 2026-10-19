package repository

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/backup/meta"
	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// startTestSFTPServer spins up an in-process SSH server exposing the SFTP
// subsystem backed by the real filesystem, and returns its listen address.
func startTestSFTPServer(t *testing.T) string {
	t.Helper()

	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatalf("host signer: %v", err)
	}

	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == "tester" && string(pass) == "secret" {
				return nil, nil
			}
			return nil, fmt.Errorf("authentication failed")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSFTPConn(conn, cfg)
		}
	}()
	return ln.Addr().String()
}

func serveSFTPConn(conn net.Conn, cfg *ssh.ServerConfig) {
	sconn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}
		ch, requests, err := newChan.Accept()
		if err != nil {
			continue
		}
		go func(in <-chan *ssh.Request) {
			for req := range in {
				// Accept only the "sftp" subsystem request.
				ok := req.Type == "subsystem" && len(req.Payload) >= 4 && string(req.Payload[4:]) == "sftp"
				_ = req.Reply(ok, nil)
			}
		}(requests)

		server, err := sftp.NewServer(ch)
		if err != nil {
			continue
		}
		go func() {
			_ = server.Serve()
			_ = ch.Close()
		}()
	}
}

func TestSFTPRepositoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	// sftp.NewServer serves the real FS, so the base must be an absolute dir.
	repo := openSFTPRepoAt(t, startTestSFTPServer(t), t.TempDir(), t.TempDir())

	if repo.Scheme() != model.SchemeSFTP {
		t.Fatalf("scheme = %s, want sftp", repo.Scheme())
	}

	system := model.SystemInfo{ID: "web01", Hostname: "web01"}
	if err := repo.EnsureSystem(ctx, system); err != nil {
		t.Fatalf("ensure system: %v", err)
	}

	// Stream a backup file to the server and read it back.
	const filename = "web01-sda1-deadbeef.aha"
	const payload = "streamed backup payload"

	w, err := repo.NewWriter(ctx, system, filename)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	if _, err := io.WriteString(w, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	r, err := repo.NewReader(ctx, system, filename)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("read %q, want %q", got, payload)
	}

	// Record metadata (uploaded to the server by UpdateMeta), then read it back
	// through a fresh Meta call (downloaded from the server).
	rec := model.BackupRecord{
		ID:          "b1",
		Hostname:    system.Hostname,
		Partition:   "sda1",
		FileName:    filename,
		Compression: model.CompressionZstd,
		Scheme:      model.SchemeSFTP,
		SizeBytes:   int64(len(payload)),
		Status:      model.BackupCompleted,
		CreatedAt:   time.Now().UTC(),
	}
	if err := repo.UpdateMeta(ctx, system, func(ctx context.Context, store meta.Store) error {
		if err := store.UpsertSystem(ctx, system); err != nil {
			return err
		}
		return store.InsertBackup(ctx, rec)
	}); err != nil {
		t.Fatalf("update meta: %v", err)
	}

	store, err := repo.Meta(ctx, system)
	if err != nil {
		t.Fatalf("meta open: %v", err)
	}
	defer store.Close()
	records, err := store.ListBackups(ctx)
	if err != nil {
		t.Fatalf("list backups: %v", err)
	}
	if len(records) != 1 || records[0].ID != "b1" {
		t.Fatalf("records = %+v, want one record b1", records)
	}
}

// TestSFTPMetaIsReadOnly verifies that changes written through Meta are not
// persisted: listing a repository must never publish metadata of its own.
func TestSFTPMetaIsReadOnly(t *testing.T) {
	ctx := context.Background()
	repo := openSFTPRepoAt(t, startTestSFTPServer(t), t.TempDir(), t.TempDir())
	system := model.SystemInfo{ID: "web01", Hostname: "web01"}

	store, err := repo.Meta(ctx, system)
	if err != nil {
		t.Fatalf("meta open: %v", err)
	}
	if err := store.InsertBackup(ctx, model.BackupRecord{
		ID: "ghost", Hostname: system.Hostname, Partition: "sda1", FileName: "x.aha",
		Compression: model.CompressionZstd, Scheme: model.SchemeSFTP,
		Status: model.BackupRunning, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert backup: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("meta close: %v", err)
	}

	store2, err := repo.Meta(ctx, system)
	if err != nil {
		t.Fatalf("meta reopen: %v", err)
	}
	defer store2.Close()
	records, err := store2.ListBackups(ctx)
	if err != nil {
		t.Fatalf("list backups: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("records = %+v, want none: Meta must not persist changes", records)
	}
}

// TestSFTPConcurrentUpdateMeta covers the lost-update this locking exists to
// prevent: several backup processes recording different partitions of the same
// system at once. Each repository instance holds its own lock file descriptor,
// so they contend exactly as separate processes do.
func TestSFTPConcurrentUpdateMeta(t *testing.T) {
	ctx := context.Background()
	addr := startTestSFTPServer(t)
	base := t.TempDir()
	lockDir := t.TempDir()
	system := model.SystemInfo{ID: "web01", Hostname: "web01"}

	const writers = 4
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			repo := openSFTPRepoAt(t, addr, base, lockDir)
			rec := model.BackupRecord{
				ID:          fmt.Sprintf("b%d", i),
				Hostname:    system.Hostname,
				Partition:   fmt.Sprintf("sda%d", i),
				FileName:    fmt.Sprintf("web01-sda%d-deadbeef.aha", i),
				Compression: model.CompressionZstd,
				Scheme:      model.SchemeSFTP,
				Status:      model.BackupCompleted,
				CreatedAt:   time.Now().UTC(),
			}
			errs <- repo.UpdateMeta(ctx, system, func(ctx context.Context, store meta.Store) error {
				if err := store.UpsertSystem(ctx, system); err != nil {
					return err
				}
				return store.InsertBackup(ctx, rec)
			})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("update meta: %v", err)
		}
	}

	repo := openSFTPRepoAt(t, addr, base, lockDir)
	store, err := repo.Meta(ctx, system)
	if err != nil {
		t.Fatalf("meta open: %v", err)
	}
	defer store.Close()
	records, err := store.ListBackups(ctx)
	if err != nil {
		t.Fatalf("list backups: %v", err)
	}
	if len(records) != writers {
		t.Fatalf("got %d records, want %d: concurrent updates lost records (%+v)", len(records), writers, records)
	}
}

// openSFTPRepoAt opens a repository against an already-running test server.
// Several repositories can share addr, base and lockDir to stand in for
// concurrent backup processes on one host.
func openSFTPRepoAt(t *testing.T, addr, base, lockDir string) Repository {
	t.Helper()

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}

	repo, err := Open(context.Background(), fmt.Sprintf("sftp://tester@%s:%s%s", host, port, base),
		WithSFTPAuth(SFTPAuth{Password: "secret", Insecure: true, ConnectTimeout: 10 * time.Second}),
		WithLockDir(lockDir))
	if err != nil {
		t.Fatalf("open sftp repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}
