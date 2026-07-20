# SQLON AI Database Operations Platform

<p align="center">
  <img src="internal/mcp/webui/logo-transparent.png" alt="sqlon Logo" width="180" />
</p>

SQLON is an evidence-driven database operations platform for DBAs and SREs.
The existing metadata-grounded NL2SQL flow remains available as **SQL Lab**;
the product is being transitioned to fleet observation, diagnosis, and
approval-gated change control across PostgreSQL, MySQL, MariaDB, and Oracle.

The standard image supports PostgreSQL/MySQL/MariaDB with pure Go drivers.
Oracle profiles, capability contracts, and fail-closed SQL policy are present;
the OCI/godror runtime is intentionally delivered in the separate
`sqlon-oracle` build.

The server loads JSON metadata from a dataset directory (e.g. `data/metadb`,
`data/sakila`), compiles it into an in-memory catalog, search index, join
graph, prompt registry, and static SQL guardrails, then exposes them through
MCP tools, resources, and prompts. Generated SQL can be executed read-only
against any of the three target engines through pure-Go drivers — no CGO, no
client libraries, no build tags.

## Built with Codex and GPT-5.6

SQLON was developed through a human-directed AI engineering workflow using OpenAI Codex and GPT-5.6.

### How Codex Was Used

Codex served as the primary implementation agent throughout the project. It was used to:

* Explore and understand the existing Go codebase
* Implement MCP tools, resources, prompts, and transport behavior
* Develop metadata catalog, search, and join-graph features
* Add PostgreSQL, MySQL, and MariaDB connectivity
* Implement SQL validation and read-only execution guardrails
* Create REST APIs, administration features, and integration tests
* Refactor duplicated code and improve error handling
* Update technical documentation alongside source-code changes

Development was performed iteratively. Each task was defined with explicit goals and constraints, and Codex generated or modified the relevant code. The resulting changes were then reviewed, tested, and refined before being accepted.

### How GPT-5.6 Was Used

GPT-5.6 was used as the architecture, reasoning, and review layer of the development process. It helped with:

* Designing the metadata-grounded NL2SQL architecture
* Defining safe and explainable SQL-generation workflows
* Identifying schema-hallucination and incorrect-join risks
* Designing clarification, validation, and query-execution stages
* Reviewing MCP client compatibility and session behavior
* Developing multi-database abstraction strategies
* Creating test scenarios and evaluation criteria
* Reviewing security, maintainability, and enterprise-readiness
* Improving project documentation and presentation materials

GPT-5.6 was particularly useful for reasoning across multiple system concerns at once, including metadata quality, SQL dialect differences, MCP protocol behavior, database security, and LLM reliability.

### Human Oversight

AI-generated changes were not accepted automatically. The project owner remained responsible for:

* Defining product goals and technical requirements
* Reviewing generated code and architectural decisions
* Running unit and integration tests
* Verifying SQL safety rules
* Evaluating generated queries against expected results
* Approving the final implementation

This combination allowed Codex to accelerate implementation while GPT-5.6 supported architectural reasoning and systematic review, with human judgment controlling the final result.

**📚 상세 문서**: [docs/README.md](docs/README.md) — 아키텍처, MCP 도구
레퍼런스(97종), SQL 생성 워크플로, 검증 룰 카탈로그(33종), 데이터셋
가이드(18종), REST API, DB 커넥터, 운영/평가/보안/개발자 가이드.

## Quick Start

| 목적 | 명령 |
| --- | --- |
| 로컬 HTTP MCP + 운영 콘솔 | `go run ./cmd/sqlon -transport http -addr 127.0.0.1:9797` |
| 로컬 stdio MCP | `go run ./cmd/sqlon -transport stdio` |
| 표준 컨테이너 | `docker build -t sqlon/sqlon:v0.59.0 .` |
| 통합 테스트 DB 3종 기동 | `docker compose -f deploy/test/docker-compose.yml up -d` |
| 통합 테스트 (pg+mysql+mariadb) | `go test -tags integration ./test/integration -v` |

기본 시작은 기존 `data/metadb`를 완전 백업한 뒤 빠진 파일만
`data/sqlon`으로 병합합니다. 기존 프로파일·카탈로그·감사 로그를 보존하는 규칙과
복구 방법은 [마이그레이션 가이드](docs/migration.md)를 참조하세요.

HTTP 모드 기본 진입점:

- MCP endpoint: `http://127.0.0.1:9797/mcp`
- Web admin: `http://127.0.0.1:9797/admin`
- Swagger UI: `http://127.0.0.1:9797/docs`
- Health check: `http://127.0.0.1:9797/healthz`

## Supported Target Databases

| DB | 프로파일 `type` | 드라이버 | read-only 세션 강제 |
| --- | --- | --- | --- |
| PostgreSQL | `postgres` (기본) | `pgx/v5` (pure Go) | `default_transaction_read_only=on` |
| MySQL 8.x | `mysql` | `go-sql-driver/mysql` (pure Go) | `transaction_read_only=1` |
| MariaDB 10.x/11.x | `mariadb` | `go-sql-driver/mysql` (pure Go) | `tx_read_only=1` |

`connect_string`은 `host:port/dbname` 축약형, `postgres://`/`mysql://` URL,
go-sql-driver DSN을 모두 허용합니다. 생성 SQL의 방언은 데이터셋의
`databases.json`(`dbms`) 또는 `overrides.json`(`dialect`)이 결정하며 기본은
postgres입니다. 상세: [docs/db-connector.md](docs/db-connector.md).

## NL2SQL Recommended Flow

대부분의 질문은 개별 도구를 여러 번 오케스트레이션하지 말고
`prepare_sql_context`부터 호출하세요.

1. `prepare_sql_context(question)` 호출
2. 응답이 `status: "needs_clarification"`이면 SQL을 만들지 말고
   `clarifications`의 질문을 사용자에게 되묻습니다.
3. 답을 받은 뒤 `prepare_sql_context(question, clarifications={...})`로 다시 호출합니다.
4. `status: "ready"`이면 `skeleton.skeleton_sql`의 `/* SLOT */`만 채워 SQL을 완성합니다.
5. `validate_sql` → `explain_sql` → 필요 시 `run_sql_safely` 순서로 진행합니다.

이 흐름은 테이블/컬럼/지표/시간조건/조인 경로/검증 힌트를 한 번에 묶어
LLM이 스키마를 추측하거나 필수 검증 단계를 건너뛰는 일을 줄입니다.

## Transports

- `stdio`: newline-delimited JSON-RPC over standard input/output. Use this for desktop MCP clients that launch a local subprocess.
- `http`: Streamable HTTP at a single MCP endpoint. Use this for local HTTP clients, gateways, or remote service wrapping.

