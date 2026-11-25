# H0 — DRBD 수동 검증 런북

[ha-roadmap.md](ha-roadmap.md) H0 단계의 수행 절차다. 코드를 쓰기 전에 대상 커널에서 DRBD 동작을
확인하고, 구현에 쓸 명령 순서와 상태 출력 원문(픽스처)을 확보한다.

- 모든 명령은 대상 VM에서 root로 실행한다(`sudo -i`). 개발 장비에서는 실행하지 않는다.
- 각 절의 "예상"은 확인할 가설이다. 결과가 다르면 그대로 12절에 적는다. 다른 결과가 이 런북의
  목적이다.
- 결과는 12절 표에 적고, 수집물은 11절대로 회수한다.
- **AWS에서 수행 완료(2026-11-12, 12절).** GCP·Azure는 13절의 보완 확인만 한다. 그중 모듈 설치·적재는
  확인되었다(2026-11-13, 13.1절).

---

## 0. 환경

| 항목 | 값 |
| --- | --- |
| 클라우드 | 1차 수행은 AWS. GCP·Azure는 13절 |
| 리전·네트워크 | 한 리전, 같은 가상 네트워크, **서로 다른 가용 영역**의 서브넷 2개 |
| AMI·이미지 | Ubuntu 24.04 LTS 공식 이미지 (x86_64). 백업·복구 데모와 같은 이미지면 더 좋다 |
| 인스턴스 | 같은 유형 2대. GPU는 필요 없다(예: AWS `t3.medium`). 지연·처리량은 나중에 최종 GPU 유형에서 다시 잰다 |
| Secure Boot | **비활성.** HA·백업 이용의 제한사항이다([ha-roadmap.md](ha-roadmap.md) 7절 결정 5). AWS의 Ubuntu 공식 AMI와 GCP는 기본이 비활성이다. Azure는 기본이 활성(Trusted Launch)이므로 만들 때 끄거나(`--enable-secure-boot false`), 만든 뒤 VM을 중지하고 설정을 바꿔 다시 시작한다(13.1절). 어느 쪽이든 1절 `mokutil --sb-state`로 확인한다 |
| 데이터 볼륨 | 각 인스턴스에 같은 크기의 **빈** 블록 볼륨 1개(예: 20 GiB — AWS EBS, GCP Persistent Disk, Azure Managed Disk). 루트와 별도 |
| 방화벽 | 두 노드 사이 TCP 7789 허용(AWS 보안 그룹, GCP VPC 방화벽 규칙, Azure NSG), SSH |
| 클라우드 CLI 또는 콘솔 | 8절에서 인스턴스를 강제 중지·시작한다 |

이하 **A** = 처음 운영 노드, **B** = 처음 대기 노드.

### 0.1 변수 (양쪽)

```bash
sudo -i
mkdir -p /root/h0
A_HOST=ip-10-0-1-10     # A에서 `uname -n`
B_HOST=ip-10-0-2-10     # B에서 `uname -n`
A_IP=10.0.1.10
B_IP=10.0.2.10
```

데이터 볼륨의 안정 경로를 찾는다. 장치 이름(`nvme1n1`, `sdc`)은 인스턴스마다, 같은 인스턴스에서도
부팅마다 바뀔 수 있으므로(Azure에서 재부팅 뒤 `sda`·`sdb`가 서로 바뀌었다, 13.1절) 쓰지 않고, udev가
만드는 `/dev/disk/by-id` 링크를 쓴다. `ID-LINK` 열이 그 이름이다. 형태는 클라우드마다 다르다(AWS
`nvme-Amazon_Elastic_Block_Store_vol…`, GCP `google-<장치 이름>`, Azure `scsi-3<WWN>`).

```bash
lsblk -o NAME,SIZE,TYPE,MOUNTPOINTS,SERIAL,ID-LINK
ls -l /dev/disk/by-id/
DISK="/dev/disk/by-id/<이 노드 데이터 볼륨의 ID-LINK>"    # 실제 이름으로 바꾼다. 파티션 아님
lsblk -J -O --bytes > /root/h0/lsblk-$(uname -n).json    # 인벤토리 dataDisks 픽스처
```

A의 `DISK` 값을 `A_DISK`, B의 값을 `B_DISK`로 양쪽 셸에 함께 넣어 둔다(3절 리소스 파일에 쓴다).

7·8절에서 재부팅하면 셸 변수와 함수가 사라진다. 이 절의 변수, 0.2의 `cap` 함수, 1.1의 `SANDBOX`를
`/root/h0-env.sh`에 적어 두고 재부팅 뒤 `source /root/h0-env.sh`로 다시 읽는다. 공유 비밀(3.1)은
이 파일에 넣지 않는다.

### 0.2 상태 수집 함수 (양쪽)

