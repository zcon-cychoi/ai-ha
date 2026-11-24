# 테스트 환경 구성 — AWS EC2 백업·복구 (저장소: AWS S3 / 사내 MinIO)

사내 VM(Ubuntu 24.04)을 프레임워크 서버로 두고, **AWS EC2 인스턴스를 백업해 다른 EC2 인스턴스로
복구**하는 절차다. 백업 저장소는 **AWS S3** 또는 **사내 MinIO** 중 하나(또는 둘 다)를 쓴다.

| 경로 | 무엇을 확인하나 | 절 |
| --- | --- | --- |
| **A. 백업** | 원본 EC2에 에이전트 설치·등록 → 인벤토리 보고 → 백업 잡 → 등록된 저장소로 백업 | 3절 |
| **B. 복구** | 타깃 EC2를 레스큐로 부팅 → claim으로 복구 정보·저장소 정보 수령 → 백업 세트를 디스크에 복구 → 진행·결과 보고 | 4절 |

- **인스턴스는 모두 AWS EC2다.** 원본(백업 대상)과 타깃(복구 대상) 모두 같은 AMI의 Ubuntu 24.04다.
  타깃은 **원본과 같은 AMI로 새로 만든 인스턴스**이고, 레스큐 에이전트가 그 파티션을 **포맷한다**.
- **저장소는 프레임워크에 등록한다(2.6).** 백업 잡은 등록된 저장소를 지정하고, 에이전트(원본 EC2)와
  레스큐(타깃 EC2)는 claim할 때 그 저장소의 주소와 자격증명을 받는다. EC2에 저장소 설정을 둘 필요가 없다.
- **경로 B는 경로 A의 결과를 쓴다.** 복구 잡은 백업 세트 ID만으로 만든다 — 원본 시스템과 저장소는
  프레임워크가 백업 잡 기록에서 찾는다(4.2).

| 저장소 | 등록 | EC2에서 닿는 길 | 비고 |
| --- | --- | --- | --- |
| **AWS S3** | `--provider AWS --region <리전>` | 아웃바운드 HTTPS(443) | 같은 리전이면 빠르고 전송 비용이 없다 |
| **사내 MinIO** | `--provider Minio --endpoint http(s)://<외부주소>:<포트>` | 사내망 포트포워딩 | 원본·타깃 EC2 모두 외부 주소로 닿아야 한다 |

범위와 설계는 [roadmap.md](roadmap.md), [protocol.md](protocol.md),
[Restore.md](Restore.md) 5절을 따른다.

---

## 0. 전체 구성

```
[개발 PC]  make build → aiha-framework, aiha-agent, aiha-rescue (정적 바이너리)

[사내망]
  사내 VM:  PostgreSQL 18 (컨테이너, 127.0.0.1 전용)
            aiha-framework server :8443 ──── 포트포워딩 https://<외부주소>:<외부포트>
  사내 MinIO :9000 ─────────────────────── 포트포워딩 http(s)://<외부주소>:<MinIO외부포트>
            (MinIO 저장소를 쓸 때, 레스큐 이미지·에이전트 배포에도 사용)

[AWS]
  원본 EC2 (Ubuntu 24.04)                         타깃 EC2 (같은 AMI)
    aiha-agent run (root, 상주)                     user-data(aiha-init.sh) → 레스큐 부팅
    → 등록 · 인벤토리 · 롱폴 · 백업                 → autorun2 → aiha-rescue rescue
    │                                               → claim → 포맷 → 복구 → 진행·결과 보고
    │ 잡 claim 때 저장소 주소·자격증명 수령            ▲ claim 때 저장소 주소·자격증명 수령
    ▼                                               │ rclone cat (매니페스트·백업 파일)
  [저장소: AWS S3 버킷  또는  사내 MinIO 버킷] ──────┘
```

| 준비물 | 어디에 | 필요한 경로 |
| --- | --- | --- |
| `aiha-framework`, PostgreSQL 18, 자체 서명 인증서, 비밀 키 파일 | 사내 VM | A, B |
| 프레임워크 포트포워딩 (`<외부포트>` → 사내 VM:8443) | 공유기/방화벽 | A, B — EC2가 밖에서 들어온다 |
| 저장소: S3 버킷 + IAM 액세스 키 **또는** MinIO 버킷 + 키(+ 포트포워딩) | AWS / 사내 | A, B |
| 원본 EC2 + `aiha-agent` | AWS | A |
| 타깃 EC2 (원본과 같은 AMI) | AWS | B |
| `aiha-rescue`, `setup-sysres.sh`, `sysres.tar` | 사내 MinIO 또는 S3 (presigned URL) | B |
| `sh/aiha-init.sh` (값 채운 것) | 타깃 EC2 user-data | B |

---

## 1. 개발 PC — 빌드와 배포

```bash
make build VERSION=test          # bin/aiha-framework, bin/aiha-agent, bin/aiha-rescue
scp bin/aiha-framework <user>@<사내VM>:~/aiha/
scp -i <키페어.pem> bin/aiha-agent ubuntu@<원본EC2 공인IP>:/tmp/
```

받는 쪽에서 실행 권한을 확인한다(`chmod +x ~/aiha/aiha-framework`). scp는 보통 모드를
유지하지만 umask에 따라 달라진다.

- CGO 없이 정적 빌드된다. 사내 VM·EC2가 x86_64가 아니면 `GOARCH`를 맞춘다(고정 시나리오는 x86_64).
- `bin/aiha-rescue`는 레스큐 전용 슬림 빌드다(백업 엔진·저장소 백엔드 제외, 저장소는 rclone으로
  읽는다). 경로 B에서 presigned URL로 내려받게 하는 것이 **`bin/aiha-rescue`** 다 — aiha-init.sh가
  `AGENT_URL`로 받아 `/autorun/aiha/aiha-rescue`에 둔다.

---

## 2. 사내 VM — 프레임워크 서버

### 2.1 PostgreSQL 18 (Docker Compose)

**Docker 설치** — Docker 공식 저장소 기준 (Ubuntu 24.04):

```bash
sudo apt-get update
sudo apt-get install -y ca-certificates curl
sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
sudo chmod a+r /etc/apt/keyrings/docker.asc
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" | sudo tee /etc/apt/sources.list.d/docker.list > /dev/null
sudo apt-get update
sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
docker compose version
```

`sudo` 없이 쓰려면 `sudo usermod -aG docker $USER` 후 다시 로그인한다. docker 그룹은 root와
같은 권한이라는 점만 알고 쓴다.

**compose 파일** — `~/aiha/postgres/compose.yaml`:

```yaml
services:
  postgres:
    image: postgres:18
    container_name: aiha-postgres
    restart: unless-stopped
    environment:
      POSTGRES_USER: aiha
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?set POSTGRES_PASSWORD in .env}
      POSTGRES_DB: aiha
    ports:
      - "127.0.0.1:5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U aiha -d aiha"]
      interval: 5s
      timeout: 3s
      retries: 10

volumes:
  pgdata:
```

**비밀번호** — 같은 디렉터리의 `.env` (0600). 명령줄에 비밀번호를 쓰지 않도록 바로 생성한다:

```bash
mkdir -p ~/aiha/postgres && cd ~/aiha/postgres
( umask 077; printf 'POSTGRES_PASSWORD=%s\n' "$(openssl rand -hex 24)" > .env )
```

16진수라 2.3의 DSN에 넣을 때 URL 인코딩이 필요 없다.

**기동과 확인**:

```bash
cd ~/aiha/postgres
docker compose up -d --wait                  # 헬스체크를 통과할 때까지 기다린다
docker compose exec postgres psql -U aiha -d aiha -Atc 'show server_version'
docker compose port postgres 5432            # 127.0.0.1:5432 여야 한다
```

주의할 점:

- **볼륨은 `/var/lib/postgresql`에 붙인다.** PostgreSQL 18부터 공식 이미지의 데이터 경로가
  `/var/lib/postgresql/18/docker`로 바뀌었다. 17 이하 예제처럼 `/var/lib/postgresql/data`에
  붙이지 않는다.
- **포트는 `127.0.0.1`에만 연다.** Docker가 연 포트는 ufw 규칙을 거치지 않으므로 `"5432:5432"`로
  쓰면 ufw를 켜 두어도 외부에 노출된다. **5432는 포트포워딩하지 않는다.**
- `restart: unless-stopped`라 VM이 재부팅돼도 Docker 서비스와 함께 다시 뜬다.
- 데이터는 이름 있는 볼륨(`postgres_pgdata`)에 남는다. `docker compose down`은 데이터를
  남기고, `docker compose down -v`는 지운다.

**테이블은 `aiha-framework`가 시작할 때 만든다.** 있으면 만들지 않는다. 이전 버전이 만든
`restore_jobs`에는 진행·결과 보고 컬럼과 `repository_id`를 추가하고, 평문 자격증명이 들어 있던
`repository` 컬럼은 **삭제한다** — 그 전에 만든 복구 잡은 claim할 수 없으니 다시 만든다.
(컬럼을 지워도 PostgreSQL은 그 공간을 바로 회수하지 않는다. 남은 흔적까지 없애려면
`VACUUM FULL restore_jobs`.)

| 테이블 | 무엇 |
| --- | --- |
| `registration_tokens`, `agents` | 에이전트 등록 |
| `jobs`, `attempts` | 백업잡 큐와 실행 시도 |
| `system_inventories` | 시스템별 구성 정보 (시스템당 1행) |
| `repositories` | 등록된 저장소. 자격증명은 비밀 키로 암호화 |
| `restore_jobs` | 복구 잡과 레스큐 에이전트가 보고한 진행·결과 (경로 B) |

### 2.2 작업 디렉터리, 인증서, 비밀 키

```bash
mkdir -p ~/aiha && chmod 700 ~/aiha && cd ~/aiha
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -days 365 -subj /CN=aiha-framework -keyout framework.key -out framework.crt
chmod 600 framework.key
( umask 077; openssl rand -hex 32 > secret.key )     # 저장소 자격증명 암호화 키
```