## Build

순수 Go 빌드 하나로 세 DB 모두 지원합니다 (CGO 불필요, 클라이언트 라이브러리
불필요):

Windows PowerShell:

```powershell
.\scripts\build.ps1
```

Linux/macOS shell:

```sh
sh ./scripts/build.sh
```

Artifacts:

```text
dist/sqlon-windows-amd64.exe
dist/sqlon-linux-amd64
dist/sqlon-linux-arm64
```

Single-platform builds:

```powershell
go build -o .\bin\sqlon.exe .\cmd\sqlon
```

```sh
go build -o ./bin/sqlon ./cmd/sqlon
```
```

## Docker Image

표준판과 Oracle판 이미지를 분리해 빌드합니다:

```sh
docker build -t sqlon/sqlon:v0.59.0 .
docker build -f Dockerfile.oracle -t sqlon/sqlon-oracle:v0.59.0 .
docker run --rm -p 9797:9797 \
  -e SQLON_ADMIN_TOKEN=change-me \
  -e PG_PROD_PW=... \
  sqlon/sqlon:v0.59.0
```

DB 프로파일은 `/admin/db` 또는 DB profile REST/MCP API로 구성한 뒤
`run_sql_safely`로 read-only 실행합니다. See
[docs/db-connector.md](docs/db-connector.md).

## Integration Test Environment (pg + mysql + mariadb)

SQLON의 **메타 DB 스키마 자체를 text2sql 대상**으로 세 엔진에 적재한
테스트 환경이 포함되어 있습니다:

```sh
docker compose -f deploy/test/docker-compose.yml up -d
# postgres:16  → 127.0.0.1:55432 (db sqlon_meta; 메타 DB 겸 대상 DB)
# mysql:8.4    → 127.0.0.1:53306 (database `public`)
# mariadb:11.4 → 127.0.0.1:53307 (database `public`)

go test -tags integration ./test/integration -v   # ping/guard/limit/explain/
                                                   # 오류코드/text2sql 골든셋 8종 × 3개 DB

# 서버를 이 데이터셋으로 직접 띄워보기
go run ./cmd/sqlon -data data/metadb -addr 127.0.0.1:9797
# (선택) 메타 DB 모드: -meta-db 'postgres://postgres:metapw@127.0.0.1:55432/sqlon_meta'
```

카탈로그 데이터셋은 `data/metadb/`(물리/논리 모델, 관계, 용어집, 지표/코드
사전, 컬럼 통계, 예제 SQL, 골든셋, 프로파일 3종)이며
`python3 deploy/test/gen_testenv.py`로 재생성합니다.

### 유명 오픈소스 스키마 데이터셋 (sakila / northwind / wordpress)

같은 컨테이너에 유명 오픈소스 서비스 스키마 3종이 시드되어 있고, 각각 독립
데이터셋으로 text2sql을 검증합니다 (`python3 deploy/test/gen_oss_testenv.py`로
재생성):

| 데이터셋 | 스키마 | 유래 | 골든셋 |
| --- | --- | --- | --- |
| `data/sakila` | sakila (9 tables: film/actor/customer/rental/payment...) | MySQL 공식 샘플 DB (DVD 렌탈) | 6 (정답 검증 포함) |
| `data/northwind` | northwind (8 tables: products/orders/customers...) | 고전 주문관리 샘플 | 6 (정답 검증 포함) |
| `data/wordpress` | wordpress (8 tables: wp_posts/wp_comments/wp_terms...) | WordPress CMS 핵심 테이블 | 5 (정답 검증 포함) |

세 스키마 모두 PostgreSQL(스키마)·MySQL/MariaDB(동명 데이터베이스)에 동일하게
적재되어 `sakila.film` 같은 스키마 한정 SQL이 세 엔진에서 그대로 실행되고,
골든셋의 기대 정답(예: 카테고리별 영화 수 1위)이 세 엔진에서 일치하는지까지
통합 테스트가 검증합니다:

```sh
go run ./cmd/sqlon -data data/sakila -addr 127.0.0.1:9797   # 프로파일: pg-sakila / mysql-sakila / mariadb-sakila
```

## Run With stdio

Windows:

```powershell
.\dist\sqlon-windows-amd64.exe -transport stdio -data .\data\metadb
```

Linux:

```sh
chmod +x ./dist/sqlon-linux-amd64
./dist/sqlon-linux-amd64 -transport stdio -data ./data/metadb
```

Example MCP client config for Windows:

```json
{
  "mcpServers": {
    "sqlon": {
      "command": "C:\\Users\\USER\\projects\\sqlon\\dist\\sqlon-windows-amd64.exe",
      "args": ["-transport", "stdio", "-data", "C:\\Users\\USER\\projects\\sqlon\\data\\metadb"]
    }
  }
}
```

Example MCP client config for Linux:

```json
{
  "mcpServers": {
    "sqlon": {
      "command": "/opt/sqlon/dist/sqlon-linux-amd64",
      "args": ["-transport", "stdio", "-data", "/opt/sqlon/data/metadb"]
    }
  }
}
```

stdio smoke test:

```powershell
$msg = '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0.0.1"}}}'
$msg | .\dist\sqlon-windows-amd64.exe -transport stdio -data .\data\metadb
```

```sh
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0.0.1"}}}' \
  | ./dist/sqlon-linux-amd64 -transport stdio -data ./data/metadb
```

## Run With Streamable HTTP

```powershell
go run ./cmd/sqlon -transport http -data .\data\metadb -addr 127.0.0.1:9797
```

MCP endpoint:

```text
http://127.0.0.1:9797/mcp
```

Health check:

```powershell
Invoke-RestMethod http://127.0.0.1:9797/healthz
```

## MCP Transport

This implements the MCP Streamable HTTP transport:

- `POST /mcp` accepts one JSON-RPC MCP message.
- `GET /mcp` opens a Server-Sent Events stream for server-to-client messages.
- `DELETE /mcp` closes a stateful session.
- `Mcp-Session-Id` is issued after `initialize` but never required: clients
  that do not echo the header (qwen-code, opencode, ...) are served normally
  (lenient session policy).
- `MCP-Protocol-Version: 2025-06-18` is accepted on subsequent requests.
- Origin validation allows empty origins, `localhost`, `127.0.0.1`, and `::1`.

For stateless local testing:

```powershell
go run ./cmd/sqlon -transport http -data .\data\metadb -stateless
```

To return POST responses as SSE:

```powershell
go run ./cmd/sqlon -transport http -data .\data\metadb -sse-post
```

## Curl Smoke Test

Initialize:

```powershell
$init = @{
  jsonrpc = "2.0"
  id = 1
  method = "initialize"
  params = @{
    protocolVersion = "2025-06-18"
    capabilities = @{}
    clientInfo = @{ name = "curl"; version = "0.0.1" }
  }
} | ConvertTo-Json -Depth 10

