package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/backup/meta"
	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloudsoda/go-smb2"
	"github.com/rs/zerolog/log"
)

// defaultSMBPort is used when the URI omits a port.
const defaultSMBPort = "445"

// SMBAuth holds the credentials and options for the SMB/CIFS backend. Values are
// typically loaded from AIHA_BACKUP_SMB_* environment variables.
type SMBAuth struct {
	// User overrides the user in the URI when set. Empty means guest.
	User string
	// Password for NTLM authentication.
	Password string
	// Domain (or workgroup) for NTLM authentication.
	Domain string
	// ConnectTimeout bounds the TCP dial and negotiation; defaults to 30s.
	ConnectTimeout time.Duration
}

// smbDialer captures the resolved connection parameters so a fresh
// connection + session + share mount can be established on demand. The
// repository dials once for its own use; a resumable sink holds the dialer to
// reconnect on its own after a network interruption (see smbSink).
type smbDialer struct {
	addr      string
	shareName string
	base      string // base directory within the share (forward-slash)
	initiator smb2.Initiator
	timeout   time.Duration
}

// dial establishes a new TCP connection, negotiates an SMB session and mounts
// the share.
func (d *smbDialer) dial(ctx context.Context) (net.Conn, *smb2.Session, *smb2.Share, error) {
	conn, err := (&net.Dialer{Timeout: d.timeout}).DialContext(ctx, "tcp", d.addr)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("smb dial %s: %w", d.addr, err)
	}
	sd := &smb2.Dialer{Initiator: d.initiator}
	session, err := sd.DialConn(ctx, conn, d.addr)
	if err != nil {
		_ = conn.Close()
		return nil, nil, nil, fmt.Errorf("smb session %s: %w", d.addr, err)
	}
	share, err := session.Mount(d.shareName)
	if err != nil {
		_ = session.Logoff()
		_ = conn.Close()
		return nil, nil, nil, fmt.Errorf("mount share %q: %w", d.shareName, err)
	}
	return conn, session, share, nil
}

// closeSMB tears down a connection triple, ignoring errors.
func closeSMB(conn net.Conn, session *smb2.Session, share *smb2.Share) {
	if share != nil {
		_ = share.Umount()
	}
	if session != nil {
		_ = session.Logoff()
	}
	if conn != nil {
		_ = conn.Close()
	}
}

// smbRepository stores backups on an SMB/CIFS share. Backup bytes are streamed
// directly to the share (the real-time transfer); per-system SQLite metadata is
// synced through a local working copy, as with the sftp backend.
type smbRepository struct {
	*metaSync
	conn    net.Conn
	session *smb2.Session
	share   *smb2.Share
	base    string // base directory within the share (forward-slash)
	dialer  *smbDialer
}

func newSMB(ctx context.Context, uri string, auth SMBAuth, lockDir string) (Repository, error) {
	host, port, user, shareName, base, err := parseSMBTarget(uri)
	if err != nil {
		return nil, err
	}
	if auth.User != "" {
		user = auth.User
	}

	timeout := auth.ConnectTimeout
	if timeout <= 0 {
		timeout = defaultConnectTimeout
	}

	dialer := &smbDialer{
		addr:      net.JoinHostPort(host, port),
		shareName: shareName,
		base:      base,
		initiator: &smb2.NTLMInitiator{
			User:     user,
			Password: auth.Password,
			Domain:   auth.Domain,
		},
		timeout: timeout,
	}

	log.Info().Str("host", dialer.addr).Str("share", shareName).Str("user", user).Str("base", base).Msg("Connecting to SMB repository")

	conn, session, share, err := dialer.dial(ctx)
	if err != nil {
		return nil, err
	}

	if base != "" {
		if err := share.MkdirAll(base, 0o750); err != nil {
			closeSMB(conn, session, share)
			return nil, fmt.Errorf("create repository base %q: %w", base, err)
		}
	}

	sync, err := newMetaSync(uri, "aiha-smb-meta-", lockDir)
	if err != nil {
		closeSMB(conn, session, share)
		return nil, err
	}

	return &smbRepository{metaSync: sync, conn: conn, session: session, share: share, base: base, dialer: dialer}, nil
}

func (r *smbRepository) Scheme() model.TransportScheme { return model.SchemeSMB }

// systemDir returns the share-relative directory for a system, guarding against
// path traversal via a crafted hostname.
func (r *smbRepository) systemDir(system model.SystemInfo) (string, error) {
	name := system.DirName()
	if name == "" || name != path.Base(name) || name == "." || name == ".." {
		return "", fmt.Errorf("invalid system name %q", name)
	}
	return path.Join(r.base, name), nil
}

