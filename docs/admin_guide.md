# SQLON 시스템 관리자 및 DBA 가이드

> **문서 보안 등급**: 대외비 (Confidential)  
> **최종 수정일**: 2026년 7월 20일  
> **문서 버전**: v0.1.2  
> **대상**: 시스템 관리자, 데이터베이스 관리자(DBA), DevOps 엔지니어 및 정보보안 담당자  

---

## 1. 시스템 아키텍처 및 작동 원리

**SQLON**은 이종 멀티 데이터베이스 환경을 통합 관리하고, 자연어-SQL 변환 및 안전한 읽기 전용 실행 서비스를 제공하는 고성능 Go 기반 엔터프라이즈 마이크로서비스입니다.

```mermaid
graph TB
    Client[Client Apps / MCP Clients / Web UI] -->|HTTP / stdio| Server[SQLON Core Server :6767]
    
    subgraph Core Engine
        Server --> Admin[Admin & Fleet Manager]
        Server --> Sync[Metadata Sync Engine]
        Server --> Guard[Safety Guardrail & EXPLAIN Analyzer]
        Server --> Exec[Read-Only Execution Engine]
    end

    subgraph Internal Storage
        Sync <--> MetaDB[(Built-in MetaDB / SQLite)]
    end

    subgraph Target Databases
        Exec -->|PostgreSQL Driver| PG[(PostgreSQL)]
        Exec -->|MySQL Driver| MY[(MySQL / MariaDB)]
        Exec -->|godror / ODPI-C| ORCL[(Oracle Client)]
    end
```

### 지원 데이터베이스 및 요구사항
* **PostgreSQL**: v12 이상 지원 (Standard Alpine 빌드 포함)
* **MySQL / MariaDB**: MySQL v8.0+, MariaDB v10.5+ 지원 (Standard Alpine 빌드 포함)
* **Oracle**: 19c, 21c, 23c 지원 (CGO 및 Oracle Instant Client 패키징된 `sqlon-oracle` 이미지 사용)

---

## 2. 설치 및 오프라인망(폐쇄망) 배포 가이드

SQLON은 인터넷 연결이 불가능한 **오프라인망(Air-Gapped Network)** 환경에서도 완벽히 작동할 수 있도록 사전 구성된 독립형 도커 패키지(`.tar.gz`)를 제공합니다.

### 2.1 도커 이미지 패키지 구성

GitHub Release에서 제공하는 두 가지 도커 이미지 아카이브 중 요구되는 DB 환경에 맞춰 선택합니다.

| 배포 패키지 파일명 | 파일 크기 | 대상 DB 엔진 | 특징 |
| :--- | :--- | :--- | :--- |
| `sqlon-v0.1.2-docker.tar.gz` | ~24.4 MB | PostgreSQL, MySQL, MariaDB | 경량 Alpine Linux 3.21 기반 이미지 |
| `sqlon-oracle-v0.1.2-docker.tar.gz` | ~138.1 MB | Oracle (선택적 PG/MySQL 지원) | Oracle Linux 9 slim + Instant Client 번들 |

---

### 2.2 오프라인망 배포 절차

```mermaid
sequenceDiagram
    participant Admin as 관리자
    participant File as Release Tarball
    participant Docker as Offline Docker Host
    
    Admin->>File: 인터넷 환경에서 tar.gz 및 SHA256SUMS 다운로드
    Admin->>File: SHA256 해시 검증
    Admin->>Docker: 오프라인망 서버로 파일 이관 (USB / SFTP)
    Docker->>Docker: docker load -i sqlon-v0.1.2-docker.tar.gz
    Docker->>Docker: docker run (볼륨 마운트 & 환경변수 설정)
```

#### Step 1. 파일 검증 및 이관
```bash
# SHA256 해시 검증
sha256sum -c SHA256SUMS.txt
```

#### Step 2. 도커 이미지 로드 (Load)
```bash
# 표준판 로드
docker load -i sqlon-v0.1.2-docker.tar.gz

# Oracle판 로드 시
docker load -i sqlon-oracle-v0.1.2-docker.tar.gz
```

#### Step 3. 컨테이너 기동 (Run)
메타데이터 지속성 및 감사 로그 보관을 위해 호스트 디렉토리를 마운트합니다.

```bash
mkdir -p /opt/sqlon/data /opt/sqlon/logs

docker run -d \
  --name sqlon-app \
  --restart always \
  -p 6767:6767 \
  -e SQLON_ADMIN_TOKEN="SecureMasterToken2026!" \
  -v /opt/sqlon/data:/app/data/sqlon \
  sqlon/sqlon:v0.1.2
```

