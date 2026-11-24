# 이중화 실환경 시험 — 헬스 판정, 복제 구성·해제 (H1·H3a·H3b)

2026-11-19 기준 구현된 이중화 기능을 클라우드 VM 두 대로 직접 확인하는 절차다. 결과는 맨 끝 결과 표에 적어
[ha-roadmap.md](ha-roadmap.md)의 H1·H3b 실환경 검증으로 반영한다.

| 구현됨 — 이 문서로 시험 | 아직 없음 — 시험하지 않음 |
| --- | --- |
| 그룹 등록과 헬스 보고·판정·이벤트 (`ha create`, `ha status`, `ha events`) | 계획 전환·장애 전환·복귀·재합류 (`ha switchover`, `ha failover`, `ha resume`, `ha rejoin`, H4) |
| 인벤토리의 데이터 디스크 목록 | 복구된 시스템을 대기 노드로 등록하는 시드 흐름 (H2) |
| 복제 구성·해제 흐름과 실패 후 재요청 (`ha setup`, `ha teardown`, `ha acknowledge`) | 북향 REST `/aiha/ha/*` (501. CLI만 쓴다) |
| 복제 볼륨의 백업 후보 제외, 리소스 파일의 백업 제외 | |

관련 문서: [test-setup.md](test-setup.md) (프레임워크·에이전트 설치), [ha-h0.md](ha-h0.md) (DRBD 설치, 수동 정리),
[protocol.md](protocol.md) 2.8·3.4·3.5 (헬스 보고, 이중화 잡, 작업 흐름)

---

## 0. 구성과 전제

```text
[사내 VM]  aiha-framework server (+ PostgreSQL)  ←── 두 노드의 에이전트가 아웃바운드로 접속
                                                     (롱폴, 인벤토리, 헬스 보고, 잡)
[클라우드, 같은 VPC]
  노드 A (운영 primary)                 노드 B (대기 standby)
    aiha-agent run                        aiha-agent run
    DRBD 9, 데이터 디스크(빈 것)  ◄─ 7789/tcp 복제 ─►  DRBD 9, 데이터 디스크(빈 것, 같은 크기)
    시험 서비스 :8000  ◄──────── 8000/tcp 상호 프로브 ──  (B가 A의 서비스를 확인)
```

| 항목 | 값 |
| --- | --- |
| VM | Ubuntu 24.04 x86_64 두 대, 같은 사양. Secure Boot 비활성(전제) |
| 데이터 디스크 | 노드마다 **새로 붙인 빈 디스크 하나**, 두 노드 같은 크기. 파티션을 만들지 않는다. 시험에서 포맷된다 |
| 보안 그룹·방화벽 | 두 노드 사이 **7789/tcp**(복제) 양방향, **B → A 8000/tcp**(프로브). 프레임워크로 나가는 아웃바운드 |
| 프레임워크 | [test-setup.md](test-setup.md) 2절대로 기동. `AIHA_FRAMEWORK_SECRET_KEY_FILE` 필수(공유 비밀 봉인) |
| 에이전트 | 두 노드 모두 [test-setup.md](test-setup.md) 3.1~3.4절대로 등록하고 `aiha-agent run`(systemd) 상주. 각 `systemId`를 적어 둔다 |

아래 명령의 `<A>`, `<B>`는 두 노드의 `systemId`, `<A_IP>`, `<B_IP>`는 사설 IP다. `aiha-framework` 명령은
`framework.env`를 source한 사내 VM 셸에서 실행한다([test-setup.md](test-setup.md) 2.4).

### 0.1 프레임워크 설정 (기본값이면 생략)

`framework.env`에 넣지 않으면 기본값이 쓰인다. 고쳤으면 서버를 다시 띄운다.

```bash
AIHA_FRAMEWORK_HA_REPORT_INTERVAL=30s   # 노드 헬스 보고 주기. 흐름 진행·평가도 이 주기로 돈다
AIHA_FRAMEWORK_HA_STALE_AFTER=90s       # 보고 끊김·연결 끊김 지속·운영 실패 지속의 유예. 주기의 2배 이상
AIHA_FRAMEWORK_HA_JOB_TTL=5m            # 이중화 잡이 에이전트를 기다리는 시간
```