이후 절의 `cap <이름>`은 그 시점의 상태를 모든 형식으로 저장한다. DRBD 버전에 따라 일부 명령은
실패하는데, **실패 자체가 결과다**(어떤 조회 방식이 있는지 알려 준다).

```bash
cap() {
  local d=/root/h0/$1-$(uname -n); mkdir -p "$d"
  date -u +%FT%T.%NZ                                > "$d/at.txt"
  cat /proc/drbd                                    > "$d/proc-drbd.txt" 2>&1
  for c in role cstate dstate status; do drbdadm "$c" r0 > "$d/drbdadm-$c.txt" 2>&1; done
  drbdsetup status --verbose --statistics r0        > "$d/drbdsetup-status.txt" 2>&1
  drbdsetup status --json r0                        > "$d/drbdsetup-status.json" 2>&1
  drbdsetup events2 --now r0                        > "$d/drbdsetup-events2.txt" 2>&1
  # show는 공유 비밀을 출력한다. 가리고 저장한다.
  drbdsetup show r0 2>&1 | sed -E 's/(shared-secret[[:space:]]+)"[^"]*"/\1"<redacted>"/' > "$d/drbdsetup-show.txt"
  journalctl -k -b --no-pager | grep -i drbd | tail -n 50 > "$d/kernel-log.txt"
}
```

`drbdadm dump`와 `/etc/drbd.d/r0.res`는 공유 비밀을 담으므로 수집하지 않는다.

---

## 1. 도구와 모듈 (양쪽)

Ubuntu 커널 내장 모듈은 DRBD 8.4다. H0는 LINBIT PPA의 DRBD 9를 DKMS로 빌드해 수행했고, 아래가 그
절차다([ha-roadmap.md](ha-roadmap.md) 7절 결정 5).

```bash
uname -r
mokutil --sb-state            # 없으면 apt-get install -y mokutil. "SecureBoot disabled"여야 한다(0절)
modinfo drbd 2>&1 | head -n 5 # 내장 모듈 버전 (참고)
add-apt-repository -y ppa:linbit/linbit-drbd9-stack
apt-get update
apt-get install -y linux-headers-$(uname -r)
apt-get install -y --no-install-recommends drbd-dkms drbd-utils fio
modprobe drbd && head -n 2 /proc/drbd
drbdadm --version
dpkg-query -W drbd-dkms drbd-utils
systemctl list-unit-files 'drbd*'
grep -n usage-count /etc/drbd.d/global_common.conf
```

- `usage-count yes`이면 `no`로 바꾼다. 외부로 사용 보고를 보내지 않게 한다.
- `drbd.service`는 설치 기본값이 disabled다. 그대로 둔다(2026-11-16, H3b). 기동 스크립트가 모든 리소스의 상대
  연결을 무제한 기다린다. 프레임워크가 구성하는 노드는 리소스마다 `drbd@<리소스>.target`으로 올라오고,
  `drbd.service`가 enable된 노드는 구성이 거절한다([ha-roadmap.md](ha-roadmap.md) 7절 결정 7).
- 모듈이 적재되지 않으면 여기서 멈추고 `mokutil --sb-state`, `dkms status`, 커널 로그를 함께 보고한다.

### 1.1 에이전트 샌드박스 안의 모듈 적재

`deploy/aiha-agent.service`와 같은 보호 옵션을 준 임시 유닛으로 확인한다. 이후 절에서도 `$SANDBOX`를
쓴다.

```bash
SANDBOX="-p ProtectKernelLogs=yes -p ProtectKernelModules=yes -p ProtectControlGroups=yes \
  -p RestrictRealtime=yes -p RestrictSUIDSGID=yes -p LockPersonality=yes -p StateDirectory=aiha-h0"
rmmod drbd_transport_tcp drbd     # DRBD 9는 전송 모듈이 drbd를 잡고 있다
systemd-run --wait --pipe $SANDBOX $(command -v modprobe) drbd; echo "exit=$?"
lsmod | grep -w drbd || echo 'not loaded'
modprobe drbd          # 결과와 관계없이 이후 진행을 위해 적재
```

예상: 샌드박스 안의 `modprobe`는 실패한다.

---

## 2. 기준 쓰기 지연 (A, DRBD 구성 전)

빈 볼륨에 직접 쓴다. **반드시 3절 `create-md` 전에** 한다. 블록 장치에 직접 쓴 값이라 5절의 파일
시스템 위 값과는 직접 비교하지 않는다(12절 5번).

```bash
fio --name=raw --filename=$DISK --rw=randwrite --bs=4k --iodepth=1 --direct=1 --fsync=1 \
    --size=1G --runtime=60 --time_based --group_reporting > /root/h0/fio-raw-$(uname -n).txt
ping -c 20 $B_IP > /root/h0/ping-a-to-b.txt
```

---

## 3. 리소스 구성과 초기 동기화

### 3.1 리소스 파일 (양쪽)

