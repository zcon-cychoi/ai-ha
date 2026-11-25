# 1차년도 로드맵 (데모 목표)

1차년도(2026) 데모까지의 구현 순서와 범위를 정한다. 대상 요구사항은
**SFR-HA-01(백업), SFR-HA-02(복구), SFR-HA-03(이력)** 이다.

관련 문서: [architecture.md](architecture.md), [protocol.md](protocol.md),
[data-model.md](data-model.md), [Restore.md](Restore.md),
[open-decisions.md](open-decisions.md)

---

## 1. 데모 목표

**사용자 요청 → 백업 → 이력 조회 → 복구 요청 → 새 인스턴스가 복구된 상태로 부팅 →
부팅 확인**까지 프레임워크를 통해 시연한다.

기간: 약 1개월.

---

## 2. 전제: 복구 메커니즘은 이미 검증되었다

- system-rescue + `copytoram` 방식의 레스큐 진입과 복구·부팅이 **AWS Ubuntu 인스턴스에서
  수동으로 성공**했다.
- **GCP Ubuntu → AWS 교차 클라우드 복구**도 OS 부팅 및 SSH 로그인까지 확인되었다.
- 사내에 클라우드 리눅스 마이그레이션 기술이 정형화되어 있어, 수동으로는 대부분의
  클라우드/온프레미스 환경에서 부팅까지 가능하다.

따라서 **남은 작업은 R&D가 아니라 정해진 시나리오에 대한 자동화**다. 이 전제가 아래
범위 결정의 근거다.

### 2.1 user-data 레스큐 진입 — 검증 완료 (2026-09-14)

`sh/setup-sysres.sh`를 EC2 user-data로 넣어 **Ubuntu 24.04에서 system-rescue가
`copytoram`으로 올라오는 것을 확인**했다. 이로써 복구 경로의 **기술적 미지수는
남아 있지 않다.** 남은 것은 구현 과제뿐이다(Phase 0 결과 참조).

---

## 3. 범위 방침: 일반화하지 않는다

사내 방침상 **모든 상황에 대응하는 완벽한 자동화를 목표로 하지 않는다.** 정해진
시나리오에서 동작하는 수준으로 구현한다. 이는 CLAUDE.md의 "불필요한 복잡성 배제",
"요청받은 것만 수정" 원칙과도 일치한다.

### 3.1 고정 시나리오

ETRI 측과 합의된 스펙에 맞춘다.

| 항목 | 값 |
| --- | --- |
| OS | **Ubuntu 24.04** |
| 아키텍처 | **x86_64** |
| 가속기 | **NVIDIA GPU** |
| 클라우드 | AWS (소스·타겟) |
| 디스크 구성 | 단일 디스크, LVM 없음 |
| 저장소 | S3 (사내 MinIO 포함) |
| Secure Boot | **비활성.** 백업·복구와 이중화 공통의 이용 제한사항이다. 레스큐 진입(`sh/setup-sysres.sh`는 켜져 있으면 중단한다)과 이중화의 DRBD 9 커널 모듈(DKMS 빌드, 서명 없음)이 모두 이를 전제로 한다 ([ha-roadmap.md](ha-roadmap.md) 7절 결정 5) |

### 3.2 설계 문서에 있으나 1차년도에 구현하지 않는 것

모델에는 자리를 남기되 **구현은 고정 경로 하나만** 한다. 나중에 스키마를 바꾸지 않기
위해 필드는 두고, 분기 로직은 만들지 않는다.