시험 시간을 줄이려면 `10s` / `30s`로 낮춰도 된다(에이전트는 10초 미만으로는 보고하지 않는다). 아래 "기다리는
시간"은 기본값 기준이다.

---

## 1. 노드 준비 (A, B 모두)

### 1.1 DRBD 9 설치

[ha-h0.md](ha-h0.md) 1절과 같다.

```bash
sudo add-apt-repository -y ppa:linbit/linbit-drbd9-stack
sudo apt-get update
sudo apt-get install -y linux-headers-$(uname -r)
sudo apt-get install -y --no-install-recommends drbd-dkms drbd-utils
sudo modprobe drbd && head -n 2 /proc/drbd            # version: 9.x
grep -n usage-count /etc/drbd.d/global_common.conf     # yes 이면 no로 바꾼다
systemctl is-enabled drbd.service                      # disabled (설치 기본값, 그대로 둔다)
systemctl list-unit-files 'drbd@*'                     # drbd@.service, drbd@.target 이 있어야 한다
```

- `modprobe`는 설치 확인용이다. 이후 모듈 적재와 리소스 기동은 프레임워크가 내린 잡이 `drbd@<리소스>.target`으로
  한다.
- `drbd.service`가 enabled면 구성이 거절된다. `sudo systemctl disable drbd.service`로 되돌린다.

### 1.2 데이터 디스크 확인

```bash
lsblk -o NAME,SIZE,TYPE,FSTYPE,MOUNTPOINTS,ID-LINK
```

새 디스크에 파티션·파일시스템·마운트가 없어야 한다. `ID-LINK`가 있어야 한다(없으면 `ls -l /dev/disk/by-id/`).
이 값은 2절에서 프레임워크 인벤토리로도 보이므로 따로 적지 않아도 된다.

서명이 없다고 빈 디스크는 아니다. `drbdadm create-md`는 0이 아닌 데이터가 있으면 확인을 묻고 멈추는데,
에이전트는 `blkid`로만 판단하므로 구성 1단계가 `Do you want to proceed?`로 실패한다. 디스크가 실제로
0인지 본다(스냅샷에서 만든 볼륨이나 재사용한 볼륨은 아닐 수 있다).

```bash
D=/dev/nvme1n1; S=$(sudo blockdev --getsize64 $D)
sudo head -c 1048576 $D | tr -d '\0' | wc -c                                              # 0
sudo dd if=$D bs=1M skip=$((S/1048576-1)) count=1 status=none | tr -d '\0' | wc -c       # 0
# 0이 아니면, 지워도 되는 디스크일 때만: sudo blkdiscard -z $D   (되돌릴 수 없다, 8 GiB에 약 1분)
```

### 1.3 시험 서비스

보호할 서비스 대신 쓰는 유닛이다. `/data`에 1초마다 시각을 쓰고, `:8000`에서 HTTP로 응답한다(헬스 경로 `/`).
**두 노드에 똑같이** 만든다. 구성은 양쪽에 같은 유닛이 있다고 가정한다. 부팅 시 자동 기동되지 않도록
`[Install]` 절을 두지 않는다.

```bash
sudo tee /etc/systemd/system/aiha-test.service >/dev/null <<'EOF'
[Unit]
Description=AI-HA test service

[Service]
ExecStart=/bin/sh -c 'mkdir -p /data/www; while :; do date -u +%%FT%%T.%%NZ >> /data/writer.log; sync /data/writer.log; sleep 1; done & exec python3 -m http.server 8000 --bind 0.0.0.0 --directory /data/www'
EOF
sudo systemctl daemon-reload
systemctl is-active aiha-test.service      # inactive 여야 한다. 구성 전에는 띄우지 않는다
```

구성(`ha setup`)이 이 유닛에 `/data` 마운트를 요구하는 드롭인을 붙인다. 그 뒤로는 운영 노드에서만 뜬다.

---

## 2. 시험 1 — 인벤토리의 데이터 디스크

에이전트를 띄운 뒤 첫 인벤토리 보고가 오면(기동 직후, 이후 `AIHA_AGENT_INVENTORY_INTERVAL`마다):

```bash
./aiha-framework agent inventory <A>
./aiha-framework agent inventory <B>
```

기대 — 파티션 표 아래에 데이터 디스크 표가 나온다:

```text
Data disks (whole, unpartitioned, unmounted):
BY-ID PATH                                                DEVICE NOW    SIZE   HOLDS
/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_vol0abc   /dev/nvme1n1  8.0GB  nothing
```

- `BY-ID PATH`를 노드별로 적는다(`<A_DISK>`, `<B_DISK>`). AWS는 `nvme-Amazon_Elastic_Block_Store_vol…`, GCP는
  `google-<장치 이름>`, Azure는 `scsi-3<WWN>` 형태다.
- 루트 디스크는 나오지 않는다(파티션이 있다).

---

## 3. 시험 2 — 그룹 등록과 헬스 보고

```bash
./aiha-framework ha create --name ha-test \
  --primary <A> --primary-address <A_IP> --primary-disk <A_DISK> \
  --standby <B> --standby-address <B_IP> --standby-disk <B_DISK> \
  --service-unit aiha-test.service --service-port 8000 --health-path / \
  --max-health-latency 1s
```

- 출력에서 **리소스 이름**(`resource aiha-xxxxxxxx`)을 적는다(`<RES>`). 장치는 `/dev/drbd0`, 마운트는 `/data`,
  복제 포트는 7789가 기본이다.
- systemd 유닛 이름에서는 리소스 이름의 `-`가 `\x2d`로 이스케이프된다(`<RES_UNIT>`, 예: `aiha\x2d16c07671`).
  셸에서는 작은따옴표로 감싼다: `systemctl status 'drbd@aiha\x2d16c07671.target'`. `systemd-escape <RES>`로도 얻는다.
- 거절되어야 하는 입력(선택): `--primary-disk /dev/nvme1n1` 같은 커널 이름, 디스크를 한쪽만 준 경우,
  디스크와 `--resource`를 함께 준 경우.

약 1분(보고 두 번) 기다린 뒤:

```bash
./aiha-framework ha status ha-test
```

기대:

- `State: registered`. 노드마다 `Data disk: <by-id> (node 0|1)`이 나온다.
- 두 노드 모두 `Reported: … (agent …, host <호스트명>)`. 아직 복제가 없으므로 `Replication: unreadable: …`
  (리소스 없음), 서비스는 `inactive`다. 판정은 이 상태를 오류로 보지만 그룹 상태는 `registered`에서 움직이지
  않는다(평가기는 `registered`를 바꾸지 않는다).

`Reported: never under this role`이 계속되면 그 노드의 `journalctl -u aiha-agent`를 본다.

---

## 4. 시험 3 — 복제 구성과 실패 후 재요청 (H3b 완료 조건)

**빈 디스크에서 사람이 노드에 접속하지 않고 `protected`까지 가는지**, 그리고 **운영 노드의 단계를 실패시킨 뒤
다시 요청해도 다시 포맷하지 않는지**를 한 번에 본다. 실패를 원하지 않으면 4.1을 건너뛴다.

### 4.1 운영 노드(A)에 실패 심기

서비스 기동만 실패하게 하는 드롭인이다. 승격·포맷·마운트는 그 전에 끝난다.

```bash
# A에서
sudo mkdir -p /etc/systemd/system/aiha-test.service.d
printf '[Service]\nExecStartPre=/bin/false\n' | sudo tee /etc/systemd/system/aiha-test.service.d/zz-fail.conf
sudo systemctl daemon-reload
```

### 4.2 구성 요청

```bash
./aiha-framework ha setup ha-test
# Started setup <flowId> on ha-test (<groupId>); the standby goes first (job <jobId>).
```

30초 간격으로 지켜본다.

```bash
watch -n 10 ./aiha-framework ha status ha-test
./aiha-framework ha events ha-test
```

| 단계 | `ha status`에서 보이는 것 | 기다리는 시간 |
| --- | --- | --- |
| 1단계: B 구성 | `State: configuring`, `Operation: setup …, running at step 1`, B의 잡 `running` → `completed`(메시지 `Set up as standby of <RES> (Secondary, disk Inconsistent); did …`) | 잡 수 초 + B의 다음 보고(≤30s) |
| 2단계: A 구성 | `step 2`, A의 잡. 4.1을 했으면 `failed`: `… start aiha-test.service: …` | 연결 대기 최대 2분 포함 수십 초 |
| 실패 | `State: error`, `Operation: setup …, failed at step 2`, `ha events`에 `flow_ended failed`와 `state_changed configuring -> error` | |