$res = Invoke-WebRequest `
  -Uri http://127.0.0.1:9797/mcp `
  -Method POST `
  -ContentType "application/json" `
  -Headers @{ Accept = "application/json, text/event-stream" } `
  -Body $init

$sid = $res.Headers["Mcp-Session-Id"]
$res.Content
```

List tools:

```powershell
$body = @{
  jsonrpc = "2.0"
  id = 2
  method = "tools/list"
} | ConvertTo-Json -Depth 5

Invoke-RestMethod `
  -Uri http://127.0.0.1:9797/mcp `
  -Method POST `
  -ContentType "application/json" `
  -Headers @{
    Accept = "application/json, text/event-stream"
    "Mcp-Session-Id" = $sid
    "MCP-Protocol-Version" = "2025-06-18"
  } `
  -Body $body
```

Search schema:

```powershell
$body = @{
  jsonrpc = "2.0"
  id = 3
  method = "tools/call"
  params = @{
    name = "search_schema"
    arguments = @{
      question = "최근 6개월간 신용카드 이용 내역이 있는 고객 수"
      top_k = 5
      include_columns = $true
    }
  }
} | ConvertTo-Json -Depth 10

Invoke-RestMethod `
  -Uri http://127.0.0.1:9797/mcp `
  -Method POST `
  -ContentType "application/json" `
  -Headers @{
    Accept = "application/json, text/event-stream"
    "Mcp-Session-Id" = $sid
    "MCP-Protocol-Version" = "2025-06-18"
  } `
  -Body $body
```

## Tools

