# H4 — 전환 설계 (계획 전환, 장애 전환, 복귀, 재합류)

[ha-roadmap.md](ha-roadmap.md) H4의 구현 설계다. 로드맵은 무엇을 할지(유형별 승격 조건, 펜싱 기록, 검증
목록)를 정했고, 이 문서는 그것을 H3b의 작업 흐름 장치 위에 어떻게 올릴지를 정한다. 근거 사실은
[ha-h0.md](ha-h0.md) 12절(6·8·10·11번)과 [ha-test.md](ha-test.md) 12절이다.

상태: **개정 6 (2026-11-26) — 프로토타입 범위, 설계 고정.**
- 초안부터 개정 5까지 외부 검토(codex) 다섯 차례를 코드와 DRBD·drbd-utils 소스로 대조해 반영했다(12절).
- 5차 검토에서 프로토타입 범위의 막는 문제는 0건이었고, 개정 6은 그 범위로 줄인 판이다.
- 잡이 데이터를 버리는 것과 해제(`dissolve`)는 미뤘다(결정 D·N).
- **이 판에서 설계를 고정한다**(결정 S). 이후의 지적은 13절 강화 목록에 쌓고, 1.1절의 안전 불변식을 깨는
  결함일 때만 문서를 고친다.
- 실환경 확인이 남은 것은 11절에 모았다.

---

## 1. 범위

| 명령 | 유형 | 하는 일 |
| --- | --- | --- |
| `ha switchover <group>` | 계획 전환 | 운영 노드 강등(서비스 정지 → 언마운트 → 최종 동기화 확인 → Secondary) 뒤 대기 노드 승격 |
| `ha failover <group>` | 장애 전환 | 운영 노드가 펜싱된 것을 기록하고 대기 노드를 승격. 강등 잡은 없다 |
| `ha resume <group>` | 복귀 | 역할을 바꾸지 않고 기록상 운영 노드에서 다시 승격·마운트·기동(재부팅·서비스 정지 뒤) |
| `ha rejoin <group>` | 재합류 | 펜싱된 노드를 일반 연결로 대기로 되돌린다. split-brain이면 버리지 않고 실패하고, 운영자가 손으로 푼 뒤 다시 요청한다(3.7절) |
| `ha transitions <group>` | 조회 | 전환 이력(요청·승인자, 펜싱 기록, 손실 승인, 단계, 결과, 소요 시간) |

하지 않는 것:

- 로드맵 9절 그대로: 자동 전환, 인스턴스 정지·서비스 주소 재지정의 자동화(완료 메시지로 안내만), 북향 REST(501
  유지).
- **잡이 데이터를 버리는 것.** 어떤 잡도 `--discard-my-data`를 쓰지 않는다. split-brain은 H0 9절에서 검증한 손
  절차로 푼다(3.7절, 결정 D).
- **terminate로 사라진 노드가 있는 그룹의 정리(해제)와 노드 교체** — H2로 미룬다(결정 N, 13절). 그런 그룹은
  `degraded`로 남고 운영 노드는 계속 서비스한다.

**프로토타입 전제.**
- AWS 노드 두 대와 프레임워크 하나.
- 에이전트와 프레임워크는 같은 빌드를 함께 배포한다(3.1절).
- 운영자는 이 문서의 절차를 따른다.
- 데모의 펜싱은 다시 켤 수 있는 `stop`이다.

### 1.1 안전 불변식

범위를 줄여도 아래는 지킨다. 13절의 어떤 항목을 미루더라도 이것을 깨서는 안 된다.

1. 계획 전환은 옛 운영 노드의 서비스 정지·언마운트·최종 동기화·Secondary를 확인한 뒤에만 교대하고 승격한다(3.2절).
2. 장애 전환과 정지 기록 복귀는 상대가 멈췄다는 운영자의 기록이 있어야 한다. 승격은 펜싱되지 않은 노드만,
   `--force`와 포맷 없이 한다(3.1·3.5절).
3. 어떤 잡도 데이터를 버리지 않는다. 버리는 것은 운영자가 split-brain을 확인하고 손으로 한다(3.7절).
4. 잡은 시작이 승인되기 전에 아무것도 바꾸지 않는다. 충돌·실패·결과 미확인 뒤에는 다음 단계로 가지 않고 `error`로
   멈춘다(2.3절).
5. 펜싱 해제는 두 조건 중 하나가 확인된 뒤에만 한다.
   - rejoin: 두 노드를 잡 뒤에 잰 측정으로 안전한 복제가 확인됨(3.7절).
   - teardown: 두 노드의 리소스 부재와 서비스 정지가 확인됨(4절).

---

## 2. 구조 — 기존 흐름 장치를 그대로 쓴다

전환은 새 장치가 아니라 **`ha_flows`의 새 종류**다. H3b의 흐름 규칙은 그대로 받는다.
- 그룹당 흐름 하나.
- 단계와 잡을 한 트랜잭션에서 만든다.
- 보고가 뒷받침한 뒤에만 다음 단계로 간다.
- 실패·미확인이면 `error`.
- 늦은 잡은 claim·시작 보고에서 거절한다.

여기에 아래를 더한다.

1. **흐름 종류** `switchover`, `failover`, `resume`, `rejoin` (`ha_flows.kind` 제약 확장).
2. **전환 기록** `ha_transitions` — 흐름 하나에 한 행. 승인·펜싱·손실 승인처럼 감사에 남길 사실만 담고, 진행
   상태는 `ha_flows`에서 읽는다.
3. **흐름 이동의 부수 효과** — `FlowMove`에 역할 교대(`SwapRoles`)와 펜싱 해제(`Unfence`)를 더한다. 같은
   트랜잭션에서 `ha_nodes`를 바꾼다.
4. **흐름 중 충돌 감시** — 보고를 기록하는 트랜잭션에서 본다(2.3절). 흐름 중의 양쪽 Primary는 흐름이 잡아야 한다.
   평가기는 `switching`을 건드리지 않기 때문이다(`evaluate.go` `nextState`는 `switching`이면 양쪽 Primary 검사
   전에 반환한다).
5. **에이전트 확인** — 흐름을 시작할 때 잡 종류(`jobKinds`)와 보고 형식(`haReportVersion`)을 확인한다(3.1절).

```text
보고 기록 (그룹 행 잠금) ──> 충돌이면 그 트랜잭션에서 흐름 실패
                │ (주기 루프)
                v
DecideFlow(kind, step, 잡 상태·결과, 두 노드 보고) ──> FlowMove{Wait|Next|Complete|Fail, Jobs, To, SwapRoles, Unfence}
                                                        │
AdvanceFlow (그룹 행 잠금, 한 트랜잭션) ───────────────┴─> 다음 잡 발행 / 역할 교대 / 펜싱 해제 / 그룹 상태 / 이벤트
```

`DecideFlow`와 `bearsOut`은 종류마다 분기를 명시하고, **모르는 종류는 오류**로 돌려준다. 지금 코드의 기본 분기는
teardown처럼 처리해 "리소스 없음"을 성공으로 본다(`flow.go` `DecideFlow`, `bearsOut`의 `default`).

### 2.1 단계표

| 흐름 | 시작 상태 | 1단계 | 2단계 | 역할 교대 | 끝난 뒤 그룹 |
| --- | --- | --- | --- | --- | --- |
| `switchover` | `protected` | 운영 `ha-demote` | 새 운영 `ha-promote`(`peer: connected`) | **1단계 확인 직후**, 2단계 발행과 같은 트랜잭션 | `degraded` → 평가기가 `protected` |
| `failover` | `failover_pending` (재요청은 3.4절) | 대기 `ha-promote`(`peer: fenced`) | — | 1단계 확인 뒤, 완료와 같은 트랜잭션. 옛 운영 노드는 펜싱된 대기가 된다 | `degraded`(펜싱된 노드) |
| `resume` | `failover_pending`, `degraded` | 운영 `ha-promote`(`peer: connected` 또는 `stopped`) | — | 없음 | `degraded` → 평가기 |
| `rejoin` | `degraded`이고 펜싱된 대기가 있음 | 운영 `ha-rejoin`(`side: survivor`) | 펜싱 노드 `ha-rejoin`(`side: victim`, 일반 연결만) | 없음. 완료 때 펜싱 해제 | `degraded` → 평가기가 `protected` |

진행 중 그룹 상태는 모두 `switching`이다.

**끝난 뒤 `protected`가 아니라 `degraded`로 두는 이유.** 역할이 바뀐 직후에는 새 역할로 받은 보고가 없다. 보고는
배정과 짝이 맞아야 기록된다(H1 구현 메모 "보고와 배정의 짝"). 평가기가 새 역할의 보고로 동기화를 확인해야
`protected`가 된다. 걸리는 시간의 상한은 두지 않는다.

### 2.2 단계 확인과 역할 교대

**단계 확인(모든 흐름, H3b 흐름 포함).** `bearsOut`이 쓰는 보고는 셋을 모두 만족해야 한다.

1. **잡 뒤에 잰 것 — 확인 토큰**(결정 M).
   - 프레임워크는 헬스 보고 응답마다 확인 토큰(`observeToken`)을 준다. 값은 응답을 만들 때 읽은 DB 시각
     (`clock_timestamp()`)이다.
   - 에이전트는 받은 마지막 토큰을 메모리에 두고, **측정을 시작할 때** 그 값을 떠 두었다가 그 측정의 보고에 싣는다
     (`observedAfter`). 응답을 받은 뒤에 측정을 시작하므로, 그 측정은 토큰 시각 뒤의 것이다.
   - 확인에는 `observedAfter`가 **그 단계 잡의 종료 시각(`jobs.finished_at`)보다 뒤**인 보고만 쓴다. 단계 잡을
     실행하지 않은 노드(rejoin 완료처럼 두 노드를 함께 보는 경우)는 **이 흐름의 마지막 잡 종료**보다 뒤여야 한다.
     두 시각 모두 프레임워크 DB 시계라 노드 시계와 섞지 않는다.
   - "잡 종료 뒤 받은 보고"만 보면 안 되는 이유: 에이전트는 잰 다음에 보낸다(`aiha-agent/cmd/health.go`
     `reportHealth`). 그래서 잡이 끝나기 전에 잰 측정이 잡 뒤에 도착해도 통과한다.
   - 재시작한 에이전트의 첫 보고와 잡 직후의 첫 주기 보고는 잡 전 토큰을 달고 있어 증거가 못 된다. 그다음 보고가
     증거다. 결과 응답을 못 받은 경우도 같다 — 프레임워크가 결과를 기록했으면 그 뒤 헬스 응답의 토큰이 종료 시각보다
     뒤다. 보고 주기가 30초면 단계마다 확인에 최대 약 1분이 걸린다(2.4절을 미뤘기 때문).
   - 토큰이 보고 수신 시각보다 늦으면 그 보고는 증거가 아니다.
