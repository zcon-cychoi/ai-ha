# aiha-agent

The AI-HA agent. It runs on the instance being protected: it archives Linux
partitions with `tar` + `zstd` into a backup repository, keeping per-system
metadata in SQLite, and in a rescue environment booted for a restore it claims
the instance's restore job from the framework.

## Two builds, one program

| Binary | Build | Contains |
| --- | --- | --- |
| `aiha-agent` | `go build ./aiha-agent` | everything |
| `aiha-rescue` | `go build -tags rescue ./aiha-agent` | `rescue`, `version` |

`aiha-rescue` is what the rescue environment downloads over the network into a
RAM-backed filesystem, so it leaves out the backup engine and every repository
backend. Restoring reads the repository through `rclone`, which the rescue
image already carries, so none of that code is needed there. `make build`
produces both.

## Status

| Area | State |
| --- | --- |
| Linux partition backup (read-only bind mount) | ✅ implemented |
| tar archiving | ✅ implemented |
| Compression: zstd (default), none | ✅ implemented |
| Compression: gzip and others | 🔌 pluggable, not implemented |
| Repository: local (files + per-system SQLite) | ✅ implemented |
| Repository / streaming: sftp (key/password auth, known_hosts) | ✅ implemented |
| Repository / streaming: smb/cifs (NTLM auth) | ✅ implemented |
| Repository / streaming: s3 | ✅ implemented |
| Repository / streaming: http | 🔌 interface only |
| Archive extraction (`extract`) | ✅ implemented |
| Restoring a backup set in the rescue environment (`rescue`, `restore`) | ✅ implemented — same-image target only, see [plan/Restore.md](../plan/Restore.md) 3.8 |
| Rescue claim and restore progress/result reports (`rescue`) | ✅ implemented |
| Enrolment (`register`, `reset`) and the long-poll loop (`run`) | ✅ implemented |
| Local identity and enrolment (`status`) | ✅ implemented |
| Backup ↔ framework: job queue (claim, progress, result) | ❌ not implemented — see [plan/protocol.md](../plan/protocol.md) |
| Container backup (Velero) | 🔌 interface only |

`extract` unpacks an archive into a directory and nothing more. It does not
create filesystems, rewrite `fstab` or install a bootloader, so it is not a
system restore; that is `restore`, which runs in the rescue environment.

## Architecture

```
aiha-agent/             CLI (cobra): register / reset / run / rescue /
  cmd/                                 restore / backup / extract / repo / sysinfo
config/                 configuration (viper, AIHA_AGENT_* and AIHA_BACKUP_* env)
deploy/                 systemd unit
pkg/agent/
  rescue/               restore-job claim: recovery.env → framework → tmpfs,
                        and the progress/result report client
  restore/              restore executor: manifest → mkfs → mountall →
                        rclone | tar → reconfigure → grub in arch-chroot
  framework/            southbound client: enrol, wait for work
  transport/            pinned-TLS HTTP client (shared with rescue)
  state/                agent-local state (/var/lib/aiha)
pkg/backup/
  model/                shared types, file naming, IDs
  source/               backup sources — Linux partition bind-ro-mount
  archive/              tar Archiver (Archive / Extract)
  compress/             Compressor registry — zstd, none (extensible)
  repository/           storage backends — local, sftp, smb, s3 (+ http stub)
  meta/                 per-system SQLite metadata store
  engine/               orchestration: source → tar → codec → repo → metadata
  container/            Velero-based container backup (stub)
```

Backup pipeline: `Source.Prepare` (bind-mount ro) → `TarArchiver.Archive` →
`Compressor` → `Repository.NewWriter` (live stream to the backend) → record the
result in the system's SQLite store.

Backup files are named `{hostname}-{partition}-{randomID}.aha`. Each target
system has its own directory and its own `meta.db` in the repository.

## Usage

Back up a partition (requires root for the bind mount):

```bash
sudo aiha-agent backup --device /dev/sda1 --repo /var/lib/aiha/repo
sudo aiha-agent backup --device /dev/mapper/rl-root --compression none --repo /var/lib/aiha/repo
```

Show what this installation is — the system its backups are filed under, the
agent it enrolled as, and the framework that issued the credential:

```bash
aiha-agent status
```

List a system's backup history and extract an archive. `--system` defaults to
this agent's own, so it is only needed to inspect another machine's backups:

```bash
aiha-agent repo list --repo /var/lib/aiha/repo
aiha-agent extract --file web01-sda1-1a2b3c4d.aha --dest /mnt/restore --repo /var/lib/aiha/repo
aiha-agent repo list --system web01 --repo /var/lib/aiha/repo   # another system
```

Back up to a remote SFTP repository (streamed directly to the server):

```bash
export AIHA_BACKUP_REPOSITORY=sftp://backup@nas:22/srv/aiha
export AIHA_BACKUP_SFTP_KEY=$HOME/.ssh/id_ed25519   # or AIHA_BACKUP_SFTP_PASSWORD
sudo -E aiha-agent backup --device /dev/sda1
```

The host must be in `~/.ssh/known_hosts` (override with
`AIHA_BACKUP_SFTP_KNOWN_HOSTS`); set `AIHA_BACKUP_SFTP_INSECURE=true` to skip
host key verification (testing only). Per-system SQLite metadata is synced to
the server through a local working copy.

