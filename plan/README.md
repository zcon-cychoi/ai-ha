# plan/

AI-HA 백업/복구의 **설계·계획 문서**. 우리가 작성한 것이며, 연구과제 원본 문서와
분리되어 있다.

| 위치 | 내용 |
| --- | --- |
| `plan/` | 우리가 만든 설계·계획 (이 디렉터리) |
| `docs/` | **연구과제 원본 문서** (`req.docx`, `dev.pdf`) — 수정하지 않는다 |

---

## 읽는 순서

| 문서 | 내용 |
| --- | --- |
| [roadmap.md](roadmap.md) | **1차년도 데모 로드맵.** 구현 순서, 고정 시나리오, 구현하지 않을 것 |
| [ha-roadmap.md](ha-roadmap.md) | **이중화 구현 로드맵.** TM-AI-HA-02 기준 H0~H4 순서, 코드 변경 지점, 설계서 보강 사항, 결정 사항 |
| [ha-h0.md](ha-h0.md) | 이중화 H0 런북 — 클라우드 VM에서 DRBD 동작을 수동 검증하고 픽스처를 회수하는 절차와 결과 표(AWS 완료), 다른 클라우드 보완 확인(GCP·Azure 모듈 적재와 `by-id` 확인) |
| [ha-test.md](ha-test.md) | 이중화 실환경 시험 절차 — 데이터 디스크 인벤토리, 그룹 등록·헬스 판정, 복제 구성·실패 후 재요청·해제, 장애 주입, 재부팅, 결과 표 (H1·H3b 검증) |
| [test-setup.md](test-setup.md) | 테스트 환경 구성 — AWS EC2 백업·복구 (저장소: AWS S3 / 사내 MinIO) |
| [architecture.md](architecture.md) | 프레임워크/에이전트 역할 경계, 바이너리 구성, 요구사항 매핑 |
| [protocol.md](protocol.md) | 남향 롱폴링 프로토콜, 작업 실행 계약, 잡 상태머신, 북향 API |
| [data-model.md](data-model.md) | 정본 분리, 식별자, 레이아웃 모델, 백업 세트 |
| [backup.md](backup.md) | 백업 방식 (원본 설계 노트) |
| [Restore.md](Restore.md) | 복구 시퀀스, 보안 계약, 재조정 항목 |
| [open-decisions.md](open-decisions.md) | 미결정 사항 |

처음 보는 경우 `roadmap.md` → `architecture.md` 순으로 읽으면 된다.

---

## 확정된 사항

각 문서에 근거와 함께 기술되어 있다.

- 프레임워크↔에이전트: **아웃바운드 롱폴링 작업큐** (`protocol.md` 1절)
- 이력 정본: **프레임워크 DB(PostgreSQL)** + 세트별 **불변 매니페스트** (`data-model.md` 1절)
- 바이너리: 소스 `aiha-framework` / `aiha-agent` 2개, 산출물 3개 —
  레스큐용 경량 빌드는 빌드 태그로 만든다 (`architecture.md` 3절)
- 인증: **등록 토큰(TTL·1회) → 에이전트 토큰**, 자격 4종 분리 (`open-decisions.md` 8번)
- 복구 매핑: 목표는 **논리 참조 기반 선언적 레이아웃** + `fsUUID` 보존 (`data-model.md` 4절).
  1차년도 구현은 같은 이미지 전제로 **PARTUUID로 파티션을 찾고 원본 UUID·label로 포맷** (`Restore.md` 3.8)
- 저장소: 프레임워크가 **등록·암호화 보관**하고 잡 claim 때만 전달. 복구 잡은 **세트 ID만으로** 생성
  (`roadmap.md` Phase 3, `protocol.md` 2.5)
- 복구 후 에이전트 **자격 격리** 규칙 (`Restore.md` 5.4) — 자격 파일을 백업에서
  제외하는 1차 방어는 구현됨
- 1차년도 대상: **Mode A**, Ubuntu 24.04 / x86_64 고정 시나리오 (`roadmap.md` 3절)