2. **지금도 신선한 것**(`now - receivedAt ≤ STALE_AFTER`). 프레임워크가 오래 멈췄다 재시작하면 1을 만족하는 보고도
   오래된 것일 수 있다.
3. 단계별 기대 상태(각 절).

**확인 기한.** 지금 코드의 순서 그대로다(`flow.go` `DecideFlow`).
- 진행할 때마다 먼저 1~3을 만족하는 보고를 찾고, 있으면 진행한다.
- 없고 잡 종료 뒤 `STALE_AFTER`가 지났으면 `error`다.

**교대 시점 (switchover).** 강등이 확인된 순간 옛 운영 노드는 Secondary이고, 서비스가 멈췄으며, 두 디스크가 같다.
이때부터 "운영 노드"는 승격할 쪽이다. 그래서 **강등 확인 직후 교대**하고 승격 잡은 새 역할로 발행한다.
- 승격이 실패하면 기록상 운영 노드는 이미 새 쪽이다. `ha acknowledge` → `ha resume`으로 새 운영 노드에서 다시
  시도한다.
- 교대를 완료 때 하면, 승격이 반쯤 된 대기(Primary)가 "대기가 Primary"로 `acknowledge`를 막고, 되돌릴 명령이 없다.

강등 확인의 기대 상태:

- 잡 `completed`이고, **잡 결과**가 강등 직전의 최종 동기화 증거를 담는다: `Connected`, `Established`, 자기·상대
  `UpToDate`, `outOfSyncBytes`가 0으로 측정됨(값 없음은 증거가 아니다).
- 위 1·2를 만족하는 옛 운영 노드의 보고가 `Secondary`, 서비스 비활성, 복제 조회 오류 없음.

failover는 단계가 하나라 완료 때 교대한다.

**방법.** `ha_nodes_group_role_unique`(`UNIQUE (group_id, role)`)는 지금 지연 불가라 PostgreSQL이 행마다 검사한다.
그래서 한 UPDATE로 맞바꿔도 중간에 중복이 생겨 실패한다.
- 제약을 `DEFERRABLE INITIALLY IMMEDIATE`로 다시 만든다. 문장 끝에 검사되므로 `SET CONSTRAINTS`가 필요 없다.
- 마이그레이션은 카탈로그(`pg_constraint.condeferrable`)를 확인한 뒤에만 `DROP CONSTRAINT` / `ADD CONSTRAINT`를
  한다(protocol 3.5 "스키마 적용과 잠금").

교대는 한 문장이다:

```sql
UPDATE ha_nodes SET role = CASE role WHEN 'primary' THEN 'standby' ELSE 'primary' END,
       report = NULL, report_collected_at = NULL, report_received_at = NULL
 WHERE group_id = $1
```

- 그룹 행 잠금 아래에서 하고, 바뀐 행이 정확히 둘이 아니면 트랜잭션을 되돌린다.
- 보고 열을 비워 옛 역할로 받은 보고를 새 역할로 판정하지 않는다.
  - `system_id`, `node_id`, `disk`, `address`, `fenced`, `fenced_by_flow`, `last_contact_at`은 물리 노드를 따라
    그대로다.
  - 그룹의 `primary_failing_since`, `link_down_since`도 비운다.
- 교대는 2단계 잡 발행(또는 완료)과 같은 트랜잭션이므로, 둘 중 하나가 실패하면 둘 다 없다.
  - `AdvanceContext.Group`은 교대 전 스냅샷이다. 그래서 2단계 잡의 대상은 **교대 전 대기의 `system_id`**로 정하고,
    발행 전에 그것을 확인한다.
- 재시작 뒤 다시 교대하지 않는 근거: switchover는 흐름 단계가 이미 2, failover는 흐름이 이미 `completed`.

### 2.3 흐름 중 충돌 감시

**어디서 보나.** 보고를 기록하는 트랜잭션(그룹 행 잠금)에서 본다.
- 그룹 행을 잠근 뒤 진행 중인 흐름이 보이면, 새 보고를 아래 표에 대어 본다.
- 충돌이면 **그 트랜잭션에서** 흐름을 실패시킨다(이 절 끝의 "단계 잡 처리", 그룹 `error`, 이벤트).
- 흐름 진행에서만 보면, 보고 커밋과 진행 사이에 같은 노드의 다음 보고가 와서 증거를 덮을 수 있다.
- 잠금 순서는 그룹 → 잡 → 시도로 `AdvanceFlow`와 같다.

**시각이 아니라 잠금 순서로 판정한다.** "흐름 시작 뒤의 보고"를 보고 트랜잭션의 시각으로 가리지 않는다.
- 보고 트랜잭션이 흐름 시작보다 먼저 열리고 그룹 잠금을 나중에 얻으면, 그 트랜잭션의 `now()`는 흐름 시작보다 앞선다.
- 그룹 잠금을 얻은 뒤 진행 중인 흐름이 보인다는 것 자체가, 그 보고가 흐름 시작 뒤에 기록된다는 뜻이다.
- 흐름 시작 전의 접촉은 시작 검사가 본다(3.3·3.4·3.6절).

**접촉도 본다.** 지금 저장소는 두 종류의 보고를 기록하지 않는다(`ha/pgstore.go` `Report`): 배정이 없는 보고
(재부팅한 에이전트의 첫 보고, `GroupID` 없음)와 배정이 맞지 않는 보고. 그래서 "펜싱된 노드가 보고함"을 보고 열로는
알 수 없다.
- 인증된 시스템의 헬스 보고가 오면 배정과 상관없이 `ha_nodes.last_contact_at`을 갱신한다. 값은 그룹 잠금을 얻은
  뒤의 시각(`clock_timestamp()`)이고, 뒤로 가지 않게 `GREATEST`를 쓴다.
- 역할 판정에는 지금처럼 배정이 맞는 측정값만 쓴다.
- 새 열은 마이그레이션 때 `report_received_at`으로 채운다.
- **접촉은 헬스 보고만이다.** 잡 진행 보고는 잡 행을 잠근 채 처리된다(`job/pgstore.go` `RecordProgress`). 거기서
  그룹을 잠그면 순서가 거꾸로 되어 교착할 수 있다. 흐름 중에는 다른 잡이 없다(시작 검사가 끝나지 않은 잡을 거절하고,
  흐름 중 백업 생성도 거절한다).

확인식은 **배정 역할**(보고가 따른 `role`)과 **실제 DRBD 역할**(`replication.role`, 상대는 `peerRole`)을 따로 쓴다.

| 흐름·단계 | Primary여도 되는 노드 (실제 역할) | 멈추는 조건 |
| --- | --- | --- |
| 모든 흐름 | — | 한 노드가 자기 `Primary`, 상대 `Primary`를 함께 보고 |
| 모든 흐름 | — | 승격 대상 노드가 상대를 `Primary`로 봄(`peerRole`) |
| switchover 1단계 | 옛 운영 | 대기가 Primary |
| switchover 2단계 (교대 뒤) | 새 운영 | 옛 운영(이제 대기)이 Primary |
| failover | 대기(배정은 아직 standby) | **펜싱된 운영 노드의 보고가 흐름 중에 기록됨**(배정 없는 보고 포함), 또는 대기가 복제 연결 `Connected`를 보고. 펜싱 기록과 모순이다 |
| resume (`peer: connected`) | 운영 | 대기가 Primary |
| resume (`peer: stopped`) | 운영 | **정지 기록한 대기의 보고가 흐름 중에 기록됨**, 또는 운영이 `Connected`를 보고 |
| rejoin | 운영 | 펜싱 노드가 Primary, 또는 운영이 실제 `Primary`가 아님(3.7절의 방향 전제) |

failover 도중 "배정 standby, 실제 Primary, 서비스 active"인 대기 보고는 정상 중간 상태다(1단계의 확인 대상).
멈추는 조건과 구별하는 시험을 둔다(9절).

**단계 잡 처리.** 흐름을 실패시키는 트랜잭션은 그룹 잠금 아래에서 그 단계의 잡 행을 잠그고(`FOR UPDATE`), **잠근
뒤의 상태로** 처리를 고른다.
- 시작 전(`queued`·`claimed`)이면 끝낸다(`RejectTx`와 같다).
- `running`이면 같은 트랜잭션에서 취소를 요청한다(`cancelling`).
- 잠그기 전에 읽은 상태로 고르면, 그 사이 시작한 잡의 취소가 빠진다.
- 공개 `RequestCancel`은 HA 잡을 거절하고 자기 트랜잭션을 따로 연다. 그래서 트랜잭션을 받는 내부 함수
  (`job.CancelTx`)를 `RejectTx` 옆에 둔다.
- 흐름 메시지에는 `job <id> was running when the operation stopped`를 남긴다.

에이전트와 실행기 쪽:

- 에이전트는 시작 보고 응답의 취소 요청을 읽어, 요청이 있으면 아무것도 하지 않고 취소 결과를 보낸다.
  - 지금은 시작 보고 응답을 버리고(`worker/reporter.go` `reportStart`), 다음 진행 보고(기본 10초) 응답에서야
    취소를 안다.
  - 모든 잡 종류에 같다(백업 포함).
- **`ha-promote`는 상태를 바꾸는 단계(Primary 전환, 마운트, 서비스 기동) 바로 앞에서 진행 보고를 한 번 보내고, 그
  응답의 취소 요청을 확인한다**(3.5절). 데이터에 영향을 주는 곳이 이 셋이기 때문이다.
  - 다른 실행기는 지금처럼 주기 진행 보고의 취소를 따른다.
  - 진행 보고를 보낼 수 없으면(리스 만료, 거절) 그 단계로 가지 않고 멈춘다.
  - 이미 시작한 외부 명령은 끝까지 가고 결과에 남는다.
- 그 잡이 끝날 때까지 새 흐름과 `ha acknowledge`는 "끝나지 않은 잡"으로 거절된다(지금 규칙).
- 리스 만료로 끝난 잡은 잡이 `failed`, 시도가 결과 `unknown`이다(`job/sweep.go` "running but abandoned",
  "cancel unconfirmed"). 이것만으로는 풀리지 않는다.
  - `acknowledge`와 3.4절 failover 재요청은 그 잡의 노드에서 **토큰이 그 잡 종료 뒤인** 신선한 보고를 요구한다
    (4절, 2.2절 1).

### 2.4 단계 진행 — 주기 루프만 (미룸)

프로토타입은 지금의 주기 진행(서버의 흐름 진행 주기, 기본 30초)만 쓴다. 다음 셋은 13절로 미뤘다.
- 보고 직후 진행
- 잡 결과 뒤 두 번 잇달아 보고
- 응답 배정 재조회

계획 전환의 서비스 중단 시간은 11절 5번에서 재고, 길면 넣는다. 교대 뒤 옛 역할로 온 보고는 지금처럼 기록되지 않고,
다음 응답이 새 배정을 준다.