| 설계 문서의 항목 | 1차년도 |
| --- | --- |
| Tier 1/2/3 판정 ([data-model.md](data-model.md) 4.7) | **구현 안 함.** 고정 시나리오 하나 |
| `driverStrategy` 도출 로직 ([data-model.md](data-model.md) 7절) | **구현 안 함.** Ubuntu 24.04 → AWS 조합을 템플릿으로 고정 |
| Platform 전체 필드 ([data-model.md](data-model.md) 3.3) | **수집만** 한다(비용이 낮고 이력 가치가 있음). **분기 로직은 만들지 않음** |
| 레이아웃 추론·적응 엔진 | **구현 안 함.** 알려진 형상만 재현 |
| BIOS/UEFI 분기 | 레스큐가 부팅된 방식으로 `grub-install` 대상만 가른다. 실환경 검증은 UEFI만 |
| LVM, Btrfs 서브볼륨, RAID, LUKS | **구현 안 함** |
| 디스크 축소(shrink) | **구현 안 함** ([data-model.md](data-model.md) 4.8) |
| `path` / `container` 대상 | **구현 안 함.** `filesystem`만 |
| 스냅샷 일관성 | **구현 안 함.** `consistencyMode: live` 표기만 |
| 인증·RBAC | **등록 토큰 → 에이전트 토큰** (open-decisions 8번, 2026-09-17 확정·구현). 사용자 RBAC은 SFR-API-03, 2027년 |
| `Idempotency-Key`, 취소, `attemptId` fencing | `attemptId` **기록만**. 리스 만료 시 자동 재실행 안 하는 방침 덕에 fencing 로직 불필요 |
| 진행률 3종 분리 ([data-model.md](data-model.md) 6.3) | 기존 `countingWriter`의 업로드 바이트만 |
| 재조정 항목 대부분 ([Restore.md](Restore.md) 6절) | `fsUUID` 보존으로 자동 해소되는 것 외에 netplan MAC 치환만. cloud-init은 처리하지 않고, user-data 재실행만 aiha-init.sh에서 막는다 |
| Mode B (제자리 복구) | **구현 안 함.** 레스큐 진입 절차를 공유하므로 나중에 |

**GCP→AWS 교차 클라우드에서 겪은 까다로운 처리들은 코드로 옮기지 않는다.**

---

## 4. 구현 순서

### Phase 0 — user-data 레스큐 진입 자동화 — **완료 (2026-09-14)**

수동 grub/`copytoram` 절차를 `sh/setup-sysres.sh`로 감싸 user-data로 실행했다.

| 확인 항목 | 결과 |
| --- | --- |
| user-data 실행 → system-rescue 다운로드 → grub 설정 → `grub-reboot` → 재부팅 → 레스큐 `copytoram` 부팅 | ✅ **성공** (Ubuntu 24.04). 재부팅은 스크립트 끝에 강제 `reboot`을 추가해 수행 |
| sysres.tar SHA-256 검증 | ✅ 스크립트에 추가됨 (검증 로직 단위 확인, 실인스턴스 미실행) |
| 복구 정보 파일(`recoveryId`, 프레임워크 주소, 클레임 토큰) 전달 | ✅ M1에서 구현 (2026-09-15, 아래 M1) |
| 레스큐 부팅 후 프레임워크 도달 | ✅ EC2 → 사내 프레임워크(포트포워딩) claim 확인 (2026-10-12) |

남은 항목은 **미지수가 아니라 구현 과제**였다. 레스큐 부팅 자체가 막히면 설계가
바뀌었겠지만, 그 부분은 통과했다.

결정된 운영 방식:

- **다운로드 URL은 프레임워크가 생성한 값으로 대체한다.** 현재 스크립트의 URL은 기술
  확인용 임시값(사내 MinIO)이다. URL을 통한 다운로드 방식 자체는 유지한다.
- **스크립트는 GitHub Releases에 올리고 user-data에서 `curl ... | bash`로 받아
  실행한다.** HTTPS로 받으므로 스크립트 체크섬은 두지 않고, 대신 태그 고정 URL과
  HTTPS 강제 옵션을 쓴다. 재부팅은 `SYSRES_REBOOT=1`
  ([Restore.md](Restore.md) 5.2).

### M1 — 레스큐 에이전트의 claim (2026-09-15 구현)

Phase 2·3 중 **레스큐 에이전트가 프레임워크에서 복구 정보·저장소 정보를 받아오는 경로**를
먼저 구현했다. 실제 복구 수행은 포함하지 않는다.

| 구성 | 내용 |
| --- | --- |
| `aiha-framework` | `server`(HTTPS, 인증서 지문 고정), `restore create`(당시 플랜 파일 등록 → aiha-init.sh 값 출력, 이후 `--backup-set`으로 대체). PostgreSQL(`pgx`) |
| `aiha-rescue rescue` | 지문 고정 TLS로 claim → `/run/aiha/<recoveryId>/`에 `restore-plan.json`·`rclone.conf`·`report.token`(tmpfs에만) |
| `sh/aiha-init.sh` | setup-sysres.sh 실행 후 `/autorun/aiha`에 복구 정보·에이전트 설치, 재부팅 |
| `/autorun/autorun2` (이미지에 포함, 원본 `sh/autorun2`) | 레스큐 부팅 후 SystemRescue가 자동 실행 → 에이전트 claim·복구. `copytoram` 덕분에 복구 대상 디스크를 마운트하지 않는다 |