에이전트가 프레임워크 인증서를 확인하는 방식은 `AIHA_FRAMEWORK_CERT_PIN`(2.3)으로 정한다.
`agent token issue`(3.1)와 `restore create`(4.2)가 이 설정에 맞는 값을 출력하므로, 에이전트 쪽에
지문을 손으로 옮길 일은 없다.

| `AIHA_FRAMEWORK_CERT_PIN` | 인증서 | 에이전트의 확인 방식 |
| --- | --- | --- |
| `true` (기본) | 위의 자체 서명 인증서 | **인증서 DER의 SHA-256 지문**만 확인한다. CN·도메인은 상관없고 시계가 틀어져 있어도 영향 없다 |
| `false` | 공인 인증서(Let's Encrypt 등) | OS 신뢰 저장소로 체인과 `AIHA_FRAMEWORK_PUBLIC_URL`의 호스트 이름을 확인한다. 지문은 쓰지 않는다 |

- `true`에서 **인증서를 다시 만들면 지문이 바뀌므로**, 그 전에 등록한 에이전트와 만들어 둔 복구
  잡의 값은 쓸 수 없게 된다.
- `false`는 `PUBLIC_URL`이 인증서에 적힌 **공인 도메인**이어야 하고(IP 주소 불가), 원본 EC2와
  레스큐 이미지에 CA 번들이 있고 시계가 맞아야 한다. 인증서 갱신은 지문과 무관하지만, 갱신된
  인증서를 읽으려면 서버를 다시 시작한다.

`secret.key`는 DB에 저장하는 저장소 자격증명(S3 IAM 키, MinIO 키)을 암호화한다. **DB와 따로
보관한다**(DB 백업에 같이 넣으면 암호화한 의미가 없다). 잃어버리면 새 키를 만들고 저장소마다
자격증명을 다시 입력한다(2.6의 `update-credentials`) — 저장소 ID가 그대로라 기존 잡·백업 세트는 유효하다.

### 2.3 설정 파일 `~/aiha/framework.env` (0600)

```bash
AIHA_FRAMEWORK_PORT=8443
AIHA_FRAMEWORK_TLS_CERT=/home/<user>/aiha/framework.crt
AIHA_FRAMEWORK_TLS_KEY=/home/<user>/aiha/framework.key
# 자체 서명 인증서면 true, 공인 인증서면 false (2.2)
AIHA_FRAMEWORK_CERT_PIN=true
AIHA_FRAMEWORK_DB_DSN='postgres://aiha:<비밀번호>@127.0.0.1:5432/aiha?sslmode=disable'
AIHA_FRAMEWORK_SECRET_KEY_FILE=/home/<user>/aiha/secret.key
# 관리 API를 열 토큰 파일 (2.7). 비워 두면 관리 API는 열리지 않는다
AIHA_FRAMEWORK_ADMIN_TOKEN_FILE=/home/<user>/aiha/admin.token
AIHA_FRAMEWORK_LOG_LEVEL=info

# EC2가 프레임워크에 닿는 외부 주소 (2.5의 포트포워딩). agent token issue와 restore create가 출력한다
AIHA_FRAMEWORK_PUBLIC_URL=https://<외부주소>:<외부포트>
# 수동 테스트는 인스턴스 생성·부팅·SSH까지 시간이 걸리므로 넉넉하게 (기본 30m)
AIHA_FRAMEWORK_CLAIM_TTL=2h
AIHA_FRAMEWORK_CLAIM_REDELIVERY=10m

# --- 백업잡 큐 (기본값 그대로 두어도 된다) ---
AIHA_FRAMEWORK_JOB_TTL=24h
AIHA_FRAMEWORK_JOB_CLAIMED_TIMEOUT=5m
AIHA_FRAMEWORK_JOB_PROGRESS_INTERVAL=10s
AIHA_FRAMEWORK_JOB_LEASE=90s
AIHA_FRAMEWORK_JOB_SWEEP_INTERVAL=30s
```

- DSN의 비밀번호는 2.1 `~/aiha/postgres/.env`의 `POSTGRES_PASSWORD` 값이다. DSN은
  **작은따옴표로 감싼다.** 비밀번호를 직접 정했다면 `@ : / ? # %`는 URL 인코딩해야 한다.
- `chmod 600 ~/aiha/framework.env` — DB 비밀번호가 들어 있다.
- `CERT_PIN`은 `true`/`false`만 받는다. 다른 값은 오류다 — 오타가 "꺼짐"으로 읽혀 확인이 약해지지 않게 하려는 것이다.
- **값 뒤에 주석(`# …`)을 붙이지 않는다.** systemd `EnvironmentFile`은 줄 중간의 주석을
  지원하지 않아 값에 공백이 남는다. 주석은 따로 한 줄에 쓴다.
- **`JOB_LEASE`는 `JOB_PROGRESS_INTERVAL`의 3배 이상이어야 한다.** 아니면 서버가 기동을
  거부한다 — 보고가 한 번 늦은 것만으로 정상 에이전트가 잡을 잃기 때문이다.
- 전체 목록과 설명은 `conf/template-setup.env`에 있다.

### 2.4 실행

```bash
cd ~/aiha
set -a; . ./framework.env; set +a
./aiha-framework server
```

- 시작 로그의 `certSha256=...`가 인증서 지문이다. `agent token issue`·`restore create`가 같은 값을
  출력하므로 옮겨 적을 필요는 없고, 2.5에서 포트포워딩을 대조할 때 쓴다.
- `framework.env`를 고쳤으면 **다시 source한 셸에서** 실행한다. 프레임워크는 파일이 아니라 셸
  환경변수를 읽는다.
- 계속 띄워 둘 거면 `tmux`/`screen` 안에서 실행한다. systemd(`deploy/aiha-framework.service`)로
  띄울 때는 유닛이 `ProtectHome=true`라 `/home` 아래 파일(인증서·키·`secret.key`)을 읽지 못한다 —
  그 경우 파일을 `/etc/aiha`로 옮기고 `framework.env`의 경로와 유닛의 `EnvironmentFile`을 맞춘다.
- 방화벽(ufw)을 쓰고 있으면: `sudo ufw allow 8443/tcp`
- 스위퍼가 함께 돈다(`JOB_SWEEP_INTERVAL`마다). 리스가 끊긴 잡은 이것이 정리한다.

VM 안에서 확인:

```bash
curl -k https://127.0.0.1:8443/aiha/readyz        # {"message":"Service ready"}
```

`aiha-framework`의 CLI 하위 명령(`agent`, `job`, `repository`, `restore`)은 **서버를 거치지 않고
DB에 직접 붙는다.** 그래서 같은 `framework.env`를 source한 셸에서 실행해야 하고, 서버와 같은
호스트에서 쓰는 것이 간단하다.

### 2.5 포트포워딩 — 경로 A·B 모두

원본 EC2의 에이전트와 타깃 EC2의 레스큐 에이전트 모두 **AWS에서 사내로 들어온다.** 공유기/방화벽에서
`<외부포트>` → `<사내VM>:8443` (TCP)로 포워딩한다. 사내 MinIO를 저장소로 쓰면 MinIO도
`<MinIO외부포트>` → `<MinIO>:9000`으로 포워딩한다(2.6.2).

**EC2를 띄우기 전에 외부에서 반드시 확인한다** — 포워딩이 다른 곳에 연결돼 있으면 에이전트는
"지문 불일치"나 인증서 오류만 보고하므로, 여기서 먼저 대조하는 편이 빠르다. 사내망 밖의 호스트(또는 EC2)에서:

```bash
# CERT_PIN=true — 지문이 서버 로그의 certSha256 과 같아야 한다
curl -k https://<외부주소>:<외부포트>/aiha/readyz
openssl s_client -connect <외부주소>:<외부포트> </dev/null 2>/dev/null \
  | openssl x509 -outform der | sha256sum

# CERT_PIN=false — -k 없이 성공해야 한다(체인·호스트 이름 확인)
curl https://<도메인>:<외부포트>/aiha/readyz
```

### 2.6 저장소 준비와 등록

저장소는 둘 중 하나를 쓴다. 둘 다 등록해 두고 잡마다 골라도 된다. 자격증명은 **플래그가 아니라
환경변수**로 넣는다(명령줄은 같은 호스트의 다른 프로세스에 보인다). 등록한 키는 백업(쓰기)과
복구(읽기)에 모두 쓰인다.

#### 2.6.1 AWS S3

**버킷** — 원본·타깃 EC2와 **같은 리전**에 만든다(전송이 빠르고 리전 간 전송 비용이 없다).
퍼블릭 액세스 차단은 켜 둔 채로 둔다.

```bash
aws s3api create-bucket --bucket <버킷> --region <리전> \
  --create-bucket-configuration LocationConstraint=<리전>   # us-east-1이면 이 줄을 뺀다
```

**IAM 사용자와 정책** — 이 버킷(과 접두사)에만 권한을 준다. 에이전트는 버킷 확인(`HeadBucket`),
객체 읽기·쓰기, 멀티파트 업로드·중단을 하고, 레스큐는 읽기만 한다.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow", "Action": ["s3:ListBucket"],
      "Resource": "arn:aws:s3:::<버킷>" },
    { "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"],
      "Resource": "arn:aws:s3:::<버킷>/<접두사>/*" }
  ]
}
```

이 사용자에 액세스 키를 발급한다. 접두사 없이 버킷 루트를 쓰면 두 번째 `Resource`는
`arn:aws:s3:::<버킷>/*`이다.

**등록**:

```bash
cd ~/aiha
set -a; . ./framework.env; set +a
read -rs -p 'access key: ' AIHA_REPOSITORY_ACCESS_KEY; echo
read -rs -p 'secret key: ' AIHA_REPOSITORY_SECRET_KEY; echo
export AIHA_REPOSITORY_ACCESS_KEY AIHA_REPOSITORY_SECRET_KEY
./aiha-framework repository add --name s3-main \
  --provider AWS --region <리전> --bucket <버킷> --prefix <접두사>
unset AIHA_REPOSITORY_ACCESS_KEY AIHA_REPOSITORY_SECRET_KEY
```

- AWS는 **엔드포인트를 주지 않는다**(주면 거절). `--region`은 **필수**이고 버킷의 리전과 같아야 한다.
- 임시 자격증명(STS)을 쓰면 `AIHA_REPOSITORY_SESSION_TOKEN`도 넣는다. 프레임워크는 갱신하지 않으므로
  만료되면 `update-credentials`로 바꾼다. 테스트에는 IAM 사용자 키가 간단하다.
- EC2 보안 그룹은 아웃바운드 HTTPS(443)만 열려 있으면 된다(기본 전체 허용이면 그대로).

#### 2.6.2 사내 MinIO

**외부 노출** — 원본·타깃 EC2가 닿아야 하므로 `<MinIO외부포트>` → `<MinIO>:9000`을 포워딩한다.
가능하면 MinIO를 TLS(https)로 연다 — http면 자격증명과 백업 데이터가 평문으로 인터넷을 지난다.

**버킷과 사용자** — `mc`로 만든다. 사용자 정책은 S3와 같은 범위(위 JSON의 `Action`·`Resource`)로 준다.

```bash
mc alias set aiha-minio http(s)://<MinIO주소>:9000 <관리자키> <관리자시크릿>
mc mb aiha-minio/<버킷>
mc admin user add aiha-minio <액세스키> <시크릿키>
mc admin policy create aiha-minio aiha-backup policy.json      # 위 JSON
mc admin policy attach aiha-minio aiha-backup --user <액세스키>
```

**등록**:

```bash
cd ~/aiha
set -a; . ./framework.env; set +a
read -rs -p 'access key: ' AIHA_REPOSITORY_ACCESS_KEY; echo
read -rs -p 'secret key: ' AIHA_REPOSITORY_SECRET_KEY; echo
export AIHA_REPOSITORY_ACCESS_KEY AIHA_REPOSITORY_SECRET_KEY
./aiha-framework repository add --name minio-main \
  --provider Minio --endpoint http://<외부주소>:<MinIO외부포트> --bucket <버킷> --prefix <접두사>
unset AIHA_REPOSITORY_ACCESS_KEY AIHA_REPOSITORY_SECRET_KEY
```

- `--endpoint`는 **EC2(에이전트·레스큐)에서 닿는 외부 주소**다. 사내 주소를 넣으면 EC2가 닿지 못한다.
  `http://` 또는 `https://`로 시작하고, 경로·쿼리·사용자정보는 거절한다. region은 생략하면 `us-east-1`.

#### 공통

```
Repository:   <repositoryId> (s3-main)
Location:     AWS - bucket=<버킷> prefix=<접두사>
...
```

- `--prefix`의 앞뒤 `/`는 지워서 저장한다. `..`·빈 구간은 거절한다. 백업은 `<접두사>/<systemId>/`에 쌓인다.
- **위치(provider·엔드포인트·리전·버킷·접두사)는 등록 후 바꿀 수 없다.** 위치가 바뀌면 새 이름으로 등록한다.
- 확인: `./aiha-framework repository list`, `repository get s3-main` (자격증명은 나오지 않는다).
- **키 교체·세션 토큰 만료·비밀 키 분실** 시 같은 환경변수로
  `./aiha-framework repository update-credentials <이름>`.
- 프레임워크는 등록할 때 저장소에 접속해 보지 않는다. 접속 가능 여부는 첫 백업 잡에서 드러난다.

### 2.7 관리 API (REST)

CLI로 하는 등록·백업·복구는 REST로도 할 수 있다. 포털이나 다른 프레임워크와 연동할 때 쓴다.
경로 전체는 `protocol.md` 4.1, 계약은 `AIHA_FRAMEWORK_SWAGGER=true`로 띄운 뒤
`GET /aiha/swagger.json`에서 본다.

```bash
( umask 077; openssl rand -hex 32 > ~/aiha/admin.token )   # 32자 이상. 처음 한 번만
```

서버는 기동할 때 이 파일을 읽으므로, 바꾸면 재시작한다. `AIHA_FRAMEWORK_ADMIN_TOKEN_FILE`을 설정하면
`AIHA_FRAMEWORK_PUBLIC_URL`도 있어야 한다. 없으면 서버가 기동을 거부한다.

```bash
FW=https://127.0.0.1:8443/aiha
AUTH="Authorization: Bearer $(cat ~/aiha/admin.token)"

curl -k -H "$AUTH" $FW/systems                                # 보호 대상
curl -k -H "$AUTH" $FW/systems/<systemId>/inventory           # 파티션 구성
curl -k -H "$AUTH" -H 'Content-Type: application/json' \
     -d '{"repositoryId":"<저장소 이름 또는 ID>"}' \
     $FW/systems/<systemId>/backups                           # 백업 요청 (202, jobId)
curl -k -H "$AUTH" $FW/jobs/<jobId>                           # 진행 조회
curl -k -H "$AUTH" "$FW/backups?systemId=<systemId>"          # 백업 이력
curl -k -H "$AUTH" -H 'Content-Type: application/json' \
     -d '{"backupSetId":"<setId>"}' $FW/restores              # 복구 요청 (202, rescue 값)
curl -k -H "$AUTH" $FW/restores/<restoreId>                   # 복구 진행 조회
```

`POST /restores` 응답의 `rescue`는 이번 한 번만 나오는 값이다. `restore create`가 출력하는 값과 같으므로
4.2처럼 `sh/aiha-init.sh`에 넣는다. 등록 토큰(`POST /registration-tokens`)도 같다.

501(`NOT_IMPLEMENTED`)은 고장이 아니라 아직 구현되지 않은 경로다.

---

## 3. 경로 A — 원본 EC2 백업

### 3.1 등록 토큰 발급 (사내 VM)

```bash
cd ~/aiha
set -a; . ./framework.env; set +a
./aiha-framework agent token issue --ttl 1h --uses 1 --note "원본 EC2 최초 등록"
```

에이전트 설정 파일(3.2)에 넣을 네 줄이 출력된다. **토큰은 이때 한 번만 보인다**(DB에는 해시만 저장).

```
# Registration token <tokenId> (usable until …, 1 use(s))
# Shown once. Put these in the agent's environment file; remove the token after enrolment.
AIHA_AGENT_FRAMEWORK_URL='https://<외부주소>:<외부포트>'
AIHA_AGENT_FRAMEWORK_CERT_PIN='true'
AIHA_AGENT_FRAMEWORK_CERT_SHA256='…'
AIHA_AGENT_REGISTRATION_TOKEN='…'
```

- URL·인증서 확인 방식·지문은 2.3의 `PUBLIC_URL`·`CERT_PIN`·인증서에서 나온다. `CERT_PIN=false`면
  `CERT_PIN='false'`, `CERT_SHA256=''`로 나온다 — 그대로 쓴다(에이전트는 핀을 끈 채 지문이 있으면 거부한다).
- `PUBLIC_URL`이 없거나 인증서를 읽지 못하면 **토큰을 발급하기 전에** 멈춘다.

- 등록 토큰은 **등록만** 할 수 있다. 잡을 받을 수 없고, 에이전트 토큰으로 다른 에이전트를
  등록할 수도 없다.
- `--uses`를 늘리면 여러 대가 한 토큰으로 등록할 수 있다.
- 발급 내역은 `./aiha-framework agent token list`, 회수는 `agent token revoke <tokenId>`.

### 3.2 원본 EC2 준비

**인스턴스** — 타깃(4.3)도 같은 조건으로 만든다.

| 항목 | 값 |
| --- | --- |
| AMI | Ubuntu 24.04 LTS, x86_64 (단일 디스크, LVM 없음). 파티션은 `/`(ext4) · `/boot`(ext4) · `/boot/efi`(vfat) |
| 인스턴스 타입 | Nitro 계열 (장치명이 `nvme0n1pN`) |
| 부팅 모드 | AMI 기본(UEFI) |
| 보안 그룹 인바운드 | SSH 22 — 내 IP만 |
| 보안 그룹 아웃바운드 | 프레임워크 외부포트, 저장소(S3는 443, MinIO는 `<MinIO외부포트>`). 기본 전체 허용이면 그대로 |

**에이전트 설치** (원본 EC2에서):

```bash
sudo install -m 0755 /tmp/aiha-agent /usr/local/bin/aiha-agent
sudo mkdir -p /etc/aiha /var/lib/aiha
sudo install -m 0600 /dev/null /etc/aiha/agent.env    # root 소유 0600으로 만들어 둔다
sudo nano /etc/aiha/agent.env
```

`/etc/aiha/agent.env` — 등록 토큰이 들어가므로 **root 소유 0600**이다:

```bash
# --- 프레임워크 접속 (register, run) — 3.1 출력의 네 줄을 그대로 붙인다 ---
AIHA_AGENT_FRAMEWORK_URL='https://<외부주소>:<외부포트>'
AIHA_AGENT_FRAMEWORK_CERT_PIN='true'
AIHA_AGENT_FRAMEWORK_CERT_SHA256='…'
AIHA_AGENT_REGISTRATION_TOKEN='…'

AIHA_AGENT_STATE_DIR=/var/lib/aiha
AIHA_AGENT_POLL_WAIT=45s
AIHA_AGENT_INVENTORY_INTERVAL=5m
AIHA_AGENT_LOG_LEVEL=info

# --- 스풀 (run, backup) — 아래 설명 참조 ---
AIHA_BACKUP_BUFFER_CAP=268435456
```

- **저장소 설정(`AIHA_BACKUP_REPOSITORY`, `AIHA_BACKUP_S3_*`)은 넣지 않는다.** 프레임워크 잡은
  등록된 저장소(2.6)를 잡과 함께 받아 쓰고, 이 호스트의 저장소 설정은 보지 않는다.
- **값 뒤에 주석(`# …`)을 붙이지 않는다.** systemd `EnvironmentFile`은 줄 중간의 주석을 지원하지
  않아 값에 공백이 남는다(엔드포인트에 붙으면 `invalid port`로 실패). 주석은 따로 한 줄에 쓴다.
- `register`는 `AIHA_AGENT_*`만 읽는다. `run`은 스풀·락 설정(`AIHA_BACKUP_BUFFER_CAP` 등)도 읽는다.
- **스풀**: 백업은 로컬 디스크의 스풀을 거쳐 저장소로 올라간다. 스풀은 `$TMPDIR`(기본 `/tmp`)에 청크를
  쌓고, 업로드가 압축보다 느리면 `AIHA_BACKUP_BUFFER_CAP`(기본 2 GiB) + 청크 2개(기본 128 MiB)까지
  쓴다. 그 파일시스템의 여유 공간이 이보다 작으면 백업을 **시작하기 전에** 거절한다
  (`not enough free space for the spool`). AMI 기본 루트(8 GB)는 여유가 적으므로 위처럼
  256 MiB(최대 384 MiB 사용)로 줄인다. 상한이 작으면 업로드가 느린 만큼 압축이 기다릴 뿐 결과는 같다.
  스풀 디렉터리는 루트를 백업할 때 아카이브에서 자동으로 제외된다.
- `AIHA_AGENT_INVENTORY_INTERVAL` 최소값은 5m이다. 테스트에서 재보고를 빨리 보려고 5m으로
  둔 것이고, 운영 기본값은 1h다.

**손으로 백업할 때만** (`aiha-agent backup`) 이 호스트의 저장소 설정을 쓴다. 그렇게 만든 세트는
프레임워크 기록에 없어 `restore create`로 복구할 수 없다 — 문제 확인용으로만 쓴다:

```bash
# AWS S3
AIHA_BACKUP_REPOSITORY=s3://<버킷>/<접두사>
AIHA_BACKUP_S3_REGION=<리전>
AIHA_BACKUP_S3_ACCESS_KEY=<키>
AIHA_BACKUP_S3_SECRET_KEY=<시크릿>

# 사내 MinIO — 엔드포인트는 스킴 없이 host:port, http면 USE_SSL=false
AIHA_BACKUP_REPOSITORY=s3://<버킷>/<접두사>
AIHA_BACKUP_S3_ENDPOINT=<외부주소>:<MinIO외부포트>
AIHA_BACKUP_S3_USE_SSL=false
AIHA_BACKUP_S3_ACCESS_KEY=<키>
AIHA_BACKUP_S3_SECRET_KEY=<시크릿>
```

```bash
sudo bash -c 'set -a; . /etc/aiha/agent.env; set +a; exec aiha-agent backup --device /dev/nvme0n1p16'
```

이 파일을 읽히지 않고 그냥 실행하면 `AIHA_BACKUP_REPOSITORY` 기본값(`./aiha-repo`, 현재
디렉터리)에 백업한다.

### 3.3 등록

```bash
sudo bash -c 'set -a; . /etc/aiha/agent.env; set +a; exec aiha-agent register'
```

설정 파일을 root 셸 안에서 읽는다. 파일이 root 소유 0600이라 일반 사용자는 읽을 수 없고,
`sudo -E`는 sudoers 설정에 따라 환경을 넘겨주지 않는다.

```
Enrolled as agent <agentId> for system <systemId>
Remove the registration token from the environment: it is spent.
```

- **`systemId`를 적어 둔다.** 이후 `agent inventory`·`job backup`이 이 값을 쓴다.
- 자격증명은 `AIHA_AGENT_STATE_DIR`에 저장된다. 등록 토큰은 소진됐으므로
  `/etc/aiha/agent.env`에서 지운다.
- 다시 등록해야 하면 `aiha-agent reset`으로 로컬 자격증명을 지우고 새 토큰으로 등록한다.
  프레임워크 쪽 옛 에이전트는 `agent revoke <agentId>`로 회수한다.

사내 VM에서 확인:

```bash
./aiha-framework agent list      # AGENT  SYSTEM  HOST  STATE  LAST SEEN  TOKEN  KEY
```

`STATE`는 마지막 통신 시각으로 판정한다. 아직 `run`을 띄우지 않았으면 `offline`이 정상이다.

### 3.4 상주 실행

수동으로 (포그라운드, 로그를 바로 보려면):

```bash
sudo bash -c 'set -a; . /etc/aiha/agent.env; set +a; exec aiha-agent run'
```

systemd로 (권장):

```bash
sudo install -m 0644 deploy/aiha-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now aiha-agent
journalctl -u aiha-agent -f
```

- **root로 실행해야 한다.** 백업이 파티션을 읽기 전용으로 바인드 마운트하기 때문이다.
  일반 사용자로 돌리면 잡은 받지만 `create temp mount dir: permission denied`로 실패한다.
- 기동 로그: `Agent running agentId=… systemId=… pollWait=… inventoryInterval=…`
- 곧이어 `Reported this system's configuration complete=true partitions=N`
- **고쳐지지 않을 상태에는 전용 종료 코드를 쓴다.** systemd가 무한 재시작하지 않도록
  유닛의 `RestartPreventExitStatus=10 11 12`가 받는다.

| 코드 | 뜻 | 조치 |
| --- | --- | --- |
| 10 | 재등록 필요 (자격증명 거부·회수됨) | `aiha-agent reset` 후 새 토큰으로 등록 |
| 11 | 복구 표식 있음 (복구된 디스크로 부팅) | 부팅 확인 절차. 원본 시스템의 신원을 그대로 들고 있다 |
| 12 | 설정 오류 | `agent.env` 확인 |

프레임워크에 닿지 않는 것은 여기 없다 — 그건 고장이 아니라 대기이고, 에이전트가 백오프하며
계속 재시도한다.

### 3.5 인벤토리 확인 (사내 VM)

```bash
./aiha-framework agent inventory <systemId>
```

```
System:     02b9d3fd003ad048
Agent:      6d62d7ef8e590887
Collected:  2026-10-12T05:14:44Z (16s ago)
Reported:   2026-10-12T05:14:44Z
Platform:   ubuntu 24.04 x86_64 uefi 6.14.0-1015-aws
Complete:   yes — backup targets can be chosen automatically

DEVICE           MOUNT       FS    SIZE     USED     BACKUP     NOTE
/dev/nvme0n1p1   /           ext4  6.8GB    6.0GB    automatic  -
/dev/nvme0n1p16  /boot       ext4  880.4MB  183.0MB  automatic  -
/dev/nvme0n1p15  /boot/efi   vfat  104.3MB  6.2MB    automatic  -
```

읽는 법:

- **`COLLECTED`와 `REPORTED`는 다르다.** 앞은 에이전트가 시스템을 읽은 시각, 뒤는
  프레임워크가 들은 시각이다. 제때 보고하면서 낡은 값을 보내는 에이전트와 연락이 끊긴
  에이전트는 다른 고장이고, **없어진 파티션을 잡에 넣을 수 있는 것은 전자뿐**이다.
- **`BACKUP` 열이 셋으로 갈린다.** `automatic`은 자동 선택 대상, `by name`은 수동 지정만
  가능, `no`는 이 에이전트가 백업할 수 없는 것이다. 마지막 것도 **사유와 함께 목록에 남는다** —
  없어진 파티션과 일부러 뺀 파티션을 구분하기 위해서다.
- 같은 장치가 여러 줄 나오는 것은 정상이다. 한 파일시스템이 여러 곳에 마운트되면 마운트마다
  한 줄이고, 대표 마운트 하나만 `automatic`이 된다.
- **`Complete: no`면 자동 선택을 거절한다.** 마운트 테이블은 마운트된 것만 알기 때문에,
  fstab이 선언한 `/boot/efi`가 마운트 해제돼 있으면 그것으로 고른 세트는 **백업은 성공하고
  복구만 실패한다.** 이때는 누락 목록이 함께 표시된다.

아직 안 보고했으면 이렇게 나온다 — 에이전트가 기동 직후와 이후 주기적으로 보고한다.

```
System … has not reported what it is made of.
```

### 3.6 백업잡 생성과 추적 (사내 VM)

**대상을 자동으로 고르게 한다** (권장). `--repository`에는 2.6에서 등록한 이름이나 ID를 준다:

```bash
./aiha-framework job backup --system <systemId> --repository s3-main      # 또는 minio-main
```

`--repository`는 필수다. 잡에는 저장소 ID만 기록되고, 에이전트는 claim 응답으로 주소·자격증명을
받는다(그 응답에서만 복호화된다).

```
Backing up 3 partition(s) reported by the agent:
  /dev/nvme0n1p1       /                ext4
  /dev/nvme0n1p16      /boot            ext4
  /dev/nvme0n1p15      /boot/efi        vfat
Reported 1m26s ago, at 2026-10-12T05:14:44Z
Queued backup job <jobId> for system <systemId> (expires …)
```

**직접 지정한다** — 같은 인벤토리와 대조해 검증한다:

```bash
./aiha-framework job backup --system <systemId> --repository s3-main \
  --device /dev/nvme0n1p1 --device /dev/nvme0n1p16 --device /dev/nvme0n1p15
```

없는 장치나 백업할 수 없는 장치를 주면 **잡을 만들지 않고** 거절하며, 고를 수 있는 것을
보여 준다:

```bash
./aiha-framework job backup --system <systemId> --repository s3-main --device /dev/nonexistent
```

```
Partitions this system reported that can be backed up:
  /dev/nvme0n1p1       /                ext4
  /dev/nvme0n1p16      /boot            ext4
  /dev/nvme0n1p15      /boot/efi        vfat
ERR Command failed error="the inventory does not list this device: /dev/nonexistent"
```

인벤토리가 아직 없으면 `--device`는 **검증 없이 통과한다.** 그 사실을 출력하고 진행하며,
에이전트가 쓰기 직전에 실제 장치를 다시 확인한다([protocol.md](protocol.md) 2.7의 예외 정책).

추적:

```bash
./aiha-framework job list --system <systemId>
./aiha-framework job get <jobId>
./aiha-framework job cancel <jobId>
```

`job get`에서 볼 것:

- `Spec` — `devices`, `expectedFsUuids`, `repositoryId`. `expectedFsUuids`는 잡을 만든 시점의
  인벤토리에서 온 값으로, 에이전트가 쓰기 전에 대조한다. 자격증명은 들어 있지 않다.
- `Last seen`과 `Last progress`가 **따로 나온다.** 보이는데 진행이 없으면 에이전트는 살아
  있고 작업이 멈춘 것이고, 안 보이면 연락이 끊긴 것이다. 조치가 다르다.
- `Uploaded` — 업로드된 바이트와 몇 번째 보고인지.
- `Lease expires` — 이 시각까지 보고가 없으면 스위퍼가 잡을 정리한다.

### 3.7 확인할 것

| 확인 | 기대 |
| --- | --- |
| `aiha-agent run` 기동 직후 | `Agent running` → `Reported this system's configuration complete=true` |
| `agent list` | 해당 에이전트가 `online` |
| `agent inventory <systemId>` | `/`, `/boot`, `/boot/efi`가 `automatic` |
| `job backup --system` (`--device` 없이) | 세 파티션이 출력되고 잡 생성 |
| `job backup --device /dev/nonexistent` | **잡 생성 시점에** 거절 + 선택 가능 목록 안내 |
| 에이전트 로그 | `Job claimed` → `Connecting to S3 repository endpoint=…` → `Starting backup set` → `Job finished outcome=completed` |
| `job get <jobId>` | `Status: completed`, `Message`에 `Backup set <backupSetId> (completed, 3 item(s))` |
| 저장소 (S3) | `aws s3 ls s3://<버킷>/<접두사>/<systemId>/`에 `<backupSetId>-manifest.json`과 `.aha` 파일 |
| 저장소 (MinIO) | `mc ls aiha-minio/<버킷>/<접두사>/<systemId>/`에 같은 파일들 |
| `agent.env`에 저장소 설정 없이 | 백업 성공 — 저장소는 잡과 함께 왔다 |
| 실행 중 `job cancel` | `cancelling` → 에이전트가 중단 확인 후 `cancelled` |
| 에이전트를 죽인 뒤 리스 만료 | 스위퍼가 `failed`로. **다른 에이전트에 넘기지 않는다** |

**파티션이 바뀐 경우를 확인하려면**: 잡을 만든 뒤 대상 파티션을 다른 파일시스템으로 바꾸고
에이전트를 붙인다. 에이전트가 `expectedFsUuids`와 대조해 **아무것도 올리지 않고** 실패한다.

```
ERR Job did not complete error="/dev/nvme1n1p1 holds filesystem <현재>, but this backup was asked for
<잡의 값>: the partition has been replaced since the job was created"
```

로그에 `Starting backup set`이 **없어야** 한다. 있으면 쓰기가 시작된 것이다.

---

## 4. 경로 B — 타깃 EC2 복구

원본과 같은 AMI로 만든 **타깃 EC2**를 레스큐로 부팅시켜, 레스큐 에이전트가 복구 정보를 받고
**백업 세트를 그 인스턴스의 디스크에 복구**하는 것까지 확인한다. 재부팅은 하지 않는다 —
복구된 OS로의 부팅은 수동으로 확인한다(4.6).

> **타깃 인스턴스의 파티션은 포맷된다.** 레스큐 부팅 후 autorun2가 claim에 이어 곧바로
> 복구를 시작한다. 지워져도 되는 인스턴스에서만 실행한다.

### 4.1 사전 준비 — 원본 백업과 레스큐용 파일

**원본 백업** — 3절로 원본 EC2를 백업한다.

```bash
./aiha-framework job backup --system <systemId> --repository s3-main   # /, /boot, /boot/efi 를 한 세트로
./aiha-framework job get <jobId>          # Status: completed, Message의 backupSetId
```

- **세트에 `/`가 반드시 있어야 하고, UEFI 인스턴스면 `/boot/efi`도 있어야 한다.** `--device` 없이
  만들면 인벤토리에서 셋 다 고른다.
- **세트가 `completed`여야 한다.** 한 파티션이라도 실패한 세트는 복구를 거부한다.
- `backupSetId`를 적어 둔다(4.2). 시스템과 저장소는 프레임워크가 잡 기록에서 찾는다.
- **프레임워크 잡으로 만든 세트만** 복구 잡을 만들 수 있다. 손으로 `aiha-agent backup`한 세트는
  프레임워크 기록에 없다.

**레스큐용 파일** — 사내 MinIO 또는 S3에 올리고 **presigned URL**을 만든다. 타깃 EC2가 user-data
단계(Ubuntu)에서 받는다.

| 파일 | 용도 | 해시 |
| --- | --- | --- |
| `sh/setup-sysres.sh` (현재 버전) | aiha-init.sh의 `SCRIPT_URL` | — |
| `sysres.tar` | `SYSRES_URL`. **`autorun/autorun2`(= `sh/autorun2`, 모드 0755)를 포함해야 한다** | `sha256sum sysres.tar` → `SYSRES_SHA256` |
| `bin/aiha-rescue` | `AGENT_URL` — aiha-init.sh가 받아 `/autorun/aiha/aiha-rescue`에 둔다 | `sha256sum bin/aiha-rescue` → `AGENT_SHA256` |

```bash
# S3
aws s3 cp sysres.tar s3://<버킷>/rescue/sysres.tar
aws s3 presign s3://<버킷>/rescue/sysres.tar --expires-in 86400
# MinIO
mc cp sysres.tar aiha-minio/<버킷>/rescue/sysres.tar
mc share download --expire 24h aiha-minio/<버킷>/rescue/sysres.tar
```

- presigned URL은 **타깃 EC2에서 닿는 주소**여야 한다(MinIO는 외부 주소로 만든다). 유효 기간이
  인스턴스를 띄울 때까지 남아 있어야 한다.
- 레스큐용 파일을 백업 저장소와 같은 버킷에 두면, 2.6.1의 IAM 정책 접두사 밖(`rescue/`)이므로
  에이전트 키로는 건드릴 수 없다. presigned URL은 관리자 권한으로 만든다.

이미지 안 `autorun1`이 에이전트를 내려받고 있었다면 **이제 중복**이다. 빼거나 그대로 두어도
되지만, `/autorun/aiha/aiha-rescue`를 덮어쓰지 않는지만 확인한다.

레스큐 이미지(SystemRescue)에 복구가 쓰는 도구가 있어야 한다: `rclone`, `tar`, `zstd`,
`mkfs.ext4`, `mkfs.vfat`(xfs면 `mkfs.xfs`), `blkid`, `lsblk`, `mountall`, `mount`, `umount`,
`sync`, `arch-chroot`. 하나라도 없으면 **포맷 전에** 중단한다.

### 4.2 복구 잡 만들기 (사내 VM)

세트 ID만 준다. 프레임워크가 그 세트를 보고한 **완료된 백업 잡**을 찾아 원본 시스템과 저장소를
정하고, 레스큐 에이전트는 claim할 때 그 저장소의 주소·자격증명을 받는다. 복구 잡에는 저장소
ID만 기록된다(자격증명은 복사하지 않는다).

```bash
cd ~/aiha
set -a; . ./framework.env; set +a
./aiha-framework restore create --backup-set <backupSetId>
```

- 그 세트를 완료로 보고한 백업 잡이 없으면(손으로 한 백업, 실패한 세트, 틀린 ID) 거절한다.
  같은 세트 ID를 보고한 잡이 둘 이상이면 하나를 고르지 않고 거절한다.
- "완료"는 에이전트가 보고한 사실이다. 그 뒤 저장소에서 파일이 지워졌는지는 프레임워크가 모르며,
  레스큐가 매니페스트를 읽을 때(`preparing`) 드러난다.
- 레스큐가 받는 자격증명은 저장소에 등록한 그 키다 — 백업에 쓰는 쓰기 가능한 키이므로, 레스큐
  환경의 tmpfs에만 둔다(Restore.md 5.5). 복구 전용 읽기 키·단기 자격증명은 남은 과제다.
- 복구 대상 인스턴스는 플랜이 아니라 아래 aiha-init.sh 값으로 만든 인스턴스가 정한다.

출력되는 다섯 줄을 복사한다. **클레임 토큰은 이때 한 번만 보인다**(DB에는 해시만 저장).
첫 줄 주석의 `restore <restoreId>`도 적어 둔다(4.5에서 진행 조회에 쓴다).

```
# sh/aiha-init.sh values for restore <restoreId> (first claim before …)
AIHA_RECOVERY_ID='…'
AIHA_FRAMEWORK_URL='https://<외부주소>:<외부포트>'
AIHA_CLAIM_TOKEN='…'
AIHA_FRAMEWORK_CERT_PIN='true'
AIHA_FRAMEWORK_CERT_SHA256='…'
```

`CERT_PIN`·`CERT_SHA256`은 3.1과 같은 규칙으로 2.3 설정을 따른다(`false`면 지문이 비어 있다).

**클레임 토큰을 다시 받으려면 `restore create`를 다시 해야 하고, 그러면 새 `recoveryId`가 나온다.**
이미 user-data를 넣어 둔 인스턴스라면 user-data도 새 값으로 바꾼다. 옛 값이 남아 있으면 복구된 OS의
표식(새 ID)과 user-data(옛 ID)가 달라, 복구 후 부팅할 때마다 레스큐로 돌아간다(5.2).

첫 claim은 이 시점부터 `AIHA_FRAMEWORK_CLAIM_TTL`(2.3에서 2h) 안에 해야 한다.

### 4.3 타깃 EC2 — user-data와 인스턴스

`sh/aiha-init.sh`를 복사해 **10개 값**을 채운다. 저장소의 원본에는 자리표시자만 둔다.

| 변수 | 값 |
| --- | --- |
| `SCRIPT_URL` | setup-sysres.sh presigned URL |
| `SYSRES_URL` | sysres.tar presigned URL |
| `SYSRES_SHA256` | sysres.tar의 SHA-256 |
| `AGENT_URL` | aiha-rescue presigned URL |
| `AGENT_SHA256` | aiha-rescue의 SHA-256 |
| `AIHA_RECOVERY_ID` … `AIHA_FRAMEWORK_CERT_SHA256` | 4.2의 다섯 줄 그대로 (스크립트의 변수 순서가 출력과 같다) |

스크립트는 값 검증 → 다운로드(에이전트는 해시 검증) → `setup-sysres.sh`(재부팅 없이 sysres.tar
해제·grub 1회 부팅 설정) → 이미지에 `autorun2`가 있는지 확인 → `/autorun/aiha`에 복구 정보·에이전트
설치 → 재부팅 순서로 진행한다. 이미지에 `autorun2`가 없으면 재부팅 전에 멈춘다. 복구 정보 5개의
형식이 틀리면 **아무것도 하지 않고 멈춘다**(레스큐로 넘어가지 않음).

복구 실행기는 **타깃이 원본과 같은 이미지라고 가정한다.** 파티션 테이블을 만들지 않고, 매니페스트에
기록된 원본 파티션의 **PARTUUID**로 타깃의 파티션을 찾아 포맷한다. 장치 이름은 쓰지 않는다 — NVMe
번호는 장치가 인식된 순서라, 원본에 볼륨이 하나 더 있었으면 원본 `nvme1n1p1`이 타깃에서는
`nvme0n1p1`이 된다. 같은 AMI로 만든 디스크는 파티션 테이블이 같아 PARTUUID가 같다.

| 항목 | 값 |
| --- | --- |
| AMI | **원본과 같은 AMI** |
| 리전 | 원본·저장소와 같은 리전 권장(S3 저장소면 전송 비용·시간이 준다) |
| 인스턴스 타입 | 원본과 같은 계열 (Nitro — 장치명이 `nvme0n1pN`으로 같아야 한다) |
| 루트 볼륨 크기 | **원본 이상.** 첫 부팅에 cloud-init이 루트 파티션을 볼륨 크기로 늘린다. 원본의 사용량이 들어가야 한다 |
| 부팅 모드 | 원본과 같게 (UEFI). 레스큐가 부팅된 방식으로 `grub-install` 대상을 정한다 |
| 네트워크 인터페이스 | **1개.** 원본 netplan의 MAC 1개를 이 인스턴스 NIC의 MAC으로 바꾼다. 2개 이상이면 포맷 전에 중단 |
| Secure Boot | 꺼져 있어야 한다 (setup-sysres.sh가 확인 후 중단) |
| 보안 그룹 인바운드 | SSH 22 — 내 IP만 (레스큐 접속용) |
| 보안 그룹 아웃바운드 | 프레임워크 외부포트, 저장소(S3 443 / MinIO 외부포트), presigned URL 호스트. 기본 전체 허용이면 그대로 |
| 키 페어 | aiha-init.sh가 실패해 Ubuntu에 머물 때, 복구 후 부팅 확인할 때 |
| user-data | 값을 채운 aiha-init.sh 전체 |

### 4.4 레스큐에서 claim과 복구 (자동)

레스큐로 부팅하면 SystemRescue가 `/autorun/autorun2`를 실행한다. 별도 조작은 없다.
`copytoram` 덕분에 복구 정보와 에이전트가 이미 램에 있으므로 레스큐가 복구 대상 디스크를
쓰지 않는다. 네트워크가 늦게 올라와도 claim은 `--timeout`(기본 30분)까지 재시도한다.

autorun2는 `aiha-rescue rescue`를 실행하고, 이것이 claim에 이어 복구를 수행한다:

| 단계 | 하는 일 | 실패하면 |
| --- | --- | --- |
| 1 `preparing` | `rclone cat`으로 매니페스트를 읽고 검증. 세트·항목 `completed`, `/` 존재, 원본 UUID 기록, 도구, **PARTUUID로 타깃 파티션 찾기**, 각 장치가 마운트되지 않았는지, NIC 매칭, UEFI면 `/boot/efi` | **디스크는 바뀌지 않는다.** 원인을 고친 뒤 같은 잡으로 다시 실행할 수 있다(아래) |
| 2 `formatting` | 원본 **label·UUID 그대로** `mkfs.ext4 -U` / `mkfs.vfat -i` / `mkfs.xfs -m uuid=`. `blkid`로 UUID가 원본과 같은지 확인 | 여기부터 디스크는 부팅 불가 상태 |
| 3 `mounting` | `mountall --no-bind` → 각 파티션이 `/mnt/<장치명>`에 있는지 확인 | |
| 4 `extracting` | 백업 파일마다 `rclone cat \| zstd -d \| tar -x -p --numeric-owner` | |
| 5 `reconfiguring` | netplan의 원본 MAC → 타깃 MAC. fstab·grub defaults·EFI grub stub의 UUID가 이 머신에 맞는지 **검사만**(틀리면 경고, 파일은 그대로). `/var/lib/aiha/recovery-pending.json` 기록 | |
| 6 `bootloader` | `/boot`·`/boot/efi`를 루트 아래로 bind 마운트 → `arch-chroot`에서 `grub-install` → `update-grub` → `update-initramfs -u -k all` → bind 마운트 해제 | |

레스큐 콘솔 출력 (로그는 JSON 줄로 함께 나온다):

```
Restore job claimed: restore … (recovery …)
Repository:    AWS  bucket=<버킷> prefix=<접두사>          (MinIO면 Minio http(s)://… bucket=…)
Restore plan:  /run/aiha/<recoveryId>/restore-plan.json
rclone config: /run/aiha/<recoveryId>/rclone.conf (remote "aiha")
… Restore phase started phase=preparing step=1 totalSteps=6
…
Backup set <backupSetId> restored: 3 partition(s), uefi boot (…s)
Restored root: /mnt/nvme0n1p1 (still mounted; the machine was not rebooted)
```

검사에서 불일치가 있으면 끝에 `WARNING: …` 줄이 붙는다. 복구는 끝까지 진행하지만 그대로
재부팅하면 부팅에 실패할 수 있다.

**받은 저장소 정보를 직접 확인하려면** (S3·MinIO 같은 명령):

```bash
rclone --config /run/aiha/<recoveryId>/rclone.conf lsf aiha:<버킷>/<접두사>/<systemId>/
```

**복구만 다시 실행하려면** (claim 없이, **다시 포맷한다**):

```bash
umount -R /mnt/nvme0n1p1 /mnt/nvme0n1p15 /mnt/nvme0n1p16    # 이전 실행이 마운트한 것을 먼저 해제
/run/aiha/aiha-rescue restore --recovery-file /run/archiso/bootmnt/autorun/aiha/recovery.env --out-dir /run/aiha
```

마운트된 파티션이 남아 있으면 `Partition … is in use: unmount it before restoring`으로 포맷 전에
멈춘다.

**`preparing`에서 실패했으면** 디스크는 그대로이고 복구 잡은 `failed`가 아니라 다시 `claimed`가 된다
(`restore get`의 `Message`에 원인). 같은 레스큐에서 원인을 고친 뒤 위의 `restore`로 다시 실행하면
된다 — 새 복구 잡·새 인스턴스가 필요 없다. 포맷 이후(`formatting`~`bootloader`)의 실패는 최종이다.

> autorun2를 다시 실행해도 된다. 재수신 기한(`CLAIM_REDELIVERY`, 10m) 안이면 claim이 같은 응답을
> 받고 **복구를 처음부터 다시 한다.** 기한이 지나면 claim이 `410`으로 끝나고 복구는 시작되지
> 않는다 — 그때는 위의 `restore`를 쓴다.

### 4.5 진행 조회 (사내 VM)

```bash
./aiha-framework restore get <restoreId>     # 또는 <recoveryId>
```

```
Restore:        <restoreId>
Recovery:       <recoveryId>
Status:         restoring
Plan:           {"sourceSystemId": "…", "sourceBackupSetId": "…"}
Created:        …
Claimed:        …
Phase:          extracting (step 4 of 6)
Restored:       1073741824 of 3695548422 bytes (29.1%)
Current file:   <host>-nvme0n1p1-<id>.aha
Last progress:  … (report 37)
```

- **상태**: `awaiting_agent` → `claimed` → (첫 보고) `restoring` → 성공 `awaiting_boot` / 실패 `failed`.
  `preparing`에서 실패하면 `failed`가 아니라 `claimed`로 돌아가 다시 실행을 기다린다.
  `awaiting_boot`은 "디스크 복구 완료, 부팅 확인 전"이다 — 부팅 확인 보고(boot-verify)는 아직
  없으므로 여기서 멈춘다.
- **`Restored`** 는 저장소에서 읽은 바이트(압축된 백업 파일 기준)와 세트의 백업 파일 크기 합이다.
- 단계 보고는 바뀔 때 즉시, 그 외에는 10초마다 간다. 빨리 끝나는 단계(`mounting` 등)는 건너뛰어
  보일 수 있다.
- 끝나면 `Outcome`, `Finished`, `Message`가 나온다. 실패 메시지는 "어느 단계에서 무엇이" 수준이다 —
  도구의 원문 오류는 레스큐 콘솔 로그에만 있다. 성공해도 검사 경고가 있으면 `Message`에
  `…; N warning(s): …`로 붙는다.
- **보고가 끊긴 `restoring` 잡을 정리하는 스위퍼는 아직 없다.** 레스큐가 죽으면 `restoring`에 머문다.

### 4.6 확인할 것

| 확인 | 기대 |
| --- | --- |
| 레스큐 부팅 직후 | autorun2가 자동 실행되고 `Restore job claimed`, 이어서 6단계 진행 |
| `/run/aiha/<recoveryId>/` | 디렉터리 700, 파일 600(`claim.key`, `restore-plan.json`, `rclone.conf`, `report.token`). `restore-plan.json`에 키·토큰이 없음 |
| 프레임워크 로그 | `Restore job claimed recoveryId=…` → `Restore finished outcome=succeeded status=awaiting_boot` |
| `restore get <restoreId 또는 recoveryId>` | 진행 중 `restoring`·단계 n/6·바이트, 끝나면 `awaiting_boot`, `Outcome: succeeded` |
| 레스큐에서 `blkid` | 복구한 파티션의 UUID·label이 원본(매니페스트의 `blockDevices`)과 같다 |
| 콘솔 출력 | `WARNING:` 줄이 없다 |
| `/mnt/<루트>/etc/netplan/*.yaml` | `macaddress`가 타깃 NIC의 MAC (`cat /sys/class/net/*/address`) |
| `/mnt/<루트>/var/lib/aiha/recovery-pending.json` | `recoveryId`가 있다 |
| **수동 재부팅** (`reboot`) | 복구된 Ubuntu로 부팅하고 SSH 접속된다 |
| 복구된 OS 부팅 직후 `/var/log/cloud-init-output.log` | `[aiha-init] … this is the restored system … nothing to do.` — 다시 레스큐로 넘어가지 않는다 |
| 부팅 후 `aiha-agent` 서비스 | 복구 표식을 먼저 보고 종료 코드 11로 멈춘다(`RestartPreventExitStatus`라 재시작 안 함). 에이전트 자격증명(`/var/lib/aiha`)은 백업에서 제외돼 원래 없다 |
| 저장소별 | 같은 절차를 `s3-main`과 `minio-main` 각각으로 한 번씩 해 본다 |

**복구된 OS에서도 user-data가 다시 실행된다.** user-data는 OS가 아니라 인스턴스에 붙어 있고, 복구된
OS의 cloud-init은 원본 인스턴스의 기록(`/var/lib/cloud`)을 갖고 있어 타깃을 "새 인스턴스"로 보고
user-data(aiha-init.sh)를 다시 실행한다. aiha-init.sh는 복구 표식(`/var/lib/aiha/recovery-pending.json`)이
있으면 아무것도 하지 않고 끝나므로 레스큐로 다시 넘어가지 않는다. 같은 이유로 cloud-init의 다른 인스턴스당
작업(호스트명, SSH 호스트 키 재생성, 타깃 키 페어의 공개키 추가, netplan 재생성)은 복구된 OS에서 실행된다 —
SSH 호스트 키가 바뀌어 접속 시 경고가 날 수 있다. (cloud-init 처리는 [open-decisions.md](open-decisions.md) 5번)

`grub-reboot`이 1회성이므로, 복구 도중 레스큐가 재부팅돼도 원래 부팅 항목으로 돌아간다. 다만
포맷 이후라면 그 디스크는 부팅되지 않는다.

---

## 5. 문제 해결

### 5.1 경로 A — 백업

| 증상 | 원인·조치 |
| --- | --- |
| `register`가 `401 Registration token rejected` | 토큰 복사 오류, 만료, 사용 횟수 소진, 회수됨. `agent token list`로 확인 |
| `register`가 `409 Registration already completed` | 같은 요청 키로 이미 등록됨. 에이전트 토큰은 재발급되지 않는다 — `aiha-agent reset` 후 새 토큰으로 |
| 에이전트가 종료 코드 12로 죽음 | 설정 오류. `AIHA_AGENT_FRAMEWORK_URL`은 https여야 하고, `CERT_PIN`은 `true`/`false`, 지문은 `true`면 64자리 16진수·`false`면 빈 값이어야 한다. 3.1 출력을 다시 확인한다 |
| 에이전트가 종료 코드 10으로 죽음 | 자격증명이 거부됨. 프레임워크에서 회수했다면 `aiha-agent reset` 후 재등록 |
| `certificate does not match the pinned fingerprint` | 인증서를 다시 만들었거나 다른 서버에 붙고 있다. 2.5절 openssl 대조 |
| `x509: certificate signed by unknown authority` / `certificate is valid for …, not …` | `CERT_PIN=false`인데 자체 서명 인증서이거나 URL이 인증서의 도메인과 다르다. 2.2 표 확인. 자체 서명이면 `CERT_PIN=true`로 두고 토큰을 다시 발급한다 |
| `x509: certificate has expired or is not yet valid` | `CERT_PIN=false`에서 인증서 만료 또는 EC2 시계 오류 |
| `reach framework: … timeout` (원본 EC2) | 포트포워딩·방화벽, 또는 `AIHA_AGENT_FRAMEWORK_URL`에 사내 주소를 넣음. EC2에서 `curl -k https://<외부주소>:<외부포트>/aiha/readyz` |
| `agent inventory`가 "has not reported" | 에이전트가 아직 안 떴거나 보고가 실패 중. 에이전트 로그의 `Could not report this system's configuration` 확인 |
| 인벤토리가 `Complete: no` | 누락 목록을 본다. fstab이 선언한 파일시스템이 마운트 해제돼 있으면 마운트하고 다음 보고를 기다리거나, `--device`로 직접 지정한다 |
| `job backup`이 `the inventory does not list this device` | 장치명 오류. 출력된 선택 가능 목록에서 고른다 |
| `job backup`이 `this device cannot be backed up` | 인벤토리에 있지만 백업 불가로 기록된 것(tmpfs, 바인드 마운트, btrfs 등). 사유가 함께 나온다 |
| `job backup`이 `a repository is required` / `repository "…": repository not found` | `--repository`를 빠뜨렸거나 등록되지 않은 이름. `repository list` |
| `repository add`가 `an AWS repository takes no endpoint` / `requires a region` | AWS는 `--endpoint` 없이 `--region`만 준다 |
| `repository add`·`server`가 `AIHA_FRAMEWORK_SECRET_KEY_FILE is required` | 2.2의 `secret.key`를 만들고 2.3에 경로를 넣은 뒤 **다시 source**한다 |
| 에이전트가 claim에서 `500 Job request failed` 반복, 서버 로그에 `secret cannot be decrypted with this key` | 비밀 키가 바뀌었다. `repository update-credentials`로 자격증명을 다시 넣는다. 잡은 그대로 대기한다 |
| 잡이 `409 Job names no repository` | 저장소를 지정하기 전 버전으로 만든 잡. 다시 만든다 |
| 잡이 `failed`, `the framework sent no repository with this job` | 프레임워크가 이전 버전이다. 프레임워크와 에이전트를 같은 빌드로 맞춘다 |
| 잡이 `failed`, `access bucket "…": Access Denied` (S3) | IAM 정책에 `s3:ListBucket`이 없거나 버킷 ARN이 틀림. 2.6.1 정책 확인 |
| 잡이 `failed`, `… AccessDenied … PutObject` (S3) | 정책의 객체 `Resource` 접두사가 등록한 `--prefix`와 다르다 |
| 잡이 `failed`, `… region … is wrong; expecting '…'` / `PermanentRedirect` (S3) | 등록한 `--region`이 버킷 리전과 다르다. 위치는 바꿀 수 없으니 올바른 리전으로 새 이름으로 등록한다 |
| 잡이 `failed`, `bucket "…" not found or not accessible` | 버킷 이름 오류, 또는 키에 그 버킷 권한이 없음 |
| 잡이 `failed`, `… connection refused / i/o timeout` (MinIO) | MinIO 포트포워딩·방화벽, 또는 등록한 엔드포인트가 사내 주소. EC2에서 `curl http(s)://<외부주소>:<MinIO외부포트>/minio/health/live` |
| 잡이 `failed`, `not enough free space for the spool: … has N MiB free but the spool may use up to M MiB` | 3.2의 `AIHA_BACKUP_BUFFER_CAP`을 줄이거나 `TMPDIR`을 여유 있는 파일시스템으로. 백업은 시작되지 않았다 |
| 잡이 `failed`, `… write /tmp/aiha-spool-…: no space left on device` | 이 검사가 없던 이전 빌드. 에이전트를 새 빌드로 바꾼다 |
| 잡이 `failed`, `s3 client: parse "…:포트   ": invalid port` | 손으로 백업할 때의 `agent.env` 값 뒤에 주석·공백이 있다. 3.2 참조 |
| 잡이 `failed`, `holds filesystem … but this backup was asked for …` | 잡 생성 이후 파티션이 바뀌었다. **정상 동작이다** — 새 인벤토리로 잡을 다시 만든다 |
| 잡이 `queued`에서 안 움직임 | 에이전트가 안 떴거나 `systemId`가 다르다. `agent list`의 `STATE` 확인 |
| 실행 중이던 잡이 `failed`로 끝남 | 리스 만료. **다른 에이전트에 넘기지 않는다** — 옛 에이전트가 아직 쓰고 있을 수 있어서다. 시도 결과는 `unknown`으로 남는다 |
| 서버가 `AIHA_FRAMEWORK_JOB_LEASE … must be at least three times …`로 기동 거부 | 2.3의 리스·보고 간격 비율 |