---

## 3. 흐름별 규칙

### 3.1 공통 시작 검사

지금의 `startable`은 두 가지를 함께 본다: "진행 중 흐름 없음, 허용 상태, 끝나지 않은 잡 없음"과 "두 노드 보고
신선·호스트명".
- 앞의 셋은 공통으로 둔다.
- 뒤의 것은 `bothReporting`으로 떼어 setup·teardown·switchover·rejoin이 그대로 쓴다.
- failover·resume은 3.3·3.6절의 노드별 조건을 쓴다.
- setup·teardown에는 아래의 보고 형식 검사와 펜싱 검사(setup)가 새로 붙는다.

모든 흐름에 공통으로:

- 전환에는 `--approved-by`가 필수다(`model.HAApproval`). `--reason`, `--incident`는 선택. 요청자(`cli`)와 승인자를
  따로 기록한다.
- **승격 대상은 펜싱되지 않은 노드여야 한다.** 시작 때 보고, 승격 잡을 발행할 때 다시 본다.
  - 장애 전환 뒤 펜싱된 옛 운영 노드는 자기 디스크를 `UpToDate`로 보지만, 새 운영이 쓴 것이 없다.
  - 그 뒤 새 운영이 고장 나 `failover_pending`이 되어도, 펜싱된 대기로는 장애 전환할 수 없다(`ha rejoin`이 먼저다).
- **잡을 받을 노드가 그 잡 종류를 실행할 수 있어야 한다.** 헬스 보고에 에이전트가 실행하는 HA 잡 종류
  (`jobKinds`)를 싣는다. 흐름 시작 때 **잡을 받을 노드**의 최신 보고가 그 종류를 담는지 본다.
  - 대상: switchover·rejoin은 두 노드, failover는 대기, resume은 운영.
  - 없으면 `upgrade the agent on <system>`으로 거절한다.
  - 구버전 에이전트는 잡을 시작 전에 거절한다(`worker.go`). 하지만 계획 전환에서 대기만 구버전이면, 강등과 교대가
    끝난 뒤에야 드러나 서비스가 멈춘 채 남는다.
- **보고 형식이 새것이어야 한다.** 헬스 보고에 `haReportVersion`(6절)을 싣는다. 흐름 시작 때 잡을 받거나 보고로
  확인할 노드의 최신 보고가 2 이상인지 본다.
  - 대상은 위 줄과 같고, setup·teardown은 두 노드다.
  - 펜싱되거나 정지 기록된 노드처럼 보고를 요구하지 않는 노드는 보지 않는다.
  - 구버전 보고에는 확인 토큰과 `resourceAbsent`가 없어 흐름이 끝나지 않는다.
- **배포**: 프로토타입은 에이전트와 프레임워크를 같은 빌드로 올린다.
  1. 에이전트를 먼저 올린다. 새 필드는 옛 프레임워크가 버린다(Echo `c.Bind`).
  2. 진행 중인 흐름이 없는지 확인한다(`ha list`에 `configuring`·`switching` 그룹이 없음).
  3. 프레임워크를 올린다.
  4. 각 에이전트의 다음 보고(한 주기) 뒤에 흐름을 요청한다.

  버전 사이의 섞인 운영과 되돌리기는 13절이다.
- **펜싱된 노드가 있으면 `ha setup`을 거절한다**(결정 O): `<system> is fenced; run ha teardown first`.
  - 펜싱이 남는 경로: teardown이 펜싱 해제를 확인하지 못하고 실패한 그룹, 또는 그 뒤 `acknowledge`로 `registered`가
    된 그룹.
  - 펜싱된 노드로 복제를 새로 세우면, 승격 대상이 될 수 없는 대기가 생긴다.
- 시작 검사와 흐름 생성, 펜싱 표시, 전환 기록은 그룹 행 잠금 아래 한 트랜잭션이다(`flowstore.go` 흐름 시작).

### 3.2 switchover

시작 조건(최신 보고 기준):

- 그룹 `protected`, 펜싱된 노드 없음, `bothReporting`.
- 두 노드 모두 `Connected`/`Established`, `UpToDate`/`UpToDate`, 미동기 0으로 **보고**됨.

`ha-demote` (옛 운영 노드, 실행기 순서):

1. 실행 전 검사(아무것도 바꾸기 전): 리소스가 이 노드에서 Primary이거나 이미 Secondary. 상대가 연결되어 있고 상대
   디스크 `UpToDate`이며 **상대가 Primary가 아니다.** 아니면 거절.
2. 서비스 정지 → 마운트 유닛 정지(언마운트).
3. 최종 동기화 확인: `Connected`, `Established`, `UpToDate`/`UpToDate`, `out-of-sync` 0을 최대 `finalSyncTimeoutSec`
   (명세, 기본 120) 동안 기다린다.
4. `drbdadm secondary`. 결과에 3의 측정값(연결·복제 상태, 두 디스크, 미동기량)을 싣는다.

3에서 기한을 넘기면 서비스는 멈춘 채 실패하고, 다시 띄우지 않는다(결정 F). 교대도 승격 잡도 없다. 운영자는 원인을
보고 `ha acknowledge` → `ha resume`(옛 운영 노드에서 다시 기동, 3.6절 조건)한다.

멱등: 이미 Secondary이고 서비스·마운트가 멈춰 있으면 각 단계를 `already so`로 기록하고, 3의 확인은 그대로 한다.

2단계 `ha-promote`(3.5절). 확인: 새 운영 노드의 보고(새 역할로 기록된 것)가 3.5절 끝의 완료 상태.

### 3.3 failover

시작 조건:

- 그룹 `failover_pending`(운영 보고 끊김 또는 운영 서비스 실패가 유예 이상 지속).
- 펜싱 기록: `--fenced-instance <인스턴스 ID>`, `--fence-method stop|terminate`. 필수. 확인자는 승인자다.
  북향 계약의 `confirmFenced`는 이 기록이 있으면 참으로 본다(REST는 501이므로 CLI가 기준이다).
- `--accept-data-loss` 필수(결정 2. 최신성을 입증할 수 없으므로 항상 요구).
- **운영 노드의 접촉이 신선하면 거절**(결정 E). 보고 수신 시각이 아니라 `last_contact_at`(2.3절)으로 본다.
  - 문구: `the primary contacted the framework 12s ago; stop it and wait until that is older than 90s`.
  - 정지 직전의 접촉은 정지 뒤에도 한동안 신선하다. 그래서 이 거절은 펜싱이 거짓이라는 뜻이 아니다 — 기다리면 된다.
  - 침묵이 펜싱의 증거도 아니다. 펜싱은 운영자의 기록이고, 이 검사와 2.3절의 접촉 감시는 그 기록과 모순되는 증거만
    거른다.
- 대기 노드: 펜싱 안 됨, 보고 신선, `Secondary`, 디스크 `UpToDate`, 상대 역할이 `Primary`가 아니고 연결이
  `Connected`가 아님. 연결이 살아 있으면 운영 노드가 살아 있다 — 펜싱이 안 된 것이다.

기록(`ha_transitions`): 펜싱 대상 시스템·인스턴스·방법·확인자·확인 시각, 손실 승인, 요청 시점의
`last_sync_confirmed_at`. 흐름 시작 트랜잭션에서 옛 운영 노드를 `fenced = true`, `fenced_by_flow = <이 흐름>`으로
표시한다.

`last_sync_confirmed_at`은 지금 평가 시각(`gs.Now`)으로 기록되어, 같은 두 보고를 다시 읽어도 앞으로 간다
(`evaluate.go` `Assess`).
- **두 보고 중 먼저 받은 것의 수신 시각**으로 바꾼다(H1 동작 수정, H4에 포함). 이것도 쓰기 보존의 경계가 아니라
  관측 시각이다.
- CLI는 `Last sync observed …`로 보이고, 한 번도 확인되지 않았으면(`NULL`) `never`로 보인다.

1단계: 대기 노드 `ha-promote`(`peer: fenced`). 확인: 대기의 보고(배정 standby)가 3.5절 끝의 완료 상태.
완료: 역할 교대. 옛 운영 노드는 펜싱된 대기가 된다. 그룹은 `degraded`. 평가기는 펜싱된 노드가 있는 한 `protected`로
올리지 않는다(기존 `nextState`의 `fenced`).

**terminate로 펜싱하면** 그 노드는 돌아오지 않는다. 그러면 rejoin도 teardown(두 노드 보고 필요)도 할 수 없다.
그룹은 H2의 노드 교체 전까지 `degraded`로 남고, 운영 노드는 계속 서비스한다(1절, 13절).

### 3.4 failover 재요청

failover 1단계가 실패하면 흐름 `failed`, 그룹 `error`, 역할은 그대로, 펜싱 표시는 남는다. 대기는 승격이 되었을
수도(서비스만 실패), 아닐 수도 있다.

**재요청 조건은 그룹 상태가 아니라 마지막 흐름으로 정한다**(결정 C). 시작 트랜잭션에서 그룹 잠금 아래 함께 읽는다:

- 마지막 흐름이 실패한 failover이고, 그 대상이 같은 대기다.
- 같은 운영 노드가 그 failover의 펜싱으로 아직 펜싱되어 있다(`fenced_by_flow`가 그 흐름).
- 대기의 보고가 신선하고, **토큰이 실패한 흐름의 마지막 잡 종료 뒤**다(2.2절 1). 그 잡이 리스 만료로 끝났어도(결과
  모름) 이 보고가 그 뒤의 대기를 보여 준다. 대기가 실제 `Primary`여도 된다.
- 펜싱된 운영 노드의 `last_contact_at`이 그 펜싱 뒤(펜싱한 failover의 시작 뒤)가 아니다. 펜싱한 노드가 그 뒤에
  보고했다면, 얼마나 오래전이든 모순이므로 거절한다.

이 조건이면 `error`·`degraded`·`failover_pending`에서 재요청을 받는다.
- `ha-promote`는 멱등이라 이미 Primary면 마운트·기동부터 한다.
- 펜싱 플래그와 손실 승인은 다시 요구한다(새 전환 기록).
- `--fenced-instance`는 처음 펜싱 기록의 인스턴스와 같아야 한다(다르면 거절, 오타 방지).
- `fenced_by_flow`는 처음 펜싱한 흐름 그대로 둔다.

이렇게 하는 이유: `acknowledge`는 대기의 보고가 신선할 때만 "대기가 Primary"를 본다. 대기가 승격된 채 보고가
늦으면 `acknowledge`가 통과해 `degraded`가 되고, 그 뒤 일반 failover 조건(대기 Secondary)에도 막혀 갈 곳이 없다.

### 3.5 `ha-promote` 실행기

명세: 흐름 ID·단계, 리소스, 장치, 마운트 지점, 서비스 유닛, `peer`(`connected` | `fenced` | `stopped`).