검증: 단위·PostgreSQL 통합 테스트(동시 claim, 응답 유실 포함)와 로컬 E2E. EC2 → 사내 프레임워크
실흐름은 2026-10-12에 확인했다. 레스큐 자동 기동과 에이전트 전달은 `/autorun` 방식으로 해결했다.

### Phase 1 — 백업 측 메타데이터 (저장 형식 확정) — **완료 (2026-09-17)**

**이것을 먼저 한 이유는 하나뿐이지만 결정적이다: 그 전까지 쌓은 백업본에는 복구가
읽을 정보가 없었다.** 이제 복구 실행기(Phase 2)가 읽을 포맷이 정해졌으므로 그것을
대상으로 개발할 수 있다.

| 항목 | 상태 |
| --- | --- |
| 시스템 정보 수집 (`pkg/backup/sysinfo`) | ✅ `lsblk -J -O`, `sfdisk -d`, LVM, UEFI 판별, fstab, os-release, uname, mountinfo, netplan |
| `systemId` 도입 ([data-model.md](data-model.md) 2.1) | ✅ `/var/lib/aiha/system-id`. 저장소 경로가 `<systemId>/`로 바뀜 |
| `BackupSet` / `BackupItem` 그룹핑 (5.2, 5.4) | ✅ `backup --device`를 반복해 한 세트로 캡처 |
| 세트별 `manifest.json` (1.5) | ✅ `<systemId>/<setId>-manifest.json` |
| 에이전트 자격을 백업에서 제외 | ✅ `/var/lib/aiha/` 제외 + 매니페스트에 `excludedPaths` 기록 |

수집 대상은 **수동 복구 때 실제로 참조한 정보**로 정했다. `blkid`는 넣지 않았다 —
`lsblk -O`가 UUID/PARTUUID/FSTYPE/LABEL을 모두 포함해 완전히 중복이다. `uname`도
`syscall.Uname`으로 대체해, 외부 명령 실행은 `lsblk`/`sfdisk`/`lvm`/`vgcfgbackup`
넷으로만 한정하고 `pkg/backup/sysinfo/runner.go` 한 곳에 가뒀다(이 리포지토리 최초의
`os/exec` 사용이다). **수집 실패는 백업을 중단시키지 않는다** — 항목별로 기록하고
진행한다.

**저장소 레이아웃**: 매니페스트는 세트 하위 디렉터리가 아니라 시스템 디렉터리에
`<setId>-manifest.json`으로 평면 배치한다. 모든 저장소 백엔드가 파일명의 경로
구분자를 거부하는데(시스템 디렉터리 밖으로 나가는 것을 막는 방어), 중첩을 위해 그
검증을 5개 백엔드에서 고치는 것보다 세트 id를 파일명에 담는 편이 안전하다. 그룹핑은
매니페스트가 아이템 파일명을 나열해 유지한다. ([data-model.md](data-model.md) 8절의
스케치와 이 점이 다르다.)

**이 단계가 끝났으므로 이제 데모용 백업 데이터를 쌓을 수 있다.**

### Phase 2 — 복구 실행기 — **구현 (2026-10-08), EC2 실환경 복구·부팅 확인 (2026-10-12)**

실환경(AWS EC2, Ubuntu 24.04, 사내 MinIO): 3개 파티션 복구 약 74초, 수동 재부팅 후 복구된 OS 부팅 확인.
그 과정에서 찾아 고친 것:

- 이미지의 `autorun2`가 aiha-init.sh의 것을 덮어씀 → `autorun2`를 이미지에 두기로(`sh/autorun2`)
- autorun의 짧은 PATH로 `mountall`을 못 찾음 → `autorun2`가 `/etc/profile`을 읽음
- 원본 `nvme1n1`, 타깃 `nvme0n1`처럼 장치 이름이 다름 → **PARTUUID로 파티션을 찾음**
- `preparing` 실패가 잡을 끝내 버림 → 디스크 무변경 단계의 실패는 같은 잡으로 재실행 가능
- 복구된 OS에서 cloud-init이 user-data(aiha-init.sh)를 다시 실행해 레스큐로 재부팅 →
  aiha-init.sh가 이 복구의 표식이 있으면 아무것도 하지 않음

구현 결과 (`pkg/agent/restore`, `aiha-rescue rescue` / `restore`):