---

## 3. 데이터베이스 프로필 및 관리자 보안 설정

### 3.1 마스터 보안 토큰 (`SQLON_ADMIN_TOKEN`)
관리자 REST API 및 `/admin/db` 관리자 웹 페이지는 마스터 토큰으로 보호됩니다.
* 환경 변수 `SQLON_ADMIN_TOKEN`을 설정하지 않으면 관리자 설정 변경 기능이 비활성화됩니다.
* API 호출 시 Header에 `Authorization: Bearer <TOKEN>`을 전송해야 합니다.

---

### 3.2 데이터베이스 프로필 등록 (`/admin/db` 또는 REST API)

관리자 API를 통해 대상 DB 연결 정보를 안전하게 등록합니다.

```bash
curl -X POST http://localhost:6767/api/fleet/instances \
  -H "Authorization: Bearer SecureMasterToken2026!" \
  -H "Content-Type: application/json" \
  -d '{
    "profile_id": "pg_analytics",
    "engine": "postgres",
    "host": "192.168.10.50",
    "port": 5432,
    "database": "dw_prod",
    "username": "sqlon_ro_user",
    "password_secret": "env:PG_PROD_PW",
    "max_open_conns": 20,
    "max_idle_conns": 5
  }'
```

> [!IMPORTANT]
> **비밀번호 마운트 모범 사례**
> 비밀번호는 평문 저장을 지양하고 `env:VAR_NAME` (컨테이너 환경변수 참조) 또는 `file:/path/to/secret` (보안 파일 마운트) 방식으로 지정하십시오.

---

### 3.3 DB 계정 권한 구성 (최소 권한 원칙)

SQLON이 사용할 DB 계정은 반드시 **Read-Only** 권한만 부여되어야 합니다.

#### PostgreSQL
```sql
CREATE USER sqlon_ro WITH PASSWORD 'secure_password';
GRANT CONNECT ON DATABASE dw_prod TO sqlon_ro;
GRANT USAGE ON SCHEMA public TO sqlon_ro;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO sqlon_ro;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO sqlon_ro;
```

#### MySQL / MariaDB
```sql
CREATE USER 'sqlon_ro'@'%' IDENTIFIED BY 'secure_password';
GRANT SELECT, SHOW VIEW ON dw_prod.* TO 'sqlon_ro'@'%';
FLUSH PRIVILEGES;
```

#### Oracle
```sql
CREATE USER sqlon_ro IDENTIFIED BY secure_password;
GRANT CREATE SESSION TO sqlon_ro;
GRANT SELECT ANY TABLE TO sqlon_ro;
```

---

### 3.4 다른 서비스로 문서 넘기기 — 허용 목록 설정 (`handoff_targets`)

SQLON은 사내 「서비스 간 문서 넘기기 표준(HANDOFF-STANDARD)」의 **보내는 쪽**입니다.
DBA 코파일럿(`/admin/dba`) 상단의 DBA 다이제스트를 `markdown` 으로 `muni`·`ptium`·`weekly`
에 넘길 수 있습니다. 사람이 파일을 내려받아 다시 올리지 않습니다.

* 서비스끼리 자격 증명을 들고 있지 않습니다. 로그인한 사용자가 **한 번만 쓸 수 있는
  5분짜리 표(claim)** 를 발급받고(`POST /api/v1/handoff/claims`), 브라우저가 받는 쪽의
  `/handoff?source=<이 서비스>&claim=<표>` 를 새 창에서 엽니다. 받는 쪽은 표만 들고
  `GET /api/v1/handoff/claims/{claim}` 으로 본문을 직접 받아 갑니다(인증 없음, 1회, 이후 `404`).
* 표는 **그 사용자가 읽을 수 있는 프로파일 하나의 다이제스트**에만 묶입니다. 남의
  비공개 프로파일로는 표가 만들어지지 않습니다(`404`). 표 값은 감사 로그에 남지 않으며
  문서 id(`dba-digest:<profile>`)와 사용자만 `handoff_claim_create`/`handoff_claim_serve` 로 기록됩니다.
* 재기동하면 발급된 표는 사라집니다(메모리 보관). 5분짜리 표이므로 다시 누르면 됩니다.

**설정 방법** — `/admin/settings` (admin) → 「문서 넘기기」 그룹, 또는 `PUT /api/settings`:

| 키 | 값 | 설명 |
| --- | --- | --- |
| `handoff_targets` | `서비스=오리진` 을 쉼표로 나열 | 예: `muni=https://muni.intra, ptium=https://ptium.intra, weekly=https://weekly.intra`. 서비스 이름은 표준의 형식 표(`muni`·`kanpic`·`ptium`·`weekly`)에 있어야 하며, **markdown 을 받을 수 있는 서비스만** 단추에 나타납니다(`kanpic` 은 적어도 무시). 오리진은 `http(s)://호스트[:포트]` 만 허용되고 경로·쿼리가 있으면 무시됩니다. |
| `handoff_public_url` | (선택) `https://sqlon.intra` | 받는 쪽이 표를 가져올 때 쓰는 이 서비스의 오리진. 비우면 요청의 `Host`(리버스 프록시의 `X-Forwarded-Proto`/`X-Forwarded-Host` 반영)로 만듭니다. 프록시 뒤에서 `Host` 가 바뀌는 환경이면 명시하십시오. |

```bash
curl -X PUT http://localhost:6767/api/settings \
  -H "X-Admin-Token: $SQLON_ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"handoff_targets":"muni=https://muni.intra, ptium=https://ptium.intra, weekly=https://weekly.intra",
       "handoff_public_url":"https://sqlon.intra"}'
```

> [!NOTE]
> 기본값은 **비어 있음**이며 그때는 「다른 서비스로 보내기」 단추가 보이지 않습니다.
> 새로 설치한 곳에서는 아무것도 달라지지 않습니다. 저장 즉시 적용되며 재기동은 필요 없습니다.
> 받는 쪽(muni·ptium·weekly)의 허용 목록에도 이 서비스의 오리진(`handoff_public_url`)이
> 등록되어 있어야 문서가 열립니다 — 그쪽 관리자에게 함께 요청하십시오.

---

## 4. 메타데이터 동기화 및 관측성(Observability)

### 4.1 스키마 메타데이터 자동 동기화 (`metasync`)
SQLON은 데이터베이스 테이블 구조, 인덱스 현황 및 테이블/컬럼 Comment 정보를 지속적으로 캡처하여 메타 DB에 저장합니다.

```bash
# 수동 메타데이터 동기화 트리거
curl -X POST http://localhost:6767/api/meta/sync \
  -H "Authorization: Bearer SecureMasterToken2026!" \
  -d '{"profile_id": "pg_analytics"}'
```

---

### 4.2 플릿 헬스 체크 및 워크로드 모니터링

SQLON은 시스템 상태 점검 및 관측성 엔드포인트를 제공합니다.

| 엔드포인트 | Method | 역할 | 비고 |
| :--- | :--- | :--- | :--- |
| `/healthz` | GET | Liveness & Readiness 점검 | Load Balancer 헬스체크용 |
| `/api/fleet/instances` | GET | 등록된 DB 인스턴스 상태 및 핑 현황 | 인증 필요 |
| `/api/observability/sessions` | GET | 현재 실행 중인 쿼리 세션 추적 | 롱 러닝 쿼리 감지 |
| `/api/observability/locks` | GET | DB 락(Lock) 대기 현황 점검 | 블로킹 쿼리 탐지 |
| `/api/observability/top-sql` | GET | 자원 소비 상위 SQL 분석 | 성능 튜닝 지표 |

---

### 4.3 감사 로그 (Audit Log) 관리

모든 자연어 질의, 생성된 SQL, 실행 주체, 실행 시간 및 리스크 스코어는 `/app/data/metadb/audit` 경로에 암호화되어 기록됩니다.

> [!WARNING]
> 보안 정책 준수를 위해 감사 로그 디렉토리를 정기적으로 외부 저장소에 백업하고 minimum 1년간 보관하도록 백업 스케줄러를 구성하십시오.

---

## 5. 장애 조치 (Troubleshooting)

| 발생 장애 | 원인 | 문제 해결 절차 |
| :--- | :--- | :--- |
| `DB Connection Failure (DNS)` | Docker 컨테이너 내 `localhost` 지정 오류 | `localhost`는 컨테이너 자신을 의미하므로 호스트 IP 또는 `host.docker.internal` 사용 |
| `Oracle Library Error (libclntsh.so)` | Standard 이미지를 Oracle DB에 연결 시도 | `sqlon-oracle:v0.1.2` 도커 이미지로 재배포 |
| `HTTP 401 Unauthorized` | 마스터 토큰 누락 또는 불일치 | `SQLON_ADMIN_TOKEN` 값과 API 헤더 토큰 일치 여부 확인 |
| `MetaDB Disk Full` | 감사 로그 누적에 따른 디스크 부족 | 마운트 볼륨 디스크 용량 증설 및 오래된 감사 로그 아카이빙 |