1. 실행 전 검사(아무것도 바꾸기 전): 리소스가 떠 있다. 자기 디스크 `UpToDate`. 상대가 `Primary`가 아니다.
   - `peer: connected`: 상대가 `Connected`이고 `Secondary`여야 한다. 실행 시점에 이 노드가 직접 본 상태다.
   - `peer: fenced`·`stopped`: 연결이 `Connected`면 **거절**한다(펜싱·정지 기록과 모순). `Connecting`·`StandAlone`과
     상대 디스크 `DUnknown`은 정상.
   - 서비스 유닛이 다른 유닛에 의해 기동되지 않는다(기존 `TriggeredBy` 검사).
2. Secondary면 `drbdadm primary`. **`--force`는 쓰지 않는다.** 이미 Primary면 `already so`.
3. 복제 장치에 ext4가 있는지 확인한다. **없으면 거절하고 포맷하지 않는다**(포맷은 구성에서만).
4. 마운트 유닛 기동. 마운트가 활성이면, `findmnt`로 그 마운트 지점의 장치가 명세의 복제 장치인지 확인한다(아니면
   거절).
5. 서비스 기동 → `active`가 될 때까지 최대 30초 기다린다. `activating`은 기다리고, `failed`·`deactivating`·
   `inactive`는 실패.

2·4·5 바로 앞에서 진행 보고를 보내고 그 응답의 취소 요청을 확인한다(2.3절).

H3b의 `becomePrimary`는 첫 승격(`--force`)과 `mkfs`를 품고 있으므로 그대로 부르지 않는다. 유닛·상태 조회 같은 읽기
부분만 함께 쓴다. H0 8.1에서 상대가 꺼진 채 `UpToDate`인 대기는 `--force` 없이 승격되었다.

**완료 상태(흐름의 확인).** 대상 노드의 보고가 셋을 보여야 한다. 평가기가 운영 노드에 요구하는 것과 같다
(`evaluate.go`의 `HealthPath`).
- 실제 `Primary`, 서비스 `active`, 포트 열림.
- 그룹에 헬스 경로가 설정되어 있으면, 그 응답도 성공. 포트만 열리고 헬스 경로가 실패하면 완료가 아니다.

**완료 메시지**: `<새 운영> (<주소>) now serves; repoint <service_address> to it`. switchover·failover 둘 다이고,
흐름 `message`에 남아 `ha status`·`ha transitions`에 보인다.

### 3.6 resume

재부팅 뒤 Secondary로 올라온 운영 노드(H0 12절 8번), 또는 운영 서비스가 멈춘 경우(ha-test 5.1)를 다시 띄운다.

시작 조건:
- 그룹 `failover_pending` 또는 `degraded`.
- 운영 노드가 펜싱 안 됨, 보고 신선, 디스크 `UpToDate`, 대상 에이전트가 `ha-promote`를 실행함.
- 대기 쪽은 둘 중 하나다.
  - **연결됨**: 운영 노드의 신선한 보고가 `Connected`이고 상대가 `Secondary`다 → `peer: connected`.
    - 실행기가 실행 시점에 같은 것을 다시 본다(3.5절 1).
    - 대기 자신의 보고가 `Secondary`라는 것만으로는 부족하다. 그 보고 뒤에 연결이 끊기고 대기가 손으로
      승격되었어도, DB에는 신선한 `Secondary` 보고가 남는다.
  - **그 밖 전부**(연결 안 됨, 대기 침묵, 판독 불가) → **정지 기록을 요구한다**: `--standby-stopped-instance <ID>
    --stop-method stop|terminate` → `peer: stopped`.
    - 흐름 시작 트랜잭션에서 대기를 펜싱으로 표시하고(`fenced`, `fenced_by_flow`) 기록을 남긴다.
    - 대기에는 잡을 보내지 않으므로 대기의 `jobKinds`는 보지 않는다.
    - 대기의 `last_contact_at`이 `STALE_AFTER` 안이면 거절한다(3.3절과 같은 이유와 문구).
    - 운영 노드의 최신 보고가 `Connected`여도 거절한다 — 정지 기록과 모순이다.
    - 그 뒤 대기의 보고가 기록되면 흐름이 멈춘다(2.3절).

정지 기록이 막는 경우: 대기가 손으로 승격되어 쓰고 있는데, 보고와 복제 연결이 모두 끊긴 상황이다.
- 운영 노드에서는 `Secondary`/`UpToDate`/상대 `Unknown`이 보이고, 승격하면 split-brain이 된다.
- 정지한 대기가 돌아오면 `ha rejoin`으로 들인다(3.7절). 갈라진 변경분이 있으면 rejoin은 버리지 않고 실패한다.
  resume은 손실을 승인받지 않았으므로, 손 절차의 승인을 따로 정한다(3.7절).

1단계: 운영 노드 `ha-promote`. 확인: 3.5절 끝의 완료 상태. 완료: `degraded`.

### 3.7 rejoin

**운영 절차.** 장애 전환(또는 정지 기록을 남긴 resume) 뒤 펜싱된 노드는, `ha rejoin` 전까지 서비스 기동 없이 켠다.
- 켜도 `auto-promote no`로 Secondary에 머물고, 서비스는 부팅 때 기동되지 않는다(H3b가 disable).
- 에이전트는 배정(대기)대로 보고한다. 그 보고가 오면 `ha rejoin`을 요청한다.
- 단절 뒤 그 노드가 쓴 것이 있으면, DRBD가 연결을 거부해 `StandAlone`에 멈춘다(H0 12절 11번).

```text
ha rejoin <group> --approved-by <who> [--reason …]
```

시작 조건:
- 그룹 `degraded`.
- 펜싱된 노드가 기록상 대기이고, 보고가 신선하며, 실제 역할이 `Primary`가 아니다.
- 운영 노드가 펜싱 안 됨, 신선, `Primary`, 서비스 `active`.
- 두 노드 모두 `ha-rejoin`을 실행함.

**방향 전제 — 운영 노드는 덮이지 않는다**(결정 Q).
- DRBD 9.3.4는 Primary이고 디스크가 `Consistent` 이상인 노드가, 받을 변경분이 있는데 재동기화 대상(SyncTarget)이
  되려 하면 연결을 끊는다. 조건은 `rr-conflict`가 `disconnect`(기본값)일 때다(`drbd_receiver.c`
  `drbd_sync_handshake`, `I shall become SyncTarget, but I am primary!`).
- 그래서 운영 노드가 rejoin 내내 Primary이고 정책이 기본값이면, 운영의 데이터는 덮이지 않는다.
- 프로토타입은 이것을 **구성 전제**로 둔다. H3b가 만드는 리소스 파일에는 `after-sb-*`·`rr-conflict`가 없어
  기본값(`disconnect`)이 쓰인다. 운영자는 `global_common.conf`의 `common` 절에서 이 값들을 바꾸지 않는다.
- 실행기의 정책 검사는 13절로 미뤘다. 실제 값은 11절 7번에서 확인한다.
- 흐름 중 운영 노드가 Primary가 아니게 되면 충돌이다(2.3절 표).

1단계 `ha-rejoin`(`side: survivor`, 운영 노드): `StandAlone`이면 `drbdadm connect`(기다리는 쪽이 된다). 그 밖이면
`already so`. 확인: 운영의 보고가 `Primary`, 서비스 `active`, 연결 `Connecting` 또는 `Connected`.

2단계 `ha-rejoin`(`side: victim`, 펜싱된 노드):

1. 실행 전 검사(아무것도 바꾸기 전):
   - 이 노드가 Primary면 **거절**(결정 H). 펜싱된 노드가 Primary라는 것은 누군가 손으로 승격했다는 뜻이고, 그
     변경분을 버릴지는 잡이 정하지 않는다.
   - 서비스가 돌고 있으면 거절. Secondary에서는 마운트가 안 되므로, 루트에 쓰고 있는 것이다.
2. 연결이 `Connected`면 끊지 않고 끝낸다. H0 8.1처럼 DRBD가 스스로 이 노드를 SyncTarget으로 따라잡는 중이거나
   끝났다.
3. 그 밖이면 끊어서 `StandAlone`을 만들고 `drbdadm connect`(**일반 연결**)한다. 최대 30초 기다린다.
   - 끊기는 멱등이다. 이미 `StandAlone`이면 `already so`, `Disconnecting`이면 `StandAlone`이 될 때까지 기다린다.
   - 끊는 사이 `Connected`가 되었으면 2로 돌아간다.
   - `Connected` → 끝(일반 재동기화).
   - 그 밖(`StandAlone`, `Connecting`, 기한 초과) → **버리지 않고 실패**한다. 결과에 연결 상태를 싣고, 메시지는
     `<node> did not connect (<state>); if the kernel log shows a split-brain, resolve it by hand and request
     rejoin again`이다.

**어떤 `ha-rejoin` 명세에도 버리는 선택지가 없다.** 실행기는 `--discard-my-data`를 쓰지 않는다.

**split-brain 손 절차**(H0 9절에서 검증한 순서).
1. 운영자가 두 노드의 커널 로그(`journalctl -k -b`)에서, 이번 연결 시도에 찍힌 그 리소스·볼륨·상대의
   `Split-Brain detected but unresolved` 줄을 확인한다.
   - 커널 로그에는 해결된 뒤에도 같은 부팅의 이전 줄이 남는다(H0 수집물 `15-rejoined`). 그래서 시각을 실패한 rejoin
     잡과 맞춰 본다.
   - 줄이 없으면 split-brain이 아니다. 연결이 안 된 다른 원인을 찾고, 버리지 않는다.
2. 버리는 쪽은 언제나 펜싱 노드다. 손실 승인은 펜싱의 출처에 따라 다르다.
   - failover에서 온 펜싱: 그 failover가 손실을 승인했다(`ha_transitions.data_loss_accepted`).
   - resume의 정지 기록에서 온 펜싱: 승인이 없다. 버리기 전에 승인자를 정하고, 다음 `ha rejoin`의 `--approved-by`와
     `--reason`에 남긴다.
3. 펜싱 노드: `drbdadm disconnect <리소스>` → `drbdadm secondary <리소스>` → `drbdadm connect --discard-my-data
   <리소스>`. 운영 노드가 `StandAlone`이면 `drbdadm connect <리소스>`.
   - DRBD 9.3.4는 split-brain이 아니어도 `--discard-my-data`로 재동기화 방향을 뒤집는다(협상 프로토콜 121 이상, H0
     협상값 125, `drbd_sync_handshake`). 그래서 1의 확인 없이 쓰지 않는다.
   - 한 번 요청한 `--discard-my-data`는 다음 연결 협상까지 남는다(13절). 3을 하다 멈췄으면, 다음 연결 전에 두 노드를
     직접 확인한다.