Back up to an SMB/CIFS share (streamed directly to the share). The first path
segment is the share name, the rest is the base path within it:

```bash
export AIHA_BACKUP_REPOSITORY=smb://nas/backups/aiha
export AIHA_BACKUP_SMB_USER=backup
export AIHA_BACKUP_SMB_PASSWORD=... # and optionally AIHA_BACKUP_SMB_DOMAIN
sudo -E aiha-agent backup --device /dev/sda1
```

Configuration can also come from the environment (see
[../conf/template-setup.env](../conf/template-setup.env)):

```bash
export AIHA_BACKUP_REPOSITORY=/var/lib/aiha/repo
export AIHA_BACKUP_COMPRESSION=zstd
```

Claim a restore job from inside the rescue environment and restore it.
SystemRescue runs this automatically through `/autorun/autorun2`; run it by
hand with the same flags when investigating:

```bash
aiha-rescue rescue --recovery-file /run/archiso/bootmnt/autorun/aiha/recovery.env --out-dir /run/aiha
```

After the claim it restores the backup set the plan names. **This formats the
partitions the set was taken from**: the target is assumed to be built from the
same image as the source. In order: read the set's manifest from the repository
and check this machine can take it (nothing changes if this fails), recreate the
filesystems with the source's labels and UUIDs, `mountall --no-bind`, stream
each backup file through `rclone cat | zstd -d | tar -x`, point netplan at this
machine's MAC address, check that the UUIDs `fstab`, grub's defaults and the EFI
grub stub name are on this machine (a mismatch is reported as a warning), leave
the recovery marker, then `grub-install`, `update-grub` and `update-initramfs` in
`arch-chroot`. Progress (step n of 6, bytes read of the set's size) and the
result are reported to the framework (`aiha-framework restore get <restoreId|recoveryId>`).
The machine is not rebooted.

To try the restore again without a new claim:

```bash
aiha-rescue restore --recovery-file /run/archiso/bootmnt/autorun/aiha/recovery.env --out-dir /run/aiha
```

## Enrolling with the framework

An agent enrols once, with a token an operator issues:

```bash
# On the framework
aiha-framework agent token issue --ttl 30m --note "web01"

# On the agent host
export AIHA_AGENT_FRAMEWORK_URL=https://framework.example:8443
export AIHA_AGENT_FRAMEWORK_CERT_SHA256=...   # printed by the framework at startup
export AIHA_AGENT_REGISTRATION_TOKEN=...      # or AIHA_AGENT_REGISTRATION_TOKEN_FILE
sudo -E aiha-agent register
sudo systemctl enable --now aiha-agent        # deploy/aiha-agent.service
```

The registration token is spent by a successful enrolment; remove it afterwards.
It is not a command-line flag on purpose — a flag is visible to every process on
the host.

`aiha-agent run` waits for work by long-polling, which is also what reports the
agent alive. `aiha-framework agent list` shows it as online.

A backup job names a repository registered on the framework (`aiha-framework
repository add`); the agent receives its address and credentials when it claims
the job and writes there. The `AIHA_BACKUP_REPOSITORY` / `AIHA_BACKUP_S3_*`
settings on the host are not used by `run` — only by a backup taken by hand with
`aiha-agent backup`, which the framework does not record and cannot restore with
`aiha-framework restore create --backup-set`. The spool settings
(`AIHA_BACKUP_BUFFER_CAP` etc.) apply to both.

### Replacing a credential

An agent token cannot be reissued, so re-enrolling is deliberate:

```bash
aiha-framework agent revoke <agentId>   # on the framework
sudo systemctl stop aiha-agent          # on the agent host
sudo aiha-agent reset                   # clears the credential, keeps the system ID
# then enrol with a new registration token
```

The system ID survives a reset. It is what this system's backup history is filed
under, and re-enrolling must not sever it.

`run` exits with a distinct status for conditions that will not improve by
restarting — 10 re-enrolment needed, 11 awaiting boot verification, 12
configuration — and the systemd unit excludes them from automatic restart. A
framework that is merely unreachable is not one of them: the agent backs off and
retries in-process.

## Agent-local state

`/var/lib/aiha/` holds what identifies this installation to the framework: the
system ID, the agent credential and the registration key. It is **excluded from
a root-partition backup**, so that a restored system does not come up holding
the original's identity and credentials. Moving it with `AIHA_AGENT_STATE_DIR`
moves the exclusion with it.

`recovery-pending.json` marks a system that was restored and whose boot the
framework has not verified. While it is present, `backup` and `run` refuse to
proceed — the system still carries the original's identity, so acting under it
would write into the original's history. Override `backup` with
`--ignore-recovery-pending` only when you know the system is not a restored
duplicate.

## Extending

- **New compression codec**: implement `compress.Compressor` and call
  `compress.Register` from an `init` function (see `compress/none.go`).
- **New repository backend**: implement `repository.Repository` and wire it into
  `repository.Open` (see `repository/local.go`, `repository/sftp.go`).
- **Commands that must stay out of the rescue build** need `//go:build !rescue`;
  see `cmd/backup.go`.

## Notes

- Requires Go 1.25+ (pulled in by `modernc.org/sqlite`).
- The partition source is Linux-only; on other platforms it returns an error.