실패했을 때 A에서 포맷된 파일시스템의 UUID를 적어 둔다(재요청 뒤 같아야 한다).

```bash
# A에서
sudo drbdadm status <RES>          # role:Primary, 상대 Secondary, 초기 동기화 진행 중
sudo blkid /dev/drbd0              # TYPE="ext4" UUID="…"  ← 적어 둔다
findmnt /data                      # /dev/drbd0 이 마운트되어 있다
```

### 4.3 고치고 확인한 뒤 재요청

```bash
# A에서
sudo rm /etc/systemd/system/aiha-test.service.d/zz-fail.conf && sudo systemctl daemon-reload

# 사내 VM에서
./aiha-framework ha acknowledge ha-test     # Acknowledged: ha-test (…) is now registered
./aiha-framework ha setup ha-test
```

기대 — 잡 메시지(`ha status`의 `Operation` 아래, 또는 `aiha-framework job get <jobId>`의 `Message`):

- B: `nothing needed doing`에 가깝다. 이미 된 단계는 `already so:`에 모인다(`create-metadata`, `bring-up-resource` 등).
- A: `did start-service; already so: …, promote, format-volume, mount-volume`. **`first-promotion`과 `format-volume`이
  `did` 쪽에 없어야 한다.**
- A에서 `sudo blkid /dev/drbd0`의 UUID가 4.2와 같다.

### 4.4 `protected`까지

2단계가 끝나면 `State: syncing`, 초기 동기화가 끝나고 판정이 `ok`가 되면 `protected`가 된다.

```bash
./aiha-framework ha status ha-test     # Replication: Primary/Secondary, Connected/Established, disk UpToDate/UpToDate, 0B out of sync
./aiha-framework ha events ha-test     # … state_changed syncing -> protected
```

**초기 동기화 시간**은 `ha events`의 `flow_ended completed`(=`syncing` 진입)부터 `syncing -> protected`까지다. A에서
`sudo drbdadm status <RES>`의 `done:` 진행률로도 볼 수 있다. H0 AWS 기준 약 80 MiB/s였다(8 GiB에 약 100초).

### 4.5 노드에 남은 것 확인

```bash
# A, B 모두
sudo ls -l /etc/drbd.d/<RES>.res                       # -rw------- (0600). 내용에 공유 비밀이 있으니 출력하지 않는다
cat /etc/systemd/system/data.mount                     # What=/dev/drbd0, Where=/data, [Install] 없음
cat /etc/systemd/system/aiha-test.service.d/aiha-replication.conf   # RequiresMountsFor=/data
systemctl is-enabled 'drbd@<RES_UNIT>.target' drbd-configured.target  # enabled enabled
systemctl is-enabled drbd.service                      # disabled 그대로

# A
systemctl is-active data.mount aiha-test.service       # active active
tail -n 2 /data/writer.log
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8000/   # 200

# B
systemctl is-active data.mount aiha-test.service       # inactive inactive
```

---

## 5. 시험 4 — 대기 노드에서 쓰기가 막히는지

```bash
# B에서
sudo mount /dev/drbd0 /mnt; echo "exit=$?"             # 실패(Wrong medium type)
sudo systemctl start aiha-test.service; echo "exit=$?" # 실패(Dependency failed: data.mount)
ls -la /data                                           # 루트 파일시스템의 빈 디렉터리
```

시도 뒤 `ha status`의 B 서비스가 `failed`로 보일 수 있다. 판정에는 대기 서비스가 "돌고 있음"만 문제가 된다.
`sudo systemctl reset-failed aiha-test.service`로 지운다.

---

## 6. 시험 5 — 장애 주입과 판정 (H1)

그룹이 `protected`인 상태에서 하나씩 주입하고, 되돌린 뒤 `protected`로 돌아오는 것까지 본다. 판정(`Verdict`)은
보고마다 바뀌고, 그룹 상태(`State`)는 조건이 `HA_STALE_AFTER`(90s) 이상 지속될 때 바뀐다.