공유 비밀은 A에서 한 번 만들고 B의 셸에 같은 값을 붙여 넣는다. 파일이나 기록에 남기지 않는다.

```bash
SECRET=$(openssl rand -hex 24)      # A에서만. B에서는 SECRET=<A와 같은 값>
cat > /etc/drbd.d/r0.res <<EOF
resource r0 {
  options { auto-promote no; }
  net {
    protocol C;
    cram-hmac-alg sha256;
    shared-secret "$SECRET";
  }
  on $A_HOST {
    node-id 0;
    device    /dev/drbd0;
    disk      $A_DISK;
    address   $A_IP:7789;
    meta-disk internal;
  }
  on $B_HOST {
    node-id 1;
    device    /dev/drbd0;
    disk      $B_DISK;
    address   $B_IP:7789;
    meta-disk internal;
  }
}
EOF
chmod 600 /etc/drbd.d/r0.res
drbdadm dump r0 > /dev/null && echo 'config OK'      # dump는 비밀을 출력하므로 버린다
```

DRBD 9 문법이다. `auto-promote no`가 없으면 마운트나 쓰기용 열기만으로 승격된다. 내장 8.4를 쓰게
되면 `options`와 `node-id`를 빼고, `startup` 섹션에 `become-primary-on`이 없는지 확인한다.

### 3.2 메타데이터와 기동 (양쪽)

```bash
{ drbdadm show-gi r0; echo "exit=$?"; } > /root/h0/show-gi-fresh-$(uname -n).txt 2>&1   # 메타데이터 없음의 모습
drbdadm create-md r0
drbdadm up r0
cap 01-new
```

예상: 양쪽 Secondary, `Connected`, `Inconsistent/Inconsistent`.

### 3.3 초기 동기화 (A)

```bash
date +%s > /root/h0/sync-start.txt
drbdadm primary --force r0
```

진행 중에 양쪽에서 `cap 02-syncing`을 한 번 실행한다. 끝날 때까지 지켜본다(`watch -n5 drbdadm status r0`).

```bash
date +%s > /root/h0/sync-end.txt
cap 03-uptodate        # 양쪽
```

처리량이 예상보다 크게 낮으면(대략 30 MB/s 미만) 설정을 바꾸지 말고 `drbdsetup show r0`의 재동기화
관련 값(가린 상태로 `cap`에 이미 있다)과 함께 기록한다.

### 3.4 파일시스템과 마운트 유닛

```bash
mkfs.ext4 -L aiha-data /dev/drbd0                         # A만
blkid /dev/drbd0 > /root/h0/blkid-drbd0-$(uname -n).txt  # A만
mkdir -p /data                                            # 양쪽
echo '/dev/drbd0 /data ext4 noauto 0 0' >> /etc/fstab     # 양쪽
systemctl daemon-reload                                   # 양쪽 — data.mount가 생긴다
systemctl start data.mount && findmnt /data               # A만
```

B(Secondary)에서는 마운트가 실패해야 한다.

```bash
{ mount /dev/drbd0 /mnt; echo "exit=$?"; } > /root/h0/mount-on-secondary-$(uname -n).txt 2>&1   # B
```

### 3.5 에이전트 샌드박스 안의 마운트 (A)

```bash
systemctl stop data.mount
systemd-run --wait --pipe $SANDBOX $(command -v drbdadm) cstate r0; echo "exit=$?"
systemd-run --wait --pipe $SANDBOX $(command -v mount) /dev/drbd0 /data; echo "exit=$?"
findmnt /data || echo 'host does not see /data'
umount /data 2>/dev/null
systemd-run --wait --pipe $SANDBOX $(command -v systemctl) start data.mount; echo "exit=$?"
findmnt /data || echo 'host does not see /data'
```

예상: 샌드박스 안의 직접 `mount`는 호스트에서 보이지 않을 수 있고, `systemctl start data.mount`는
PID 1이 마운트하므로 보인다. 끝나면 `data.mount`가 활성인 상태로 둔다.

---

## 4. 시험용 서비스와 기동 통제 (양쪽)

`/data`에 계속 쓰는 서비스다. `[Install]` 섹션이 없어 부팅 시 자동 기동되지 않는다.

```bash
cat > /etc/systemd/system/aiha-h0-writer.service <<'EOF'
[Unit]
Description=AI-HA H0 test writer
RequiresMountsFor=/data

[Service]
ExecStart=/bin/sh -c 'while :; do echo "$$(date +%%s.%%N) $$(uname -n)" >> /data/writer.log; sync /data/writer.log; sleep 0.2; done'
EOF
systemctl daemon-reload
```

A에서 기동한다.

```bash
systemctl start aiha-h0-writer && sleep 3 && tail -n 2 /data/writer.log
```

B(Secondary)에서 기동을 시도한다. **실패해야 하고, 루트 파일시스템의 `/data`에 아무것도 생기지
않아야 한다.**