### 5.2 경로 B — 레스큐 claim과 복구

| 증상 | 원인·조치 |
| --- | --- |
| EC2가 레스큐로 안 넘어가고 Ubuntu에 머묾 | aiha-init.sh가 중단됨. `/var/log/cloud-init-output.log`의 `[aiha-init] ERROR` / `[sysres-setup] ERROR` 확인 |
| aiha-init.sh가 다운로드에서 실패 (`403`/`SignatureDoesNotMatch`) | presigned URL 만료, 또는 MinIO URL을 사내 주소로 만듦. 외부 주소로 다시 만든다 |
| 레스큐로 부팅했는데 아무 일도 일어나지 않음 | `/run/archiso/bootmnt/autorun/autorun2`가 있는지, 실행 권한이 있는지 확인 |
| `statfs /run/aiha: no such file or directory` | 이미지의 `autorun2`가 `sh/autorun2`와 다르고 에이전트가 이전 빌드다. 이미지를 `sh/autorun2`로 갱신하거나 새 에이전트를 쓴다(새 에이전트는 `/run/aiha`를 스스로 만든다) |
| aiha-init.sh가 `sysres.tar has no autorun/autorun2` 로 멈춤 | 이미지에 `autorun/autorun2`를 넣고 `SYSRES_SHA256`을 다시 계산한다. grub 1회 부팅은 이미 설정됐으니 재부팅하기 전에 고친다 |
| autorun2가 실행되지 않음 | `cat /var/log/sysrescue-autorun.log`, `systemctl status sysrescue-autorun`. 이미지의 `autorun0`·`autorun1`이 실패하거나 멈춰 있지 않은지 확인 |
| `read recovery file ... no such file` | `/run/archiso/bootmnt/autorun/aiha/recovery.env` 확인. `copytoram`으로 부팅하지 않았으면 경로가 다를 수 있다 |
| 이미지의 autorun1이 받은 옛 에이전트가 실행됨 | autorun1이 `/autorun/aiha/aiha-rescue`를 덮어쓰지 않는지 확인 |
| `reach framework: ... connection refused / timeout` 반복 | 포트포워딩·방화벽. `--timeout`(기본 30m)까지 재시도한다 |
| `401 Claim token rejected` | 토큰 복사 오류 |
| aiha-init.sh가 `AIHA_FRAMEWORK_CERT_PIN must be true or false` / `CERT_SHA256 must be empty …` 로 멈춤 | 4.2 출력의 `CERT_PIN`·`CERT_SHA256` 두 줄을 그대로 옮기지 않음 |
| 레스큐에서 `x509: …` 오류 | `CERT_PIN=false`인데 레스큐 이미지에 CA 번들이 없거나 시계가 틀림, 또는 URL이 인증서 도메인과 다름 |
| `404 Recovery not found` | recoveryId 오류, 또는 다른 DB를 보는 프레임워크 |
| `410 Claim window expired` | `CLAIM_TTL` 초과. 새 복구 잡을 만든다 |
| `409 Recovery already claimed` | 다른 요청 키로 이미 claim됨(`claim.key`를 지웠거나 레스큐를 다시 부팅함). 새 복구 잡을 만든다 |
| claim이 `409 Restore job names no repository` | 업그레이드 전에 만든 복구 잡. 다시 만든다 |
| claim이 `500 Claim failed` 반복, 서버 로그에 `secret cannot be decrypted` | 비밀 키가 바뀌었다. `repository update-credentials` 후 같은 claim이 성공한다(상태는 바뀌지 않았다) |
| `is not on tmpfs` | `--out-dir`이 RAM이 아님. autorun2는 `/run/aiha`를 만들어 쓴다 |
| `the framework sent an unusable restore plan` | 복구 잡의 `plan`이 예전 형식. `restore create --backup-set`으로 다시 만든다 |
| `restore create`가 `no completed backup job recorded this backup set` | 세트 ID 오류, 실패한 세트, 또는 손으로 한 백업. `job list`·`job get`으로 완료된 잡의 세트 ID 확인 |
| `restore create`가 `more than one backup job recorded this backup set` | 같은 세트 ID가 여러 잡에 기록됨(비정상). 어느 쪽인지 확인 후 처리 |
| `Manifest of backup set … could not be read from the repository` | 저장소에 닿지 않음(S3 아웃바운드, MinIO 포트포워딩), 권한 부족, 또는 파일이 지워짐. 4.4의 `rclone lsf`로 확인 |
| `Backup set … cannot be restored` | 세트나 한 항목이 `completed`가 아님, 또는 매니페스트 버전 불일치. 원본을 다시 백업한다 |
| `Partition layout of backup set … is not supported` | 세트에 `/`가 없음, 원본 UUID·파일시스템 정보가 매니페스트에 없음, 또는 ext4/xfs/vfat이 아닌 파일시스템 |
| `This machine boots with UEFI but the backup set has no EFI system partition` | 세트에 `/boot/efi`가 빠짐. 셋 다 넣어 다시 백업한다 |
| `Network interface of the source cannot be matched to this machine` | 타깃 NIC가 2개 이상이거나 원본 netplan이 MAC을 여러 개 씀. NIC 1개로 인스턴스를 만든다 |
| `Rescue environment lacks a required tool` | 콘솔 로그에 빠진 도구 이름이 나온다. 레스큐 이미지 확인 |
| `Partitions of backup set … cannot be matched to this machine: no partition on this machine has PARTUUID …` | 타깃이 원본과 다른 AMI(파티션 테이블이 다름)다. 같은 AMI로 만든다. 디스크는 바뀌지 않았다 |
| `… more than one partition has PARTUUID …` | 같은 이미지의 디스크가 둘 붙어 있다(예: 원본 볼륨을 타깃에 붙임). 하나를 떼고 다시 실행한다 |
| `Partition … not found on this machine` | 찾은 경로가 블록 장치가 아니다. `lsblk -o PATH,PARTUUID` 확인 |
| 같은 잡을 다시 돌렸더니 `409 Restore already finished` | 이 기능 이전 빌드에서 실패를 보고해 잡이 `failed`로 끝남. 새 복구 잡을 만든다 |
| `Partition … is in use: unmount it before restoring` | 이전 실행의 마운트가 남음. `umount -R` 후 `restore` |
| `UUID of … could not be preserved` | mkfs가 원본 UUID를 받아들이지 않음. 콘솔 로그의 `blkid reports …` 확인. 이미 포맷된 상태다 |
| `Restoring … onto … failed` | 백업 파일 읽기·압축 해제·tar 중 하나가 실패. 콘솔 로그에 어느 단계(`read from the repository` / `decompress` / `unpack`)인지 나온다 |
| `… in the restored system failed` (grub-install 등) | chroot 안 명령 실패. 콘솔 로그의 원문 확인. 파일은 복구돼 있으므로 `restore`로 재시도하거나 수동 조치 |
| 콘솔에 `WARNING: …` | fstab·grub defaults·EFI stub이 이 머신에 없는 UUID를 가리킴. 재부팅 전에 해당 파일을 확인한다 |
| `The framework refused a progress report` | 보고 토큰이 바뀌었거나(다른 곳에서 claim 재수신) 잡이 이미 끝남. 복구는 계속되지만 결과 보고도 거부된다 |
| 복구된 OS로 부팅했다가 잠시 뒤 레스큐로 다시 넘어감 (표식은 있음) | `sudo cat /var/lib/aiha/recovery-pending.json`의 `recoveryId`와 user-data의 `AIHA_RECOVERY_ID`가 다르다 — 복구 도중 `restore create`를 다시 해 새 ID로 복구했는데 user-data는 옛 값 그대로다(4.2). 인스턴스 메타데이터(EC2 user-data, GCP startup-script)를 비운다 |
| 복구된 OS로 부팅했다가 잠시 뒤 레스큐로 다시 넘어감 (표식 검사 없음) | 복구 표식 검사가 없는 이전 aiha-init.sh가 user-data에 있다. 인스턴스를 **중지**하고 user-data를 비우거나 새 aiha-init.sh로 바꾼 뒤 시작한다(레스큐의 claim은 `409`로 거절되므로 디스크는 다시 포맷되지 않는다) |
| `restore finished, but its result could not be reported` | 디스크는 복구됐지만 프레임워크가 모른다. 연결 확인 후 `restore`를 다시 하면 처음부터 다시 포맷하므로, 그대로 두고 수동 확인한다 |

