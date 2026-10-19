#!/bin/bash
set +x
set -Eeuo pipefail
umask 077

# 프레임워크가 인스턴스 생성 요청마다 채워 넣는 값
SCRIPT_URL='http://배포서버/setup-sysres.sh'
export SYSRES_URL='http://스토리지/아카이브?발급한-presigned-파라미터'
export SYSRES_SHA256='아카이브의_SHA256_64자리'
AGENT_URL='http://스토리지/aiha-rescue?발급한-presigned-파라미터'
AGENT_SHA256='레스큐_에이전트의_SHA256_64자리'
# 복구 정보 — "aiha-framework restore create" 출력값을 그대로 붙여 넣는다
# AIHA_FRAMEWORK_CERT_PIN 은 프레임워크 설정을 따른다. true: 지문으로 인증서를 고정한다(자체 서명
# 인증서). false: 공인 인증서를 OS 신뢰 저장소로 표준 검증하고, 지문은 비어 있다.
AIHA_RECOVERY_ID='복구ID'
AIHA_FRAMEWORK_URL='https://프레임워크주소:포트'
AIHA_CLAIM_TOKEN='클레임토큰'
AIHA_FRAMEWORK_CERT_PIN='true'
AIHA_FRAMEWORK_CERT_SHA256='프레임워크_인증서_SHA256_64자리'

# 레스큐에서 에이전트를 실행하는 /autorun/autorun2 는 이미지(sysres.tar)에 들어 있다(sh/autorun2).
# 여기서는 인스턴스마다 다른 것 — 에이전트와 recovery.env — 만 /autorun/aiha 에 설치한다.
# sysres.tar 를 푼 **뒤에** 설치한다: tar 는 같은 이름의 파일을 덮어쓴다.
# 레스큐로 부팅하면 copytoram 이 이 디렉터리를 램으로 올리고, SystemRescue 가 autorun0..9 를
# 차례로 자동 실행한다. 덕분에 레스큐에서 복구 대상 디스크를 마운트할 일이 없다.
AUTORUN_DIR=/autorun

die() { printf '[aiha-init] ERROR: %s\n' "$*" >&2; exit 1; }

# fetch URL 저장경로 [SHA256]
fetch() {
    curl --fail --silent --show-error --location \
        --proto '=http,https' --proto-redir '=http,https' \
        --connect-timeout 30 --max-time 600 --retry 3 \
        --output "$2" "$1"
    if [[ -n ${3:-} ]]; then
        local actual
        actual=$(sha256sum "$2" | awk '{ print $1 }')
        [[ $actual == "${3,,}" ]] || die "Checksum mismatch for $2: expected ${3,,}, got $actual"
    fi
}

# 레스큐 에이전트와 같은 규칙으로 먼저 검증한다 (pkg/agent/rescue/recovery.go).
id_re='^[A-Za-z0-9-]{1,64}$'
url_re='^https://[A-Za-z0-9._~:/-]+$'
token_re='^[A-Za-z0-9_-]{16,256}$'
sha_re='^[0-9a-fA-F]{64}$'
[[ $AIHA_RECOVERY_ID =~ $id_re ]] || die 'AIHA_RECOVERY_ID must be 1-64 letters, digits and hyphens.'
[[ $AIHA_FRAMEWORK_URL =~ $url_re ]] || die 'AIHA_FRAMEWORK_URL must be an https URL.'
[[ $AIHA_CLAIM_TOKEN =~ $token_re ]] || die 'AIHA_CLAIM_TOKEN is malformed.'
case $AIHA_FRAMEWORK_CERT_PIN in
    true) [[ $AIHA_FRAMEWORK_CERT_SHA256 =~ $sha_re ]] || die 'AIHA_FRAMEWORK_CERT_SHA256 must be 64 hexadecimal digits.' ;;
    false) [[ -z $AIHA_FRAMEWORK_CERT_SHA256 ]] || die 'AIHA_FRAMEWORK_CERT_SHA256 must be empty when AIHA_FRAMEWORK_CERT_PIN is false.' ;;
    *) die 'AIHA_FRAMEWORK_CERT_PIN must be true or false.' ;;