4. `ha acknowledge` → `ha rejoin`. 2단계가 `Connected`를 보고 끝나고, 완료 확인이 재동기화의 끝을 본다.

**완료(펜싱 해제) 조건**은 두 노드의 신선한 보고로 본다. 운영 쪽 연결만 보면 동기화 방향이 거꾸로여도
`Connected`다.

- 두 노드 모두 보고 토큰이 이 흐름의 마지막 잡 종료 뒤다(2.2절 1).
- 운영: 실제 `Primary`, `Connected`, `Established`, 디스크 `UpToDate`.
- 펜싱 노드: 실제 `Secondary`, 서비스 비활성(조회 성공), `Connected`, `Established`, 디스크 `UpToDate`.
- 미동기 0.

재동기화 중(`SyncSource`/`SyncTarget`)이면 확인 기한(2.2절) 안에 끝나기를 기다린다. 넘기면 펜싱을 유지한 채
`error`다. 재동기화가 끝난 뒤 `ha acknowledge` → `ha rejoin`을 다시 요청한다. 이미 연결된 펜싱 노드는 끊지 않으므로
재요청은 확인만 한다. 운영 `SyncTarget`이나 펜싱 노드 `SyncSource`는 방향이 거꾸로라 `error`다.

---

## 4. 확인(`ha acknowledge`)과 해제의 변경

- 지금은 "마지막 흐름이 실패면 `registered`"다. 전환이 실패한 그룹을 `registered`로 보내면, 복제가 살아 있는데
  `ha setup`을 받게 된다. **`registered`는 마지막 실패 흐름이 `setup`·`teardown`일 때만**이고, 그 밖은 `degraded`.
- "대기가 Primary면 거절"은 그대로다. 그 경우의 출구는 `acknowledge`가 아니라 3.4절의 failover 재요청이다.
- **결과를 모르는 단계 잡 뒤의 `acknowledge`.** 흐름의 마지막 단계 잡이 리스 만료로 끝났으면(잡 `failed`, 시도
  결과 `unknown`. 2.3절), 그 노드의 보고 중 **토큰이 그 잡 종료 뒤인** 신선한 것이 있어야 받는다.
  - 없으면 `waiting for a report from <system> measured after job <id> ended`로 거절한다.
  - 그 노드가 무엇을 했는지 모른 채 `degraded`로 내리면, 다음 흐름이 옛 보고로 시작 검사를 통과할 수 있다.
- `ha teardown`이 완료되면(`registered`) 펜싱 표시를 지운다(`fenced = false`, `fenced_by_flow = NULL`).
- **teardown의 확인 자체를 바꾼다.** 지금은 복제 조회 오류와 리소스 부재를 함께 "리소스 없음"으로 본다(`flow.go`
  `bearsOut`, `!readable`).
  - teardown은 두 단계다(1단계 운영, 2단계 대기, `flow.go` `FlowTeardown`).
  - 각 단계의 확인: 그 단계 노드의 보고(토큰이 그 잡 종료 뒤)가 `resourceAbsent`(6절)이고, 서비스 비활성이 조회에
    성공한 값(`inactive`·`failed`)이다.
  - 완료와 펜싱 해제는 두 단계 확인에 기댄다.
  - 모듈이 없는 노드도 `resourceAbsent`이므로(결정 R), 구성 전에 실패한 그룹이나 재부팅한 노드의 teardown도 끝난다.
- terminate로 펜싱한 노드가 있는 그룹은 teardown을 시작할 수 없다(`bothReporting`). H2까지 `degraded`로 둔다(3.3절).

---

## 5. 저장

```sql
-- ha_flows.kind: 'setup','teardown' + 'switchover','failover','resume','rejoin'
--   (조건부 교체, jobs_kind_check와 같은 방식 — 카탈로그 확인 뒤에만 ALTER)
-- ha_nodes_group_role_unique: DEFERRABLE INITIALLY IMMEDIATE로 재생성 (2.2절, 조건부)
-- ha_nodes.last_contact_at timestamptz: 배정과 상관없이 마지막 인증 헬스 보고 시각 (2.3절)
-- ha_nodes.fenced_by_flow text: 이 노드를 지금 펜싱하고 있는 흐름 (fenced와 함께 쓰고 함께 지운다)

CREATE TABLE IF NOT EXISTS ha_transitions (
    flow_id            text PRIMARY KEY REFERENCES ha_flows (flow_id) ON DELETE CASCADE,
    group_id           text NOT NULL REFERENCES ha_groups (group_id) ON DELETE CASCADE,
    kind               text NOT NULL,            -- ha_flows.kind와 같다 (목록 조회용)
    from_primary       text NOT NULL,            -- 시작 시 운영 시스템
    to_primary         text NOT NULL,            -- 끝나면 운영일 시스템 (resume·rejoin은 같다)
    approved_by        text NOT NULL,
    reason             text,
    incident_id        text,
    fenced_system_id   text,                     -- failover의 옛 운영, resume의 정지 기록한 대기
    fenced_instance    text,
    fence_method       text CHECK (fence_method IN ('stop', 'terminate')),
    fence_confirmed_at timestamptz,
    data_loss_accepted boolean NOT NULL DEFAULT false, -- failover는 항상 참
    last_sync_confirmed_at timestamptz           -- 요청 시점 값의 사본
);
-- (group_id, flow_id) 인덱스도 조건부로 만든다.
```

- `ha_nodes.fenced`는 이미 있다(H1 스키마). 펜싱의 원인은 **`fenced_by_flow`로 찾는다**.
  - failover 재요청이 같은 펜싱인지를, `ha status`가 펜싱 기록을, 손 절차가 손실 승인을 이것으로 본다.
  - `ha_transitions.fenced_system_id`로 "가장 최근 기록"을 찾으면, teardown으로 풀린 옛 펜싱의 기록과 섞인다.
  - 펜싱 표시(failover·resume 시작)는 두 열을 함께 쓰고, 해제(rejoin 완료·teardown 완료)는 함께 지운다.
- 이벤트: `flow_started`/`flow_ended`는 기존 것을 쓰고, `role_changed`(누가 운영이 되었나), `node_fenced`,
  `node_unfenced`, `flow_conflict`(2.3절)를 더한다. TM-AI-HA-04가 순번으로 받는 그룹 이벤트에 전환 사실이 남는다
  (로드맵 4절 항목 17).
- 비밀: 전환 잡은 공유 비밀이 필요 없다. `Admit`은 지금처럼 `replication-setup`에만 비밀을 준다.
- 새 스키마 문장은 모두 최신 DB에서 표 잠금을 다시 잡지 않아야 한다(`migrate_test.go`의 잠금 시험에 포함).

---

## 6. 계약 (에이전트)

`model`에 명세 셋을 더한다. 모두 흐름 ID·단계를 갖고 비밀을 담지 않는다.

| 종류 | 명세 | 결과 `detail` |
| --- | --- | --- |
| `ha-demote` | `HADemoteSpec{flowId, step, resource, mountPoint, serviceUnit, finalSyncTimeoutSec}` | `ReplicationJobResult` + 강등 직전 측정(`connection`, `replication`, `peerDisk`, `outOfSyncBytes`) |
| `ha-promote` | `HAPromoteSpec{flowId, step, resource, device, mountPoint, serviceUnit, peer}` | `ReplicationJobResult` |
| `ha-rejoin` | `HARejoinSpec{flowId, step, resource, mountPoint, serviceUnit, side: victim\|survivor}` | `ReplicationJobResult` + 끝났을 때의 `connection` |

헬스 보고(`AgentHealthReq`)에 셋을 더하고, 응답(`AgentHealthResp`)에 하나를 더한다. 보고의 셋은 모두 생략 가능하다
(구버전 에이전트는 보내지 않는다).

- `haReportVersion`: 보고 형식. 2는 아래 `observedAfter`와 복제 상태의 `resourceAbsent`를 보낸다는 뜻이다. 없으면
  1로 본다(3.1절).
- `jobKinds`: 에이전트가 실행하는 HA 잡 종류(3.1절).
- `observedAfter`: 이 측정을 시작할 때 에이전트가 가진 마지막 확인 토큰(2.2절 1). 받은 값을 그대로 돌려준다.
  토큰이 없으면(시작 뒤 첫 보고) 비운다. 에이전트는 토큰을 메모리에만 두고, 보고 루프가 측정 함수를 부르기 직전에
  떠 둔다.
- 응답 `observeToken`: 프레임워크가 응답을 만들 때 읽은 DB 시각. 에이전트에게는 불투명한 값이다. 배정이 없는
  응답에도 싣는다.

복제 상태(`HAReplicationStatus`)에 `resourceAbsent`를 더한다.
- 리소스가 이 노드 커널에 없다는 **명시적 판정**이며, 그때 `error`는 비어 있다.
- 지금 에이전트는 `drbdsetup status`의 모든 실패를 `error`로 보낸다(`health.go` `replication`). 그래서 둘을 구별할
  수 없다.

판정은 둘 중 하나일 때만 한다(결정 R).

- **모듈이 없다.** `/proc/drbd`를 열 때 파일이 없다(`ENOENT`). 그러면 DRBD 모듈이 적재되지 않은 것이고, 이 노드
  커널에 어떤 리소스도 떠 있을 수 없다. 권한 거부 같은 다른 오류는 오류다.
- **완전한 목록에 없다.** `drbdsetup events2 --now all`로 떠 있는 객체를 모두 읽는다.
  - 출력의 마지막 줄이 덤프 끝 표시 `exists -`이고, 그 앞에 이 리소스의 `exists resource name:<리소스>` 줄이 없어야
    한다.
  - drbd-utils 9.36.0에서 이 끝 표시는 커널이 덤프 끝 메시지(`NL_OBJ_INITIAL_STATE_DONE`)를 보냈을 때만 찍힌다
    (`drbdsetup_events2.c`).
- **인자 없는 `drbdsetup status --json`은 쓰지 않는다.** 9.36.0의 `list_resources`는 커널 조회가 실패하면 빈 목록을
  돌려준다(`drbdsetup.c`). `status`는 그때도 `[]`를 찍고 0으로 끝나고, 우리 명령 실행기는 성공한 명령의 표준 오류를
  버린다(`sysinfo/runner.go`).
- 쓰는 곳: teardown의 확인과 펜싱 해제(4절). 평가기의 판정은 지금과 같고(복제 없음), 사유 문구만 나눈다.

**단계 잡의 취소.** 에이전트는 시작 보고 응답의 취소 요청을 따른다. `ha-promote`는 단계 직전에 진행 보고로
취소를 확인한다(2.3절).

단계 이름(결과의 `steps`, 잡 메시지의 `did … / already so: …`): `stop-service`, `unmount-volume`,
`confirm-final-sync`, `demote`, `promote`, `mount-volume`, `start-service`, `disconnect`, `connect`. 기존 상수를
재사용하고 새 것만 더한다.