| # | 주입 (노드) | 기대 판정과 사유 | 기대 상태 | 되돌리기 |
| --- | --- | --- | --- | --- |
| 5.1 | `sudo systemctl stop aiha-test.service` (A) | `error`: 운영 서비스 정지, 포트 닫힘, 헬스 실패. B의 프로브 실패는 운영 노드 자체 점검이 실패하면 중복이라 사유에 넣지 않는다 | 90초 뒤 `failover_pending` | `sudo systemctl start aiha-test.service` → `protected` |
| 5.2 | `sudo drbdadm disconnect <RES>` (A) | `attention`(연결 끊김) → 90초 지속 시 `error`(연결 끊김 지속). B 디스크가 `Outdated`가 되어 곧바로 `error`(대기 디스크 Outdated)도 붙는다 | `degraded` | `sudo drbdadm connect <RES>` (A, B가 `StandAlone`이면 B에서도) → 재동기화 → `protected` |
| 5.3 | `sudo systemctl stop aiha-agent` (B) | `attention`: 대기 보고 지연 | `degraded` | `sudo systemctl start aiha-agent` → `protected` |
| 5.4 | `sudo systemctl stop aiha-agent` (A) | `error`: 운영 보고 끊김(원인 미확인) | 90초 뒤 `failover_pending` | `sudo systemctl start aiha-agent` → `protected` |
| 5.5 | `sudo iptables -I INPUT -p tcp --dport 8000 -s <B_IP> -j DROP` (A) | `attention`: B의 프로브 실패. A 자신의 점검은 정상 | `protected` 유지 | `sudo iptables -D INPUT -p tcp --dport 8000 -s <B_IP> -j DROP` |

- 사유는 `ha status`의 `Verdict` 아래와 `ha events`의 `verdict_changed`에 나온다. `ha events`는 오래된 것부터
  50개를 보여 주므로, 시험이 길어지면 `--after <마지막 번호>`로 이어 본다.
- 5.2 뒤 `Last sync confirmed`가 멈춰 있다가 재동기화 후 다시 갱신되는지 본다.
- `failover_pending`에서 장애 전환은 아직 구현되지 않았다(H4). 원인을 되돌리면 평가기가 `protected`로 되돌린다.

---

## 7. 시험 6 — 재부팅

### 7.1 대기 노드 재부팅

```bash
# B에서
sudo reboot
# 부팅 뒤
sudo drbdadm status <RES>          # 저절로 올라와 Connected, UpToDate (drbd@<RES>.target)
systemctl is-active data.mount aiha-test.service     # inactive inactive
```

`ha status`는 재부팅 동안 `degraded`, 돌아오면 `protected`.

### 7.2 상대가 꺼진 채 재부팅

B를 클라우드 콘솔에서 **중지**한 상태로 A를 재부팅한다. `drbd@<RES>.target`이 상대를 기다리지 않고 기동을 끝내는지 본다
(`drbd.service`를 쓰지 않는 이유다).

```bash
# A에서, 부팅 뒤
systemctl is-system-running        # running (starting 에 멈추지 않는다)
sudo drbdadm status <RES>          # Secondary, 상대 Connecting
systemctl is-active data.mount aiha-test.service     # inactive inactive — 재부팅한 노드는 Secondary 로 올라온다
```

B를 다시 시작한 뒤에도 A는 Secondary로 남는다. **재부팅 뒤 운영 복귀(`ha resume`)는 아직 없으므로(H4) 손으로
되돌린다.**

```bash
# A에서
sudo drbdadm primary <RES> && sudo systemctl start data.mount && sudo systemctl start aiha-test.service
```

그 사이 `ha status`는 운영 노드 역할 불일치로 `error` 판정이고, 그룹은 `degraded` 또는 `failover_pending`이 된다.
손으로 되돌리면 `protected`로 돌아온다. B를 중지한 채 재부팅하면 B의 `by-id` 이름이 그대로인지도 함께 본다
([ha-h0.md](ha-h0.md) 13절, 중지 후 시작).

---

## 8. 시험 7 — 흐름 중 거절 (선택)

