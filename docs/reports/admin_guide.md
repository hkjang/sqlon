# [관리자 가이드] JAMYPG NL2SQL MCP 시스템 관리자 가이드

**보고 부서**: AI 인프라실  
**작성 일자**: 2026년 7월 8일  
**문서 버전**: v1.1.0  

---

## 1. 아키텍처 구성 및 핵심 컴포넌트

JAMYPG는 경량 인메모리 카탈로그 컴파일 파이프라인을 탑재하여 가볍고 견고하게 동작하는 엔터프라이즈 NL2SQL 게이트웨이 서비스입니다.

```
                  ┌─────────────────────────────────────┐
                  │          Client (AI Portal)         │
                  └──────────────────┬──────────────────┘
                                     │ JSON-RPC (HTTP SSE / stdio)
┌────────────────────────────────────▼──────────────────────────────────┐
│                            JAMYPG MCP Server                          │
│                                                                       │
│  ┌──────────────────┐    ┌──────────────────┐    ┌─────────────────┐  │
│  │   internal/mcp   │    │ internal/catalog │    │ internal/dbconn │  │
│  │  (SSE/NDJSON)    │    │ (Compilation/    │    │ (pgx / go-sql-  │  │
│  │   [API Layer]    │ ──►│   Verification)  │ ──►│  driver Pools)  │  │
│  └──────────────────┘    │ [Domain Engine]  │    └────────┬────────┘  │
│                          └────────┬─────────┘             │           │
└───────────────────────────────────┼───────────────────────┼───────────┘
                                    │                       │ read-only SQL
                       materialize  │                       ▼
┌───────────────────────────────────▼──┐          ┌───────────────────┐
│              Postgres                │          │ PostgreSQL/MySQL/ │
│            (Meta Database)           │          │ MariaDB (Targets) │
└──────────────────────────────────────┘          └───────────────────┘
```

### 아키텍처 레이어 요약
- **`internal/mcp`**: JSON-RPC 프로토콜 및 stdio/HTTP SSE 전송 채널을 핸들링하며, 웹 관리 뷰 및 OpenAPI Swagger 문서를 배선합니다.
- **`internal/catalog`**: 물리/논리 스키마를 메모리에 색인화하고, Glossary 어휘 치환, **GraphRAG 기반 컨텍스트 검색(`graph.go`)**, 조인 그래프 탐색, 33종 정적 검증(`validate.go`)을 통과시키는 코어 엔진입니다.
- **`internal/dbconn`**: 순수 Go 드라이버(`jackc/pgx/v5`, `go-sql-driver/mysql`)를 포장하여 PostgreSQL·MySQL·MariaDB 3종 방언 추상화, 쿼리 실행 제어, 타임아웃 강제화, 결과 캐시 및 서킷 브레이커를 물리 제어하는 커넥터 계층입니다.

---

## 2. 빌드 및 오프라인 배포 가이드

### 2.1. 순수 Go 단일 빌드 (CGO 불필요)
PostgreSQL·MySQL·MariaDB 3종 연결 풀 및 실시간 실행이 모두 순수 Go 드라이버로 구동되므로, CGO나 별도 빌드 태그 없이 단일 빌드로 전체 기능을 지원합니다.

```bash
# [Linux/Unix 빌드 환경]
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ./bin/jamypg-mcp ./cmd/jamypg-mcp

# [Windows 빌드 환경]
go build -o ./bin/jamypg-mcp.exe ./cmd/jamypg-mcp

# [3종 아티팩트 일괄 빌드: windows-amd64 / linux-amd64 / linux-arm64]
./scripts/build.sh   # dist/jamypg-mcp-windows-amd64.exe, -linux-amd64, -linux-arm64 산출
```

### 2.2. 오프라인 폐쇄망 Docker 패키징 가이드
인터넷 접근이 제한된 금융 오프라인 데이터 센터 기동 시에도 별도의 DB 클라이언트 라이브러리 반입이 전혀 필요 없습니다. 저장소 루트의 단일 `Dockerfile`(정적 바이너리, `CGO_ENABLED=0`) 하나로 3종 DB를 모두 커버합니다.

```bash
# 1. 단일 Dockerfile 컴파일 기동 빌드 (클라이언트 라이브러리 사전 준비 불필요)
docker build -t jamypg-mcp:v0.1.0 .

# 2. 이미지 압축 내보내기
docker save jamypg-mcp:v0.1.0 | gzip > jamypg-mcp-v0.1.0.tar.gz

# 3. 폐쇄망 반입 후 로드 및 기동
docker load < jamypg-mcp-v0.1.0.tar.gz
docker run -p 9797:9797 jamypg-mcp:v0.1.0
```

---

## 3. 세부 설정 사양 및 환경변수

서버의 작동 한계를 물리적으로 통제하기 위한 실행 플래그 및 환경변수 목록입니다.

