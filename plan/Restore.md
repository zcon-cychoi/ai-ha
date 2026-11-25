# Restore

복구 시퀀스, 보안 계약, 재조정 항목을 정의한다.

관련 문서: [architecture.md](architecture.md), [protocol.md](protocol.md),
[data-model.md](data-model.md), [backup.md](backup.md),
[roadmap.md](roadmap.md), [open-decisions.md](open-decisions.md)

---

## 0. 검증 현황

**복구 메커니즘 자체는 수동으로 검증되었다.**

| 항목 | 상태 |
| --- | --- |
| system-rescue + `copytoram` 부팅 (AWS Ubuntu) | ✅ 수동 성공 |
| 백업 데이터로 OS 복구 후 부팅 | ✅ 수동 성공 |
| GCP Ubuntu → AWS 교차 클라우드 복구 (부팅 + SSH 로그인) | ✅ 수동 성공 |
| user-data(cloud-init)를 통한 레스큐 진입 자동화 (Ubuntu 24.04) | ✅ 성공 (2026-09-14) — `sh/setup-sysres.sh` |
| 레스큐 이미지 SHA-256 검증 | ✅ 구현 — 실인스턴스 미실행 |
| 복구 정보(`recoveryId` 등) 레스큐 전달 → 프레임워크 claim | ✅ 구현·로컬 E2E 검증 (2026-09-15) — `sh/aiha-init.sh`가 `/autorun/aiha`에 설치 → 레스큐 부팅 시 `copytoram` → 이미지의 `autorun2`(`sh/autorun2`)가 `aiha-rescue rescue` 실행. EC2 실환경 확인 (2026-10-12) |
| 복구 절차의 코드 자동화 | ✅ 구현 (2026-10-08) — `pkg/agent/restore`, 3.8절 방침. **EC2 실환경 복구 후 수동 재부팅으로 부팅 확인 (2026-10-12)**. 자동 재부팅·boot-verify는 아직 (8절) |

`copytoram`은 선택이 아니라 **필수**다. 레스큐가 올라온 디스크가 곧 포맷 대상이므로,
레스큐 시스템이 RAM에 완전히 올라가 있지 않으면 포맷하는 순간 실행 중인 시스템이
사라진다.

user-data를 통한 레스큐 진입까지 검증되어 **기술적 미지수는 남아 있지 않다.** 남은
복구 정보 전달과 절차 자동화는 구현 과제다([roadmap.md](roadmap.md) Phase 0 결과).

---

## 1. 두 가지 진입 경로

복구에는 성격이 다른 두 모드가 있다. 초기 문서가 이 둘을 한 절차로 섞어 기술했으나,
목적과 전제가 다르므로 분리한다.

| | **Mode A — 신규 인스턴스 복구** | **Mode B — 기존 VM 제자리 복구** |
| --- | --- | --- |
| 상황 | 원본 VM이 사라졌거나 다른 환경에 복구 | 살아 있는 VM을 이전 시점으로 되돌림 |
| 대상 인스턴스 | 프레임워크가 새로 생성 | 이미 존재 |
| 레스큐 진입 | user-data 초기화 스크립트 | 실행 중 OS에서 grub 1회 부팅 설정 |
| 디스크 | 비어 있음 | 기존 데이터를 덮어씀 |
| 마이그레이션 관계 | **목표 클라우드가 다르면 그것이 곧 인프라 마이그레이션** | 없음 |

2026년 1차 구현 대상은 [open-decisions.md](open-decisions.md) 1번에서 정한다.

---

## 2. Mode A — 신규 인스턴스 복구

### 2.1 시나리오

1. **사용자가 복구를 요청한다.** 백업본 선택 + 복구 지점(목표 환경) 지정.
   현재는 소스·타겟 모두 AWS 한정. GCP, Azure 등은 이후 추가.
2. **AI-HA 프레임워크가 복구 잡을 생성한다.**
   1. `restoreId` 및 `recoveryId` 생성 (분리하는 이유는 [protocol.md](protocol.md) 4.2)
   2. 복구할 백업 항목 목록
   3. **`targetLayoutSpec`** — 논리 참조만 담은 선언적 레이아웃 명세
      ([data-model.md](data-model.md) 4.4). **구체적 디바이스 경로를 담지 않는다**
   4. `sourcePlatform` / `targetPlatform`에서 도출한 `driverStrategy`
   5. 백업 저장소 연결 정보 (**단기 스코프 자격증명**, 5절)
   6. AI-HA 프레임워크 서버 주소
   7. 백업 대상 OS 정보
   8. 1회용 클레임 토큰 및 부팅 확인 토큰
   9. Tier 판정 ([data-model.md](data-model.md) 4.7). Tier 3이면 여기서 거부한다