```bash
systemctl start aiha-h0-writer; echo "exit=$?"
systemctl status aiha-h0-writer data.mount --no-pager > /root/h0/start-on-secondary-$(uname -n).txt 2>&1
ls -la /data >> /root/h0/start-on-secondary-$(uname -n).txt
```

### 4.1 다른 기동 경로 점검

같은 명령을 나중에 실제 AI 서비스 유닛에도 적용한다. 소켓·타이머·경로 유닛이나 다른 유닛의
의존성으로 서비스가 시작되는 경로가 없어야 한다.

```bash
U=aiha-h0-writer.service
{ systemctl is-enabled $U
  systemctl show -p WantedBy -p RequiredBy -p TriggeredBy $U
  systemctl list-dependencies --reverse $U --no-pager
  systemctl list-units --all --type=socket,timer,path --no-pager | grep -i "${U%.service}" || echo 'no trigger units'
} > /root/h0/activation-paths-$(uname -n).txt 2>&1
```

---

## 5. 쓰기 지연과 변경분 재동기화 (A)

```bash
systemctl stop aiha-h0-writer
fio --name=drbd --filename=/data/fio.test --rw=randwrite --bs=4k --iodepth=1 --direct=1 --fsync=1 \
    --size=1G --runtime=60 --time_based --group_reporting > /root/h0/fio-drbd-connected.txt
drbdadm disconnect r0
cap 04-graceful-disconnect          # 양쪽 — 정상 단절 때 B의 디스크 상태
fio --name=drbd --filename=/data/fio.test --rw=randwrite --bs=4k --iodepth=1 --direct=1 --fsync=1 \
    --size=1G --runtime=60 --time_based --group_reporting > /root/h0/fio-drbd-disconnected.txt
drbdadm connect r0                  # B가 StandAlone이면 B에서도 실행
cap 04-resync                       # 양쪽, 재동기화 중에 — 변경 블록만 보내는지
rm /data/fio.test
systemctl start aiha-h0-writer
```

---

## 6. 계획 전환

A에서 강등한다. `cap 05-before-demote`가 최종 동기화 확인 신호(양쪽 `UpToDate`, 미동기 0)를 담는다.

```bash
date +%s.%N > /root/h0/switch-start.txt
systemctl stop aiha-h0-writer
systemctl stop data.mount
cap 05-before-demote                # 양쪽에서 실행
drbdadm secondary r0
```

B에서 승격한다.

```bash
drbdadm primary r0
systemctl start data.mount
systemctl start aiha-h0-writer
date +%s.%N > /root/h0/switch-end.txt
tail -n 3 /data/writer.log > /root/h0/after-switch-data-writer-log.txt        # A의 마지막 줄 뒤로 B의 줄이 이어지는지
cap 06-after-switch                 # 양쪽
```

같은 절차로 A를 다시 운영 노드로 돌린다.

---

## 7. 재부팅 동작

### 7.1 대기 노드 재부팅 (B)

```bash
reboot
# 부팅 뒤
cap 07-secondary-rebooted
systemctl is-active data.mount aiha-h0-writer
```

리소스가 저절로 올라와 다시 연결되는지 본다. 설치 직후에는 리소스를 올리는 유닛이 하나도 enable되어 있지
않다(`drbd.service`도 기본 disabled). 리소스별 유닛을 enable하고 다시 재부팅해 확인한다.

```bash
systemctl enable drbd-configured.target drbd@r0.target
```

H0(AWS, 2026-11-12) 당시에는 `drbd.service`를 enable해 확인했다(12절 3번). 이 유닛은 상대 연결을 무제한
기다려 상대가 꺼진 재부팅에서 기동이 끝나지 않으므로 쓰지 않는다([ha-roadmap.md](ha-roadmap.md) 7절 결정 7).

### 7.2 운영 노드 재부팅 (A, 쓰기 중)

```bash
reboot
# 재부팅하는 동안 B에서
cap 08-peer-rebooting
# A 부팅 뒤
cap 08-primary-rebooted
systemctl is-active data.mount aiha-h0-writer    # 둘 다 inactive여야 한다
findmnt /data || echo 'not mounted'
ls -la /data                                      # 루트의 빈 디렉터리여야 한다
```

**A는 Secondary로 올라오고, 마운트도 서비스도 없어야 한다.** 이어서 `ha resume`이 할 순서로 복귀한다.

```bash
drbdadm primary r0 && systemctl start data.mount && systemctl start aiha-h0-writer
```

---

## 8. 장애 전환 두 가지

전원 차단에 해당하는 중지를 쓴다. AWS는 `aws ec2 stop-instances --force --instance-ids <A>`, Azure는
`az vm stop --skip-shutdown -g <그룹> -n <A>`, GCP는 `gcloud compute instances stop <A>`(GCP에는 강제
중지가 없어 일반 중지다. 하드 리셋인 `reset`은 곧바로 다시 켜지므로 쓰지 않는다).