| 항목 | 상태 |
| --- | --- |
| 플랜 = `sourceSystemId` + `sourceBackupSetId`, 파일 목록은 매니페스트에서 | ✅ 프레임워크가 세트 ID로 생성(2026-10-12) |
| 포맷 전 검증 (매니페스트, 도구, PARTUUID 매칭, 마운트 여부, NIC 매칭, UEFI면 ESP) | ✅ 실패 시 디스크 무변경, 같은 잡으로 재실행 가능 |
| 포맷 (원본 label·**UUID 보존**, `blkid`로 확인) → `mountall --no-bind` | ✅ |
| `rclone cat \| zstd -d \| tar -x --numeric-owner` | ✅ 읽은 바이트를 진행률로 보고 |
| netplan MAC 치환, fstab / grub defaults / EFI stub의 UUID 검사(불일치는 경고) | ✅ |
| `recovery-pending.json` 기록 | ✅ (`bootVerifyToken` 없음 — boot-verify 미구현) |
| `arch-chroot`: `grub-install` → `update-grub` → `update-initramfs -u -k all` | ✅ |
| 진행·결과 보고 (복구 전용 자격 `reportToken`) | ✅ `aiha-framework restore get` 으로 확인 |
| 체크섬 검증, 재부팅, `restoring` 타임아웃 스위퍼 | ❌ |

결정 사항: 타깃이 원본과 같은 이미지라는 전제로 **파티션 테이블은 건드리지 않고**
매니페스트에 기록된 **PARTUUID로 타깃 파티션을 찾는다**(장치 이름은 인스턴스마다 다르다). 원본 `fsUUID`·label로 포맷하므로 fstab 등은
고치지 않고 검사만 한다(UUID를 고치는 코드는 마이그레이션용으로 남김). netplan은 타깃 설정으로 바꾸지 않고 **MAC 주소만 치환**한다
(원본 MAC 1개·타깃 NIC 1개일 때만, 그 외는 포맷 전에 중단).

아래는 착수 전 계획이다.

검증된 수동 런북을 에이전트 코드로 옮긴다. 플랜 JSON을 손으로 만들어 두면 Phase 1의
메타데이터 수집을 기다리지 않고 개발할 수 있다.

**rclone을 활용한다.** system-rescue에 rclone이 설치되어 있으므로 **복구 경로에서
`pkg/backup/repository`가 불필요하다.**

```
rclone cat <repo>:<path>/<item>.aha | zstd -d | tar -x -C /mnt/target
```

에이전트는 이를 오케스트레이션한다. S3/SFTP/SMB 백엔드를 레스큐용으로 다시 만들지
않는다. 백업 경로는 기존 Go 스트리밍/재개(`pkg/backup/spool`)가 rclone보다 나으므로
그대로 둔다. 백업과 복구의 이 비대칭은 의도된 지름길이며 문서에 남긴다.

**자격증명 주의**: rclone 설정 파일을 **tmpfs 경로에만** 쓴다. `copytoram`이므로
레스큐 파일시스템은 RAM에만 존재해 재부팅과 함께 사라지지만, 실수로 복구 대상 디스크에
남기면 그대로 유출된다 ([Restore.md](Restore.md) 5.5).

핵심 절차 ([Restore.md](Restore.md) 3절):

- 실행 가능성 검증 후 파티션·파일시스템 생성 (**원본 `fsUUID` 보존**)
- 마운트 → 추출 → 체크섬 검증
- 원본 에이전트 자격증명 제거 ([Restore.md](Restore.md) 5.4)
- chroot에서 grub 설치 + initramfs 리빌드
- 부팅 확인 토큰 기록 → 완료 보고 → **응답 확인 후** 재부팅

### Phase 3 — 프레임워크 — **남향 백업 경로 완료 (2026-09-17)**

**PostgreSQL로 시작한다** ([open-decisions.md](open-decisions.md) 6번). 착수 전에
드라이버 의존성 승인 절차를 밟는다.

남향 ([protocol.md](protocol.md) 1.6):

| | 상태 |
| --- | --- |
| `register` + 인증 미들웨어 + 등록 토큰 발급·폐기 CLI | ✅ |
| `jobs`(롱폴) — 대기 중인 잡 전달 | ✅ |
| `claim`, `progress`, `result` + 잡 상태머신 + 타임아웃 스위퍼 | ✅ |
| 에이전트 워커 — 잡 수신·실행·보고 | ✅ |
| `inventory` | ✅ 2026-09-18 |
| `restore/{recoveryId}/claim` | ✅ M1 |
| `restore/{recoveryId}/progress`, `/result` (복구 전용 자격) | ✅ 2026-10-08 |
| `restore/{recoveryId}/boot-verify` | ❌ (open-decisions 3번에 걸림) |