- `prepare_sql_context` — 질문 분석→검색→지표→스키마→조인→SQL 골격을 한 번에 생성하는 권장 진입점
- `analyze_question` — 질문 분해: intent, 지표(사전 매칭), 차원, 필터, 시간범위, 정렬/limit, 모호성, 적용 기본값
- `retrieve_context` — 검색 후보와 조인 그래프 확장을 결합하고 선정 근거·조인 경로·값 증거를 반환
- `search_schema` — 다중 신호 스코어링(물리/논리명, 설명, 동의어, 도메인, 지표사전, 샘플값, 과거 성공 SQL, 조인 연결성) + 매칭 사유 + 제외 후보/사유
- `get_schema_context` — 압축 컨텍스트: 선택 테이블/컬럼 Top-K, 필수 조인 조건, 지표 계산식, 시간 조건, PII 표시, 제외 컬럼 로그
- `get_join_paths` — 조인 그래프 기반 경로(모든 쌍), confidence/preferred/caution, 저신뢰·경로없음 가이던스, 금지 조인 차단
- `get_metric_definition` — 지표 사전(`metrics.json`) 우선 조회; exact/business name/alias와 glossary·토큰 근접도를 결합하고 confidence/evidence를 반환, 없으면 추정 후보를 명확히 분리
- `get_column_stats` — 메타 + 프로파일 통계(null 비율, distinct, min/max, top values, 포맷 패턴)
- `find_filter_columns` — 질문 속 리터럴 값(서울, 정상, 개인사업자...)을 코드사전/top values로 필터 컬럼에 매핑
- `resolve_time` — 시간 표현(오늘/지난달/최근 3개월/2025년 6월/상반기/전월 대비...)을 semantic_type별 SQL 조건으로 변환
- `search_examples` — golden SQL 예제 검색 (질문의 intent 시그니처와 예제 `target_intent`의 구조 유사도로 랭킹 — 같은 SQL 형태의 예제 우선)
- `build_sql_skeleton` — 복잡/다중 테이블 질문용: 검증된 부품(카탈로그 조인 조건+alias, 지표사전 expression, semantic_type별 시간 조건, 정책 필터)을 조립한 SQL 골격 반환. LLM은 `/* SLOT */` 주석만 채움
- `rank_candidates` — 후보 SQL 여러 개를 서버측 객관 신호(검증 오류/경고, 리스크, 결과 스키마 커버리지, 지표 일치)로 정렬해 최선안 반환 — self-consistency를 LLM 자기평가 대신 객관 점수로 구현
- `suggest_joins` — 단일 컬럼 PK 마스터를 참조하는 미연결 테이블을 발굴해 조인 엣지 후보 제안(FK/인덱스/타입/동시출현 근거 + overrides.json 스니펫). **운영자 검토용 — 자동 적용되지 않음**
- `suggest_join_relations` — 골든셋에서 조인 경로가 끊긴 테이블 쌍을 찾고 공통 키 기반 relation 보강 후보를 제안
- `validate_sql` — 정적 검증: 미존재 테이블/컬럼, 조인 그래프, 카티션, GROUP BY, 방언(postgres/mysql/mariadb — Oracle 전용 문법 차단, 교차 방언 함수 경고), 날짜 타입, PII, 지표식 일치, **코드사전 값 검증**(존재하지 않는 코드 리터럴 차단), **결과 스키마 검증**(`expected_outputs`로 요구 차원/지표 누락 감지), CTE/인라인뷰 스코프 인식, 구조화된 `fix_hints`(최대 2회 자동수정 루프용)
- `explain_sql` — 리스크 추정: 정적 분석 + `profile` 지정 시 **실측 EXPLAIN**(postgres `EXPLAIN (FORMAT JSON)`, mysql/mariadb `EXPLAIN FORMAT=JSON`) — full scan/카티션/대량 정렬/고비용 탐지, 개선 제안
- `list_db_profiles` — 호출자가 사용할 수 있는 DB 연결 프로파일 id와 마스킹된 접속·정책 정보를 반환
- `list_database_instances` — 대상 DB에 접속하지 않고 권한 범위의 플릿 인벤토리, 환경·업무서비스·중요도·역할·담당팀과 엔진 Capability를 반환
- `get_fleet_health` — 사용 가능한 DB를 독립적으로 병렬 점검하고 연결·배포판·구성 위험을 수집 시각과 근거 데이터가 포함된 위험도 순으로 반환
- `list_sessions` — 선택한 DB의 활성·비활성 세션을 조회하고 SQL 실행시간과 트랜잭션 지속시간, 대기 이벤트, 보호 세션을 분리해 근거·수집 시각과 함께 반환. Oracle은 `INST_ID:SID:SERIAL#` 세션 키 사용
- `get_lock_tree` — 엔진 시스템 뷰의 blocker→blocked 관계를 정규화해 루트 블로커, 영향받는 세션 수, 잠금 유형과 대기시간을 반환하며 어떠한 세션 변경도 수행하지 않음
- `get_replication_status` — 복제 역할(primary/replica/standby/standalone)과 구성 요소별 상태·지연을 반환. PostgreSQL은 standby·slot·WAL receiver, MySQL/MariaDB는 채널별 IO/SQL 스레드, Oracle은 Data Guard lag·archive destination(base 라이선스 뷰만). 측정 불가 지연은 lag_seconds=-1로 구분
- `get_backup_status` — DB 서버가 스스로 보고하는 백업·아카이브 상태를 반환. PostgreSQL WAL 아카이버 성공/실패, MySQL/MariaDB binlog(PITR 기반), Oracle ARCHIVELOG·RMAN 이력·FRA 사용률(base 뷰만). 외부 백업 도구 잡 상태는 limitation으로 명시
- `get_security_posture` — 사용자·권한 진단: 로그인 가능한 비기본 SUPERUSER/DBA 역할, 위험 시스템 권한, 와일드카드 호스트 고권한 계정, 만료 비밀번호를 근거·심각도와 함께 반환. 읽기 전용 진단이며 조치는 변경계획으로만 수행
- `compare_configuration` — 설정 드리프트 감지: 프로파일 config_baseline과 라이브 서버 파라미터(pg_settings/global_variables/V$PARAMETER) 대조. 선언된 키만 검사, on/off↔true/false↔1/0 동치, pending_restart 표시. 읽기 전용, 조치는 변경계획으로
- `diagnose_connection_pool` — SQLON이 대상 DB로 유지하는 커넥션 풀 진단: sql.DB 통계(획득 대기·사용/유휴/전체·유휴/수명 초과 종료)를 max_open_conns/max_idle_conns와 대조해 오버·언더 프로비저닝 평가·권고. 대상 DB에 쿼리하지 않는 SQLON 측 텔레메트리, 풀 미생성 시 not_collected
- `get_workload_summary` — 대상 DB의 저장된 누적 시스템 카운터와 이전 스냅숏 차이에서 계산한 QPS/TPS, 연결, I/O, 대기 이벤트를 근거·수집 시각과 함께 반환
- `get_top_sql` — SQL 원문·bind 없이 fingerprint/SQL ID별 호출·elapsed·CPU·reads·rows와 Oracle plan hash를 반환하고 확장·권한 제한을 명시
- `get_storage_status` — DB·테이블·tablespace 사용량, 사용률과 이전 스냅숏 대비 일간 증가량을 반환하고 80/90퍼센트 위험 evidence를 제공
- `route_db_profile` — 프로파일이 많을 때 SQL이 참조하는 테이블을 방언 파서로 추출해 각 프로파일의 실측 인벤토리(information_schema)·선언 스키마·방언·헬스·우선순위로 점수화하여 실행 대상 프로파일을 판정. 명확한 승자가 있으면 `decisive=true`로 `selected_profile`을, 애매하면 후보 목록을 반환. `run_sql_safely(profile="auto")`가 내부적으로 사용
- `run_sql_safely` — 검증 후 **실제 DB 실행** (`profile` 지정 시; postgres/mysql/mariadb, read-only 세션, 타임아웃·행 제한·truncated·감사 로그). `profile="auto"`면 router가 대상 프로파일을 판정(애매하면 `profile_choice_required`로 재질문). 프로파일 미지정 시 dry-run 가드. 검증 실패 SQL은 실행하지 않음
- `execute_with_repair` — **자기수정 실행**: 검증→실행→진단을 한 번에 수행하고 실패 시 `repair` 키트(실패 단계, 분류된 error_code+힌트, 카탈로그 fix_hints, 참조 테이블 스키마)를 반환해 한 턴에 SQL 교정 가능. 0행이면 `executed_empty`+zero_row_hints. run_sql_safely와 동일 가드, 반복이 예상되면 이 도구 우선
- `list_metadata_sources` — 자동 메타데이터 수집 원천으로 쓸 수 있는 DB 프로파일(source_id/name/type/마스킹 접속대상) 목록. 물리 메타데이터는 자동 수집하되 업무 의미는 승인 기반으로 관리
- `discover_metadata` — 원천 DB의 비시스템 스키마 목록 조회(information_schema만 읽는 read-only). 수집 범위 지정용
- `db_health_report` — **DBA 헬스 점검**: 연결된 프로파일 DB의 시스템 카탈로그를 읽어 PK 없는 테이블(high)·인덱스 없는 FK 컬럼(medium)·미사용 인덱스(low)·통계 오래됨/없음(medium)·대형 테이블(코멘트 여부, info)을 진단. PostgreSQL 전체, MySQL/MariaDB는 이식 가능 항목만. 읽기 전용(수정·실행 없음, 개선은 DBA 검토 후)
- `suggest_indexes` — **인덱스 어드바이저**: 쿼리 감사 로그(query-*.jsonl)에서 느린 성공 쿼리를 분석해 인덱스가 없는 WHERE/JOIN/ORDER BY 컬럼을 집계하고, 영향도(발생 횟수 × 평균 지연) 순으로 후보 인덱스를 제안. 각 후보에 검토용 `CREATE INDEX` DDL과 대표 쿼리 포함. 읽기 전용·권고용(자동 생성하지 않으며 DBA가 카디널리티·쓰기부하 검토 후 수행). `profile`(선택)·`min_elapsed_ms`(기본 200)·`days`(기본 7)
- `lint_sql` — **SQL 안티패턴 린트**: 단일 문장을 정적 분석해 고전적 성능·정합성 스멜을 진단 — `SELECT *`, 선두 와일드카드 `LIKE '%…'`, `NOT IN (서브쿼리)`, 인덱스 컬럼을 함수로 감싼 비-sargable 조건, 인덱스 컬럼 부등호, 콤마 크로스 조인, `WHERE`의 `OR`, `LIMIT` 없는 `ORDER BY`, `WHERE` 없는 DML. 각 항목에 심각도와 개선 제안 포함. 카탈로그 인덱스 커버리지 인식·권고용(자동 수정 안 함). `sql`·`profile`(선택)
- `suggest_sql_rewrite` — **SQL 재작성 코파일럿**: 안티패턴을 탐지해 before→after 재작성 템플릿 제안. `SELECT *`는 카탈로그 실제 컬럼으로 정확 확장(단일 테이블), 그 외는 의미 동치 미보장 검토용 템플릿. `profile` 지정 시 원본 쿼리 실제 EXPLAIN 기준선(위험도·비용) 첨부. 실행·자동 수정 안 함. `sql`·`profile`(선택)
- `explain_sql_in_words` — **SQL 자연어 설명**: SQL이 어떤 테이블(카탈로그 논리명)에서 무엇을 필터·조인·그룹·정렬하고 어떤 집계를 계산하는지 한국어로 요약. 정적 구조 분석(실행 안 함). `sql`·`profile`(선택)
- `workload_report` — **워크로드 리포트**: 감사 로그를 기간별로 집계해 총/성공/오류 건수·오류율, 지연 분포(avg/p50/p95/p99/max), 느린 쿼리 수, 가장 많이 접근한 테이블, 상위 오류 코드, 툴·프로파일별 사용량, 가장 느린 문장, 피크 시간대를 리포트. 읽기 전용. `profile`(선택)·`days`(기본 7)·`slow_ms`(기본 200)
- `get_dba_digest` — **DBA 다이제스트**: 워크로드 리포트와 인덱스 어드바이저를 압축한 능동형 운영 스냅샷 — 쿼리량·오류율·p95/최대 지연·느린 쿼리 수·핫 테이블·상위 인덱스 후보와 한 줄 헤드라인. 읽기 전용. 스케줄러(`-sync-interval` + `-digest-webhook` + `-dba-digest`)가 틱마다 웹훅으로 push하는 것과 동일한 데이터. `profile`(선택)·`days`(기본 7)·`slow_ms`(기본 200)

