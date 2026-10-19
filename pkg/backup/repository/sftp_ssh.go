package repository

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"

	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// parseSFTPTarget splits an sftp:// URI into host, port, user and remote base
// path. e.g. "sftp://backup@nas:2222/srv/aiha" ->
// ("nas", "2222", "backup", "/srv/aiha").
func parseSFTPTarget(uri string) (host, port, user, base string, err error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", "", "", "", fmt.Errorf("parse sftp URI: %w", err)
	}
	if u.Scheme != "sftp" {
		return "", "", "", "", fmt.Errorf("not an sftp URI: %q", uri)
	}
	host = u.Hostname()
	if host == "" {
		return "", "", "", "", fmt.Errorf("sftp URI missing host: %q", uri)
	}
	port = u.Port()
	if port == "" {
		port = defaultSFTPPort
	}
	if u.User != nil {
		user = u.User.Username()
	}
	base = path.Clean(u.Path)
	if base == "" || base == "." {
		return "", "", "", "", fmt.Errorf("sftp URI missing base path: %q", uri)
	}
	return host, port, user, base, nil
}

// sshAuthMethods builds the SSH auth methods from the configured credentials.
// Public-key auth is preferred; password auth is added when a password is set.
func sshAuthMethods(auth SFTPAuth) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	if auth.PrivateKeyPath != "" {
		key, err := os.ReadFile(auth.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("read sftp private key: %w", err)
		}
		var signer ssh.Signer
		if auth.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(key, []byte(auth.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(key)
		}
		if err != nil {
			return nil, fmt.Errorf("parse sftp private key: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if auth.Password != "" {
		methods = append(methods, ssh.Password(auth.Password))
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("no sftp credentials: set AIHA_BACKUP_SFTP_KEY or AIHA_BACKUP_SFTP_PASSWORD")
	}
	return methods, nil
}

// sshHostKeyCallback returns the host key verification callback. By default it
// verifies against a known_hosts file (~/.ssh/known_hosts unless overridden).
// Verification is skipped only when Insecure is explicitly set.
func sshHostKeyCallback(auth SFTPAuth) (ssh.HostKeyCallback, error) {
	if auth.Insecure {
		log.Warn().Msg("SFTP host key verification is DISABLED (insecure); transfer is exposed to man-in-the-middle attacks")
		return ssh.InsecureIgnoreHostKey(), nil //nolint:gosec // explicit user opt-in
	}

	khPath := auth.KnownHostsPath
	if khPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home for known_hosts: %w", err)
		}
		khPath = filepath.Join(home, ".ssh", "known_hosts")
	}
	cb, err := knownhosts.New(khPath)
	if err != nil {
		return nil, fmt.Errorf("load known_hosts %q (set AIHA_BACKUP_SFTP_KNOWN_HOSTS or AIHA_BACKUP_SFTP_INSECURE=true): %w", khPath, err)
	}
	return cb, nil
}