| 실행 플래그 | 매핑 환경변수명 | 세부 기능 및 설정 범위 |
| :--- | :--- | :--- |
| `-transport` | `JAMYPG_TRANSPORT` | 호출 채널 형식 제어 (`stdio` 혹은 `http` / 기본값: `stdio`) |
| `-addr` | `JAMYPG_ADDR` | HTTP 통신 가동 시 리슨 소켓 주소 (`0.0.0.0:9797` 등 / 기본값: `127.0.0.1:9797`) |
| `-admin-token` | `JAMYPG_ADMIN_TOKEN` | 웹 뷰`/admin` 데이터셋 교체/삭제 시 요구할 마스터 인증 키 |
| `-meta-db` | `JAMYPG_META_DB` | 인증/사용자/권한 관리를 위해 가동할 Postgres 접속 DSN 주소 |
| `-bootstrap-admin` | `JAMYPG_BOOTSTRAP_ADMIN` | 메타 DB 최초 기동 시 생성할 초기 관리자 계정 (`username:password` 형식) |
| `-oidc-issuer` | `JAMYPG_OIDC_ISSUER` | Keycloak SSO 서버 주소 영역 (OIDC Provider URL) |
| `-oidc-client-id` | `JAMYPG_OIDC_CLIENT_ID` | Keycloak SSO 인증용 클라이언트 식별자 ID |
| `-oidc-client-secret`| `JAMYPG_OIDC_CLIENT_SECRET`| Keycloak SSO Secret UUID 문자열 |
| `-oidc-redirect-url` | `JAMYPG_OIDC_REDIRECT_URL` | SSO 성공 후 돌아올 콜백 가상 라우터 주소 |

---

## 4. 메타데이터셋 규격 및 관리

JAMYPG는 재기동 없이 카탈로그 정보를 핫스왑 컴파일할 수 있으며, 이의 원천이 되는 JSON 파일들의 정합성을 보존해야 합니다.

### 4.1. 18종 메타데이터 레지스트리 핵심 요약
- **`meta_physical_models.json` (필수)**: 물리 테이블명, 컬럼명, 데이터 유형 및 길이 원장.
- **`meta_logical_models.json` (필수)**: 물리 스키마 정보에 일대일 매핑되는 논리 한글 이름 명세서.
- **`topology_relations.json` (선택)**: 조인 경로의 뼈대가 되는 테이블 간의 릴레이션 매핑 키 정의서.
- **`glossary.json` (선택)**: 한글 동의어 사전. 물리 컬럼 축약 단어를 동의어에 주입하여 그라운딩 정확도를 개선합니다.
- **`metrics.json` (선택)**: 비즈니스 통계 지표 사전. 표준 expression 식을 주입해 둡니다.
- **`overrides.json` (선택)**: PII 개인정보 지정, 운영자 기본 필터 지침, 생성 SQL 방언(`dialect`) 지정을 기재하여 물리 구조 변경 없이 패치 적용하는 보정용 원장.

---

## 5. 인증 및 권한 거버넌스 체계

Postgres 메타 DB 모드를 켤 경우 아래의 다층 접근 제어 체계가 활성화됩니다.

- **역할군 통제 (Roles)**:
  - `admin`: 전체 데이터셋 변경 권한, 전체 DB 프로파일 관리 권한, 사용자 추가 및 역할 수여 권한을 가집니다.
  - `user`: 본인의 개인용 DB 프로파일 등록 권한, 본인의 MCP API Key 제어 권한만 가지며 관리자가 허용한 공유 프로파일만 볼 수 있습니다.
- **DB 프로파일 visibility & grant**:
  - `visibility: private`: 프로파일 생성자와 admin만 조회가 가능합니다.
  - `visibility: shared`: 로그인한 모든 사내 사용자가 `use` 권한으로 실행에 투입할 수 있습니다.
  - `grant`: `/admin/db` 화면의 **[권한·공유]** 버튼을 통해 특정 사용자를 골라 `use` 또는 `manage` 세부 권한을 직접 양도할 수 있습니다.
- **감사 추적 (Audits)**:
  사용자 생성, API Key 발급/회전/삭제, DB 프로파일 권한 공유 등의 모든 상태 변경 이벤트가 백엔드 `audit/*.jsonl` 이력 파일에 주체자 IP 정보와 함께 고스란히 저장되며, 실 DB 쿼리 실행 이력은 `audit/query-YYYYMMDD.jsonl` 일자별 파일에 적재되어 정보보안 감사 자료로 영구 활용됩니다.

---

## 6. 프로메테우스(Prometheus) 모니터링 연동

`-addr` 기반 HTTP 가동 시 `GET /metrics` 라우터로 프로메테우스 포맷 메트릭을 출력하여 사내 Grafana 시스템 등에 결착할 수 있습니다.

