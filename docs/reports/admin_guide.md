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

### 3.4 방문 추적 스크립트 (`/admin/settings` → 방문 추적)

어떤 화면이 실제로 쓰이는지 재기 위해 관리자가 **화면에서** 추적 도구를 붙일 수 있습니다. 설정은 메타 DB(`jasql_settings`)에 저장되고 저장 즉시 재기동 없이 적용됩니다. **기본값은 꺼짐**이며, 새로 설치한 서버는 켜기 전까지 화면도 응답 헤더도 달라지지 않습니다.

| 설정 키 | 뜻 |
| :--- | :--- |
| `tracking_enabled` | 켜야 스니펫이 붙습니다. 기본 꺼짐. 끄면 화면과 정책이 원래대로 돌아갑니다 |
| `tracking_provider` | `none` · `momento` · `ga4` · `gtm` · `matomo` · `custom` |
| `tracking_momento_url` · `tracking_momento_site_id` | Momento 수집기 주소와 사이트 id |
| `tracking_momento_proxy` | 기본 켜짐. 이 서버가 `/momento/*` 를 수집기로 넘깁니다(아래 참고) |
| `tracking_measurement_id` | GA4 · GTM 의 측정 id |
| `tracking_matomo_url` · `tracking_matomo_site_id` | Matomo 주소와 사이트 id |
| `tracking_custom_snippet` | 붙여넣은 `<script>` 스니펫. **8KB** 를 넘으면 저장되지 않습니다 |
| `tracking_allowed_hosts` | 스니펫에서 자동으로 읽지 못한 출처를 더하는 자리(쉼표 구분) |
| `tracking_include_admin` | `/admin` 아래 관리 화면도 추적할지. 기본 아니오 |
| `tracking_placement` | `head`(기본) 또는 `body` |

여러 키가 함께 뜻을 이루므로(제공자 + 주소 + 사이트 id) 저장 시 **바뀐 뒤의 조합 전체**를 검사하며, 잘못된 조합은 아무것도 저장하지 않고 이유를 돌려줍니다. 스니펫은 `/api/*`·`/healthz`·`/mcp`·정적 자산(`nav.js` 등)에는 절대 붙지 않고 HTML 화면에만 붙습니다.

#### Momento (사내 수집기) — 권장

Momento 는 사내 자체 호스팅 수집기라 방문 데이터가 밖으로 나가지 않는 유일한 선택지이며, 제공자 목록의 첫 자리에 있습니다.

1. `tracking_provider` = `momento`, `tracking_momento_url` = 수집기 주소(예: `https://momento.internal:8443`), `tracking_momento_site_id` = 사이트 id 를 넣고 `tracking_enabled` 를 켭니다.
2. `tracking_momento_proxy` 가 켜져 있으면(기본) 화면에는 다음 스니펫이 들어가고, 이 서버가 `/momento/*` 요청을 수집기로 대신 전달합니다. 전달할 때 이 서버의 세션 쿠키·인증 헤더는 **떼어냅니다**.

```html
<script async src="/momento/tracker.js" nonce="…"
        data-site-id="<site_id>" data-environment="prd"
        data-contract-version="1" data-endpoint="/momento"></script>
```

같은 오리진으로 나가므로 외부 출처가 정책(CSP)에 아예 등장하지 않습니다. 프록시를 끄면 수집기 주소가 스니펫과 정책에 직접 적힙니다.

#### 콘텐츠 보안 정책(CSP)과 nonce

`<script>` 한 줄을 넣는 일이 아니라 어려운 쪽은 CSP 입니다. 스니펫을 정책 없이 붙이면 브라우저가 조용히 막고 관리자는 화면이 비어 있는 이유를 알 수 없습니다. SQLON 은 다음과 같이 처리합니다.

* **요청마다 nonce** 를 만들어 스니펫의 **모든** `<script>` 태그에 붙이고(이미 nonce 가 있는 태그는 그대로 둠), 같은 값을 `script-src 'nonce-…'` 에 넣습니다. `'unsafe-inline'` 으로 정책을 푸는 일은 **하지 않습니다** — 한 번 풀면 추적을 끈 뒤에도 느슨한 채로 남기 때문입니다.
* **정책 출처는 스니펫에서 읽어 냅니다.** 붙여 넣은 스니펫 안의 `http(s)://…` 출처를 긁어 `script-src`·`connect-src`·`img-src` 에 자동으로 더하고, 제공자별로 필요한 출처(GA4/GTM 의 googletagmanager 등)도 함께 더합니다. 못 읽은 출처는 `tracking_allowed_hosts` 에 손으로 더합니다.
* 추적이 **켜진 화면**에만 다음 두 헤더가 나갑니다. 꺼지면 두 헤더 모두 사라져 원래대로 돌아갑니다.

| 헤더 | 내용 | 비고 |
| :--- | :--- | :--- |
| `Content-Security-Policy` | `img-src 'self' data: blob: <출처…>; connect-src 'self' ws: wss: <출처…>; report-uri /api/tracking/csp-report` | **강제**. 스니펫의 신호(beacon)·픽셀은 여기에 적힌 출처로만 나갈 수 있습니다 |
| `Content-Security-Policy-Report-Only` | `script-src 'self' 'nonce-<요청 nonce>' <출처…>; report-uri /api/tracking/csp-report` | **보고 전용**. 현재 관리 화면이 인라인 이벤트 핸들러(`onclick="…"`)를 쓰고 있어 script-src 를 강제하면 화면이 멈춥니다. 대신 위반을 신고만 받아 아래 표에 보이며, 인라인 핸들러를 걷어낸 뒤 같은 헤더를 강제로 바꿀 수 있게 준비되어 있습니다 |

> [!NOTE]
> 스니펫에 `İ`(U+0130) 나 `K`(U+212A 켈빈 기호)처럼 소문자로 바꾸면 바이트 길이가 달라지는 글자가 섞여 있어도 nonce 는 태그에 제대로 붙습니다. 태그 검색은 ASCII 만 접어서 비교합니다.

#### 차단된 출처 보기와 허용

추적이 켜져 있는 동안 브라우저는 정책에 막힌 요청을 `POST /api/tracking/csp-report` 로 신고하고, 서버는 **출처와 지시어**를 메모리에 기억합니다(같은 출처는 횟수만 늘어나며 최대 100건, 재기동 시 비워짐). `/admin/settings` 아래 **"방문 추적 — 차단된 출처"** 표에 `차단`(강제 정책이 막음) / `보고`(보고 전용 정책이 표시) / `허용됨`(이미 정책에 있음)으로 보이고, **[허용]** 을 누르면 `tracking_allowed_hosts` 에 더해져 즉시 정책에 반영됩니다.

```bash
# 차단된 출처 목록 (admin)
curl -H "X-Admin-Token: $SQLON_ADMIN_TOKEN" http://localhost:6767/api/tracking/violations
# 한 번에 허용
curl -X POST -H "X-Admin-Token: $SQLON_ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"origin":"https://collector.example.com"}' http://localhost:6767/api/tracking/allow
# 기록 지우기
curl -X DELETE -H "X-Admin-Token: $SQLON_ADMIN_TOKEN" http://localhost:6767/api/tracking/violations
```

> [!IMPORTANT]
> 로그인 화면(`/auth/login`)도 추적 대상이지만 스니펫은 페이지 방문만 보냅니다. 자격 증명이나 개인 식별 값을 보내는 스니펫을 붙이지 마십시오. 관리 화면(`/admin/*`)은 `tracking_include_admin` 이 켜졌을 때만 추적합니다.

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