### 변경 관리 도구 (privileged, `dba`/`admin` 역할 전용)

쓰기 작업은 모두 ChangePlan을 먼저 만들고 서버가 정한 위험도별 승인 정책을 거칩니다. 프로파일의 **DBA 자격증명**은 승인된 계획의 내부 실행기에서만 사용됩니다. 기존 쓰기형 `dba_*` MCP 도구와 직접 변경 REST 경로는 공개 목록에서 제거되었습니다.

- `create_change_plan` — 대상·사유·사전 상태·영향·잠금·선행조건과 실행/검증/보상 단계를 포함한 변경 초안 생성
- `evaluate_change_risk` — 서버 정책 기준 위험도와 필수 승인 수 평가
- `build_change_step` — 구조화된 권한 작업(create_user·create_database·grant·revoke)을 방언별 안전 인용으로 변경계획 단계(실행·검증·보상)로 생성. DB를 변경하지 않으며 되돌릴 수 없는 작업은 직접 작성, 비밀번호는 계획에 저장 불가
- `submit_change` — 초안을 분석 및 검토 대기 상태로 제출
- `approve_change` — 현재 DBA 자격으로 승인하고 추적 가능한 승인 ID 발급
- `execute_approved_change` — 승인 ID 확인, 실행 직전 재검증, 승인 단계 실행 및 사후 검증
- `verify_change` — 변경 상태와 승인·실행 결과 조회
- `rollback_change` — 실패한 변경의 보상 작업을 역순 실행
- `cancel_change` — 실행 전 변경 취소

### DBA 관찰 도구 (privileged, `dba`/`admin` 역할 전용)