---

## 6. 테스트 후 정리

경로 A (원본 EC2):

```bash
sudo systemctl disable --now aiha-agent          # 또는 run 프로세스 종료
./aiha-framework agent revoke <agentId>          # 프레임워크 쪽 자격 회수 (사내 VM)
sudo aiha-agent reset                            # 호스트의 자격증명 삭제
```

경로 B:

- 타깃 EC2 인스턴스 종료 (레스큐 RAM의 자격증명·보고 토큰은 함께 사라진다)
- 원본 EC2는 경로 A의 정리를 따르거나 종료한다

저장소:

- S3: 테스트 세트 삭제 `aws s3 rm s3://<버킷>/<접두사>/<systemId>/ --recursive`, 완료되지 않은
  멀티파트 업로드는 버킷 수명 주기 규칙(`AbortIncompleteMultipartUpload`)으로 정리한다.
  테스트가 끝나면 IAM 액세스 키를 비활성화·삭제한다.
- MinIO: `mc rm --recursive --force aiha-minio/<버킷>/<접두사>/<systemId>/`, 테스트 사용자 삭제.
- 저장소 등록 삭제 명령은 아직 없다. 지우려면 DB에서:
  `DELETE FROM repositories WHERE name = '<이름>';` (그 저장소를 가리키는 잡·복구 잡이 없을 때만)
- 포트포워딩(프레임워크, MinIO)을 계속 열어 둘 필요가 없으면 닫는다.

공통:

```bash
cd ~/aiha/postgres
# 테스트 데이터만 지울 때
docker compose exec postgres psql -U aiha -d aiha -c "DELETE FROM jobs WHERE system_id = '<systemId>';"
docker compose exec postgres psql -U aiha -d aiha -c "DELETE FROM system_inventories WHERE system_id = '<systemId>';"
docker compose exec postgres psql -U aiha -d aiha -c "DELETE FROM restore_jobs WHERE recovery_id = '<recoveryId>';"
# 컨테이너 정지 (데이터 유지) / 데이터까지 삭제
docker compose stop
docker compose down -v
```

`jobs`를 지우면 `attempts`도 함께 지워진다(`ON DELETE CASCADE`).

값을 채운 aiha-init.sh, `framework.env`, `secret.key`, `/etc/aiha/agent.env`, IAM·MinIO 키는
**저장소에 넣지 않는다.**