| 시도 | 기대 |
| --- | --- |
| 구성 흐름이 도는 동안 `./aiha-framework job backup --system <A> --repository <저장소>` | 거절: 시스템이 이중화 작업 중 |
| 구성 흐름이 도는 동안 `./aiha-framework ha setup ha-test` 또는 `ha teardown ha-test` | 거절: 이미 진행 중인 작업 |
| `protected`에서 `./aiha-framework ha setup ha-test` | 거절: `setup starts from registered` |
| `protected`에서 `./aiha-framework ha acknowledge ha-test` | 거절: 오류 상태가 아님 |
| 이중화 잡에 `./aiha-framework job cancel <jobId>` | 거절: 이중화 잡은 흐름과 함께 끝난다 |
| 9절 해제 뒤 다시 구성하기 전에 B에서 `sudo systemctl start aiha-test.service`(해제가 드롭인을 지웠으므로 뜬다)를 하고 `ha setup` | B의 잡이 `refused: service aiha-test.service is running, and the volume is not mounted here …`으로 실패, 그룹 `error`. B에서 서비스를 멈추고 `ha acknowledge` → `ha setup` |
| A의 백업 후보 | `agent inventory <A>`에서 `/data`가 `BACKUP no`, 사유 `a replicated (DRBD) volume …` |

흐름은 수십 초 안에 끝나므로 거절 시험은 4.2 직후나 9절 직후에 바로 한다.

---

## 9. 시험 8 — 해제와 다시 구성

```bash
./aiha-framework ha teardown ha-test
# Started teardown …; the primary goes first …
watch -n 10 ./aiha-framework ha status ha-test      # configuring → registered
```

기대:

- A 먼저: 서비스 정지 → 언마운트 → Secondary → 리소스 내림. 그 다음 B.
- 두 노드에서 `/etc/drbd.d/<RES>.res`, `/etc/systemd/system/data.mount`, 서비스 드롭인이 없어지고,
  `drbd@<RES>.target`은 disabled.
- **데이터는 남는다.** `sudo blkid <데이터 디스크>`가 `TYPE="drbd"`. 서비스는 정지·disabled 상태로 남는다.

다시 구성:

```bash
./aiha-framework ha setup ha-test
```

- 두 노드 잡 메시지의 `already so:`에 `create-metadata`, A는 `format-volume`도 있어야 한다(메타데이터·파일시스템 재사용).
- A에서 `head -n 1 /data/writer.log`가 첫 구성 때의 시각이다(이전 데이터가 그대로 있다).
- 디스크가 이미 동기화되어 있으므로 `syncing`이 짧다.

---

## 10. 정리

`ha teardown`으로 해제한 뒤 데이터 디스크를 다른 데 쓰려면 서명을 지운다. **되돌릴 수 없다.** 해제가 리소스 파일을
지웠으므로 [ha-h0.md](ha-h0.md) 10.2의 `drbdadm wipe-md`는 쓸 수 없고 `wipefs`로 한다. 바깥의 `drbd` 서명을 지우면
안쪽의 `ext4` 서명이 보이므로, 아무것도 나오지 않을 때까지 반복한다.

```bash
# 각 노드에서
D=<데이터 디스크 by-id 경로>
sudo blkid -p "$D"; sudo wipefs -a "$D"     # 출력이 없을 때까지 반복
sudo rm /etc/systemd/system/aiha-test.service && sudo systemctl daemon-reload
```

그룹 삭제 명령은 아직 없다. 다시 시험하려면 다른 `--name`으로 등록한다. 한 시스템은 한 그룹에만 속하므로, 같은
노드로 새 그룹을 만들려면 DB에서 옛 그룹을 지워야 한다(`DELETE FROM ha_groups WHERE name = 'ha-test'`, 노드·이벤트·
흐름 기록이 함께 지워진다).

---

## 11. 문제 해결