- **`db_query_total` / `db_query_success` / `db_query_failure` / `db_query_slow`**: 대상 DB 질의 누적 요청/성공/실패/슬로우 쿼리 횟수. (라벨: `profile`)
- **`db_connection_ping_failure_total`**: 대상 DB 물리 접속 실패 누적 카운트.
- **`db_pool_open_connections` / `db_pool_in_use_connections` / `db_pool_idle_connections`**: 커넥션 풀 가용 상태 지표.
- **`catalog_compilation_duration_seconds`**: 메타데이터 카탈로그 재컴파일에 소요된 레이턴시.
- **`mcp_tool_calls_total`**: 각 MCP 도구별 호출 빈도 지표. (라벨: `tool`, `error: true|false`)

---

## 7. MCP 28종 도구 목록 분류 명세 (관리자 참조용)

JAMYPG 백엔드에 기동되는 **28종 전체 MCP 도구**의 역할 기반 분류 일람표입니다.

1. **질문 해석 및 정규화 (4종)**: `prepare_sql_context`, `analyze_question`, `resolve_time`, `find_filter_columns`
2. **스키마 검색 및 탐색 (4종)**: **`retrieve_context` (GraphRAG)**, `search_schema`, `search_examples`, `get_column_stats`
3. **업무 근거 및 지표 수집 (3종)**: `get_metric_definition`, `get_join_paths`, `get_schema_context`
4. **SQL 프레임 조립 및 검증 (3종)**: `build_sql_skeleton`, `validate_sql`, `rank_candidates`
5. **데이터베이스 실행 통제 (3종)**: `explain_sql`, `list_db_profiles`, `run_sql_safely`
6. **피드백 적재 및 학습 루프 (2종)**: `record_feedback`, `learn_from_feedback` (재질문 corrected prior 학습)
7. **메타 데이터셋 조작 및 관리 (9종)**: **`suggest_join_relations` (조인 갭 분석)**, `suggest_joins`, `get_catalog_health`, `run_evaluation` (retrieval mode 포함), `list_datasets`, `get_dataset`, `put_dataset`, `remove_dataset`, `reload_catalog`

---

## 8. 상세 트러블슈팅 가이드 (Symptom-Cause-Action)

| 현상 (Symptom) | 상세 원인 (Cause) | 즉각 해결 방법 (Action) |
| :--- | :--- | :--- |
| **PG-28P01 / MY-1045 인증 에러 반환** | 1) 프로파일의 `username` 또는 `password_ref`(`env:` / `file:` / `plain:`)가 실제 계정 정보와 불일치<br>2) `env:` 참조 환경변수가 컨테이너에 미주입된 경우 | 1) `/admin/db` 화면에서 프로파일 접속 테스트를 수행하여 계정 정합성을 확인합니다.<br>2) 기동 환경에 참조 환경변수가 정상 주입되었는지 점검하고 재기동합니다. |
| **CIRCUIT_OPEN 에러 반환** | 특정 DB 프로파일에 대한 질의가 단기간 연속 3회 이상 타임아웃 혹은 접속 에러(PG-/MY- 계열)를 내어 서킷이 열린 상태 | 1) 서킷 브레이커는 30초간 강제 차단 모드를 유지합니다.<br>2) 30초 동안 DB 방화벽, 패스워드 만료 여부를 점검하고 설정을 보정한 뒤 재접속합니다. |
| **put_dataset 호출 시 롤백됨** | 새로 밀어 넣은 JSON 데이터셋 정보가 기존 카탈로그의 물리 개체들과 정합성이 맞지 않아 핫스왑 컴파일이 기각됨 | 1) 응답 내 `issues` 지표의 에러 사유(예: 존재하지 않는 컬럼 참조 등)를 파악합니다.<br>2) 오류 교정 후 재시도하거나, 불완전함을 무시하려면 `force: true`를 주입해 씁니다. |
| **Keycloak SSO 연동 후 Callback 401/500 에러** | `-oidc-redirect-url`에 지정한 가상 라우터 주소와 Keycloak 콘솔에 지정한 Client Redirect URI 정보가 일치하지 않는 경우 | 1) Keycloak 어드민 콘솔에 접속하여 Client 설정 내 Redirect URIs 목록을 조회합니다.<br>2) JAMYPG 시작 플래그에 주입한 콜백 주소 문자열과 철자 하나까지 완전히 일치시킵니다. |
| **기동 시 load catalog 에러** | 물리/논리 스키마 파일이 아예 누락되었거나 테이블 적재량이 0개인 경우 | 1) `-data` 플래그 경로 아래에 `meta_physical_models.json` 및 `meta_logical_models.json`이 존재하는지 확인합니다.<br>2) 각 JSON 파일 내 항목이 하나 이상 정상 적재되었는지 규격을 검토합니다. |