3. **인프라 프레임워크를 통해 새 인스턴스를 생성한다.**
   원본과 동일 사양, **디스크는 원본 이상 크기**([data-model.md](data-model.md) 4.8).
   EC2 User Data(다른 퍼블릭 클라우드는 호환 기능)로 초기 설정:
   1. AI-HA 에이전트 및 system-rescue 다운로드
      (테스트 환경: 사내 MinIO에서 presigned URL 생성해 제공)
   2. system-rescue로 부팅하기 위한 grub 설정
   3. **`recoveryId`와 프레임워크 주소를 기입한 파일을 저장** (프레임워크가 생성)

   **구현 현황**: `sh/setup-sysres.sh`가 3-1(system-rescue 부분)과 3-2를 수행하며,
   Ubuntu 24.04에서 user-data로 레스큐 진입까지 확인되었다. 입력은 환경변수다.

   | 변수 | 내용 | 제공 주체 |
   | --- | --- | --- |
   | `SYSRES_URL` | sysres.tar 다운로드 URL (presigned) | 프레임워크가 복구 잡마다 생성 |
   | `SYSRES_SHA256` | sysres.tar의 SHA-256. **필수** — 없거나 불일치하면 중단 | 프레임워크가 이미지 버전별로 보관 |
   | `SYSRES_REBOOT` | `1`이면 완료 후 재부팅(`--reboot`과 같음). 기본 `0`, 그 외 값은 중단 | 프레임워크가 `1`로 설정 |

   - 초기 검증은 스크립트 끝에 강제 `reboot`을 붙여 수행했다. `SYSRES_REBOOT=1`은
     setup-sysres.sh를 단독으로 쓸 때의 방법이고, aiha-init.sh는 재부팅 없이 호출한 뒤 `/autorun`
     파일을 설치하고 직접 재부팅한다(아래).
   - `grub-reboot`(1회 부팅)을 쓰므로 레스큐 부팅이 실패해도 다음 재부팅에서 원래
     OS로 돌아온다. 4.1의 6번 원칙과 같다.
   - 배포는 **GitHub Releases + `curl ... | bash`** 로 한다. 조건과 신뢰 모델,
     user-data 예시는 5.2.
   - 3-1(에이전트 전달)과 3-3(복구 정보 저장)은 `sh/aiha-init.sh`가 **sysres.tar 해제 뒤에**
     `/autorun/aiha/`에 `aiha-rescue`와 `recovery.env`를 설치해서 해결한다. 에이전트를 실행하는
     `/autorun/autorun2`는 인스턴스마다 바뀌지 않으므로 **이미지(sysres.tar)에 넣어 둔다** —
     원본은 `sh/autorun2`다(2026-10-12). 해제 뒤에 설치하는 것은 tar가 같은 이름의 파일을
     덮어쓰기 때문이고, 그래서 `setup-sysres.sh`는 재부팅 없이 호출하고 설치 후
     aiha-init.sh가 재부팅한다. 이미지에 `autorun2`가 없으면 aiha-init.sh가 재부팅 전에
     멈춘다. 레스큐 부팅 시 `copytoram`이 이 디렉터리를 램으로 올리고 SystemRescue가
     `autorun2`를 자동 실행하므로, **복구 대상 디스크를 마운트하지 않는다.**
4. **system-rescue로 부팅한다.**
5. **레스큐 에이전트가 프레임워크에 복구 준비 완료를 신고한다.**
   3번에서 주입된 `recoveryId`와 클레임 토큰으로 저장소 정보 및 복구 플랜을 요청한다
   (`POST /aiha/agent/restore/{recoveryId}/claim`).
6. **복구를 수행한다** (3절).
7. **재부팅한다.**
8. **복구된 OS가 부팅에 성공하면 부팅 확인을 보고한다** (4절).

### 2.2 잡 상태와의 대응

[protocol.md](protocol.md) 3.2의 상태머신과 1:1로 대응한다.

| 시나리오 단계 | 잡 상태 |
| --- | --- |
| 2 | `queued` |
| 3 | `provisioning` |
| 4 | `awaiting_agent` |
| 5~6 | `restoring` |
| 7 | `awaiting_boot` |
| 8 | `completed` |

**현재 구현**: 2·3단계(`queued`·`provisioning`)가 없다 — 인스턴스는 aiha-init.sh 값으로 수동
생성하고, `restore create --backup-set`이 잡을 곧바로 `awaiting_agent`로 만든다. 레스큐가 claim하면
`claimed`, 첫 진행 보고에 `restoring`, 성공하면 `awaiting_boot`에서 멈춘다(8단계 미구현).

모든 대기 상태에 타임아웃이 있다. **VM이 레스큐로 뜨지 않거나 복구된 OS가 부팅에
실패하면 콜백이 영영 오지 않으므로, 타임아웃이 없으면 잡이 영구 대기한다.**

---

## 3. 복구 수행 절차

Mode A와 Mode B 공통이다. 레스큐 환경에서 실행된다.

### 3.1 준비와 검증

1. 복구 정보 파일에서 `recoveryId`·프레임워크 주소를 읽고 플랜을 수령한다.
2. 저장소에 연결한다 (전달받은 단기 자격증명 사용).
3. **실제 블록 디바이스를 열거하고 플랜과 대조한다.** 레스큐 미디어 자신과 복구 정보
   파일이 든 디스크는 제외한다.
4. **실행 가능성을 검증한다** ([data-model.md](data-model.md) 4.6).
   - 디스크 개수와 크기가 충분한가 (`usedBytes` + 오버헤드 + 여유)
   - 아키텍처·펌웨어가 호환되는가
   - 필요한 도구(`mkfs.*`, `lvm`, `grub2-install`, `dracut`/`update-initramfs`)가 있는가

   **하나라도 어긋나면 포맷하지 않고 중단하고 `plan_mismatch`로 재계획을 요청한다.**
   정책과 계획은 중앙에서 정하되, 실제 디스크와 도구의 존재 여부는 에이전트가
   확인해야 한다.