| 증상 | 확인 |
| --- | --- |
| `ha setup`: `a node of the HA group is not reporting` | 두 노드 모두 최근 90초 안에 보고했어야 한다. `ha status`의 `Reported`, 해당 노드의 `journalctl -u aiha-agent` |
| `ha setup`: `the framework's secret key is not loaded` | `AIHA_FRAMEWORK_SECRET_KEY_FILE`이 설정된 셸인지 |
| 잡이 `queued`에서 멈추고 5분 뒤 `failed`(`No agent took this job before it expired`) | 그 노드의 에이전트가 `run` 중인지, 프레임워크에 닿는지 |
| B 잡이 `refused: … drbd@<RES_UNIT>.target is not-found` | `drbd-utils`가 LINBIT 9.x인지(1.1) |
| A 잡이 `refused: the peer did not connect within 2m0s` | 7789/tcp가 두 노드 사이에 열려 있는지, B의 `sudo drbdadm status <RES>` |
| 잡이 `refused: this node is "…" but the job is for "…"` | 그 노드의 호스트명이 보고 뒤 바뀌었다. 다음 보고 뒤 `ha acknowledge` → `ha setup` |
| 잡 완료 뒤 흐름이 `failed`: `… reports … do not show the resource up as Secondary` | 그 노드의 `ha status` `Replication` 줄과 `sudo drbdadm status <RES>` |
| `refused: … holds ext4` 등 | 빈 디스크가 아니다. 다른 디스크를 쓰거나 `wipefs -a`(데이터 삭제) |
| B 잡이 `create the replication metadata: … Do you want to proceed?` | 서명은 없지만 0이 아닌 데이터가 있다(1.2). 확인 후 `blkdiscard -z` |
| B 잡이 `A dependency job for drbd@….target failed`, 저널에 `… is not a valid number` | 에이전트가 유닛 이름의 `-`를 이스케이프하지 않는 옛 빌드다(2026-11-24 수정) |
| 판정이 계속 `attention`(B의 프로브 실패) | B → A 8000/tcp |

---

## 12. 결과 기록

2026-11-24 AWS(ap-northeast-2, 두 노드 같은 서브넷 10.0.1.0/24) 재시험. 그룹 `ha-test`(`16c07671ae68307a`),
리소스 `aiha-16c07671`, 기본 설정(보고 30s, 유예 90s).

| # | 시험 | 결과 | 비고(시간, 출력) |
| --- | --- | --- | --- |
| 1 | 인벤토리 데이터 디스크 | 통과 | AWS EBS 각 8 GiB, `nvme-Amazon_Elastic_Block_Store_vol…`, `HOLDS nothing`. 루트 디스크 제외. 구성 뒤 `HOLDS replication metadata` |
| 2 | 그룹 등록, 두 노드 보고 | 통과 | 양쪽 보고와 역할별 데이터 디스크가 `registered`에서 표시됨 |
| 3 | 구성 → 실패 → 확인 → 재요청, UUID 동일 | 통과(수정 후) | 발견 1·2 해결 뒤. A 2단계 `start aiha-test.service` 실패 → `error`. 재요청: B `nothing needed doing`, A `did start-service; already so: … promote, format-volume, mount-volume`. UUID `5473305f-…` 동일 |
| 3 | `protected` 도달, 초기 동기화 시간 | 통과 | 초기 동기화 95초(8 GiB, 88.5 MB/s, 커널 로그 `Resync done`). 흐름이 끝나기 전에 동기화가 끝나 `syncing -> protected`가 같은 시각 |
| 3 | 노드의 파일·유닛 상태 | 통과 | `.res` 0600, `data.mount`·드롭인 내용, `drbd@aiha\x2d16c07671.target` enabled, `drbd.service` disabled. A active/active·HTTP 200, B inactive/inactive |
| 4 | 대기 노드 마운트·서비스 차단 | 통과 | `mount`: `Wrong medium type`(exit 32), 서비스: `A dependency job … failed` |
| 5.1 | 운영 서비스 정지 | 통과 | 판정 `error` 16초, `failover_pending` 그 뒤 90초. B 프로브 사유는 의도적으로 생략(문서 기대 수정). 복귀 12초 |
| 5.2 | 복제 연결 끊김 | 통과 | `attention` → 90초 뒤 `error`, `degraded`. B `Outdated` 사유 추가. `Last sync confirmed` 정지 → `connect` 후 23초에 `protected`, 갱신 재개 |
| 5.3 | 대기 에이전트 정지 | 통과 | 약 2분 뒤 `attention`(보고 없음 1m57s), `degraded`. 시작 후 2초에 `protected` |
| 5.4 | 운영 에이전트 정지 | 통과 | 1m46s 무보고로 `error`(원인 미확인), 곧바로 `failover_pending`. 시작 후 1초에 `protected` |
| 5.5 | B → A 8000/tcp 차단 | 통과 | `attention`(프로브 실패), `protected` 유지 |
| 6 | 대기 재부팅 | 통과 | 재부팅 11초. 첫 보고가 DRBD 기동보다 앞서 30초간 `degraded`(`Replication status … could not be read`), 다음 보고에 `protected`. B Secondary·UpToDate, inactive/inactive |
| 6 | 상대 무응답 중 운영 재부팅 | 통과(대체 방식) | EC2 중지 권한이 없어 B에서 A의 7789/tcp를 DROP으로 대신했다. A `running`(부팅 8.9초), Secondary·Connecting, inactive/inactive. 그룹 `degraded` → `failover_pending`(역할 불일치 `error`). 손으로 승격·마운트·기동 뒤 `protected`. 중지·시작 뒤 by-id 유지 여부는 미확인 |
| 7 | 흐름 중 거절 | 통과 | 해제 중 `setup`·`teardown`: `already in an HA operation`, 백업: `taking part in an HA operation`, 잡 취소: `cannot be cancelled on its own`. `protected`에서 `setup`·`acknowledge` 거절. B 서비스 실행 중 `setup`: `refused: service … is running, and the volume is not mounted here` |
| 7 | A의 백업 후보 | 통과 | `/dev/drbd0 /data … no  a replicated (DRBD) volume, which its HA group protects rather than backups` |
| 8 | 해제 후 데이터 유지, 다시 구성 시 재사용 | 통과 | 해제 약 40초, 두 노드 파일·유닛 제거, 디스크 `TYPE=drbd`. 재구성: 양쪽 `already so: create-metadata`, A `format-volume`. `writer.log` 첫 줄 유지, UUID 동일, 재동기화 없음 |
| - | 쓰기 지연 | 측정(같은 AZ) | A `/data`, 4k randwrite, fsync, 연결 상태: 252 IOPS, 쓰기 평균 1.30 ms(p50 1.19, p99 3.33), fsync 평균 2.65 ms. ping RTT 평균 0.33 ms. 가용 영역 간은 미측정 |
| - | 커널, `drbd-dkms`·`drbd-utils` 버전 | 확인 | Ubuntu 24.04.5, `7.0.0-1018-aws`, 모듈 9.3.4, LINBIT PPA 유틸리티 9.36.0 |

