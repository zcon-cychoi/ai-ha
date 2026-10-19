package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/backup/meta"
	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/pkg/sftp"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"
)

// defaultSFTPPort is used when the URI omits a port.
const defaultSFTPPort = "22"

// defaultConnectTimeout bounds the TCP dial and SSH handshake.
const defaultConnectTimeout = 30 * time.Second

// SFTPAuth holds the SSH credentials and connection options for the SFTP
// backend. Values are typically loaded from AIHA_BACKUP_SFTP_* environment
// variables (see conf/template-setup.env).
type SFTPAuth struct {
	// User overrides the user in the URI when set.
	User string
	// Password enables password authentication.
	Password string
	// PrivateKeyPath enables public-key authentication.
	PrivateKeyPath string
	// Passphrase decrypts an encrypted private key.
	Passphrase string
	// KnownHostsPath is the known_hosts file used for host key verification.
	// Defaults to ~/.ssh/known_hosts.
	KnownHostsPath string
	// Insecure disables host key verification. Off by default; enabling it
	// exposes the transfer to man-in-the-middle attacks.
	Insecure bool
	// ConnectTimeout bounds the dial and handshake; defaults to 30s.
	ConnectTimeout time.Duration
}

// sftpDialer captures the resolved connection parameters so a fresh SSH+SFTP
// connection can be established on demand. The repository dials once for its own
// use; a resumable sink holds the dialer to reconnect on its own after a network
// interruption (see sftpSink).
type sftpDialer struct {
	addr    string
	base    string // remote base directory (POSIX path)
	sshCfg  *ssh.ClientConfig
	timeout time.Duration
}

// dial establishes a new SSH connection and opens an SFTP session on it.
func (d *sftpDialer) dial(ctx context.Context) (*ssh.Client, *sftp.Client, error) {
	conn, err := (&net.Dialer{Timeout: d.timeout}).DialContext(ctx, "tcp", d.addr)
	if err != nil {
		return nil, nil, fmt.Errorf("sftp dial %s: %w", d.addr, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, d.addr, d.sshCfg)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("sftp handshake %s: %w", d.addr, err)
	}
	sshClient := ssh.NewClient(sshConn, chans, reqs)

	client, err := sftp.NewClient(sshClient)
	if err != nil {
		_ = sshClient.Close()
		return nil, nil, fmt.Errorf("sftp session: %w", err)
	}
	return sshClient, client, nil
}

// sftpRepository stores backups on a remote SFTP server. Backup bytes are
// streamed directly to the server (the real-time transfer); per-system SQLite
// metadata is synced through a local working copy (SFTP has no random-access
// database support).
type sftpRepository struct {
	*metaSync
	ssh    *ssh.Client
	client *sftp.Client
	base   string // remote base directory (POSIX path)
	dialer *sftpDialer
}

func newSFTP(ctx context.Context, uri string, auth SFTPAuth, lockDir string) (Repository, error) {
	host, port, user, base, err := parseSFTPTarget(uri)
	if err != nil {
		return nil, err
	}
	if auth.User != "" {
		user = auth.User
	}
	if user == "" {
		return nil, fmt.Errorf("sftp repository: user is required (in URI or AIHA_BACKUP_SFTP_USER)")
	}

	authMethods, err := sshAuthMethods(auth)
	if err != nil {
		return nil, err
	}
	hostKeyCallback, err := sshHostKeyCallback(auth)
	if err != nil {
		return nil, err
	}

	timeout := auth.ConnectTimeout
	if timeout <= 0 {
		timeout = defaultConnectTimeout
	}
	dialer := &sftpDialer{
		addr: net.JoinHostPort(host, port),
		base: base,
		sshCfg: &ssh.ClientConfig{
			User:            user,
			Auth:            authMethods,
			HostKeyCallback: hostKeyCallback,
			Timeout:         timeout,
		},
		timeout: timeout,
	}

	log.Info().Str("host", dialer.addr).Str("user", user).Str("base", base).Msg("Connecting to SFTP repository")

	sshClient, client, err := dialer.dial(ctx)
	if err != nil {
		return nil, err
	}

	if err := client.MkdirAll(base); err != nil {
		_ = client.Close()
		_ = sshClient.Close()
		return nil, fmt.Errorf("create repository base %q: %w", base, err)
	}

	sync, err := newMetaSync(uri, "aiha-sftp-meta-", lockDir)
	if err != nil {
		_ = client.Close()
		_ = sshClient.Close()
		return nil, err
	}

	return &sftpRepository{metaSync: sync, ssh: sshClient, client: client, base: base, dialer: dialer}, nil
}