### 8.1 연결된 채 운영 노드 정지

A가 운영 중이고 연결이 살아 있는 상태에서 A를 중지한다. 시각을 적는다.

B에서:

```bash
cap 09-peer-lost
date +%s.%N
drbdadm primary r0 && systemctl start data.mount && systemctl start aiha-h0-writer
date +%s.%N
tail -n 3 /data/writer.log
```

A를 다시 시작한다. A에서 `cap 10-old-primary-returned`, B에서도 같은 이름으로 실행한다.
예상: A는 Secondary로 올라오고 분할 뇌 없이 SyncTarget이 되어 B를 따라잡는다.

### 8.2 복제가 끊긴 뒤 운영 노드가 계속 쓰다가 정지

이제 B가 운영(P), A가 대기(S)다. P에서 복제 포트를 막는다(재부팅하면 사라지는 규칙이다).

```bash
iptables -I INPUT  -p tcp --dport 7789 -j DROP
iptables -I INPUT  -p tcp --sport 7789 -j DROP
iptables -I OUTPUT -p tcp --dport 7789 -j DROP
iptables -I OUTPUT -p tcp --sport 7789 -j DROP
```

30초쯤 기다려 양쪽에서 `cap 11-link-lost`를 실행한다. **S의 디스크 상태가 핵심이다** — 펜싱 정책이
없으면 `UpToDate`로 남을 것으로 예상한다. P에서 1분 더 쓰게 둔 뒤 마지막 줄을 적고 P를 중지한다.

```bash
tail -n 1 /data/writer.log > /root/h0/last-line-before-stop.txt   # P
```

S에서 승격하고, 잃은 구간을 확인한다.

```bash
cap 12-before-promote
drbdadm primary r0 && systemctl start data.mount && systemctl start aiha-h0-writer
grep -v "$(uname -n)" /data/writer.log | tail -n 1     # P가 쓴 줄 중 S에 남은 마지막 줄
```

P의 마지막 줄과 S에 남은 P의 마지막 줄 사이가 손실 구간이다. P를 다시 시작하고 양쪽에서
`cap 13-split-brain`을 실행한다. 예상: 분할 뇌가 감지되어 연결이 `StandAlone`으로 멈춘다. 커널 로그의
문구를 12절에 적는다(오류 정보 수집에 쓴다).

---

## 9. 재합류 (8.2에 이어서)

변경분을 버릴 쪽은 옛 운영 노드 P다.

```bash
# P에서
drbdadm disconnect r0          # 이미 StandAlone이면 실패해도 된다
drbdadm secondary r0
drbdadm connect --discard-my-data r0
# 현재 운영 노드 S가 StandAlone이면 S에서
drbdadm connect r0
```

재동기화 중에 양쪽에서 `cap 14-rejoin-sync`, 끝나면 `cap 15-rejoined`를 실행한다.

---

## 10. 재요청 안전성과 수동 정리

### 10.1 이미 있는 메타데이터의 판별 (양쪽)

에이전트가 "이미 구성되어 있으니 건너뛴다"를 판단할 근거를 확보한다.

```bash
systemctl stop aiha-h0-writer data.mount 2>/dev/null    # 운영 노드
drbdadm secondary r0
drbdadm down r0
{ drbdadm show-gi r0; echo "exit=$?"; } > /root/h0/show-gi-existing-$(uname -n).txt 2>&1
{ drbdadm create-md r0 < /dev/null; echo "exit=$?"; } > /root/h0/create-md-again-$(uname -n).txt 2>&1
{ blkid $DISK; echo "exit=$?"; } > /root/h0/blkid-backing-$(uname -n).txt 2>&1
```

예상: `create-md`는 기존 메타데이터를 보고 확인을 요구하다가 입력이 없어 중단한다. 덮어썼다면 즉시
보고한다(구현의 재요청 규칙이 바뀐다).

### 10.2 해제와 정리 (양쪽)

해제(구현의 `teardown` 범위)와 삭제(수동 정리)를 나눠 둔다. 구현은 앞의 것만 한다.

해제는 서비스 정지 → 언마운트 → Secondary → `down`(여기까지 10.1에서 완료) → 설정 제거 순서다. 삭제
명령인 `wipe-md`는 리소스 설정을 읽으므로 설정 제거보다 먼저 와야 한다.

```bash
# [삭제] 되돌릴 수 없다. 데이터 볼륨을 다시 쓸 때만. 확인 질문에 yes
drbdadm wipe-md r0

# [해제] 설정 제거
rm /etc/drbd.d/r0.res /etc/systemd/system/aiha-h0-writer.service
sed -i '\#^/dev/drbd0 /data #d' /etc/fstab
systemctl daemon-reload

# [삭제] 남은 서명 제거
wipefs -a $DISK
```