### 3.2 레이아웃 생성

5. 파티션 테이블·파티션·LVM·파일시스템을 플랜대로 생성한다.
   **원본 `fsUUID`와 VG/LV 이름을 보존한다** ([data-model.md](data-model.md) 4.5).
   이것으로 복구된 `fstab`과 grub의 `UUID=` 참조가 그대로 동작하므로 재조정 작업이
   크게 줄어든다.
6. `mountOrder`대로 루트 트리에 마운트한다.

### 3.3 데이터 복구

7. 각 백업 항목을 지정된 마운트 지점에 압축 해제한다.
8. 체크섬을 검증한다.

**system-rescue에는 rclone이 설치되어 있으므로 복구 경로에서
`pkg/backup/repository`가 불필요하다.** 저장소에서 읽어 실시간으로 해체할 수 있다.

```
rclone cat <repo>:<path>/<item>.aha | zstd -d | tar -x -C /mnt/target
```

에이전트는 이를 오케스트레이션한다. S3/SFTP/SMB 백엔드를 레스큐용으로 다시 구현하지
않는다. 백업 경로는 기존 Go 스트리밍·재개 전송(`pkg/backup/spool`)이 더 낫기 때문에
그대로 두며, **백업(Go)과 복구(rclone)의 이 비대칭은 의도된 지름길이다.** 나중에
rclone 없이 진행하도록 바꿀 수 있으나 동작에는 영향이 없다
([roadmap.md](roadmap.md) 6절).

### 3.4 재조정

9. 6절의 재조정 항목을 적용한다.

### 3.5 부트로더

10. grub 설치 및 `grub.cfg` 업데이트 (chroot).
    `driverStrategy`에 따라 BIOS/UEFI 설치 방식을 선택한다.
11. initramfs 리빌드 (chroot).
    **`driverStrategy`가 지정한 모듈을 강제 포함한다** — 하이퍼바이저가 바뀌면
    (예: Xen → Nitro) `nvme`·`ena`가 없어 루트 디스크를 못 찾고 커널 패닉이 난다.

### 3.6 마무리

12. **원본 에이전트 자격증명을 제거하거나 비활성화한다** (5.4절). 이 단계를 빠뜨리면
    복구된 OS가 원본 시스템의 백업 잡을 가로챌 수 있다.
13. `recoveryId`, 복구 완료 시각, **부팅 확인 토큰**, 해석된 실제 매핑을 기록한 JSON
    파일을 복구한 OS 디스크에 저장한다.
14. 복구 완료를 프레임워크에 보고하고 **성공 응답을 확인한 뒤** 재부팅한다.
    응답 확인 없이 재부팅하면 프레임워크는 통신 두절로 판단한다.

### 3.7 중단 가능 지점

취소([protocol.md](protocol.md) 2.6)는 단계에 따라 처리가 다르다.

| 단계 | 중단 가능성 | 중단 후 디스크 상태 |
| --- | --- | --- |
| 3.1 준비·검증 | 안전하게 중단 가능 | 변경 없음 |
| 3.2 레이아웃 생성 이후 | **원래 상태로 돌아가지 않는다** | 포맷됨. Mode B는 원본 데이터 소실 |
| 3.3~3.5 | 중단 가능하나 부팅 불가 상태 | 불완전 |
| 3.6 이후 | 중단 무의미 | 완료됨 |

**3.2 이후의 취소는 "복구를 되돌린다"가 아니라 "여기서 멈춘다"** 이며, 사용자에게 그
사실을 명확히 알려야 한다.

---

### 3.8 1차년도 구현 방침 (2026-09-17 확정)

3.1~3.6의 일반 절차 중 1차년도에 실제로 구현할 형태를 좁혀 둔다. 소스와 타깃이 모두
**동일한 단순 구성의 VM 이미지**라는 전제([roadmap.md](roadmap.md) 3.1)에서 나온
결정이다.

**디스크 구성 재현은 하지 않는다.** 파티션 테이블을 복제하는 대신 **마운트 지점을
매칭**한다. 타깃의 파티션은 원본 파티션의 **PARTUUID로 찾는다(2026-10-12)** — 장치 이름은 NVMe
인식 순서에 따라 인스턴스마다 다르다(실환경에서 원본 `nvme1n1`, 타깃 `nvme0n1`을 겪었다). 같은
이미지의 파티션 테이블은 PARTUUID가 같고, 나중에 원본의 `sfdisk --dump`로 테이블을 복구하는
경우에도 GPT는 파티션 `uuid=`, MBR은 디스크 `label-id`(PARTUUID = `<label-id>-<번호>`)가 dump에
들어 있어 유지된다. LVM의 LV는 파티션이 아니므로 VG/LV 이름으로 맞춰야 한다(범위 밖). 소스와 타깃 구성이 같으므로 테이블 복제의 실익이 없다. 매니페스트의
`items[].mountPoint`가 이를 위해 기록된다.

> 그래도 파티션 테이블·UUID·LVM은 **수집해 둔다.** 복구 시점에는 원본이 이미 없을 수
> 있어 나중에 다시 뜰 수 없기 때문이다. 수집과 사용은 별개다.