esac
[[ $AGENT_SHA256 =~ $sha_re ]] || die 'AGENT_SHA256 must be 64 hexadecimal digits.'

# user-data 는 OS 가 아니라 인스턴스에 붙어 있다. 복구가 끝나 복구된 OS 로 부팅해도 IMDS 는 이
# 스크립트를 다시 내주고, 복구된 OS 의 cloud-init 은 원본 인스턴스의 기록을 갖고 있어 "새 인스턴스"로
# 보고 다시 실행한다. 그대로 두면 복구된 OS 가 부팅할 때마다 레스큐로 넘어간다.
# 복구 실행기가 복구된 디스크에 남기는 표식이 **이 복구의** recoveryId 를 담고 있으면 이 user-data 의
# 복구가 이미 끝난 OS 이므로 아무것도 하지 않는다. 다른 recoveryId 의 표식은 막지 않는다 — 복구된
# 인스턴스로 만든 AMI 에는 그 복구의 표식이 남아 있고, 그 AMI 로 띄운 인스턴스의 새 복구는 진행돼야 한다.
RECOVERY_MARKER=/var/lib/aiha/recovery-pending.json
if [[ -r $RECOVERY_MARKER ]] &&
    grep -Eq "\"recoveryId\"[[:space:]]*:[[:space:]]*\"${AIHA_RECOVERY_ID}\"" "$RECOVERY_MARKER"; then
    printf '[aiha-init] %s records recovery %s: this system is its result; nothing to do.\n' \
        "$RECOVERY_MARKER" "$AIHA_RECOVERY_ID"
    exit 0
fi

for c in curl sha256sum awk install mktemp sync reboot; do
    command -v "$c" >/dev/null || die "Required command missing: $c"
done

work=$(mktemp -d /var/tmp/aiha-init.XXXXXXXX)
trap 'rm -rf "$work"' EXIT

fetch "$SCRIPT_URL" "$work/setup-sysres.sh"
fetch "$AGENT_URL" "$work/aiha-rescue" "$AGENT_SHA256"
bash -n "$work/setup-sysres.sh"

printf 'AIHA_RECOVERY_ID=%s\nAIHA_FRAMEWORK_URL=%s\nAIHA_CLAIM_TOKEN=%s\nAIHA_FRAMEWORK_CERT_PIN=%s\nAIHA_FRAMEWORK_CERT_SHA256=%s\n' \
    "$AIHA_RECOVERY_ID" "$AIHA_FRAMEWORK_URL" "$AIHA_CLAIM_TOKEN" "$AIHA_FRAMEWORK_CERT_PIN" "$AIHA_FRAMEWORK_CERT_SHA256" \
    > "$work/recovery.env"

# 재부팅 없이 실행한다: 레스큐 진입 설정(sysres.tar 해제, grub 1회 부팅)만 하고 돌아온다.
# 실패하면 set -e 로 여기서 멈추고 재부팅하지 않는다.
bash "$work/setup-sysres.sh"

# 이미지에 autorun2 가 없으면 레스큐로 부팅해도 아무것도 실행되지 않는다. 재부팅 전에 멈춘다.
# (grub 1회 부팅은 이미 설정됐으므로, 그대로 재부팅하면 할 일 없는 레스큐로 한 번 부팅한다.)
[[ -f $AUTORUN_DIR/autorun2 ]] || die "sysres.tar has no autorun/autorun2 (see sh/autorun2); not rebooting."

# sysres.tar 해제 뒤에 설치해 이미지의 같은 이름 파일을 덮어쓴다.
install -d -m 0755 "$AUTORUN_DIR"
install -d -m 0700 "$AUTORUN_DIR/aiha"
install -m 0700 "$work/aiha-rescue" "$AUTORUN_DIR/aiha/aiha-rescue"
install -m 0600 "$work/recovery.env" "$AUTORUN_DIR/aiha/recovery.env"

sync
reboot