---

## 11. 회수와 정리

수집물에 공유 비밀이 없는지 확인하고 묶는다(양쪽).

```bash
grep -rn 'shared-secret' /root/h0 | grep -v '<redacted>' && echo 'SECRET FOUND — 해당 파일을 지우고 다시 확인' || echo 'clean'
tar czf /root/h0-$(uname -n).tgz -C /root h0
```

두 묶음을 개발 장비의 `testdata/`(git 제외)로 옮긴다. 끝나면 인스턴스와 데이터 볼륨을 삭제한다.

---

## 12. 결과 (AWS, 2026-11-12)

출처: `testdata/h0-ip-10-0-1-{83,25}-{first-try,2nd-try}.tgz`. 2차 시도를 기준으로 하고 1차는 대조에
쓴다. 노드는 `.83`이 A(처음 운영), `.25`가 B다. 일부 수집은 두 노드에서 같은 이름으로 실행되었거나
1차의 명령 오타(`ystemctl`)로 비었는데, 결론은 두 차례가 일치하는 것만 썼다. 공유 비밀은 수집물에
없음을 확인했다.

| # | 항목 | 결과 | 로드맵 반영 |
| --- | --- | --- | --- |
| 1 | 버전 | 커널 `7.0.0-1017-aws`. 내장 8.4 대신 LINBIT PPA의 `drbd-dkms` **9.3.4** + `drbd-utils` **9.36.0-rc.1**(릴리스 후보). DKMS로 빌드한 모듈은 서명이 없다(커널 로그 `module verification failed: signature and/or required key missing`). 이 VM은 Secure Boot가 꺼져 있어 적재되었다 → DRBD 9 확정, Secure Boot 비활성을 제한사항으로 둔다 | 7절 결정 5, 4절 항목 19 |
| 1.1 | 샌드박스 안 `modprobe` | 실패: `Module drbd not found in directory /lib/modules/…` — `ProtectKernelModules`가 모듈 디렉터리를 숨긴다. `rmmod drbd`는 `drbd_transport_tcp`가 잡고 있어 실패했지만 결론에는 영향이 없다 | 4절 항목 8 |
| 2 | 조회 방식 | `drbdsetup status --json`, `status --verbose --statistics`, `events2 --now`, `drbdadm role/cstate/dstate/status` 모두 동작. `/proc/drbd`는 버전만 보인다. JSON에 역할·연결·디스크·상대 디스크·replication 상태·`out-of-sync`(KiB)·진행률이 모두 있다 | H1 `pkg/agent/ha/drbd`, 7절 결정 6 |
| 3 | 부팅 시 리소스 기동 | `drbd.service`(설치 기본은 disabled, 7.1절에서 enable). 대기 재부팅 뒤 저절로 `Connected`·`UpToDate`로 돌아왔다 | 7절 결정 7 — **2026-11-16 변경**: `drbd.service`는 상대가 꺼져 있으면 기동이 끝나지 않으므로(이 시험에서는 상대가 살아 있었다) `drbd@<리소스>.target`을 쓴다 |
| 4 | 초기 동기화 | 8 GiB에 107초(1차), 101초(2차) — 약 80 MiB/s. DRBD가 보고한 속도는 약 90 MiB/s. 1 TiB면 약 3.6시간 | 설계서 6.5 |
| 5 | 쓰기 지연 (4 KiB 동기, iodepth 1) | 연결: p50 1.16–1.21 ms, p99 2.1–2.2 ms, 265–279 IOPS. 단절: p50 0.95 ms, p99 1.1–1.2 ms, 345 IOPS. **복제로 p50 약 +0.25 ms, IOPS 약 −20%.** 원시 볼륨(p50 0.91 ms, 1,075–1,099 IOPS)은 블록 장치에 직접 쓴 값이라 파일시스템 위 값과 비교하지 않는다. 노드 간 ping 0.19–0.20 ms이고 두 노드가 같은 서브넷(10.0.1.0/24)이었다 — **같은 가용 영역에서 잰 값이다** | 가용 영역 간 재측정 필요 |
| 6 | 계획 전환 | 명령 순서 확인: 정지 → 언마운트 → 최종 확인 → `secondary`, 상대에서 `primary` → 마운트 → 기동. 최종 동기화 확인 신호는 양쪽 `connection-state: Connected`, `replication-state: Established`, `disk-state`·`peer-disk-state: UpToDate`, `out-of-sync: 0`. 기록된 소요 시간(44초, 127초)은 수동 입력 시간이라 쓰지 않는다 | H4 `ha-demote` |
| 7 | Secondary에서 시작 시도 | `mount`: `Wrong medium type`(exit 32). 서비스: `Dependency failed`(`data.mount` 실패). 루트의 `/data`는 비어 있다 | **H0 완료 조건 충족** |
| 8 | 운영 노드 재부팅 | Secondary, 마운트 없음, 서비스 inactive, `/data` 비어 있음. 재부팅 동안 상대도 Secondary로 남았다(자동 승격 없음) | **H0 완료 조건 충족** |
| 9 | 샌드박스 | `modprobe` 실패, `drbdadm cstate` 동작, 직접 `mount`는 성공했지만 호스트에서 안 보임, `systemctl start data.mount`는 호스트에서 보임 | 4절 항목 8 |
| 10 | 대기 디스크 상태 | 정상 단절(5절): `Outdated`. 상대 정지(8.1): `UpToDate`/`DUnknown`. 상대 단절(8.2): `UpToDate`/`DUnknown` — 그동안 운영은 미동기량이 쌓였다(808 KiB, 1차 4,316 KiB). **8.1과 8.2의 대기 상태가 같아 상태만으로는 최신성을 가릴 수 없다** | 4절 항목 11 |
| 11 | 복귀 | 8.1: 분할 뇌 없이 자동 재동기화. 8.2: 양쪽 `StandAlone`. 커널 로그 `uuid_compare()=split-brain-auto-recover by rule=bitmap-both`, `Split-Brain detected but unresolved, dropping connection!`, `helper command: /sbin/drbdadm split-brain`. 상태 출력에는 사유가 없다 | 4절 항목 1·18 |
| 12 | 8.2 손실 | 2차: P의 마지막 줄 `1794471871.96`, S에 남은 P의 마지막 줄 `1794471783.08` — 약 89초 분량. 1차: `1794465674.11` / `1794465565.21` — 약 109초. 단절 뒤에 쓴 것은 모두 사라졌다 | 7절 결정 2 |
| 13 | 메타데이터 판별 | 새 디스크의 `show-gi`: 실패(1차 exit 1, 2차 `drbdmeta … show-gi` exit 255). 기존 메타데이터: 성공(세대 UUID 출력). `create-md` 재실행: TTY가 없으면 `Operation cancelled.`(exit 1)로 덮어쓰지 않는다 | 4절 항목 4 |
| 14 | 서명 | 구성 전 데이터 볼륨: 서명 없음. 메타데이터가 있는 하위 디스크: `blkid`가 `TYPE="drbd"`만 보고한다(안쪽 ext4는 안 보인다). DRBD 장치: `TYPE="ext4"` | H3b 실행 전 검사 |
| 15 | 정리 순서 | `wipe-md`(확인 `yes`) → 설정 제거 → `wipefs -a`(ext4 서명 2바이트 제거)로 확정 | H3b teardown |