마운트 지점을 판별하지 못하는 경우의 보조 수단: 마운트된 디스크의 **디렉터리 형상**을
보고 `/`, `/usr`, `/var`, `/boot`, `/boot/efi`를 구분한다.

**하드웨어 고정 정보 제거** — 이전 시스템의 식별자가 남아 있으면 부팅하지 못한다.

| 대상 | 처리 |
| --- | --- |
| `/etc/default/grub` | `CMDLINE`에서 이전 하드웨어 정보(UUID, LVM 참조) 제거 |
| `blscfg` | **false로 고정.** 복구·마이그레이션에서 방해된다 |
| `/etc/default/grub.d/` | 존재하면 하드웨어 고정 값이 든 파일을 삭제하거나 해당 값만 제거 |
| `/etc/initramfs-tools/conf.d/` | 같은 이유로 파일 삭제 |
| `/etc/fstab` | 현재 디스크의 식별자에 맞게 수정 |

**네트워크는 MAC 주소만 치환한다 (2026-10-08 변경).** 복구된 `/etc/netplan/*`에서
원본 MAC을 타깃 NIC의 MAC으로 바꾼다. 원본 netplan이 매칭하는 MAC이 1개이고 타깃의
물리 NIC가 1개일 때만 결정할 수 있으므로, 그 외에는 **포맷 전에** 중단한다. (처음에는
타깃 OS의 netplan을 포맷 전에 추출해 대체하는 방식이었으나, 같은 이미지 전제에서는
MAC 외에 다른 점이 없어 치환으로 단순화했다.)

**파일시스템은 원본 UUID와 label로 만든다 (2026-10-08 확정).** `mkfs`에 원본 UUID를
준다(ext4 `-U`, xfs `-m uuid=`, vfat `-i`). 포맷 직후 `blkid`로 읽어 원본과 다르면 실패로
중단한다 — 이후 단계 전부가 이 전제 위에 있다. 파티션 테이블도 건드리지 않으므로
PARTUUID도 원본과 같다. 따라서 복구된 설정의 UUID 참조는 **고치지 않고 검사만** 한다.

| 대상 | 검사 (불일치 시 경고, 파일은 그대로) |
| --- | --- |
| `/etc/fstab` | 복구한 마운트 지점의 `UUID=`가 그 파일시스템의 UUID인가 |
| `/etc/default/grub`, `/etc/default/grub.d/*.cfg` | 고정된 UUID/PARTUUID(`GRUB_FORCE_PARTUUID`, `root=UUID=`, `/dev/disk/by-(part)uuid/` 등)가 이 머신에 있는가 |
| ESP의 `EFI/*/grub.cfg` | grub stub(`configfile` 있고 `menuentry` 없음)의 `search` UUID가 부트 파티션(`/boot`, 없으면 `/`)의 것인가 |

불일치는 **경고만 남기고 계속한다**(사용자 결정). 경고는 레스큐 로그, CLI 출력, 결과
보고 메시지에 남는다. UUID를 고치는 코드(`rewriteFstab`, `rewriteEFIStub`,
`cleanGrubDefaults`)는 **원본 UUID를 쓸 수 없는 복구 — 마이그레이션 등 — 를 위해 남겨
두었다.** 지금은 앞의 둘을 "고칠 것이 있으면 값이 틀린 것"이라는 검사로 쓰고,
`cleanGrubDefaults`는 호출하지 않는다. (같은 날 처음 구현에서는 새 UUID로 포맷하고 이
파일들을 고쳤으나, 같은 이미지 전제에서는 원본 UUID 보존이 더 단순하고 확실해 되돌렸다.)

`blscfg`, `/etc/initramfs-tools/conf.d/` 정리는 Ubuntu 24.04 고정 시나리오에서 필요가
확인되지 않아 아직 구현하지 않았다.

**마무리**: 모든 파일 수정이 끝나면 chroot 후
`grub-install` → `update-grub` → `update-initramfs -u -k all`.

**복구 표식 파일**: 복구 실행기는 복구된 디스크에
`/var/lib/aiha/recovery-pending.json`을 남긴다.

```json
{ "recoveryId": "...", "restoredAt": "...", "bootVerifyToken": "..." }
```

복구된 OS의 에이전트는 기동 시 이 파일이 있으면 **백업과 잡 수신을 거부**한다
(5.4의 자격 격리 규칙). 읽고 거부하는 쪽은 2026-09-17에 구현되었고
(`pkg/agent/state`, `aiha-agent backup`의 `PreRunE`), 쓰는 쪽은 복구 실행기와 함께
2026-10-08에 구현되었다. `bootVerifyToken`은 boot-verify가 생길 때까지 비어 있다.
`bootVerifyToken`은 비밀이므로 로그에 남기지 않는다.

---

## 4. Mode B — 기존 VM 제자리 복구

이미 실행 중인 OS를 레스큐로 1회 부팅시켜 복구한다. Mode A의 3번(인스턴스 생성)이
없는 대신 **레스큐 진입 준비**가 대상 VM 안에서 이뤄진다.

### 4.1 레스큐 진입 준비 절차

실행 중인 OS의 에이전트가 수행한다.

1. system-rescue 파일을 OS 루트 파티션에 추출한다.
2. `/etc/grub.d/`에 시스템에 맞는 menuentry custom 파일을 추가한다.
   대상 커널과 initramfs는 system-rescue의 것이다.