에이전트는 `worker.New`의 `runners`에 세 종류를 등록한다. 명령 타임아웃은 복제 잡과 같은 5분이다. 강등의 동기화
대기와, 승격·재합류의 연결 대기는 명세의 기한으로 따로 묶는다.

**되돌리기.** 프레임워크를 H4 이전으로 되돌리지 않는다. 되돌려야 하면 두 조건이 모두 맞을 때만 한다.
- 진행 중인 흐름이 없다.
- 마지막 흐름이 실패한 H4 흐름인 그룹이 없다.

이전 버전은 모르는 흐름 종류를 teardown처럼 판정하고, `acknowledge`가 실패한 전환 그룹을 `registered`로 보낸다.

---

## 7. CLI

```text
ha switchover <group> --approved-by <who> [--reason …] [--incident …]
ha failover   <group> --approved-by <who> --fenced-instance <id> --fence-method stop|terminate --accept-data-loss [--reason …]
ha resume     <group> --approved-by <who> [--standby-stopped-instance <id> --stop-method stop|terminate] [--reason …]
ha rejoin     <group> --approved-by <who> [--reason …]
ha transitions <group>
```

- `ha status`가 보이는 것:
  - 펜싱된 노드(`Fenced: by <flow> (<instance>, stop) at …`)
  - 진행 중 전환의 단계
  - 충돌로 멈췄으면 그 사유와 아직 실행 중인 잡
  - rejoin이 연결하지 못했으면 그 상태와 손 절차 안내(3.7절)
- `ha failover`는 시작 전에 마지막 동기화 관측 시각과 지금까지의 시간을 출력한다
  (`Last sync observed 2026-11-25T04:52:35Z (3m10s ago); writes after it may be lost`, 없으면 `never`).
- `ha transitions`: 요청자·승인자·펜싱 확인자·인스턴스·방법·시각·손실 승인·잡 ID·결과·완료 메시지·소요 시간.
- 로드맵 H4 항목의 옛 `--confirm-fenced`는 `--fenced-instance`/`--fence-method`로 바뀌었다(로드맵 2026-11-26 반영).

---

## 8. 실패와 재요청 정리

| 어디서 | 노드 상태 | 그룹 | 운영자 |
| --- | --- | --- | --- |
| 시작 거절(어느 흐름이든) | 변화 없음 | 그대로 | 사유를 보고 해결 |
| switchover 강등 전 검사 거절 | 변화 없음 | `error` | `acknowledge`(→ `degraded` → 평가기 `protected`) → 재요청 |
| switchover 강등 중 실패·동기화 기한 초과 | 옛 운영: 서비스 정지·언마운트까지 됐을 수 있음 | `error`, 역할 그대로 | `acknowledge` → `ha resume` |
| switchover 승격 실패 | 교대 뒤. 새 운영: Secondary 또는 Primary(서비스 실패) | `error` | `acknowledge` → `ha resume`(새 운영에서) |
| 흐름 중 충돌(2.3절) | 두 노드를 봐야 함. 단계 잡이 시작 전이었으면 끝냄, 실행 중이었으면 취소 요청됨 | `error` | 잡이 끝난 뒤 그 결과와 두 노드를 손으로 확인 → `acknowledge`(대기가 Primary면 거절) |
| failover 승격 실패 | 대기: Secondary 또는 Primary | `error`, 역할 그대로, 펜싱 유지 | 3.4절 재요청 |
| resume 실패 | 운영: Secondary 또는 Primary | `error` | `acknowledge` → 원인 해결 → `ha resume` |
| rejoin 연결 안 됨 | 변화 없음(버리지 않음) | `error`, 펜싱 유지 | 커널 로그 확인 → split-brain이면 손 절차(3.7절) → `acknowledge` → `ha rejoin` |
| rejoin 재동기화가 확인 기한을 넘김·방향 이상 | 연결됨. 방향 이상이면 운영 노드 확인 | `error`, 펜싱 유지 | 재동기화 끝을 기다림 → `acknowledge` → `ha rejoin`(이미 연결되면 끊지 않는다) |
| 잡을 claim한 에이전트가 시작 전에 죽음 | 변화 없음 | 잡 만료 시 `error` | 2026-11-25 수정 전에는 `claimed`로 남아 흐름이 영원히 기다렸다(protocol 3.1) |
| 결과 미확인(보고가 뒷받침 안 함) | 모름 | `error` | 노드 확인 → `acknowledge` |
| 단계 잡 리스 만료(결과 모름) | 모름 | `error` | 그 노드의 보고 중 토큰이 잡 종료 뒤인 것이 올 때까지 `acknowledge`·failover 재요청 거절(4절) → 노드 확인 → `acknowledge` |
| setup 요청 때 펜싱된 노드가 있음 | 변화 없음 | 그대로 | `ha teardown`(펜싱 해제) |
| terminate로 펜싱한 그룹 | 운영 노드 서비스 중 | `degraded` 유지 | H2의 노드 교체까지 그대로(3.3절) |

자동 재시도·자동 되돌림은 없다(로드맵 "실패나 결과 미확인이면 멈춘다").

---

## 9. 검증

**단위·DB 테스트**:

- `DecideFlow` 표:
  - 네 종류의 단계 전진, 교대 시점.
  - 확인 실패·결과 미확인 → `error`, 모르는 종류 → 오류.
  - **확인용 보고가 오래되었거나 토큰이 그 잡 종료 전이면 진행 안 함**(토큰 없음, 잡 전에 잰 측정이 잡 뒤에 도착).
    토큰이 수신 시각보다 늦으면 증거가 아님. 기한이 지났어도 진행 시점에 증거가 있으면 진행.
  - 강등 결과에 최종 동기화 증거가 없거나 미동기량이 비어 있으면 교대 안 함. 헬스 경로가 실패하면 승격 완료 아님.
- 충돌 감시(2.3절):
  - 보고 트랜잭션에서 흐름이 실패하고, 뒤 보고가 증거를 덮지 않음.
  - 표의 각 행. 특히 failover 중 펜싱 노드의 **배정 없는 첫 보고**, rejoin 중 운영이 Primary가 아니게 됨.
  - **failover 중 대기가 실제 Primary인 정상 상태는 멈추지 않음**.
  - 두 세션 시험: 보고 트랜잭션이 그룹 잠금을 기다리는 동안 흐름이 시작되면, 그 보고는 흐름 중 보고로 판정됨.
  - 단계 잡을 잠근 뒤의 상태로 끝냄(`queued`·`claimed`)·취소(`running`)를 고름.
  - 잡 진행 보고는 `last_contact_at`을 바꾸지 않음.
- 시작 거절:
  - 유형별 상태 불충족, 양쪽 동기화 미확인(switchover).
  - 펜싱 기록·손실 승인 없는 failover, 운영 접촉이 신선한 failover(배정 없는 보고 포함), 대기 연결이 `Connected`인
    failover, 대기가 Outdated/Inconsistent.
  - 대기가 펜싱됨(**A→B 장애 전환 뒤 B 고장 → 펜싱된 A로 failover 거절**).
  - 다른 잡이 있을 때, 펜싱 노드가 없을 때의 rejoin.
  - `jobKinds`·`haReportVersion` 없음(정지한 대기는 보지 않음), 펜싱된 노드가 있는 그룹의 `ha setup`.
  - resume에서 연결 안 됨이고 정지 기록 없음(대기의 신선한 `Secondary` 보고가 있어도). 정지 기록인데 대기 접촉이
    신선하거나 운영이 `Connected`.
- 3.4절 재요청:
  - 같은 대상·같은 펜싱이고 토큰이 실패한 흐름의 마지막 잡 종료 뒤인 대기 보고가 있을 때만. 대기가 Primary여도 됨.
  - 펜싱된 운영이 펜싱 뒤에 접촉했으면 90초가 지났어도 거절. 인스턴스가 처음 펜싱 기록과 다르면 거절.
- rejoin:
  - victim이 `Connected`면 끊지 않음. 일반 연결 성공. 끊는 사이 `Connected`가 되면 끊지 않고 끝냄.
  - `StandAlone`·`Connecting`·기한 초과면 버리지 않고 실패.
  - **어떤 명세·실행기 경로도 `--discard-my-data`를 만들지 않음.**
  - 완료는 두 노드 모두 토큰이 마지막 잡 뒤이고 `Established`·`UpToDate`·미동기 0일 때만. 펜싱 노드의 측정이 2단계
    전이면 수신이 그 뒤라도 완료 안 함. 재동기화 중이면 기다리다 기한에 `error`. 방향이 거꾸로면 `error`.
- teardown:
  - 각 단계가 `resourceAbsent`와 조회에 성공한 서비스 비활성으로만 확인됨. 복제 조회 오류로는 확인 안 됨.
  - 완료 때 `fenced`와 `fenced_by_flow`가 함께 지워짐.
- `resourceAbsent`:
  - `/proc/drbd`가 `ENOENT`면 부재, 권한 거부는 오류.
  - `events2 --now`의 끝 표시가 없으면 부재가 아님. 끝 표시가 있고 그 리소스 줄이 없으면 부재(다른 리소스만 떠
    있을 때 포함).
- `acknowledge`:
  - 새 매핑(`registered`는 setup·teardown 실패만).
  - 리스 만료로 끝난 단계 잡 뒤 그 노드가 침묵하면 거절. 토큰이 잡 뒤인 보고가 오면 받음.
- PostgreSQL:
  - 역할 유일 제약 마이그레이션(지연 불가인 옛 표에 반복 적용, 최신 DB에서 잠금 없음).
  - 교대: 2단계 발행과 한 트랜잭션(발행 실패 시 둘 다 롤백), 대상이 두 행이 아니면 롤백, 양방향 교대 두 번,
    재시작 뒤 교대 한 번, 교대 때 보고 열이 비워짐.
  - 펜싱 표시·해제, `last_contact_at`이 배정 없는 보고에도 갱신됨.
  - `last_sync_confirmed_at`이 보고 수신 시각을 따름(`NULL` 포함).
  - 흐름 진행 중 백업 생성 거절, `ha_flows.kind` 제약 마이그레이션.
- 잡 큐(2026-11-25 수정, 테스트 있음):
  - 시작 전에 만료된 claim이 `failed`가 되고 시도도 닫힘.
  - 만료 뒤의 시작 보고는 스위퍼 전이라도 거절. 끝난 잡의 시작 보고 거절. 다른 시도에 넘어간 잡에 옛 시도의 보고
    거절.
  - 두 세션 시험 `TestStartReportRefusedWhenTheJobExpiresWhileItWaits`: 잡 행을 기다리는 사이 유효기간이 지나면
    시작 보고가 거절된다. 기한 검사를 `now()`로 되돌리면 실패함을 확인했다.