에이전트 쪽도 함께 만들었다: `aiha-agent register` / `reset` / `run`(롱폴 상주 루프)과
`deploy/aiha-agent.service`. 영구 오류에는 전용 종료 코드를 써서 systemd가 고쳐지지
않을 상태를 무한 재시작하지 않게 했다(10 재등록 필요 / 11 복구 표식 / 12 설정 오류).

잡 큐는 프로토타입 방침([protocol.md](protocol.md) 2.2)대로 구현했다:

- **`running` 중 리스가 만료되면 재실행하지 않고 `failed`** — 옛 에이전트가 아직 쓰고 있을
  수 있어서, 다른 에이전트에 넘기면 한 대상에 작성자가 둘이 된다. 시도의 결과는
  `unknown`으로 남긴다(연락이 끊긴 것이지 실패가 확인된 것이 아니다)
- **`claimed`에서 시작 보고 없이 시간이 지나면 큐로 되돌린다** — 아직 아무것도 쓰지 않았다
- 에이전트는 **첫 보고가 승인된 뒤에야 쓰기를 시작**한다. 위 되돌림이 안전하다는 전제를
  에이전트 쪽에서 지키는 장치다
- 보고 루프는 작업과 **다른 고루틴**에서 돌고, 리스를 제때 갱신하지 못하면 **리스가 끝나기
  전에** 작업을 멈춘다

**저장소 등록 (2026-10-12)**: 처음에는 잡이 저장소 자격증명을 싣지 않고 에이전트가 자기에게
설정된 저장소를 썼다. 그러면 프레임워크가 백업이 어느 저장소에 있는지 몰라, 복구 잡의 저장소
정보를 손으로 넣어야 했다. 이제 프레임워크가 저장소를 등록해 보관한다(`aiha-framework
repository`, `pkg/framework/repository`).
- 자격증명은 `AIHA_FRAMEWORK_SECRET_KEY_FILE`의 키로 AES-256-GCM 암호화해 저장한다
  (`pkg/framework/secret`). 키를 잃으면 `repository update-credentials`로 같은 저장소에 다시 넣는다.
- 백업 잡은 `--repository`로 저장소를 지정하고 **ID만** 기록한다. 자격증명은 claim 응답에서만
  복호화되며, 복호화는 claim을 기록하기 **전에** 한다 — 실패하면 상태가 바뀌지 않는다.
- 복구 잡은 `restore create --backup-set <setId>`로 만든다. 그 세트를 완료로 보고한 백업 잡에서
  시스템과 저장소를 찾는다. `restore_jobs`의 평문 자격증명 컬럼은 삭제했다.
- 범위: S3(AWS, MinIO)만, CLI 전용(북향 `/aiha/repositories`는 Phase 4). 남은 과제: 단기
  자격증명(STS)·복구 전용 읽기 키, 저장소 삭제·접속 검증, 세트 카탈로그.

**인벤토리 (2026-09-18)**: 에이전트가 자기 시스템의 파티션 구성을 보고하고
(`POST /aiha/agent/inventory`, 기동 시 1회 + 기본 1시간마다), `job backup`이 `--device`
없이도 대상을 고른다. 상세는 [protocol.md](protocol.md) 2.7절. 설계에서 중요한 것 셋:

- **비밀을 담을 수 없는 축약 모델**을 보낸다. `SystemInfo` 전체에는 `/etc/netplan`
  원문이 들어 있어 프레임워크 DB에 복제할 수 없다
- **완전성을 판정한다.** 마운트 테이블은 마운트된 것만 알므로, ESP가 마운트 해제된 채
  고른 세트는 백업은 성공하고 복구만 실패한다. fstab과 대조해 `complete: false`면
  자동 선택을 거절한다
- **에이전트가 실행 직전에 다시 확인한다.** 잡 생성과 실행 사이에 구성이 바뀔 수 있고,
  재사용된 장치 경로는 오류 없이 엉뚱한 파일시스템을 백업한다

**복구 전용 자격**([protocol.md](protocol.md) 2.5): claim 응답의 `reportToken`으로 2026-10-08 구현.