3. `grub.cfg`를 갱신한다 (`grub2-mkconfig -o ...` 또는 `update-grub`).
4. 프레임워크에서 전달받은 정보로 복구 정보 파일을 생성한다.
5. 에이전트 및 복구 정보 파일을 system-rescue에서 읽을 수 있는 위치로 옮긴다.
6. `grub-reboot`으로 **1회 부팅**을 예약한다.
   영구 기본값으로 바꾸지 않는다 — 복구 실패 시 재부팅하면 원래 OS로 돌아와야 한다.
7. 재부팅한다.

이후는 3절과 동일하다.

### 4.2 Mode B 고유의 위험

- **되돌릴 수 없다.** 3.2에서 포맷하는 순간 기존 데이터가 사라진다. 복구할 백업본이
  손상되어 있으면 원본도 백업본도 없는 상태가 된다. **복구 시작 전에 백업본 체크섬을
  검증**하고, 사용자에게 명시적으로 경고한다.
- **레스큐 부팅 실패 시 접근 불가.** grub 설정이 잘못되면 어느 쪽으로도 부팅하지
  못한다. 클라우드 콘솔의 시리얼 콘솔 외에 복구 수단이 없다.

---

## 5. 보안 계약

### 5.1 복구ID는 그 자체로 비밀이 아니다

**EC2 user-data는 인스턴스 내 아무 프로세스나 IMDS로 읽을 수 있다**
(`http://169.254.169.254/latest/user-data`). IMDSv2는 토큰을 요구하지만 인스턴스
안에서는 여전히 획득 가능하다.

따라서 **`recoveryId`만으로 인증해 백업 저장소 연결 정보(자격증명 포함)를 내주면
자격증명 유출 경로가 된다.** 복구용 인스턴스에서 돌아가는 다른 무엇이든 그 값을 읽어
백업 저장소 전체에 접근할 수 있다.

### 5.2 완화책

| 항목 | 규칙 |
| --- | --- |
| 식별자와 비밀 분리 | `recoveryId`(식별자)와 클레임 토큰(비밀)을 나눈다 |
| 토큰 수명 | 짧은 TTL. 복구 잡의 `awaiting_agent` 타임아웃 내로 제한 |
| 토큰 사용 | 1회용. 클레임 성공 시 즉시 소인 |
| 인스턴스 검증 | 클레임 시 인스턴스 아이덴티티 검증 — EC2 instance identity document 서명 검증, 또는 프레임워크가 생성 시 기록해 둔 `instanceId` 대조 |
| 저장소 자격증명 | **장기 키가 아니라 단기 스코프 자격증명.** STS AssumeRole 등으로 해당 백업 세트 prefix에 대한 **읽기 전용** 권한만 부여 |
| **레스큐 이미지 무결성** | sysres.tar를 **SHA-256으로 검증한 뒤에만** 읽고 추출한다(`sh/setup-sysres.sh`). presigned 서명은 서버 쪽 요청 인증일 뿐 **응답 바이트의 무결성은 보장하지 않으므로**, 평문 HTTP에서 이 검증이 없으면 중간자가 이미지를 바꿔치기할 수 있다 |
| 자격증명 폐기 | 복구 잡 종료 시 폐기. 프레임워크 DB에 장기 보관하지 않는다 |

**현재 구현 (2026-10-12)**: 아직 단기 자격증명이 아니다. 프레임워크에 등록된 저장소의 키
(백업에도 쓰는 쓰기 가능한 키)를 claim 응답으로 전달한다. 프레임워크 DB에는 암호화해서만 두고
(`pkg/framework/secret`), 복구 잡에는 저장소 ID만 기록한다. 복구 전용 읽기 키·STS 발급은 남은 과제다.

단기 자격증명 경로는 이미 열려 있다 — `config/config.go`의 `S3Config`에
`SessionToken` 필드가 있고, `pkg/backup/repository/s3.go`가 static 키가 비어 있으면
표준 자격증명 체인을 탄다.

#### 레스큐 이미지가 위조되면 무엇을 잃는가

레스큐 이미지는 부팅 후 복구 잡을 claim하고 **저장소 자격증명을 받는** 주체다.
바꿔치기된 이미지가 부팅하면 정상 레스큐와 똑같이 claim해서 자격증명을 가져간다.
즉 이미지 무결성은 편의 기능이 아니라 **5.2의 다른 모든 통제가 전제로 삼는 기반**이다.

#### 스크립트 배포 방식과 신뢰 모델

스크립트는 **GitHub Releases에 올리고, user-data에서 `curl ... | bash`로 받아
실행한다.** 스크립트 자체의 체크섬은 고정하지 않는다.

이것이 괜찮은 이유는 **두 다운로드의 전송 보안이 다르기 때문**이다.

| 대상 | 경로 | 무결성 근거 |
| --- | --- | --- |
| `setup-sysres.sh` | GitHub Releases (HTTPS) | TLS — 전송 중 변조 불가 |
| `sysres.tar` | presigned URL (HTTP일 수 있음) | `SYSRES_SHA256` — user-data로 전달된 해시 |

TLS로 받은 정품 스크립트가 tar를 해시로 검증하므로 **네트워크 중간자는 어느 경로로도
끼어들 수 없다.** 스크립트 체크섬을 빼도 tar 체크섬은 여전히 의미가 있다.