- 에이전트:
  - 토큰을 측정 직전에 떠서 보냄(측정 중에 온 새 토큰은 다음 측정에). 시작 응답의 취소를 따름(모든 잡 종류).
  - 강등 전 검사에서 아무것도 안 바뀜. 동기화 기한 초과 시 서비스 정지 상태로 실패.
  - 승격: `--force`를 안 씀, ext4 없으면 포맷 없이 거절, 다른 장치가 마운트돼 있으면 거절, `peer: fenced`·
    `stopped`에서 `Connected`면 거절, `activating`은 기다리고 `failed`는 실패, 단계 직전 진행 보고의 취소로 멈춤,
    상대 Primary면 거절.
  - victim이 Primary·서비스 실행 중이면 아무것도 안 바꾸고 거절.
  - 각 실행기 재실행 멱등.

**실환경** ([ha-test.md](ha-test.md)에 절 추가, AWS 먼저). 11절의 확인 항목을 함께 한다.

1. 계획 전환 A→B, 다시 B→A.
   - 보존: A가 마지막으로 기록한 `writer.log` 줄이 B에 그대로 있고, B가 그 다음 줄부터 쓰는지.
   - 서비스 중단 시간(마지막 성공 응답 → 새 노드의 첫 성공 응답)을 따로 잰다.
2. 장애 전환 — 연결된 채 운영 정지(EC2 강제 중지): 승격, 옛 운영 시작 후 `ha rejoin` → 일반 연결로 끝남.
3. 장애 전환 — 복제 차단 뒤 운영이 쓰다 정지: 승격, 시작 후 `ha rejoin` → 버리지 않고 실패.
   - 손 절차(3.7절)로 풀고 `ha rejoin` 재요청 → 완료.
   - 손실 구간을 `writer.log`로 재고, 요청 당시 화면에 보인 마지막 동기화 관측 시각과 비교한다.
4. 운영 재부팅 → `ha resume`. 대기도 꺼 둔 경우는 정지 기록과 함께 하고, 대기가 돌아온 뒤 `ha rejoin`.
5. 펜싱으로 중지한 인스턴스를 다시 시작한 뒤 데이터 볼륨 `by-id` 이름 유지(ha-h0 13절 남은 항목).

EC2 정지·시작은 운영자가 콘솔에서 한다(이 환경의 자동화에는 그 권한이 없다).

---

## 10. 결정

| # | 항목 | 결정 | 이유 |
| --- | --- | --- | --- |
| A | switchover 역할 교대 시점 | 강등 확인 직후. 확인은 잡 결과의 최종 동기화 증거 + 잡 뒤에 잰 신선한 보고(2.2절) | 승격 실패 시 `resume`이 출구가 된다. 완료 때 교대하면 출구가 없다 |
| B | 전환 뒤 그룹 상태 | `degraded`, 평가기가 `protected`로. 걸리는 시간 상한은 두지 않는다 | 새 역할의 보고 전에는 보호를 확인할 수 없다 |
| C | failover 승격 실패 뒤 | 마지막 흐름이 같은 대상의 실패한 failover이고, 같은 펜싱이 유지되며, 토큰이 실패 뒤인 대기 보고가 있고, 펜싱 뒤 운영의 접촉이 없으면 재요청 허용(상태 무관, 대기가 Primary여도) | 3.4절. `acknowledge`가 역할을 바꾸지 않게 한다 |
| D | 재합류에서 데이터 버리기 | **잡은 버리지 않는다**(2026-11-26, 개정 6). rejoin은 일반 연결만 하고, split-brain이면 실패한다. 운영자가 커널 로그로 확인하고 H0 9절 순서로 손으로 푼 뒤 재요청한다 | 자동으로 버리려면 이번 시도의 split-brain 판별(커널 로그 기준점), 남는 버리기 요청, 3단계 재연결, 승인 상속이 필요하다. 프로토타입에서는 운영자 확인이 더 단순하고 안전하다. 자동화 설계는 13절에 남겼다 |
| E | failover 요청 때 운영 접촉(`last_contact_at`)이 신선 | 거절. 흐름 중에 그 노드의 보고가 기록되면 흐름 중단 | 펜싱 기록과 모순되는 증거 |
| F | 강등 동기화 기한 초과 | 서비스 정지 상태로 실패, 자동 재기동 없음 | 되돌림도 실패할 수 있다. `resume`이 명시적 경로이고 3.6절 조건을 따른다 |
| G | `--approved-by` | 네 전환 명령 모두 필수. 펜싱·정지 기록의 확인자는 승인자 | `HAApproval` 주석("Every switch needs one") |
| H | 재합류에서 펜싱 노드가 Primary | 거절, 아무것도 바꾸기 전에 검사 | 손으로 승격한 노드의 변경분을 버릴지는 잡이 정하지 않는다 |
| I | resume의 대기 확인 | 실행 시점에 운영 노드가 직접 본 `Connected`+상대 `Secondary`가 아니면 정지 기록을 요구하고 대기를 펜싱. 대기 접촉이 신선하거나 운영이 `Connected`면 정지 기록도 거절. 그 뒤 대기의 보고는 흐름 중단 | 대기의 `Secondary` 보고는 그 뒤의 손 승격을 배제하지 못한다 |
| J | 에이전트 확인 | 잡을 받을 노드의 `jobKinds`와, 확인 대상 노드의 `haReportVersion` ≥ 2를 모든 흐름이 시작 때 확인. 배포는 같은 빌드를 에이전트 먼저, 흐름이 없을 때 프레임워크 | 계획 전환 도중에 드러나면 서비스가 멈춘 채 남고, 구버전 보고로는 확인이 끝나지 않는다 |
| K | 단계 진행 | 주기 루프만(개정 6). 보고 직후 진행·두 번 잇달아 보고·응답 배정 재조회는 13절 | 프로토타입에서 먼저 중단 시간을 재고 정한다 |
| L | 충돌 감시 위치 | 보고 기록 트랜잭션, 그룹 잠금 뒤에 판정. "흐름 중의 보고"는 잠금 순서로 정한다. 접촉 시각(`last_contact_at`, 헬스 보고만)도 기록하고 시작 검사가 쓴다 | 진행 때만 보면 뒤 보고가 증거를 덮고, 재부팅한 노드의 첫 보고는 기록되지 않는다 |
| M | 단계 확인의 인과 | **확인 토큰**. 프레임워크가 헬스 응답마다 DB 시각을 주고, 에이전트는 측정 직전에 가진 토큰을 보고에 싣는다. 토큰이 그 잡(단계를 실행하지 않은 노드는 흐름의 마지막 잡)의 종료 뒤인 보고만 쓴다 | 잡 전에 잰 측정이 잡 뒤에 도착할 수 있다. DB 시계끼리만 비교하고 저장이 필요 없다 |
| N | terminate로 펜싱한 노드가 있는 그룹 | **H2로 미룬다**(2026-11-26, 개정 6). 그룹은 `degraded`로 남고 운영 노드는 서비스한다. 개정 3~5의 `ha dissolve` 설계는 13절에 남겼다 | 데모는 다시 켤 수 있는 `stop` 펜싱이다. 해제는 `dissolved` 상태, 노드 행 삭제, 노드 없는 그룹 읽기를 끌어들여 H4에서 가장 큰 덩어리였다. H2의 노드 교체와 함께 하는 편이 자연스럽다 |
| O | 펜싱된 노드가 있는 그룹의 `ha setup` | 거절. teardown(펜싱 해제)이 먼저 | teardown 실패 뒤 `acknowledge`로 `registered`가 된 그룹에 펜싱이 남을 수 있다 |
| P | 흐름 실패 때 단계 잡 | 그룹 잠금 뒤 잡 행을 잠그고 그 상태로 끝냄(시작 전)·취소 요청(`running`)을 고른다(`job.CancelTx`). 에이전트는 시작 응답의 취소를 따르고, `ha-promote`는 데이터에 영향을 주는 세 단계 직전에 동기로 확인한다 | 잠그기 전 상태로 고르면 그 사이 시작한 잡의 취소가 빠진다. 지금 에이전트는 시작 응답의 취소를 버린다 |
| Q | rejoin의 방향 | 운영 노드가 내내 Primary이고 `after-sb-*`·`rr-conflict`가 기본값(`disconnect`)인 것을 구성 전제로 둔다. 흐름 중 운영이 Primary가 아니게 되면 충돌. 실행기의 정책 검사는 13절 | DRBD는 받을 변경분이 있는 Primary를 재동기화 대상으로 만들지 않는다(`drbd_receiver.c`) |
| R | `resourceAbsent` 판별 | `/proc/drbd`가 없음(`ENOENT`), 또는 `drbdsetup events2 --now`의 완전한 덤프(끝 표시 `exists -`)에 그 리소스가 없음. 인자 없는 `status --json`의 빈 목록은 쓰지 않는다 | 모듈이 없으면 어떤 리소스도 떠 있을 수 없다. `status`는 커널 조회가 실패해도 빈 목록과 0을 돌려준다(drbd-utils 9.36.0) |
| S | 설계 고정 | 개정 6에서 고정한다(2026-11-26). 이후의 검토 지적은 13절에 쌓고, 1.1절 안전 불변식을 깨는 결함만 문서를 고친다 | 검토를 거듭할수록 새로 넣은 장치에서 더 드문 경우가 나와 설계가 프로토타입에 비해 깊어졌다 |

---

## 11. 실환경에서 확인할 것 (다음 AWS 시험)

1. rejoin 일반 연결: 연결된 채 멈췄던 노드가 `connect`만으로 따라잡는지(H0 8.1 재확인). 재동기화 시간이 확인 기한
   (90초) 안인지.
2. split-brain 손 절차(3.7절)를 rejoin 실패 뒤에 했을 때 풀리고, 재요청한 rejoin이 완료되는지.
   - split-brain 줄의 실제 형식(`drbd <리소스>/<볼륨> <장치> <상대 호스트>:`)과, 일반 `connect`로 다시 감지했을 때
     같은 줄이 새로 찍히는지.
   - 감지 직후 두 노드의 연결 상태(양쪽 `StandAlone`인지, 먼저 감지한 쪽만인지).
3. 강등의 최종 동기화 확인이 서비스 정지 직후 몇 초 안에 끝나는지(기한 120초의 근거).
4. 승격 뒤 서비스 `active`와 포트 열림·헬스 응답 사이의 시간(완료 확인이 둘을 요구한다).
5. 주기 루프만 쓸 때(2.4절)의 계획 전환 서비스 중단 시간. 길면 13절의 "보고 직후 진행"을 넣는다.
6. `resourceAbsent` 판별(6절).
   - teardown 뒤 `drbdsetup events2 --now all`이 끝 표시 `exists -`로 끝나고, 그 리소스 줄이 없는지(다른 리소스가
     떠 있을 때와 하나도 없을 때).
   - 커널 조회를 막았을 때 끝 표시 없이 끝나는지와 종료 코드.
   - 모듈을 내린 노드에서 `/proc/drbd`가 `ENOENT`인지.