- `dba_overview` — 콘솔 개요: 방언·DBA 활성 여부·서버 버전·역할/DB 수 (읽기 전용)
- `dba_list_users` — 사용자/역할 목록과 속성(superuser·createdb·createrole·login·연결제한)
- `dba_list_databases` — 데이터베이스 목록(소유자·인코딩·콜레이션·크기)
- `dba_list_settings` — 서버 설정 파라미터(`pg_settings`/`global_variables`), 이름 부분검색
- `dba_list_sessions` — 활성 세션/백엔드(pid·사용자·상태·지속시간·현재 쿼리)
- `describe_db_schema` — 연결된 프로파일 DB의 **라이브 스키마**(information_schema)를 조회해 카탈로그에 없는 테이블도 SQL 생성 근거로 제공. **카탈로그 우선**: 등록된 테이블/컬럼엔 논리명·설명을 함께 붙이고 `in_catalog` 플래그로 구분. 읽기 전용·비저장. 라이브 전용 테이블을 검증까지 통과시키려면 `apply_metadata_sync`로 반영
- `run_metadata_sync` — 원천 DB의 물리 모델(스키마·테이블·뷰·컬럼·PK/FK/Unique/Check·인덱스·코멘트·행수추정)을 버전 스냅숏으로 수집하고 이전 스냅숏 대비 변경분을 반환. 기본 증분(스키마 해시 동일 시 스킵). 삭제는 즉시 반영하지 않고 폐기 후보로 표시. **물리 정보만 수집하며 업무 의미(논리명·지표)는 운영 카탈로그에 쓰지 않음**
- `apply_metadata_sync` — **(관리자)** 원천의 최신 스냅숏을 카탈로그에 **자동 반영**: 물리 모델(컬럼·타입·NULL·PK/FK·FK 관계)을 meta_physical_models.json/topology_relations.json에 병합(백업)하고 핫리로드. 물리 사실은 자동 반영하되 **기존 설명(업무 의미)은 보존**, 삭제분은 `prune=true`가 아니면 폐기 후보로만 표시. 스케줄러 `-sync-apply`로 매 싱크 시 자동 실행 가능
- `list_profile_catalogs` — 등록된 DB 프로파일별 **카탈로그 워크스페이스**(`<data>/profiles/<profile>/`) 유무·테이블/관계 수·구축 시각 목록. 프로파일마다 독립 메타데이터 JSON을 조회·관리
- `get_profile_catalog` — 특정 프로파일 워크스페이스의 카탈로그 요약·데이터셋 인벤토리·헬스 조회
- `build_profile_catalog` — **(관리자)** 프로파일의 **라이브 스키마로 워크스페이스 구축/갱신**(물리 모델을 프로파일 디렉터리에 기록, 기존 설명 보존·삭제는 폐기 후보)
- `get_profile_dataset` / `put_profile_dataset` — 프로파일 워크스페이스의 개별 메타데이터 JSON(overrides·glossary·physical_models 등) 조회 / **(관리자)** 검증·백업·롤백과 함께 관리
- `build_all_profile_catalogs` — **(관리자)** 등록된 모든(또는 선택) 프로파일의 워크스페이스를 라이브 DB에서 **일괄 구축/갱신**. 프로파일별 권한 확인·실패는 개별 보고(배치 중단 없음). 다수 DB 온보딩용
- `import_openmetadata_to_profile` — **(관리자)** OpenMetadata의 큐레이션 메타데이터(논리명·설명·PII·용어집)를 특정 **프로파일 워크스페이스**로 import(전역 카탈로그 아님). 각 DB의 업무 메타데이터를 그 DB 워크스페이스에 수급, 빈 필드만·기존값 보존, `apply=false` 미리보기
- `get_active_catalog` / `set_active_catalog` — 현재 NL2SQL이 쓰는 카탈로그(기본 `-data` vs 핫스왑된 프로파일 워크스페이스) 조회 / **(관리자)** **무재기동 전환**. DB 프로파일·감사·워크스페이스는 운영 디렉터리에 고정(전환 영향 없음), 단독 모드 전용·재기동 시 `-data`로 복귀
- `get_sync_status` — 원천별 저장된 스냅숏 목록(최신순, 수집시각·스키마해시·객체수)
- `diff_metadata_snapshots` — 두 스냅숏 간 변경분(테이블/컬럼 추가·삭제, 타입/Null/키/코멘트/인덱스/뷰SQL 변경, 각각 심각도·처리방침) 계산
- `profile_metadata_assets` — 컬럼 통계(행수·Null비율·distinct·min/max·상위값·포맷패턴)를 비용 제어(모드별 샘플: fast 2k / standard 100k / deep 전체)·**개인정보 보호형**(민감 컬럼은 원본값·min/max·상위값 미저장, 길이·패턴·건수만)으로 계산. 결과는 검토 후보이며 운영 카탈로그(column_stats)에 자동 반영하지 않음
- `record_feedback` — 질문/분석/후보/SQL/검증오류/채택여부/실행시간을 서버가 부여한 actor/session/dataset 범위와 함께 `pending/untrusted` 검토 큐에 저장; 승인 전에는 검색·프롬프트·학습에 사용하지 않음
- `review_feedback` — **관리자 전용** 피드백 검토 큐 조회 및 approve/reject; 승인된 레코드만 trusted 상태로 few-shot·검색 부스트·학습 룰에 사용
- `list_datasets` / `get_dataset` — 서버가 참조하는 모든 JSON 데이터셋의 라이브 레지스트리: 용도, 스키마, 사용 도구, 필수/편집가능 여부, 현재 상태(존재·크기·로드 건수·로드 이슈)와 내용 샘플
- `put_dataset` — 데이터셋 교체: JSON 형태 검증 → 기존 파일 백업(`backups/`) → 쓰기 → 카탈로그 재컴파일 → **핫스왑**(재기동 불필요). 컴파일 실패나 신규 오류 발생 시 자동 롤백(`force`로 강제 적용 가능)
- `remove_dataset` — 선택 데이터셋 제거(백업 후) + 핫스왑. 필수(`physical_models`, `logical_models`)·시스템 관리(`feedback`, `audit`) 대상은 거부
- `reload_catalog` — 디스크 파일을 직접 수정한 경우(볼륨 마운트 등) 재컴파일 + 핫스왑
- `get_catalog_health` — 메타 컴파일 검증 결과(오류/경고), 커버리지 갭, PII 목록
- `get_metadata_quality` — 테이블별 메타데이터 품질 점수(완전성·일관성·관계성·프로파일링·지표연결·사용성·보안성) 0–100 + 등급 A–E, 스키마/도메인 집계, 개선 대상. `gate=true`면 릴리스 차단 조건(로드 오류·지표/인증조인 손상·PII 미분류·품질 하한 미달) 평가로 전환
- `suggest_semantic_metadata` — 논리명·의미타입·설명이 없는 컬럼에 대해 규칙 기반(용어집·동일컬럼 재사용·약어 확장·이름/타입 패턴, 오프라인)으로 **검토 후보**를 근거·신뢰도와 함께 생성. 고신뢰 항목은 overrides.json columns[] 스니펫으로 반환. 운영 카탈로그에 자동 반영하지 않으며 LLM/담당자가 다듬어 승인
- `suggest_model_candidates` — 규칙 기반 **모델 후보** 생성: 코드사전(저카디널리티 코드 컬럼의 프로파일 top-value로 스켈레톤), 지표(AMOUNT/COUNT/RATIO/SCORE 컬럼→SUM/AVG 집계 지표), 관계(식별자 이름+PK명/테이블명 매칭+타입 호환으로 FK 추론). 근거·신뢰도 동반, 운영 카탈로그 자동 미반영
- `analyze_impact` — 테이블/컬럼 변경·폐기 전 **계보/영향도** 추적: 해당 자산에 의존하는 지표·관계·선호/금지 조인·골든셋·오버라이드·용어집·1홉 하위 테이블을 역추적하고 impact_level(지표/선호조인 의존 시 high) 산출. 카탈로그 읽기 전용 분석
- `review_candidates` — 의미보강·모델 후보를 저장된 승인/반려 결정과 조인해 **검토 큐**로 조회(상태 pending/approved/rejected 필터). 각 항목에 안정적 id 부여. 사람 개입 게이트
- `decide_candidates` — 후보를 id로 **승인/반려**. 검토자·시각·메모와 함께 영속 저장(`<data>/reviews/decisions.json`). 카탈로그 자동 미반영
- `get_metadata_digest` — 카탈로그 운영 상태 **요약 스냅숏**: 품질 점수·릴리스 게이트, 검토 큐 백로그(대기/승인/반려), 골든 승격 후보 수, 카탈로그 규모·로드 경고 + 한 줄 헤드라인. 일일 점검·알림용
- `openmetadata_status` — 설정된 OpenMetadata 서버 연결·인증·버전 확인
- `import_openmetadata` — OpenMetadata의 큐레이션 메타데이터(테이블/컬럼 displayName→논리명, 설명, PII 태그→pii/semantic_type, 용어집)를 SQLON **빈 필드에만** 후보로 가져오기. `apply=false` 미리보기(기본), `apply=true` overrides.json/glossary.json 병합+리로드(관리자, 백업·수기값 보호)
- `export_to_openmetadata` — SQLON 컬럼 설명(명시적 또는 논리명 조합)을 OpenMetadata의 **빈 설명 컬럼에만** JSON-Patch로 push. `dry_run=true` 계획만(기본), `dry_run=false` 실제 반영(관리자)
- `openmetadata_drift` — SQLON ↔ OpenMetadata **대조(reconciliation)** 리포트: 논리명·설명·PII를 `sqlon_gap`(import 후보)·`conflict`(값 불일치, 사람 결정)·`ext_gap`(export 후보)으로 분류. 읽기 전용 거버넌스 도구
- `export_lineage_to_openmetadata` — SQLON 관계 그래프를 OpenMetadata **테이블 lineage 엣지**로 push(from=참조/부모, to=기준/자식). FK 관계형 lineage 매핑(ETL 흐름 아님). `dry_run=true` 계획(기본)/`false` 반영(관리자), OM에 없는 테이블 엣지는 skip 보고
- `get_approved_overrides` — 승인된 후보를 목적 파일별(overrides.json columns[], metrics.json, relations.json, 코드사전) **적용 스니펫**으로 컴파일
- `apply_approved_candidates` — **원클릭 반영**: 승인-미반영 후보를 데이터셋 파일 4종에 파일별 백업 후 병합하고 카탈로그 핫리로드. 멱등(applied_at 스탬프+내용 중복 제거), 운영자 수기 값은 덮어쓰지 않음. 관리자 전용
- `run_evaluation` — golden query set 평가(테이블/컬럼/지표/조인/SQL 유효성 정확도, 평균 응답시간)
- `learn_from_feedback` — 반복 실패 패턴을 learned rule로 승격: 동일 검증오류 반복(예방 경고), 테이블 오선택 교정(검색 패널티), 컬럼 교정(validate_sql 경고). `learned_rules.json`에 영속화되어 운영자가 검토/수정 가능
- `suggest_golden_from_feedback` — 승인·성공·실행된 피드백을 **골든셋 후보**로 제시(질문/기대 SQL·테이블·컬럼, 질문/SQL 정규화로 기존 골든셋 중복 제외). trust 경계 승인분만 대상(fail-closed)
- `promote_golden_queries` — 선택 후보(feedback_id)를 `golden_queries.json`에 백업 후 추가하고 카탈로그 리로드. 운영 트래픽으로 평가셋을 성장시키는 명시적 관리자 행위. 관리자 전용