**남는 위험**은 릴리스 자산 자체가 바뀌는 경우(저장소 쓰기 권한 탈취)다. 네트워크
공격이 아니라 공급망 문제이며 1차년도 범위에서는 수용한다. 없애야 할 시점이 오면
스크립트 해시를 user-data에 고정하면 된다.

`curl ... | bash`를 쓰기 위한 조건:

| 조건 | 이유 |
| --- | --- |
| `-f` | 없으면 404 오류 페이지(HTML)가 그대로 bash에 들어간다 |
| `--proto '=https' --proto-redir '=https'` | HTTPS 강제. GitHub 릴리스는 다른 도메인으로 리다이렉트하므로 리다이렉트도 막아야 한다 |
| **태그 고정 URL** (`/releases/download/v1.0.0/...`) | `latest`를 쓰면 자산이 바뀔 때 조용히 다른 스크립트가 실행되고, 과거 복구를 재현할 수 없다 |
| **스크립트 전체를 `main()`으로 감싸고 마지막 줄에서 호출** | bash는 받는 대로 실행하므로 **다운로드가 끊기면 잘린 스크립트가 실행된다.** 감싸 두면 잘린 파일은 닫히지 않은 함수라 문법 오류로 아무것도 실행되지 않는다. 스크립트 안의 명령이 stdin을 읽어 나머지 스크립트를 삼키는 문제도 함께 막힌다. `sh/setup-sysres.sh`에 적용됨 |
| user-data 쪽 `set -o pipefail` | 없으면 curl이 실패해도 bash가 빈 입력으로 정상 종료해 실패가 cloud-init 로그에 드러나지 않는다 |

user-data 예:

```bash
#!/bin/bash
set -euo pipefail
curl -fsSL --proto '=https' --proto-redir '=https' \
  https://github.com/<org>/<repo>/releases/download/<tag>/setup-sysres.sh |
  SYSRES_URL='<presigned URL>' SYSRES_SHA256='<64 hex>' SYSRES_REBOOT=1 bash
```

인자는 `bash -s -- --reboot`로도 넘길 수 있으나, 프레임워크가 모든 값을 환경변수로
렌더링하도록 `SYSRES_REBOOT=1`을 쓴다.

### 5.3 클레임의 멱등성

**"1회용 토큰이므로 자연히 1회"는 멱등성 보장이 아니다.** 서버가 토큰을 소인한 직후
응답이 유실되면 에이전트는 플랜을 받지 못한 채 재요청도 거부되어 영구 대기한다.

계약([protocol.md](protocol.md) 2.5):

- 최초 성공 시 **토큰 소인, 실행 시도 생성, 응답 정보 저장을 원자적으로** 처리한다.
- 동일 주체·동일 요청 키·동일 내용의 재요청에는 **저장된 기존 응답을 반환**한다.
- 다른 내용이거나 다른 주체의 재사용은 거부한다.

M1 구현은 응답을 저장하지 않고 잡 데이터로 다시 만든다. claim 이후 변경 금지와 재수신 기한은
[protocol.md](protocol.md) 2.5의 "M1 구현"을 따른다.

### 5.4 복구된 OS의 자격 격리

복구된 OS는 백업 이미지에서 왔으므로 **원본의 `systemId`, `agentId`, 에이전트
자격증명을 전부 복제해서 갖는다.** 원본 VM이 살아 있으면 같은 자격을 가진 두
에이전트가 존재한다.

부팅 확인 토큰만으로는 부족하다. **복구된 OS가 원본 자격증명으로 먼저 롱폴링을
시작하면 부팅 확인 전에 원본의 백업 잡을 가져갈 수 있다.** 순서를 규칙으로 정한다.

1. **레스큐 단계(3.6의 12번)에서 복구 대상 디스크의 원본 에이전트 자격증명을
   제거·비활성화한다.**
2. 복구 후 최초 실행은 **일반 작업 수신이 차단된 "복구 등록 모드"** 로 시작한다.
3. 부팅 확인과 새 등록 또는 승계 처리를 수행한다.
4. **유효한 새 자격을 받은 뒤에야 일반 작업 수신을 허용한다.**

신규 등록과 원본 승계 중 무엇을 택할지는 [open-decisions.md](open-decisions.md) 3번이
정하지만, **그동안 일반 작업 수신을 차단한다는 규칙은 확정 사항이다.**

### 5.5 레스큐 환경의 자격증명 배치

복구 경로는 rclone을 쓰므로 저장소 자격증명이 **설정 파일이나 환경변수로 레스큐
환경에 놓인다.** 배치 위치를 규칙으로 정한다.

| 규칙 | 이유 |
| --- | --- |
| **tmpfs 경로에만 쓴다** | `copytoram`이므로 레스큐 파일시스템은 RAM에만 존재한다. 재부팅과 함께 사라진다 |
| **복구 대상 디스크에 절대 쓰지 않는다** | 실수로 남기면 복구된 OS에 자격증명이 평문으로 남아 그대로 유출된다 |
| 파일 권한 `0600` | 레스큐 환경의 다른 프로세스로부터 보호 |
| 잡 종료 시 삭제 | RAM에서 사라지더라도 명시적으로 지운다 |