잡 상태머신 ([protocol.md](protocol.md) 3절) — 프로토타입 방침대로 `running` 리스
만료 시 자동 재실행 없이 `failed`.

### Phase 4 — 북향 + 통합 — **API 틀 완료 (2026-11-03)**

REST API를 최우선으로 달라는 요청에 따라, 데모에 필요한 4개가 아니라 설계서가 정의한 **전체
경로를 라우트와 Swagger 계약으로 세웠다.** 구현된 기능은 동작하고 나머지는 501이다. 전체 목록과
상태는 [protocol.md](protocol.md) 4.1.

| 메서드 | 경로 | 데모 필수 |
| --- | --- | --- |
| POST | `/aiha/systems/{systemId}/backups` | ✅ |
| GET | `/aiha/backups` | ✅ |
| POST | `/aiha/restores` | ✅ |
| GET | `/aiha/restores/{restoreId}` | ✅ |

`make swag`로 Swagger 2.0 문서를 생성해 SFR-API-02의 Façade 통합 대상임을 보인다. 요구사항 문구와
베스핀글로벌은 OpenAPI 3.0을 언급했지만 주관기관 ETRI가 Swagger 중심이라고 해서, 별도 지시가 있을
때까지 Swagger 2.0을 유지하고 3.0 변환은 하지 않는다 (2026-11-03).

마지막 며칠은 리허설과 버퍼로 남긴다.

---

## 5. 병렬화

Phase 1(백업 측)과 Phase 2(복구 실행기)는 서로 독립적이다. Phase 3(프레임워크)도
Phase 0 이후라면 언제든 병렬로 시작할 수 있다.

```
Phase 0 ──┬── Phase 1 (백업 메타데이터) ──┐
          │                              ├── Phase 4 (통합)
          ├── Phase 2 (복구 실행기) ──────┤
          │                              │
          └── Phase 3 (프레임워크) ───────┘
```

인원이 한 명이면 0 → 1 → 2 → 3 → 4 순차로 가되, Phase 3의 범위를 3.2절 기준으로
최소화한다.

---

## 6. 데모 이후로 미루는 것

| 항목 | 근거 |
| --- | --- |
| Mode B (제자리 복구) | 레스큐 진입 절차 공유. 추가 비용 작음 |
| rclone 제거 (Go 네이티브 복구 전송) | 동작에는 영향 없음 |
| 스냅샷 기반 일관성 | [open-decisions.md](open-decisions.md) 11번 |
| `attemptId` fencing, 자동 재시도 | [protocol.md](protocol.md) 2.2 |
| 확정(commit) 절차의 완전한 장애 복구 | [data-model.md](data-model.md) 1.3 |
| 조정(reconciliation) 도구 | [data-model.md](data-model.md) 1.4 |
| 인증·RBAC | SFR-API-03 (2027) |
| LVM 등 Tier 2 구성 | 요구가 생기면 |
| 마이그레이션 협력 | [open-decisions.md](open-decisions.md) 9번 결정 후 |

---

## 7. 운영 준비 항목

데모 환경 구성 시 함께 처리한다.

- **S3 버킷에 미완료 multipart 자동 중단 lifecycle 규칙.** 중단된 백업이 보이지 않는
  과금을 남긴다 ([data-model.md](data-model.md) 2.2)
- PostgreSQL 컨테이너 (docker-compose)
- 사내 MinIO presigned URL로 에이전트·system-rescue 배포
  ([Restore.md](Restore.md) 2.1)
- 데모용 AMI와 인스턴스 타입 고정
- **`setup-sysres.sh`를 GitHub Releases에 태그로 게시.** user-data는 그 태그 URL을
  고정해서 참조한다(`latest` 금지)
- **레스큐 이미지 버전 관리.** system-rescue 이미지를 바꿀 때마다 URL 대상 객체와
  **SHA-256을 한 쌍으로** 갱신해야 한다. 프레임워크가 user-data에 둘을 함께 넣으므로,
  이미지 버전별 해시를 프레임워크가 알고 있어야 한다. 해시는
  `sha256sum sysres.tar`로 얻고, 틀리면 스크립트가 실제 해시를 출력하며 중단한다
- ~~`sh/setup-sysres.sh`의 기본 `SYSRES_URL` 제거~~ — **완료.** 기본값을 비웠다.
  `sh/aiha-init.sh`의 값들은 자리표시자만 둔다(프레임워크가 채운다)