7. 구성 전제(결정 Q): H3b가 만든 리소스에서 `drbdsetup show --show-defaults`가 보이는 `after-sb-0pri`·`after-sb-1pri`·
   `after-sb-2pri`·`rr-conflict` 값(기본 `disconnect`).

---

## 12. 검토 이력

| 날짜 | 내용 |
| --- | --- |
| 2026-11-25 | 초안 |
| 2026-11-25 | 개정 1 — codex 검토 16건 반영. 승격 대상의 펜싱 검사, 흐름 중 충돌 감시, 재합류 완료의 방향 확인, resume의 정지 기록, 역할 유일 제약의 지연 검사, 확인 보고의 신선도와 강등 결과의 동기화 증거, 모르는 흐름 종류 거절, `jobKinds`, failover 재요청 조건, `last_sync_confirmed_at`, 승격 실행기의 `--force`·포맷 배제, teardown 때 펜싱 해제. 같은 날 잡 큐 결함(시작 전에 만료된 claim)을 고쳤다(protocol 3.1) |
| 2026-11-25 | 개정 2 — codex 재검토 반영. 결정 D 철회(일반 연결 먼저, 이번 시도의 split-brain 확인 + 손실 승인 때만 버림), resume은 운영 노드가 직접 본 연결로만(결정 I), 충돌 감시를 보고 트랜잭션으로(결정 L), 단계 확인의 인과(결정 M). 같은 날 진행 보고를 잡 행 잠금 트랜잭션으로 고쳤다 |
| 2026-11-26 | 개정 3 — terminate로 펜싱한 노드가 있는 그룹의 해제(`ha dissolve`)를 넣었다(결정 N) |
| 2026-11-26 | 개정 4 — codex 3차 검토(13건) 반영. `haReportVersion`, rejoin 완료의 노드별 인과, 커널 로그 커서, 손실 승인을 펜싱에 묶음(`fenced_by_flow`), 잠금 순서 판정, 단계 잡을 잠근 뒤 끝냄·취소(결정 P), 펜싱된 노드가 있으면 setup 거절(결정 O). 같은 날 진행 보고의 기한 검사를 `statement_timestamp()`로 고쳤다 |
| 2026-11-26 | 개정 5 — codex 4차 검토(13건)를 DRBD 9.3.4·drbd-utils 9.36.0 소스로 대조해 반영. 확인 토큰(결정 M), rejoin 방향 보증(결정 Q), 남는 버리기 요청의 수명, `resourceAbsent`를 모듈 미적재 또는 `events2` 완전 덤프로(결정 R), 단계 직전 취소 확인, 보고 경로의 소속 재확인. 같은 날 잡 큐 수정에 두 세션 시험을 더했다 |
| 2026-11-26 | **개정 6 — 프로토타입 범위, 설계 고정**(결정 S). codex 5차 검토(프로토타입 관점)에서 막는 문제는 0건이었고, 그 범위안대로 줄였다. 잡의 데이터 버리기를 빼고 손 절차로(결정 D), 해제(`dissolve`)를 H2로(결정 N), 보고 직후 진행을 주기 루프로(결정 K), 정책 검사를 구성 전제로(결정 Q), 단계 직전 취소 확인을 `ha-promote`만(결정 P), rejoin 완료를 양쪽 `Established`만으로. 안전 불변식(1.1절)과 강화 목록(13절)을 두었다. 개정 5 원문은 git에 없어, 미룬 설계의 요점을 13절에 옮겼다 |

---

## 13. 강화 목록 (설계 고정 뒤)

프로토타입 범위에서 미룬 것과 이후 검토 지적을 여기에 쌓는다. 각 항목은 구현을 시작할 때 설계를 다시 연다.

**미룬 기능**

- **잡의 데이터 버리기(rejoin 자동화).** 개정 5의 설계 요점:
  - 펜싱 노드 2단계에서 일반 연결이 `StandAlone`으로 끝나면, 이번 시도의 split-brain 줄을 커널 로그로 판별한다.
    - 기준점은 connect 직전에 잡는 커서(`journalctl -k -b --show-cursor -n 0`), 부팅 ID, 커널 단조 시각
      (`_SOURCE_MONOTONIC_TIMESTAMP`)이다.
    - 기준점을 못 잡거나(systemd 255에서 `-n 0`이 커서를 안 낼 수 있음) 줄이 없으면 실패한다.
  - 손실이 그 펜싱에 승인됐을 때만(`fenced_by_flow`의 전환 기록) 버린다. 순서는 1의 검사 재확인 →
    `disconnect`/`secondary`/`connect --discard-my-data` → 3단계 운영 재연결.
  - resume에서 온 펜싱의 승인은 `ha rejoin --accept-data-loss`가 그 펜싱의 기록에 더한다. 처음 기록은 보존하고,
    같은 펜싱의 재요청이 물려받는다.
  - **남는 버리기 요청(DRBD 9.3.4 소스로 확인).** `connect --discard-my-data`는 연결 객체에 표시를 켠다(`drbd_nl.c`
    `drbd_adm_connect`).
    - 꺼지는 때는 둘뿐이다: UUID를 주고받아 재동기화를 판단할 때(`drbd_receiver.c`), 그 노드가 Primary가 될 때
      (`drbd_nl.c`).
    - 일반 `connect`·`disconnect`는 끄지 않고, `down`은 연결 객체와 함께 없앤다.
    - 그 표시는 그 노드를 재동기화 대상으로 만들 뿐이다. 운영 쪽은 방향 보증(결정 Q)으로 덮이지 않는다.
  - 완료 확인은 섞인 상태(`SyncSource`/`Established` 등)를 방향이 맞으면 받을 수 있다.
- **해제(`ha dissolve`, 개정 3~5).** H2의 노드 교체와 함께 한다. 요점:
  - 명령: `ha dissolve <group> --absent-node <system> --approved-by <who> [--terminated-instance <id>]`.
  - 시작 조건:
    - 사라진 노드가 펜싱된 대기다.
    - 종료 기록이 있다: 펜싱이 terminate였거나, 요청이 지금 기록한다(인스턴스 일치. 실패한 해제의 기록은 물려받지
      않음).
    - 접촉이 종료 기록 뒤에 없다(지금 기록이면 `STALE_AFTER`).
    - 남은 노드가 `Connected`가 아니다.
  - 남은 노드만 `replication-teardown`을 하고, 명세 `requirePeerAbsent`로 서비스를 멈추기 전에 `Connected`면
    거절한다. 확인은 `resourceAbsent`와 조회에 성공한 서비스 정지다.
  - 완료(한 트랜잭션): 그룹 `dissolved`(끝 상태), `ha_nodes` 두 행 삭제, 이벤트 `node_removed`(시스템 ID·역할·노드
    ID·주소 포함)·`group_dissolved`. 그룹·흐름·전환 기록·이름은 남긴다.
  - 따라오는 것:
    - 노드 없는 그룹을 한 문장이나 읽기 전용 `REPEATABLE READ`로 읽기(노드는 `dissolved`면 0개, 아니면 2개).
    - `EvaluateAll`·`AdvanceAll`이 거르고 잠근 뒤 다시 보기.
    - 보고 경로가 그룹 잠금 뒤 소속을 다시 확인하기.
    - `acknowledge` 매핑에 `dissolve` 추가, 북향 계약 `HAGroupInfo`에 `dissolved` 표현.
  - 남은 노드의 데이터로 다시 보호하는 것은 H2의 대기 시드다.
- **보고 직후 진행(2.4절).** 다음 넷이다.
  - 기록된 보고의 트랜잭션이 커밋된 뒤 `AdvanceFlow`를 부른다.
  - 응답 배정은 기록 여부와 상관없이 마지막에 다시 읽는다.
  - 잡 결과 뒤 에이전트가 두 번 잇달아 보고한다(둘째가 잡 뒤 토큰으로 잰 것. 결과 API의 204는 그대로).
  - 첫 토큰을 받으면 바로 다시 잰다.
- **rejoin 정책 검사(결정 Q).** 실행기가 두 쪽에서 `drbdsetup show --show-defaults <리소스>`의 `after-sb-0pri`·
  `after-sb-1pri`·`after-sb-2pri`·`rr-conflict`가 모두 `disconnect`가 아니면 거절한다. 손으로 만든 리소스(H3b 채택
  경로)를 쓸 때 필요하다.
- **모든 실행기의 단계 직전 동기 취소 확인**(지금은 `ha-promote`만).
- **버전 사이의 섞인 운영과 되돌리기**(지금은 같은 빌드 배포 절차).

**기존 결함 (H4와 별개로 고칠 것)**

- 결과 기록(`RecordResult`)은 시도 종료와 잡 종료를 따로 커밋한다.
  - 그 사이 프레임워크가 멈추면 잡은 `running`으로 남고, 같은 결과를 다시 보내도 잡을 끝내지 않는다
    (`reconcileRepeatedResult`).
  - 또 `finishJobSQL`에는 현재 시도 조건이 없다. 위험한 순서는 이것이다: 옛 시도 A가 현재일 때 시도 종료 커밋 →
    스위퍼 재배정(`claimed but never started`) → 새 시도 B claim → A의 `finishJobSQL`이 B의 잡을 끝냄.
  - 잡 → 시도 순서의 한 트랜잭션으로 묶는다. 재현은 하지 않았다.
- 스위퍼의 "running but abandoned"는 시도의 리스를 잠그지 않고 읽는다. 스위퍼 여러 개의 동시 실행은 순서가 없다
  (지금은 서버 하나). 백업 생성의 흐름 확인과 흐름 시작의 잡 확인 사이 경합(protocol 3.4).
- H3b 구성기의 `resourceState`는 인자 없는 `drbdsetup status --json`의 "성공 + 빈 목록"을 "아무것도 안 떠 있음"으로
  본다(`replication.go`).
  - 커널 조회가 실패해도 그렇게 나오므로, 해제 실행기가 리소스를 내리지 않고 성공할 수 있다.
  - 확인(4절)이 `resourceAbsent`를 요구하므로 펜싱이 잘못 풀리지는 않는다.
  - drbd-utils 9.36.0 소스로 확인한 경로이고, 실환경에서 관측한 적은 없다. 6절의 판별로 바꾼다.
- 두 세션 시험은 진행 보고의 기한 한 건뿐이다. 진행 보고와 스위퍼, 결과 기록과 재배정을 맞물리는 시험이 없다.
  - 진행 보고가 기다릴 수 있는 시도 행 쓰기는 모두 토큰을 바꾸거나 시도를 끝낸다. 그래서 잠금 뒤 재검사가
    거절한다(4차 검토 판정).

**이후 검토 지적** — 개정 6 뒤의 지적은 이 아래에 날짜와 함께 한 줄씩 쌓는다.