`copytoram`이 보안상 부수 효과를 주는 셈이지만, **그것에 의존하지 말고 위 규칙을
명시적으로 지킨다.** 3.6의 13번에서 JSON 파일을 복구 대상 디스크에 기록하므로, 그
경로와 자격증명 경로를 혼동하지 않도록 주의한다.

### 5.6 로깅 금지 항목

CLAUDE.md의 [Logging] 규칙에 따라 아래를 절대 로그에 남기지 않는다.

- 저장소 자격증명 (액세스 키, 시크릿, 세션 토큰, SSH 키, SMB 비밀번호)
- 복구 클레임 토큰, 부팅 확인 토큰
- 자격증명이 포함된 저장소 URI

복구 경로는 저장소 접속 정보를 에이전트에 중개하므로, 요청·응답 본문을 통째로 남기는
미들웨어가 이 엔드포인트에 걸리지 않도록 한다. 현재
`pkg/api/rest/server.go`의 `requestLogger()`는 메서드·URI·상태·지연만 기록하므로 이
성질을 유지한다.

---

## 6. 재조정 항목 (reconciliation set)

새 인스턴스에서 깨지는 것은 NIC 설정만이 아니다. **원본 `fsUUID`를 보존하면
([data-model.md](data-model.md) 4.5) 상당수가 자동 해소되므로, 재조정은 "전부 다시
쓴다"가 아니라 "보존하지 못한 것만 고친다"** 가 된다.

| 항목 | 문제 | 처리 |
| --- | --- | --- |
| 파일시스템 UUID | fstab·grub·crypttab이 참조 | **원본 UUID로 재생성해 회피** |
| LVM VG/LV 이름 | `/dev/mapper/vg-lv` 참조 | **이름 보존으로 회피** |
| `/etc/fstab` | 디바이스 경로 직접 참조 시 깨짐 | UUID 참조로 전환. 보존 못 한 항목만 수정 |
| ESP (vfat) 볼륨 ID | 32비트 형식이라 UUID 보존 방식이 다름 | fstab의 `/boot/efi` 항목 수정 |
| NIC 설정 | MAC·인터페이스 이름이 달라짐 | **새 인스턴스의 NIC 설정을 추가 디스크에 백업해 두었다가 복구한 OS에 적용** |
| udev net rules | `70-persistent-net.rules`가 옛 MAC 고정 | 제거 |
| `/etc/machine-id` | systemd 식별자 중복 | 정책에 따라 재생성 ([open-decisions.md](open-decisions.md) 4번) |
| SSH host keys | 재사용 시 원본과 중복, 재생성 시 클라이언트 경고 | 정책 결정 필요 (동 4번) |
| **cloud-init 상태** | **재실행 시 위에서 적용한 NIC 설정을 덮어쓸 수 있다** | `cloud-init clean` 또는 비활성화 (동 5번) |
| cloud-init 데이터소스 | 프로바이더가 바뀌면 엉뚱한 소스를 찾아 부팅 지연 | `datasource_list` 조정 |
| 게스트 에이전트 | `vmware-tools`가 AWS에서 무의미하게 실행 | 목표 환경의 것으로 교체 |
| initramfs 모듈 | 하이퍼바이저 변경 시 **부팅 불가** | `driverStrategy`대로 강제 포함 (3.5) |
| 부트로더 | BIOS ↔ UEFI 전환 | ESP 생성 + `efibootmgr` 등록 |
| 에이전트 자격증명 | 원본 자격 복제 | 제거·비활성화 (5.4) |

**user-data 재실행 (2026-10-12, 실환경 확인)**: user-data는 인스턴스에 붙어 있어 복구된 OS가 부팅해도
IMDS가 같은 aiha-init.sh를 내준다. 복구된 OS의 cloud-init은 원본 인스턴스의 instance-id를 기록하고 있어
타깃을 새 인스턴스로 보고 이를 다시 실행했고, 그 결과 레스큐로 다시 부팅했다(claim은 409로 거절돼 재포맷은
없었다). aiha-init.sh가 복구 표식이 있으면 아무것도 하지 않고 끝나도록 막았다. 인스턴스 생성을 프레임워크가
맡게 되면 복구 완료 후 user-data를 지우는 것이 근본 처리다.

**cloud-init 항목이 특히 중요하다.** NIC 설정을 정성껏 복구해 두어도 첫 부팅에
cloud-init이 재실행되면서 덮어쓰면 무의미해진다. 재조정 순서상 cloud-init 처리가
NIC 적용보다 먼저이거나, 최소한 함께 고려되어야 한다.

---

## 7. 복구 완료 판정

### 7.1 실행 결과와 확인 결과를 분리한다

**리스 만료로 연락이 끊긴 것은 복구 실패가 확인된 것이 아니다.** 디스크는 이미
포맷되었을 수 있으므로 "실패했으니 다시 시도하세요"와는 다른 안내가 필요하다.

| 상황 | `restoreOutcome` | `bootVerification` |
| --- | --- | --- |
| 에이전트가 복구 오류 보고 | `failed` | `n/a` |
| 복구 중 통신 두절 | **`unknown`** | `n/a` |
| 디스크 복구 성공, 부팅 확인 대기 | `succeeded` | `pending` |
| 부팅 확인 완료 | `succeeded` | `verified` |
| 부팅 확인 시간 초과 | `succeeded` | `timed_out` |