7번과 8번이 기대대로이므로 H0 완료 조건을 충족한다. 남은 것은 13절의 다른 클라우드 확인 중 남은
항목과, 최종 인스턴스 유형으로 가용 영역 간 지연을 다시 재는 것이다.

---

## 13. 다른 클라우드 보완 확인 (GCP, Azure)

클라우드마다 다를 수 있는 것만 확인한다. 런북 전체를 반복하지 않는다. 결과는 13.1절에 적는다.

| 확인 | 절 | 기대 | 상태 |
| --- | --- | --- | --- |
| Secure Boot를 끈 VM에서 `mokutil --sb-state`, 그 클라우드 커널(`linux-gcp`, `linux-azure`)로 DRBD 9 DKMS 빌드·적재 | 0, 1 | `SecureBoot disabled`, `/proc/drbd`에 9.x | **확인(2026-11-13)** |
| 데이터 볼륨의 `ID-LINK` 이름과 재부팅 후 유지 여부 | 0.1, 7 | 데이터 볼륨에 `by-id` 링크가 있고, 재부팅 후 같은 이름 | **확인(2026-11-16)** |
| 인스턴스를 중지했다가 시작(호스트 이동)한 뒤에도 같은 `by-id` 이름 | 0.1 | 재부팅과 같음 | 남음 — 그 클라우드의 H4 실환경 검증 때(펜싱이 중지다) |
| 리소스 구성, 초기 동기화, Secondary에서 마운트·서비스 차단 | 3, 4 | 12절 7번과 같음 | 남음 — 그 클라우드의 H3b 실환경 검증 때 |
| 운영 노드 재부팅 | 7.2 | 12절 8번과 같음 | 남음 — 그 클라우드의 H3b 실환경 검증 때 |
| 두 노드를 다른 가용 영역에 두고 쓰기 지연 | 2, 5 | 연결 시 증가폭을 12절 5번과 비교 | 남음 — 그 클라우드의 H3b 실환경 검증 때 |