`run_sql_safely` validates SQL and, when a DB profile is supplied, executes it
read-only against the target database (postgres/mysql/mariadb) with query
timeout, row limit, and audit logging — drivers are always compiled in.
Without a profile it stays a dry-run guard returning bounded SQL. See
`docs/db-connector.md`. Start most questions with `prepare_sql_context`,
which runs the whole analyze→skeleton pipeline in one call.

## Web Admin Console & REST API

HTTP 모드로 기동하면 브라우저 기반 관리 화면과 Swagger 문서가 함께 제공됩니다.

| 경로 | 내용 |
| --- | --- |
| `/admin` | **데이터셋 관리 콘솔** — 18개 데이터셋의 용도·스키마·상태 확인, 내용 편집·적용(백업+검증+핫스왑), 제거, 백업/복원, 카탈로그 리로드. 단계별 사용 가이드가 화면에 내장 |
| `/admin/editor` | **테이블 편집기** — 데이터셋을 표(그리드)로 렌더링해 JSON 없이 편집: 셀 클릭 인라인 수정(타입 자동 보존), 행 추가/복제/삭제, **컬럼 추가/이름변경/삭제**, 검색·페이지네이션. 저장 시 동일한 백업·검증·핫스왑·롤백 적용 |
| `/admin/db` | **DB 연결 관리·쿼리 실행** — postgres/mysql/mariadb 프로파일 추가/수정/삭제/접속 테스트, Read-Only 쿼리 콘솔(검증→미리보기→실행→취소), 실행 이력·메트릭 ([docs/db-connector.md](docs/db-connector.md)) |
| `/admin/dba` | **DBA 코파일럿** — 읽기 전용 DBA 진단 대시보드: 헬스 점검, 인덱스 어드바이저(CREATE INDEX 후보), 워크로드 리포트, SQL 안티패턴 린트, SQL 자연어 설명을 탭 UI로 제공(자동 실행·변경 없음, 권고용) |
| `/admin/dba-console` | **DBA 관리 콘솔** (`dba`/`admin` 역할 전용) — 권한 있는 쓰기 세션으로 사용자·역할, 데이터베이스, 권한(GRANT/REVOKE), 서버 설정, 세션(취소/종료), 유지보수(VACUUM/ANALYZE/REINDEX), 임의 권한 SQL을 탭 UI로 관리. 프로파일의 `dba` 자격증명 필요, 모든 변경 감사 로그 기록 |
| `/auth/login` · `/admin/users` · `/admin/keys` | **인증·사용자·MCP 키** (메타 DB 활성 시) — 로컬/Keycloak SSO 로그인, 사용자·역할 관리(admin), MCP 키 발급·회전·폐기, 프로파일별 권한(grant). 상세: [docs/auth.md](docs/auth.md) |
| `/docs` | **Swagger UI** — REST API 문서 + Try it out (오프라인 동작, 자산 임베드) |
| `/openapi.json` | OpenAPI 3.0 스펙 |
| `/api/*` | REST API: `GET /api/datasets`, `GET/PUT/DELETE /api/datasets/{name}`, `GET .../content`, `GET .../backups`, `POST .../restore`, `POST /api/reload`, `GET /api/health` |

변경 API 보호: `-admin-token <값>` 플래그(또는 `SQLON_ADMIN_TOKEN` 환경변수)를
설정하면 PUT/DELETE/POST에 `X-Admin-Token` 헤더가 필요합니다. 미설정 시 인증
없이 호출 가능하므로 내부망 외 노출 시 반드시 설정하세요. 모든 변경은
`audit/*.jsonl`에 기록되고, REST와 MCP 도구(`put_dataset` 등)는 동일한
검증·백업·롤백 코드를 공유합니다.

단독 HTTP 모드는 기본적으로 loopback 주소만 허용합니다. `0.0.0.0`, `::`,
인터페이스 IP 또는 hostname에 바인딩하려면 전면 인증을 제공하는 `-meta-db`를
구성하거나 `-public-mcp`로 공개 노출을 명시적으로 승인하고 `-admin-token`도
설정해야 합니다.
`-admin-token`은 변경·DB 실행 도구를 보호하지만 모든 읽기 전용 MCP 도구의
로그인을 강제하지 않으므로, 인터넷 노출에는 `-meta-db` 인증을 사용하세요.
피드백을 workspace별로 격리하려면 `-feedback-tenant` 또는
`SQLON_FEEDBACK_TENANT`를 서버가 관리하는 고정 값으로 설정하세요.

## Authentication (optional, Postgres meta DB)

`-meta-db <postgres DSN>`(또는 `SQLON_META_DB`)를 지정하면 전면 인증이
활성화됩니다. 미지정 시 기존 단독 모드 그대로 동작합니다(하위 호환).

```sh
sqlon -transport http -addr 0.0.0.0:9797 \
  -meta-db 'postgres://sqlon:pw@pg:5432/sqlon?sslmode=require' \
  -bootstrap-admin 'admin:첫관리자비밀번호'
```

- **로그인**: 로컬 계정(bcrypt) + 세션 쿠키, 또는 Keycloak **SSO(OIDC)**
  (`-oidc-issuer/-oidc-client-id/-oidc-client-secret/-oidc-redirect-url`)
- **역할**: `admin`(전권) / `user`. 관리자는 사용자·데이터셋·전체 프로파일·
  전체 키 관리