`bootVerification=timed_out`일 때 잡을 `failed`로 볼지 별도 종결 상태로 볼지는
[open-decisions.md](open-decisions.md) 3번과 함께 정한다.

### 7.2 부팅 확인의 주체와 인증

**복구된 OS의 부팅 확인을 일반 에이전트의 온라인 전환으로 판정해서는 안 된다**(5.4절).

복구된 OS 안의 에이전트가 3.6의 13번에서 기록된 JSON 파일을 발견하고, 거기 담긴
**1회용 부팅 확인 토큰**으로 `POST /aiha/agent/restore/{recoveryId}/boot-verify`를
호출한다. 그 토큰을 제시한 주체만이 "이 복구 잡이 만든 디스크로 부팅한 것"이다.

다만 토큰은 **"그 토큰을 가진 주체"임을 증명할 뿐**이므로, 토큰을 `recoveryId`·복구
실행 시도·예상 대상 인스턴스와 연결해 저장하고 만료·사용 처리 규칙을 함께 둔다.

### 7.3 미해결: 원본에 에이전트가 없었던 경우

복구된 OS는 백업 이미지에서 오므로, **원본에 에이전트가 설치되어 있지 않았다면 복구된
OS에도 없다.** 이 경우 부팅 확인을 보고할 주체가 없다.

가능한 처리는 [open-decisions.md](open-decisions.md) 3번에서 함께 정한다.
현재 백업 경로는 에이전트가 수행하므로 원본에 에이전트가 있는 것이 정상이지만,
외부 도구로 만든 백업본을 가져오는 경우를 고려하면 완결되지 않은 부분이다.

---

## 8. 구현 현황

**복구 실행기 구현 (2026-10-08)**: `pkg/agent/restore`가 3.8절 방침대로 복구를
수행한다. `aiha-rescue rescue`가 claim 직후 실행하고, `aiha-rescue restore`로 claim 없이
다시 실행할 수 있다.

1. `preparing` — `rclone cat`으로 `<systemId>/<setId>-manifest.json`을 읽고 검증한다.
   매니페스트 버전·세트/시스템 ID 일치, 세트와 모든 항목의 `completed`, 지원 코덱,
   `/` 항목 존재, 필요한 도구, **PARTUUID로 타깃 파티션 찾기**, 각 장치가 블록 장치이고
   마운트되지 않았는지, NIC 매칭, UEFI면 `/boot/efi` 항목. **여기서 실패하면 디스크는 바뀌지 않고**,
   복구 잡은 `claimed`로 돌아가 같은 잡으로 다시 실행할 수 있다.
2. `formatting` — 원본 label·UUID로 `mkfs.ext4|xfs|vfat`, `blkid`로 UUID가 원본과 같은지 확인한다.
3. `mounting` — `mountall --no-bind` 후 각 장치가 `/mnt/<타깃 장치명>`에 있는지 확인한다.
4. `extracting` — `rclone cat | zstd -d | tar -x -p --numeric-owner`. 세 프로세스의 종료
   코드를 모두 확인한다. 저장소에서 읽은 바이트를 진행률로 보고한다.
5. `reconfiguring` — netplan MAC 치환, fstab·grub defaults·EFI stub의 UUID 검사(불일치는 경고), 복구 표식 (3.8절).
6. `bootloader` — 루트 외 파티션을 루트 아래로 bind 마운트하고 `arch-chroot`에서
   `grub-install`(UEFI: `--target=x86_64-efi --efi-directory=/boot/efi`, BIOS:
   `--target=i386-pc <디스크>`) → `update-grub` → `update-initramfs -u -k all`.

진행(단계 n/6, 읽은 바이트/세트 크기)과 결과는 claim 응답으로 받은 복구 전용
자격(`reportToken`)으로 보고한다 ([protocol.md](protocol.md) 2.5). 아직 하지 않는 것:
체크섬 검증(8번), 재부팅·완료 후 자격증명 삭제(14번, 5.5), boot-verify.

`pkg/backup/engine/engine.go`의 `Restore`는 여전히 **저장소에서 아카이브를 읽어 지정한
디렉터리에 푸는 수준**이며 `aiha-agent extract`가 쓴다.

**정정 완료 (2026-09-17)**: 이 CLI는 `aiha-agent`로 흡수되면서 커맨드 이름이
`extract`로 바뀌었다 — 하는 일이 "아카이브를 디렉터리로 푸는 것"뿐이므로 그렇게
부르고, `restore`라는 이름은 실제 복구 실행기에 비워 두었다. README의 잘못된
"Restore ✅ implemented" 표기도 함께 고쳤다.

**레이아웃 수집도 해소되었다 (2026-09-17)**: `pkg/backup/sysinfo`가 파티션
테이블(`sfdisk -d`), UUID/PARTUUID/크기/타입(`lsblk -J -O`), LVM 구성, UEFI 여부,
fstab, os-release, 커널, 마운트 현황, netplan을 수집하고, 백업 세트마다
`<systemId>/<setId>-manifest.json`에 기록한다. 복구 실행기는 이 매니페스트를 읽으면
된다 — 레스큐 환경에서 `rclone cat`과 `encoding/json`만으로 읽히도록 SQLite가 아닌
JSON 파일로 두었다.