func (r *smbRepository) EnsureSystem(_ context.Context, system model.SystemInfo) error {
	dir, err := r.systemDir(system)
	if err != nil {
		return err
	}
	return r.share.MkdirAll(dir, 0o750)
}

func (r *smbRepository) NewWriter(_ context.Context, system model.SystemInfo, filename string) (io.WriteCloser, error) {
	dir, err := r.systemDir(system)
	if err != nil {
		return nil, err
	}
	if filename != path.Base(filename) {
		return nil, fmt.Errorf("invalid backup file name %q", filename)
	}
	// O_EXCL guards against overwriting an existing backup file. The returned
	// *smb2.File streams writes to the share as they happen.
	f, err := r.share.OpenFile(path.Join(dir, filename), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("create remote backup file: %w", err)
	}
	return f, nil
}

func (r *smbRepository) NewReader(_ context.Context, system model.SystemInfo, filename string) (io.ReadCloser, error) {
	dir, err := r.systemDir(system)
	if err != nil {
		return nil, err
	}
	if filename != path.Base(filename) {
		return nil, fmt.Errorf("invalid backup file name %q", filename)
	}
	f, err := r.share.Open(path.Join(dir, filename))
	if err != nil {
		return nil, fmt.Errorf("open remote backup file: %w", err)
	}
	return f, nil
}

// Meta downloads the system's metadata database into a local working copy, opens
// it, and returns a Store that uploads the copy back on Close.
// metaLocation returns the share-relative path of the system's metadata
// database.
func (r *smbRepository) metaLocation(system model.SystemInfo) (string, error) {
	dir, err := r.systemDir(system)
	if err != nil {
		return "", err
	}
	return path.Join(dir, metaFileName), nil
}

// Meta opens a read-only copy of the system's metadata on the share.
func (r *smbRepository) Meta(ctx context.Context, system model.SystemInfo) (meta.Store, error) {
	return r.open(ctx, r, system)
}

// UpdateMeta applies fn to the system's metadata on the share under an
// exclusive lock.
func (r *smbRepository) UpdateMeta(ctx context.Context, system model.SystemInfo, fn func(context.Context, meta.Store) error) error {
	return r.update(ctx, r, system, fn)
}

func (r *smbRepository) Close() error {
	uerr := r.share.Umount()
	lerr := r.session.Logoff()
	cerr := r.conn.Close()
	rerr := r.cleanup()
	return errors.Join(uerr, lerr, cerr, rerr)
}

// downloadMeta copies a share file into localPath. A missing remote file is not
// an error: the local copy is left empty so a fresh database is created.
func (r *smbRepository) downloadMeta(_ context.Context, remote, localPath string) error {
	rf, err := r.share.Open(remote)
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

// uploadMeta copies localPath to a share file, replacing it.
func (r *smbRepository) uploadMeta(_ context.Context, localPath, remote string) error {
	lf, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer lf.Close()

	if err := r.share.MkdirAll(path.Dir(remote), 0o750); err != nil {
		return fmt.Errorf("create metadata dir: %w", err)
	}

	rf, err := r.share.OpenFile(remote, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
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

// parseSMBTarget splits an smb:// URI into host, port, user, share and base
// path. The first path segment is the share name, the rest is the base path
// within the share. e.g. "smb://user@nas/backups/aiha" ->
// ("nas", "445", "user", "backups", "aiha").
func parseSMBTarget(uri string) (host, port, user, share, base string, err error) {
	u, perr := url.Parse(uri)
	if perr != nil {
		return "", "", "", "", "", fmt.Errorf("parse smb URI: %w", perr)
	}
	if u.Scheme != "smb" {
		return "", "", "", "", "", fmt.Errorf("not an smb URI: %q", uri)
	}
	host = u.Hostname()
	if host == "" {
		return "", "", "", "", "", fmt.Errorf("smb URI missing host: %q", uri)
	}
	port = u.Port()
	if port == "" {
		port = defaultSMBPort
	}
	if u.User != nil {
		user = u.User.Username()
	}

	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segments) == 0 || segments[0] == "" {
		return "", "", "", "", "", fmt.Errorf("smb URI missing share name: %q", uri)
	}
	share = segments[0]
	base = path.Join(segments[1:]...)
	return host, port, user, share, base, nil
}