- **MCP 키**: `/mcp` 접근용 `ssk_...` 키를 발급·회전·폐기(`/admin/keys`).
  클라이언트는 `Authorization: Bearer ssk_...` 또는 `X-MCP-Key`로 접속
- **DB 프로파일 권한**: 사용자별 소유 + `use`/`manage` grant + `shared`
  공개. Postgres에 저장되어 사용자마다 접근 범위가 다름
- 첫 기동 시 부트스트랩 관리자를 생성(비밀번호 미지정 시 로그에 1회 출력)

- **서버 설정 관리**: 마스터 토큰·허용 Origin·Keycloak SSO를 `/admin/settings`
  에서 메타 DB에 저장하고 **재기동 없이 즉시 적용**(플래그/env는 기본값)
- **데이터셋도 메타 DB에서 관리**: 편집 가능한 카탈로그 JSON 14종의 진실
  원본이 Postgres(`sqlon_meta.datasets`)가 되어 `/admin`·MCP 도구 편집이 DB에
  영속화됨(로드 시 파일로 materialize해 기존 로더 재사용)
- **MCP `list_db_profiles`**: LLM이 사용 가능한 DB 프로파일 id를 발견

메타 DB 드라이버는 순수 Go(pgx)라 CGO/외부 클라이언트가 필요 없습니다. 상세:
[docs/auth.md](docs/auth.md).

## Operator-Managed Data Files (dataset dir)

| 파일 | 용도 |
| --- | --- |
| `glossary.json` | 업무 용어/동의어 사전 (검색·질문분해·SQL생성·검증 공용) |
| `metrics.json` | 지표 사전: expression, 집계, grain, 필수 필터, 예시 SQL |
| `overrides.json` | 운영자 보정: 설명/도메인/grain, 컬럼 동의어·샘플값, PII 지정, 금지/권장 조인, 구조 검증 기본 필터(`enforcement: warn|error`), dialect(postgres/mysql/mariadb) |
| `databases.json` | 대상 DB 정보 — `dbms`(POSTGRES/MYSQL/MARIADB)가 생성 SQL 방언 결정 |
| `db_profiles.json` | 실행용 DB 접속 프로파일 (type/connect_string/password_ref/pool/policy) |
| `column_stats.json` | 컬럼 프로파일 통계 (선택; row count, null 비율, top values, 최신성) |
| `patterns.json` | 다단계 SQL 패턴 사전 (2단 집계, 그룹별 top-N, 전월/전년 대비, 비율, 분포) — 미존재 시 내장 기본값 사용 (방언에 맞게 자동 치환) |
| `golden_queries.json` | 평가용 golden query set — 수작업 케이스 + `jamypg-goldgen` 자동 선별 (CI에서 `go test ./...`로 자동 실행) |
| `learned_rules.json` | `learn_from_feedback`가 승격한 학습 룰 (운영자 검토/수정/삭제 가능) |
| `feedback/*.jsonl` | record_feedback 검토 큐 (관리자가 승인한 trusted 레코드만 성공 SQL 학습·룰 승격에 재사용) |
| `audit/*.jsonl` | 모든 tool call 감사 로그 (자동 기록, git 제외) |

## Evaluation

```sh
go test ./...                            # golden set 포함 전체 테스트 (CI)
go run ./cmd/jamypg-eval -verbose        # 평가만 실행, 케이스별 미스 출력
go run ./cmd/jamypg-eval -data data/metadb -profile pg-meta
                                         # 실행 기반 평가 (실제 DB에 COUNT 검증)
go run ./cmd/jamypg-goldgen -n 80        # sql_datasets에서 golden set 재생성
                                         # (도메인 x 난이도 층화, 카탈로그 검증 통과 케이스만,
                                         #  기존 파일 상단 수작업 케이스는 -keep 개수만큼 보존)
```

측정 항목: table_selection_acc, column_recall_avg, metric_lookup_acc,
join_path_acc, expected_sql_valid, avg_response_ms (+ `-profile` 시
execution_success_rate, row_sanity_rate).
data/metadb 8케이스(3개 DB 실측): table 1.0 / join 1.0 / sql 1.0 / 실행 성공률 1.0.
OSS 데이터셋(sakila/northwind/wordpress) 17케이스: 3개 엔진에서 동일 정답 검증 통과.

## Feedback Learning Loop

1. 클라이언트가 `record_feedback`으로 질문/SQL/검증오류/교정본/채택여부를 검토 큐에 저장
2. 관리자가 `review_feedback`으로 내용과 범위를 확인해 approve/reject
3. 승인된 trusted 성공·교정 SQL만 즉시 few-shot 예제와 검색 부스트에 반영
4. `learn_from_feedback` 호출(또는 주기 실행) 시 승인된 피드백의 반복 패턴을 룰로 승격:
   - `recurring_error` — 같은 검증 오류가 N회 이상 → 해당 테이블/컬럼 사용 시 예방 경고
   - `table_correction` — 교정에서 반복적으로 교체된 테이블 → 검색 점수 패널티 + 경고
   - `column_correction` — 반복 교체된 컬럼 → validate_sql이 대체 컬럼 힌트 제시
   - `slow_query` / `recurring_exec_error` — 실행 감사 로그에서 반복 지연·오류(PG-*/MY-*/TIMEOUT) 승격
5. 룰은 `learned_rules.json`으로 영속화; 서버 재기동 시 자동 적용, 운영자가 직접 편집 가능

## SQL Generation Flow

1. `analyze_question` → 모호성 확인 (기본값 적용 시 가정 표시), 패턴·intent 시그니처 확보
2. `search_schema` (+`find_filter_columns`, `resolve_time`)
3. `get_metric_definition` — 업무 지표는 사전 expression만 사용
4. `get_schema_context` — 압축 컨텍스트만 LLM에 전달
5. `get_join_paths` — ON 조건은 반드시 여기서 취득; 경로 없음/저신뢰 시 되묻기
6. 복잡/다중 테이블 질문이면 `build_sql_skeleton`으로 골격 확보 후 SLOT만 채움; 단순 질문은 직접 생성 (컨텍스트 내 식별자만, PII 금지, row bound(LIMIT) 필수)
7. `validate_sql` (`expected_outputs`, `metrics` 전달) — fix_hints 반영 최대 2회 재시도; 실패 SQL 실행 금지. 난이도 높은 질문은 후보 2~3개를 만들어 `rank_candidates`로 최선안 선택
8. `explain_sql` — risk=high면 기간/limit 조건 추가 후 재생성 (`profile` 지정 시 실측 EXPLAIN)
9. 구조화 JSON 응답 (sql, 사용 테이블/컬럼, 지표, 조인, 필터, 가정, 주의, 검증결과, 실행가능여부)
10. `record_feedback`