func (r *sftpRepository) Scheme() model.TransportScheme { return model.SchemeSFTP }

// systemDir returns the remote directory for a system, guarding against path
// traversal via a crafted hostname.
func (r *sftpRepository) systemDir(system model.SystemInfo) (string, error) {
	name := system.DirName()
	if name == "" || name != path.Base(name) || name == "." || name == ".." {
		return "", fmt.Errorf("invalid system name %q", name)
	}
	return path.Join(r.base, name), nil
}

func (r *sftpRepository) EnsureSystem(_ context.Context, system model.SystemInfo) error {
	dir, err := r.systemDir(system)
	if err != nil {
		return err
	}
	return r.client.MkdirAll(dir)
}

func (r *sftpRepository) NewWriter(_ context.Context, system model.SystemInfo, filename string) (io.WriteCloser, error) {
	dir, err := r.systemDir(system)
	if err != nil {
		return nil, err
	}
	if filename != path.Base(filename) {
		return nil, fmt.Errorf("invalid backup file name %q", filename)
	}
	// O_EXCL guards against overwriting an existing backup file. The returned
	// *sftp.File streams writes to the server as they happen.
	f, err := r.client.OpenFile(path.Join(dir, filename), os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		return nil, fmt.Errorf("create remote backup file: %w", err)
	}
	return f, nil
}

func (r *sftpRepository) NewReader(_ context.Context, system model.SystemInfo, filename string) (io.ReadCloser, error) {
	dir, err := r.systemDir(system)
	if err != nil {
		return nil, err
	}
	if filename != path.Base(filename) {
		return nil, fmt.Errorf("invalid backup file name %q", filename)
	}
	f, err := r.client.Open(path.Join(dir, filename))
	if err != nil {
		return nil, fmt.Errorf("open remote backup file: %w", err)
	}
	return f, nil
}

// Meta downloads the system's remote metadata database into a local working
// copy, opens it, and returns a Store that uploads the copy back on Close.
// metaLocation returns the remote path of the system's metadata database.
func (r *sftpRepository) metaLocation(system model.SystemInfo) (string, error) {
	dir, err := r.systemDir(system)
	if err != nil {
		return "", err
	}
	return path.Join(dir, metaFileName), nil
}

// Meta opens a read-only copy of the system's remote metadata.
func (r *sftpRepository) Meta(ctx context.Context, system model.SystemInfo) (meta.Store, error) {
	return r.open(ctx, r, system)
}

// UpdateMeta applies fn to the system's remote metadata under an exclusive lock.
func (r *sftpRepository) UpdateMeta(ctx context.Context, system model.SystemInfo, fn func(context.Context, meta.Store) error) error {
	return r.update(ctx, r, system, fn)
}

func (r *sftpRepository) Close() error {
	cerr := r.client.Close()
	serr := r.ssh.Close()
	rerr := r.cleanup()
	return errors.Join(cerr, serr, rerr)
}

// downloadMeta copies remote to localPath. A missing remote file is not an
// error: the local copy is left empty so a fresh database is created.
func (r *sftpRepository) downloadMeta(_ context.Context, remote, localPath string) error {
	rf, err := r.client.Open(remote)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("download metadata: %w", err)
	}
	defer rf.Close()

	lf, err := os.OpenFile(localPath, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()

	if _, err := io.Copy(lf, rf); err != nil {
		return fmt.Errorf("download metadata: %w", err)
	}
	return nil
}

// uploadMeta copies localPath to remote, replacing it.
func (r *sftpRepository) uploadMeta(_ context.Context, localPath, remote string) error {
	lf, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer lf.Close()

	if err := r.client.MkdirAll(path.Dir(remote)); err != nil {
		return fmt.Errorf("create metadata dir: %w", err)
	}

	rf, err := r.client.OpenFile(remote, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("upload metadata: %w", err)
	}
	if _, err := io.Copy(rf, lf); err != nil {
		_ = rf.Close()
		return fmt.Errorf("upload metadata: %w", err)
	}
	if err := rf.Close(); err != nil {
		return fmt.Errorf("upload metadata: %w", err)
	}
	return nil
}