### 발견 사항

1. **데이터 디스크가 비어 있지 않았다(환경).** 두 EBS 모두 4 KiB 이후 끝까지 무작위 데이터였고, 부팅 뒤 쓴 기록이
   없어 붙을 때부터 그 상태였다(출처 미확인). `blkid`는 서명을 찾지 못해 에이전트가 `create-md`를 했고,
   `drbdmeta`가 `Found some data`로 확인을 물어 실패했다. 1차 시험(codex)이 멈춘 곳이다.
   `blkdiscard -z`로 비운 뒤 진행했다. 새로 만든 인스턴스·볼륨에서는 `blkid`가 종료 코드 2(서명 없음)로 정상이고
   디스크도 0이므로, 0 검사는 고도화 때 검토한다([ha-roadmap.md](ha-roadmap.md) 9절).
2. **DRBD 유닛 이름 이스케이프 결함(코드, 수정함).** 에이전트가 `drbd@aiha-16c07671.target`을 그대로 썼는데,
   `drbd@.service`는 풀린 인스턴스 이름(`%I`)을 넘기므로 `-`가 `/`가 되어 `drbdadm adjust aiha/16c07671`(리소스
   `aiha`의 볼륨 `16c07671`)로 실행되고 `16c07671 is not a valid number`로 실패했다. 기본 리소스 이름
   `aiha-<그룹 ID 8자>`에 늘 `-`가 있어 모든 구성이 1단계에서 실패한다. `pkg/agent/ha/replication`의 유닛 이름에서
   `-`를 `\x2d`로 이스케이프하도록 고치고, 테스트가 systemd가 쓰는 이름(`drbd@aiha\x2d6a7b8c9d.target`)을 고정하게
   했다. 노드에는 수정 빌드를 배포했다(`/usr/local/bin/aiha-agent.prev`에 이전 빌드).
3. 문서 기대값 수정: 5.1의 B 프로브 사유(의도적 생략), 5.2의 B `Outdated` 사유, 유닛 이름 이스케이프(4.5, 11절).
4. 재부팅 직후 에이전트의 첫 보고가 DRBD 기동 전에 나가 한 보고 주기 동안 `degraded`가 된다. 문제는 아니지만,
   `aiha-agent.service`를 `drbd-configured.target` 뒤에 두면 없앨 수 있다(미적용).