`ID-LINK`는 H3b 구현 전에 봤다. H3b는 데이터 볼륨을 `/dev/disk/by-id/<ID-LINK>`로만 가리키므로
([ha-roadmap.md](ha-roadmap.md) 1.1절), 어느 클라우드에서 데이터 볼륨에 `by-id` 링크가 없거나 재부팅으로
이름이 바뀌면 디스크 참조 규칙을 다시 정해야 했다. 세 클라우드 모두 링크가 있고 재부팅 뒤에도 같았다
(13.1절). 나머지는 모듈이 적재된 뒤의 DRBD 동작이어서 클라우드에 따라 달라질 이유가 적으므로, 실환경
검증에서 함께 본다.

### 13.1 결과 (2026-11-13)

| 클라우드 | 이미지 | Secure Boot | DRBD 9 설치·적재 |
| --- | --- | --- | --- |
| GCP | Ubuntu 24.04 Pro | 생성 시 선택. 기본 비활성 | LINBIT PPA 추가 후 설치 성공, `modprobe drbd` 성공 |
| Azure | Ubuntu 24.04 Pro | 생성 시 선택. 기본 활성(Trusted Launch). 만든 뒤에도 VM을 중지하고 설정을 바꿔 다시 시작하면 끌 수 있다 | LINBIT PPA 추가 후 설치 성공, `modprobe drbd` 성공 |

- AWS(12절 1번)와 같이 1절 절차(PPA 추가, 설치, `modprobe drbd`)가 통했다. 세 클라우드 모두 DRBD 9를
  쓸 수 있다.
- 커널 버전과 `drbd-dkms`·`drbd-utils` 버전은 이번에 받지 않았다. 버전 고정(ha-roadmap 7절 결정 5)에
  필요하므로, 그 클라우드의 H3b 실환경 검증 때 1절의 `uname -r`, `dpkg-query` 출력을 함께 남긴다.

**데이터 볼륨의 `by-id` 링크 (2026-11-16)** — 데이터 디스크 하나를 붙이고 `ls -l /dev/disk/by-id/`를
재부팅 전후로 받았다.

| 클라우드 | 데이터 볼륨의 링크 | `ID-LINK`(`lsblk`가 고르는 것) | 재부팅 |
| --- | --- | --- | --- |
| AWS | `nvme-Amazon_Elastic_Block_Store_vol…`(0.1절, H0) | 같음 | 리소스 파일이 이 경로를 쓰고, 12절 8번 재부팅 뒤 리소스가 다시 올라왔다 |
| GCP | `google-<장치 이름>`, `scsi-0Google_PersistentDisk_<장치 이름>` | `google-<장치 이름>` | 다섯 번 재부팅. 링크와 커널 이름(`sdb`) 모두 그대로 |
| Azure | `scsi-3<WWN>`, `wwn-0x<WWN>`(길이가 같다) | `scsi-3<WWN>` | 두 번째 재부팅에서 커널 이름이 바뀜(운영체제 디스크 `sda`→`sdb`, 데이터 디스크 `sdb`→`sda`). 링크는 그대로 같은 디스크를 가리킴 |

- **커널 장치 이름은 어느 클라우드에서도 부팅마다 바뀔 수 있다고 본다.** GCP에서는 바뀌지 않았지만
  보장된 것이 아니다. 데이터 볼륨의 신원으로는 `by-id` 경로만 쓴다.
- GCP의 장치 이름은 디스크를 붙일 때 정하고(기본은 디스크 이름), `google-` 링크는 GCE 게스트 패키지의
  udev 규칙이 만든다. Azure의 WWN은 디스크마다 다르다.
- `lsblk`(util-linux 2.39)는 `by-id` 링크 중 가장 짧은 것을 `ID-LINK`로 고르고, 길이가 같으면 udev가
  주는 순서(이름순)의 앞쪽을 고른다. 그래서 Azure는 `wwn-0x…`가 아니라 `scsi-3…`이다. 같은 Hyper-V
  디스크를 단 개발 머신(WSL2)에서 디렉터리 순서가 디스크마다 달라도 네 디스크 모두 `scsi-3…`가 나오는
  것을 확인했다. 다만 이 선택은 udev 규칙과 util-linux 구현에 기대므로, 패키지가 더 짧은 링크를 추가하면
  같은 디스크의 `ID-LINK`가 바뀔 수 있다. H3b에서 이 값을 다루는 규칙은 ha-roadmap H3b 절에 있다.
- Azure가 권장하는 `/dev/disk/azure/scsi1/lun<N>`은 디스크가 아니라 LUN 번호를 가리키고 `by-id`가 아니어서
  쓰지 않는다.

---

## 부록 A. GPU 인스턴스에서 받을 출력 (H1 파서용)

GPU 인스턴스를 쓸 수 있을 때 한 번 받는다. H0의 완료 조건은 아니다.

```bash
nvidia-smi -L > /root/h0/nvidia-smi-L.txt 2>&1
nvidia-smi --query-gpu=index,name,driver_version,memory.used,memory.total \
  --format=csv,noheader,nounits > /root/h0/nvidia-smi-query.txt 2>&1
```
