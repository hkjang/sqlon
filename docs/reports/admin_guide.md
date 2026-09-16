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

### 3.4 이벤트 알림 메일 (사내 SMTP 릴레이)

승인을 기다리는 변경 계획, 실패로 멈춘 실행, 오래 걸린 쿼리의 완료처럼 **사람이
실제로 기다리는 일**을 사내 SMTP 릴레이로 알립니다. **기본값은 꺼짐**이라 새로
설치한 곳에서는 아무것도 달라지지 않으며, 메타 DB(`-meta-db`)가 있을 때만
동작합니다(설정과 사용자 주소를 거기서 읽습니다).

설정은 `/admin/settings`(또는 `PUT /api/settings`)의 「메일 알림 (SMTP)」 그룹에
있고 저장 즉시 적용됩니다(재기동 불필요). 키 이름은 사내 메일 표준과 같습니다.

| 키 | 기본값 | 뜻 |
|---|---|---|
| `mail.enabled` | `false` | 꺼짐이 기본. 관리자가 켭니다 |
| `mail.smtp_host` | — | 사내 릴레이 주소. 켜져 있어도 비어 있으면 보내지 않고 로그에 이유를 남깁니다 |
| `mail.smtp_port` | `25` | 사내 릴레이는 대개 25. 587=STARTTLS, 465=암시적 TLS(자동 인식) |
| `mail.security` | `auto` | `auto` · `none` · `starttls` · `tls`. `auto`는 서버가 STARTTLS를 알리면 쓰고 아니면 평문 |
| `mail.skip_tls_verify` | `false` | 사내 사설 인증서일 때만 `true` |
| `mail.username` · `mail.password` | 빈 값 | **선택**. 비우면 인증 없이 보냅니다. 비밀번호는 저장 뒤 「설정됨」만 보이고 API로 되읽을 수 없으며 로그에도 남지 않습니다 |
| `mail.from_address` · `mail.from_name` | `sqlon@<호스트>` · `sqlon` | 보내는 사람 |
| `mail.base_url` | — | 메일 속 「바로 열기」 링크가 가리킬 이 서버 주소(예: `https://sqlon.corp`). 비우면 링크를 넣지 않습니다 |
| `mail.timeout_seconds` | `10` | 연결·세션 제한 시간 |
| `mail.notify_change_review` | `true` | 변경 계획이 승인 대기(`review_required`)에 들어가거나 승인이 모두 모여 실행 가능(`approved`)해지면 → dba/admin |
| `mail.notify_change_failed` | `true` | 변경 실행·롤백이 실패로 멈추면(`rollback_required`/`failed`) → dba/admin |
| `mail.notify_query_finished` | `true` | 1분 넘게 걸린 비동기 쿼리가 끝나면(성공·실패) → 제출한 사용자 |
| `mail.notify_scheduler` | `true` | 예약 메타데이터 동기화가 실패로 바뀌거나 회복되면 → admin (틱마다 아니라 전환 시 1회) |

동작 원칙:

* **요청을 막지 않습니다.** 메일은 배경에서 두 번까지 시도하며, 릴레이가 죽어
  있어도 변경 제출·승인 같은 요청은 정상으로 끝납니다.
* **시도마다 기록합니다.** 언제·어떤 이벤트·누구에게·제목·결과(`sent`/`failed`)·
  시도 횟수·오류를 `data/mail/deliveries-YYYYMMDD.jsonl`에 남기고 최근 500건을
  설정 화면과 `GET /api/mail/deliveries`에서 봅니다. **본문은 저장하지 않습니다.**
* **사용자 명부를 따로 두지 않습니다.** 수신자는 사용자 관리의 계정 메일 주소를
  그대로 쓰므로, 주소가 비어 있거나 비활성인 계정은 조용히 건너뜁니다.
* **자기가 한 일은 자기에게 보내지 않습니다.** 제출자는 승인 요청 메일을, 승인자는
  실행 가능 메일을 받지 않습니다.
* 폐쇄망에서는 릴레이로 사내 메일 서비스 `postra`를 가리키면 알림이 밖으로 나가지
  않습니다.

**시험 발송** — 설정을 저장한 뒤 같은 화면의 「메일 시험 발송」 칸에 수신자를 넣고
(비우면 내 계정 주소) 단추를 누르면 저장된 설정으로 실제 한 통을 보내고 결과를
그 자리에서 보여 줍니다. 릴레이 설정은 한 번에 맞는 일이 드무니 켠 뒤 꼭 한 번
보내 보세요. REST로는:

```bash
curl -X POST http://localhost:6767/api/mail/test \
  -H "X-Admin-Token: SecureMasterToken2026!" \
  -H "Content-Type: application/json" \
  -d '{"recipient": "dba-team@corp.example"}'
# 200 {"ok":true,"recipient":"..."}  /  502 {"ok":false,"error":"SMTP 연결 실패(...)"}

curl "http://localhost:6767/api/mail/deliveries?status=failed&limit=20" \
  -H "X-Admin-Token: SecureMasterToken2026!"
```

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
