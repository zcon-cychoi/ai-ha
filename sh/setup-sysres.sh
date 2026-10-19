#!/bin/bash
# EC2 shell user-data: set SYSRES_URL and SYSRES_SHA256 below, then paste this entire file.
# Or: sudo env SYSRES_URL='https://...' SYSRES_SHA256='<64 hex>' bash setup-sysres.sh
# Or, from a pinned release over HTTPS (curl -f so an HTTP error page is never run):
#   curl -fsSL --proto '=https' --proto-redir '=https' <release-url>/setup-sysres.sh |
#     SYSRES_URL='...' SYSRES_SHA256='<64 hex>' SYSRES_REBOOT=1 bash
# SYSRES_SHA256 is required: the archive is rejected unless its SHA-256 matches.
# Requires Bash 4+, curl, GNU tar, util-linux and installed GRUB 2 tools.
# The trusted tar must contain sysresccd/ directly (not a wrapper directory).
# Always selects sysres for the next boot. Reboots only with --reboot or SYSRES_REBOOT=1.
# copytoram is configured in the SystemRescue YAML, not here.
set +x
set -Eeuo pipefail
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
umask 077

# Everything below runs inside main(), called on the last line. Under curl | bash,
# bash executes as it reads, so a download cut short would run a partial script.
# Wrapped, a truncated file is an unclosed function: a syntax error, nothing runs.
# It also means no command below can swallow the rest of the script from stdin.
main() {

SYSRES_URL=${SYSRES_URL:-}
# SHA-256 of sysres.tar (64 hex digits), e.g. from: sha256sum sysres.tar
SYSRES_SHA256=${SYSRES_SHA256:-''}
# Optional explicit settings for ambiguous/BLS configurations:
GRUB_CFG=${GRUB_CFG:-}
KERNEL_CMD=${KERNEL_CMD:-}
INITRD_CMD=${INITRD_CMD:-}
# 1 = reboot into sysres when done, same as --reboot. Lets callers that cannot pass
# arguments (pasted user-data, curl | bash without -s) request the reboot.
SYSRES_REBOOT=${SYSRES_REBOOT:-0}

log() { printf '[sysres-setup] %s\n' "$*"; }
die() { log "ERROR: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null || die "Required command missing: $1"; }

# Reject anything but 0/1: silently treating "yes" as 0 would leave the instance on
# its OS until the framework's wait timed out.
[[ $SYSRES_REBOOT == [01] ]] || die 'Set SYSRES_REBOOT to 0 or 1.'
REBOOT=$SYSRES_REBOOT

while (( $# )); do
    case "$1" in
        --reboot) REBOOT=1 ;;
        -h|--help)
            printf 'Usage: SYSRES_URL=http[s]://... SYSRES_SHA256=<64 hex> [SYSRES_REBOOT=1] bash setup-sysres.sh [--reboot]\n'
            printf 'Always selects sysres for the next boot; --reboot or SYSRES_REBOOT=1 also reboots the OS.\n'
            exit 0 ;;
        *) die "Unknown option: $1" ;;
    esac
    shift
done

[[ $EUID == 0 ]] || die 'Run as root.'
[[ $(uname -m) == x86_64 ]] || die 'This archive layout requires x86_64.'
[[ ( $SYSRES_URL == http://* || $SYSRES_URL == https://* ) && $SYSRES_URL != *$'\n'* && $SYSRES_URL != *$'\r'* ]] || die 'Set SYSRES_URL to an HTTP or HTTPS URL.'
# Checked before the download so a missing checksum fails fast.
[[ $SYSRES_SHA256 =~ ^[0-9a-fA-F]{64}$ ]] || die 'Set SYSRES_SHA256 to the 64-hex-digit SHA-256 of sysres.tar.'
SYSRES_SHA256=${SYSRES_SHA256,,}
for c in curl tar findmnt awk sort readlink mktemp cp mv chmod flock od sha256sum; do need "$c"; done
[[ -d /etc/grub.d ]] || die 'GRUB 2 /etc/grub.d is required (GRUB Legacy is unsupported).'

# Tool names are distribution-specific; this is not boot-mode inference.
if command -v grub2-probe >/dev/null; then
    PROBE=grub2-probe; MKCONFIG=grub2-mkconfig; CHECK=grub2-script-check; NEXT_BOOT=grub2-reboot
else
    PROBE=grub-probe; MKCONFIG=grub-mkconfig; CHECK=grub-script-check; NEXT_BOOT=grub-reboot
fi
for c in "$PROBE" "$MKCONFIG" "$CHECK" "$NEXT_BOOT"; do need "$c"; done
if (( REBOOT )); then need reboot; need sync; fi

exec 9>/run/sysres-setup.lock
flock -n 9 || die 'Another setup process is running.'

# Detect exposed Secure Boot state. Absence of efivars is NOT proof it is off.
if [[ -d /sys/firmware/efi ]]; then
    for flag in /sys/firmware/efi/efivars/SecureBoot-*; do
        [[ -f $flag ]] || continue
        secure=$(od -An -tu1 -j4 -N1 "$flag")
        [[ ${secure//[[:space:]]/} != 1 ]] || die 'Secure Boot is enabled; this script does not enroll SystemRescue signing keys.'
    done
fi

# Accept only a full generated config, never an EFI forwarding stub.
full_config() {
    [[ -f $1 ]] && awk '
      /^[[:space:]]*menuentry[[:space:]]/ { found=1 }
      /^[[:space:]]*blscfg([[:space:]]|$)/ { found=1 }
      END { exit !found }
    ' "$1"
}
if [[ -z $GRUB_CFG ]]; then
    candidates=()
    for p in /boot/grub/grub.cfg /boot/grub2/grub.cfg /etc/grub2.cfg /etc/grub2-efi.cfg; do
        if full_config "$p"; then candidates+=("$(readlink -f "$p")"); fi
    done
    configs=$(printf '%s\n' "${candidates[@]}" | awk 'NF' | sort -u)
    [[ -n $configs && $configs != *$'\n'* ]] || die 'Cannot identify one main grub.cfg. Set GRUB_CFG explicitly.'
    GRUB_CFG=$configs
fi
full_config "$GRUB_CFG" || die 'GRUB_CFG must point to a full GRUB 2 config, not an EFI stub.'
GRUB_CFG=$(readlink -f "$GRUB_CFG")

# Only inspect native 10_linux entries. Do not copy commands from an old
# SystemRescue/Windows/custom entry or infer linuxefi merely from UEFI.
if [[ -z $KERNEL_CMD && -z $INITRD_CMD ]]; then
    pair=$(awk '
      /^### BEGIN .*\/10_linux ###/ { native=1; next }
      /^### END .*\/10_linux ###/ { native=0 }
      native && $1=="menuentry" { kernel="" }
      native && $1 ~ /^(linux|linux16|linuxefi)$/ { kernel=$1 }
      native && kernel!="" && $1 ~ /^(initrd|initrd16|initrdefi)$/ {
        print kernel " " $1; kernel=""
      }
    ' "$GRUB_CFG" | sort -u)
    [[ -n $pair && $pair != *$'\n'* ]] || die 'Native loader commands are absent/ambiguous (for example BLS). Set verified KERNEL_CMD and INITRD_CMD explicitly.'
    read -r KERNEL_CMD INITRD_CMD <<< "$pair"
fi
case "$KERNEL_CMD $INITRD_CMD" in
    'linux initrd'|'linux16 initrd16'|'linuxefi initrdefi') ;;
    *) die 'Use a verified pair: linux/initrd, linux16/initrd16, or linuxefi/initrdefi.' ;;
esac

# Require /sysresccd on the actual root filesystem with normal root paths.
# Btrfs subvolumes need a separate archiso mount/path design.
root_fs=$(findmnt -n -o FSTYPE -T /)
case "$root_fs" in ext2|ext3|ext4|xfs) ;; *) die "Unsupported root layout in this script: $root_fs" ;; esac
[[ ! -L /sysresccd ]] || die '/sysresccd must not be a symlink.'
if [[ -e /sysresccd ]]; then
    [[ $(findmnt -n -o TARGET -T /sysresccd) == / ]] || die '/sysresccd must reside on the root mount.'
fi

UUID=$("$PROBE" --target=fs_uuid / 9>&-)
FS_MODULE=$("$PROBE" --target=fs / 9>&-)
ABSTRACTION=$("$PROBE" --target=abstraction / 9>&-)
PARTMAP=$("$PROBE" --target=partmap / 9>&-)
[[ $UUID =~ ^[a-zA-Z0-9-]+$ ]] || die 'Expected one filesystem UUID.'
[[ $FS_MODULE =~ ^[a-zA-Z0-9_]+$ ]] || die 'Unexpected filesystem module.'
modules=()
for m in $PARTMAP; do
    [[ $m =~ ^[a-zA-Z0-9_]+$ ]] || die 'Unexpected partition module.'
    modules+=("part_$m")
done
for m in $ABSTRACTION; do
    case "$m" in
        lvm|diskfilter) modules+=("$m") ;;
        *) die "Unsupported storage abstraction: $m (requires separate live-initramfs validation)." ;;
    esac
done
modules+=("$FS_MODULE" search search_fs_uuid)

work=$(mktemp -d /var/tmp/sysres-setup.XXXXXXXX)
entry=/etc/grub.d/41_sysres
entry_tmp=
cfg_tmp=
entry_changed=0
committed=0
cleanup() {
    rc=$?
    trap - EXIT
    if (( entry_changed && ! committed )); then
        if [[ -f $work/previous-entry ]]; then
            cp -p "$work/previous-entry" "$entry" || log 'ERROR: Could not restore previous custom entry.'
        else
            rm -f "$entry" || log 'ERROR: Could not remove custom entry.'
        fi
    fi
    [[ -z $entry_tmp ]] || rm -f "$entry_tmp"
    [[ -z $cfg_tmp ]] || rm -f "$cfg_tmp"
    rm -rf "$work"
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

log 'Downloading sysres.tar...'
# curl config on stdin keeps the signed URL out of curl argv and normal logs.
escaped_url=${SYSRES_URL//\\/\\\\}
escaped_url=${escaped_url//\"/\\\"}
printf 'url = "%s"\n' "$escaped_url" |
    curl --config - --fail --silent --show-error --location \
      --proto '=http,https' --proto-redir '=http,https' \
      --connect-timeout 30 --max-time 3600 --retry 3 \
      --output "$work/sysres.tar"
unset SYSRES_URL escaped_url
# Verify before reading the archive at all. The signed URL authenticates the
# request to the server, not the bytes returned, so plain HTTP needs this.
log 'Verifying sysres.tar SHA-256...'
actual_sha256=$(sha256sum "$work/sysres.tar" | awk '{ print $1 }')
[[ $actual_sha256 == "$SYSRES_SHA256" ]] || die "Checksum mismatch: expected $SYSRES_SHA256, got $actual_sha256"
tar -tf "$work/sysres.tar" > "$work/members"
for member in sysresccd/boot/x86_64/vmlinuz sysresccd/boot/x86_64/sysresccd.img sysresccd/x86_64/airootfs.sfs; do
    awk -v wanted="$member" '{ sub(/^\.\//, ""); if ($0==wanted) found=1 } END { exit !found }' "$work/members" || die "Archive is missing $member"
done
# Trusted because its SHA-256 matched above; this is not an untrusted-archive sandbox.
log 'Extracting trusted sysres.tar into /...'
tar --extract --file="$work/sysres.tar" --directory=/ --no-same-owner --no-overwrite-dir
for f in /sysresccd/boot/x86_64/vmlinuz /sysresccd/boot/x86_64/sysresccd.img /sysresccd/x86_64/airootfs.sfs; do
    [[ -s $f && ! -L $f ]] || die "Missing/invalid extracted file: $f"
    [[ $(findmnt -n -o TARGET -T "$f") == / ]] || die "File is outside the root mount: $f"
    [[ $("$PROBE" --target=fs_uuid "$f" 9>&-) == "$UUID" ]] || die "Filesystem changed: $f"
done

entry_tmp=$(mktemp /etc/grub.d/.sysres.XXXXXXXX)
{
    printf '#!/bin/sh\nexec tail -n +3 "$0"\n'
    printf '# Managed by setup-sysres.sh\nmenuentry "sysres" --id sysres {\n'
    for m in "${modules[@]}"; do printf '    insmod %s\n' "$m"; done
    printf '    search --no-floppy --fs-uuid --set=root %s\n' "$UUID"
    printf '    %s /sysresccd/boot/x86_64/vmlinuz archisobasedir=sysresccd archisodevice=/dev/disk/by-uuid/%s\n' "$KERNEL_CMD" "$UUID"
    printf '    %s /sysresccd/boot/x86_64/sysresccd.img\n}\n' "$INITRD_CMD"
} > "$entry_tmp"
sh "$entry_tmp" > "$work/entry.cfg"
"$CHECK" "$work/entry.cfg"
[[ ! -L $entry ]] || die 'Custom entry must not be a symlink.'
if [[ -e $entry ]]; then cp -p "$entry" "$work/previous-entry"; fi
chmod 755 "$entry_tmp"
entry_changed=1
mv -f "$entry_tmp" "$entry"
entry_tmp=

log "Regenerating $GRUB_CFG ($KERNEL_CMD / $INITRD_CMD; UUID=$UUID)..."
# Same generator used by update-grub; stage and check before replacing main cfg.
cfg_tmp=$(mktemp "${GRUB_CFG%/*}/.sysres-grub.XXXXXXXX")
"$MKCONFIG" -o "$cfg_tmp" 9>&-
"$CHECK" "$cfg_tmp"
awk '/^menuentry "sysres" --id sysres / { found=1 } END { exit !found }' "$cfg_tmp" || die 'Generated config is missing the sysres entry.'
backup="${GRUB_CFG}.before-sysres.$(date +%Y%m%d%H%M%S).$$"
cp -p "$GRUB_CFG" "$backup"
chmod --reference="$GRUB_CFG" "$cfg_tmp"
chown --reference="$GRUB_CFG" "$cfg_tmp"
mv -f "$cfg_tmp" "$GRUB_CFG"
cfg_tmp=
committed=1
if command -v restorecon >/dev/null; then restorecon "$entry" "$GRUB_CFG"; fi
log "Done. Custom entry: $entry; previous config: $backup"
# Keep FD 9 only in this parent shell, including during GRUB helper calls.
log 'Selecting sysres for the next boot...'
"$NEXT_BOOT" sysres 9>&- || die 'Could not set the next boot entry; reboot was not requested.'
log 'Next boot entry set to sysres.'
if (( REBOOT )); then
    log 'Rebooting...'
    sync
    reboot 9>&-
else
    log 'Setup complete. Reboot skipped; use --reboot or SYSRES_REBOOT=1 to reboot automatically.'
fi
}

main "$@"
