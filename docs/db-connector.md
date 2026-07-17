# DB Query Connector (PostgreSQL / MySQL / MariaDB)

자연어→SQL 생성 결과를 **실제 대상 DB에 read-only로 실행**하는 커넥터입니다.
`database/sql` 기반이며 드라이버는 모두 순수 Go입니다:

| 대상 DB | `type` | 드라이버 | 비고 |
| --- | --- | --- | --- |
| PostgreSQL | `postgres` (기본) | `github.com/jackc/pgx/v5` (`pgx`) | 세션 `default_transaction_read_only=on` 강제 |
| MySQL 8.x | `mysql` | `github.com/go-sql-driver/mysql` | 세션 `transaction_read_only=1` 강제 |
| MariaDB 10.x/11.x | `mariadb` | `github.com/go-sql-driver/mysql` | 세션 `tx_read_only=1` 강제 |

## 활성화 (빌드·런타임 요건)

**없습니다.** 두 드라이버 모두 순수 Go라 CGO·클라이언트 라이브러리·빌드 태그가
필요 없습니다. `go build ./...` 한 번이면 세 DB 모두 실행 가능하며, 단일
`Dockerfile`(CGO_ENABLED=0)로 빌드됩니다. 과거 Oracle 시절의
`-tags oracle`/`Dockerfile.oracle`/Instant Client 요건은 모두 제거되었습니다.

```sh
docker build -t jamypg-mcp .
docker run -d -p 9797:9797 -v jamypg-data:/app/data/metadb \
  -e JAMYPG_ADMIN_TOKEN='...' -e PG_PROD_PW='...' jamypg-mcp
```

## DB 프로파일

접속 정의는 데이터셋 `db_profiles.json`(메타 DB 사용 시 사용자별 프로파일)에
저장되며, **`/admin/db` 화면에서 추가·수정·삭제·접속 테스트**를 수행합니다
(REST `/api/db-profiles*` 동일).

```json
[{
  "id": "pg-prod-01",
  "name": "운영 PostgreSQL",
  "type": "postgres",
  "connect_string": "db.example.com:5432/appdb",
  "username": "app_readonly",
  "password_ref": "env:PG_PROD_PW",
  "pool":   { "max_open_conns": 10, "max_idle_conns": 2,
              "conn_max_lifetime_seconds": 1800, "conn_max_idle_time_seconds": 600 },
  "policy": { "query_timeout_seconds": 30, "connection_test_timeout_seconds": 5,
              "default_max_rows": 100, "max_rows": 1000,
              "max_response_bytes": 10485760, "denied_keywords": [],
              "plan_gate": true, "plan_gate_risk": "high" }
}]
```

- **type**: `postgres`(기본) | `mysql` | `mariadb`. `driver`는 type에서
  자동 유도(`pgx`/`mysql`)되므로 생략합니다. MariaDB는 프로토콜은 MySQL과
  같지만 read-only 세션 변수와 EXPLAIN JSON 형태가 달라 별도 type을 씁니다
- **connect_string**: 공통 축약형 `host:port/dbname` 권장.
  전체 URL(`postgres://host:5432/db?sslmode=require`, `mysql://host:3306/db`)과
  go-sql-driver DSN(`user:pass@tcp(host:3306)/db`)도 허용 — 어떤 형식이든
  프로파일의 username/password가 우선 적용되고 read-only 세션 옵션이
  주입됩니다. PostgreSQL은 `connect_timeout=5`가 기본 추가됩니다
- **password_ref**: `env:변수명`(권장) / `file:경로`(Secret 마운트) /
  `plain:값`(개발용, API 응답에서 `plain:****`로 마스킹). **평문 저장 금지
  원칙(AC-012)** — 스킴 없는 값은 저장이 거부됩니다
- **username**: SELECT 권한만 가진 전용 계정 사용 — 서버측 차단과 별개로
  DB 권한이 최종 방어선
- **policy.plan_gate / plan_gate_risk**: 실행계획 승인 게이트.
  `plan_gate`(기본 `true`)가 켜져 있으면 실행 전 실측 EXPLAIN을 수행해
  위험도가 `plan_gate_risk`(`low`|`medium`|`high`, 기본 `high`) 이상이면
  실행을 거부합니다 — 아래 [실행계획 승인 게이트](#실행계획-승인-게이트-plan-gate) 참조
- **policy.max_plan_cost / max_plan_rows**: **비용 상한(절대 가드)**. `> 0`으로
  설정하면 EXPLAIN 예상 총비용/최대 카디널리티가 이 값을 초과하는 쿼리를 실행
  전 거부합니다. `plan_gate`와 달리 **`approve_plan`으로 우회할 수 없는 하드
  서킷 브레이커**로, 운영 DB에서의 런어웨이 쿼리를 원천 차단합니다(`0`=비활성,
  상한 조정은 관리자 프로파일 정책 변경). MCP 응답은 `status:
  blocked_cost_ceiling`(run_sql_safely) / `phase: cost_ceiling`
  (execute_with_repair)로 반환됩니다.
- **routing**: 다중 프로파일 자동 라우팅 메타데이터
  (`schemas`/`tags`/`priority`/`default`/`discover`) —
  아래 [프로파일 라우팅](#프로파일-라우팅-여러-프로파일-자동-선택) 참조
- 프로파일 저장 시 이전 파일이 자동 백업되고 해당 커넥션 풀이 재생성됩니다.
  기본값: 생략한 풀/정책 값은 표의 권장값으로 채워집니다

## 프로파일 라우팅 (여러 프로파일 자동 선택)

프로파일이 여러 개 등록되면 "이 SQL은 어느 DB로 보내야 하나"가 문제가
됩니다. 라우터는 SQL에서 **참조 테이블을 방언 AST 파서로 추출**한 뒤
(postgres는 `go-pgquery`, mysql/mariadb는 TiDB 파서 — SQL 가드와 동일
파서라 주석/문자열/특수 인용부호로 속일 수 없고, CTE 이름은 물리 테이블이
아니므로 제외), 호출자가 사용 가능한(권한 필터 통과) 프로파일들을 5가지
시그널로 스코어링합니다 (`internal/dbconn/router.go`, `tables.go`):

| # | 시그널 | 점수 |
| --- | --- | --- |
| 1 | **Live capability** — 대상 DB의 `information_schema.tables` 인벤토리에 참조 테이블이 실존하는지 검증. **최강 시그널** | 전체 존재 +50 / 일부 +15 / 없음 −40 |
| 2 | **Declared scope** — 참조 스키마가 `routing.schemas` 선언과 일치하는지. DB가 일시 다운이어도 동작 | 전체 선언 +30 / 일부 +10 / 불일치 −20 |
| 3 | **방언** — 테이블 추출은 방언 파서로 수행. 라우팅에 방언이 지정되면 프로파일 `type` 불일치는 후보에서 **하드 배제** (MCP/REST 경로는 방언 미지정 — postgres → mysql 순으로 파싱을 시도하고 성공한 방언을 응답 `dialect`로 표시) | 하드 배제 |
| 4 | **헬스** — 서킷 브레이커 오픈 프로파일 | 후보에서 **하드 배제** |
| 5 | **운영자 선호** — `routing.default` / `routing.priority` | default +8 / priority 1~100 → 최대 +9.9 tie-breaker |

- **인벤토리 캐시**: 프로파일별 live 인벤토리는 **10분 TTL**로 캐시되고
  프로파일 정의가 변경되면 무효화됩니다. 시스템 스키마(pg_catalog,
  information_schema, mysql, performance_schema, sys)는 제외.
  인벤토리 조회 실패는 배제가 아니라 `reasons`에 사유만 기록됩니다
- **결정 정책**: 1위 후보가 **확실한 커버리지(`full` 또는 `declared`)** 를
  갖고, **2위와 10점 이상 차이**가 나거나 커버리지 등급에서 앞설 때만
  자동 선택(`decisive: true`)합니다. 동점이거나 검증 불충분이면
  `decisive: false`와 순위별 `candidates`(개별 `reasons` 포함)를 반환해
  호출자(LLM → 사용자)가 명시적으로 지정하게 합니다 — 모호 테이블
  재질문 룰과 동일하게 **절대 임의 추측하지 않습니다**. 테이블을
  참조하지 않는 SQL(`SELECT 1` 등)은 적격 프로파일이 정확히 1개일
  때만 자동 선택됩니다

### 프로파일별 라우팅 설정 (`routing` 객체)

`db_profiles.json`의 각 프로파일에 `routing` 필드를 추가합니다:

```json
{
  "id": "pg-prod-01",
  "name": "운영 PostgreSQL",
  "type": "postgres",
  "connect_string": "db.example.com:5432/appdb",
  "username": "app_readonly",
  "password_ref": "env:PG_PROD_PW",
  "routing": { "schemas": ["sales"], "tags": ["prod"], "priority": 10, "default": false }
}
```

- **schemas** (`[]string`): 이 프로파일이 담당하도록 선언된 스키마 목록.
  대상 DB가 일시적으로 접속 불가여도 이 선언만으로 매칭됩니다
- **tags** (`[]string`): 임의 라벨(`env:prod`, `replica`, `team-a` ...).
  점수에는 반영되지 않고, 후보 목록에 표면화되어 사람/LLM의 선택을 돕습니다
- **priority** (`int`): 1 = 최우선, 100 = 최하위(기본값). 다른 조건이
  비슷할 때의 tie-breaker
- **default** (`bool`): capability 시그널로 후보를 가르지 못할 때 선호할
  fallback 프로파일 표시
- **discover** (`bool`, 기본 `true`): live `information_schema` 인벤토리
  프로빙 여부. 느리거나 비용이 큰 대상은 `false`로 꺼서 declared
  `schemas` 기반 매칭만 사용할 수 있습니다

### 호출 경로

```text
MCP:  route_db_profile {sql}            — 라우팅 판단만 반환
MCP:  run_sql_safely {sql, profile:"auto"} — 라우팅 + (decisive면) 실행
REST: POST /api/query/route {sql}
```

- **`route_db_profile`**: 응답은 `{decisive, reason, referenced_tables:
  [{schema,name}], dialect, selected_profile(결정 시), candidates:
  [{profile_id, name, type, score, coverage(full|partial|declared|none|
  unknown), reasons, default, priority}], excluded}` 형태입니다
  (REST `/api/query/route`도 동일 형태)
- **`run_sql_safely(profile="auto")`**: 라우팅이 decisive면 선택된
  프로파일로 그대로 실행하고, 아니면 실행하지 않고
  `status=profile_choice_required` + `candidates`를 반환합니다 —
  사용자와 후보 중 하나를 확정한 뒤 `profile=<id>`로 재호출하세요
- **권한**: 두 경로 모두 사용 가능한 프로파일들의 DB를 프로빙하므로,
  standalone HTTP에서는 다른 DB 접근 도구와 동일하게 **admin token**이
  필요합니다. 인증(메타 DB) 모드에서는 `list_db_profiles`와 같은
  프로파일별 권한 필터가 적용되어 **소유·grant·shared 프로파일만**
  후보가 됩니다

## 실행 경로

```text
MCP: run_sql_safely {sql, profile, limit, timeout_seconds}
REST: POST /api/query/execute | /api/query/preview
UI:  /admin/db 쿼리 콘솔 (검증 → 미리보기 → 실행 → 취소)

공통 파이프라인:
  카탈로그 검증(validate_sql 33종 룰) — 실패 시 실행 안 함
→ 커넥터 SQL 가드: SELECT/WITH만, DML/DDL/트랜잭션/세션 변경 차단 +
  방언별 위험 함수 차단(postgres: pg_sleep/pg_read_*/dblink/COPY/DO 등,
  mysql/mariadb: LOAD_FILE/OUTFILE/DUMPFILE/SLEEP/BENCHMARK/HANDLER 등),
  다중 statement 차단 (주석·문자열 리터럴을 벗겨낸 형태로 검사 — 우회 불가)
→ 방언 AST 가드: 대상 방언의 실제 파서로 SQL을 파싱해 read-only
  SELECT/WITH 단일문만 허용 (파싱 실패 = 거부, 아래 상세)
→ 서킷브레이커 확인 (연속 3회 실패 → 30초 차단)
→ 실행계획 승인 게이트: 실측 EXPLAIN 위험도 ≥ plan_gate_risk면
  plan_approval_required로 거부 (preview·approve_plan은 통과, 아래 상세)
→ LIMIT 래핑: SELECT * FROM (<sql>) AS jamypg_q LIMIT maxRows+1
  (초과 시 truncated=true; mysql/mariadb의 WITH 쿼리는 래핑 대신 LIMIT 부가)
→ QueryContext(프로파일 query_timeout, 요청이 더 짧으면 요청값)
→ 스캔: 타입 정규화([]byte/DATETIME/NUMERIC → JSON-safe),
  rows.Close/rows.Err 보장, 응답 바이트 캡
→ 감사 로그(audit/query-YYYYMMDD.jsonl, tool="db:execute") +
  이력(메모리 200건) + 메트릭
```

- **바인드 변수**: REST `binds` 배열로 전달 (postgres `$1, $2...`,
  mysql/mariadb `?`) — 사용자 입력은 문자열 조합 금지(GO-SQL-009)
- **취소**: 실행 응답·이력의 `execution_id`로
  `POST /api/query/cancel/{id}` — context cancel로 중단
- **미리보기 vs 실행**: preview는 프로파일 `default_max_rows` 강제, execute는
  요청 `max_rows`(프로파일 `max_rows`로 캡)
- **0행 힌트**: 결과가 0행이면 응답 `hint`로 기간/코드값 필터 재확인을 안내

### AST 기반 read-only 검증 (방언 파서)

키워드 가드(`sqlguard.go`)에 더해, 실행 전 SQL을 **대상 방언의 실제 문법
파서**로 파싱해 정확히 1개의 read-only SELECT/WITH 문인지 검사합니다
(`internal/dbconn/astguard.go`):

| 방언 | 파서 | 비고 |
| --- | --- | --- |
| postgres | `github.com/wasilibs/go-pgquery` | 실제 PostgreSQL 파서(libpg_query)를 WASM으로 컴파일해 wazero로 실행 — 순수 Go, cgo 불필요 |
| mysql / mariadb | `github.com/pingcap/tidb/pkg/parser` | TiDB의 MySQL 호환 파서. MariaDB는 `SetMariaDB` 모드 |

정책은 **fail-closed**: 방언 문법으로 파싱되지 않는 SQL은 거부됩니다
(어차피 실제 DB에서도 실패할 SQL). 문자열/정규식 가드가 원리상 놓칠 수 있던
우회 클래스를 구문 트리 수준에서 차단합니다:

- data-modifying CTE: `WITH x AS (DELETE ...) SELECT ...`
- `SELECT ... INTO`(테이블 생성) / `INTO OUTFILE` / `INTO DUMPFILE`
- 잠금 읽기: `FOR UPDATE` / `FOR SHARE` / `LOCK IN SHARE MODE`
- 세션 변수 할당: `SELECT @v := ...`
- `DO` 블록, `PREPARE`/`EXECUTE` 등 SELECT 외 모든 statement 노드
  (트리 어디에 중첩돼도 거부)
- 다중 statement 밀반입 (정확히 1문만 허용)

진입점: `dbconn.ValidateReadOnly(dialect, sql, extraDenied)`가 키워드 가드 +
AST 가드를 순서대로 수행하며(Execute/CountRows/Metadata/ExplainPlan이 사용),
`dbconn.ValidateReadOnlyAST(dialect, sql)`는 AST 가드만 수행합니다.
`POST /api/query/validate`도 동일 가드를 태우며, 방언은 `profile_id`가
주어지면 프로파일 type, 없으면 카탈로그 방언을 사용합니다.

## 실행계획 기반 보호 (실측 EXPLAIN)

`explain_sql`(MCP, `profile` 지정 시) / `POST /api/query/explain` /
`/admin/db`의 **[② 실행계획]** 버튼은 방언별 JSON EXPLAIN을 수행해
플랜 트리를 분석합니다. EXPLAIN은 세 엔진 모두 쿼리를 실제 실행하지
않으며 서버측 상태를 만들지 않습니다(과거 PLAN_TABLE 정리 같은 절차 불필요):

- PostgreSQL: `EXPLAIN (FORMAT JSON) <sql>`
- MySQL / MariaDB: `EXPLAIN FORMAT=JSON <sql>`
  (MariaDB의 EXPLAIN JSON에는 cost가 없어 `total_cost`는 0으로 옵니다)

| 탐지 항목 | 임계값 | 판정 |
| --- | --- | --- |
| Full scan (pg Seq Scan / mysql access_type=ALL) | 예상 10만 행↑ | +30 (미만이면 +8 기록만) |
| 카티션 의심 (pg: 조인 조건 없는 Nested Loop) | 발생 즉시 | +60 |
| 조인버퍼 대량 조인 (mysql: using_join_buffer + 1만 행↑) | 발생 즉시 | +40 |
| Nested Loop 대량 조인 (pg) | 1만 행↑ | +20 |
| 대량 정렬 (pg Sort / mysql filesort) | 50만 행↑ | +15 |
| 대량 집계 (pg Aggregate) | 100만 행↑ | +15 |
| 예상 최대 행 수 | 100만↑ | +20 (row 과다) |
| 전체 cost | 10만↑ | +15 (timeout 가능성) |

score ≥60 → `high`(실행 금지, suggestions 반영해 재생성), ≥25 → `medium`.
suggestions에는 인덱스 컬럼 조건·기간 조건·조인 키 인덱스·사전 집계 등
구체적 보강안이 담깁니다. MCP `explain_sql`은 실측 risk가 high면 응답의
`recommended_action`을 `regenerate_with_constraints`로 승격합니다.
응답의 `dialect` 필드로 어떤 엔진의 플랜인지 구분합니다.

권장 게이트 순서: **validate_sql → explain_sql(profile) → (low/medium)
preview → execute**. 0행이면 `hint`에 따라 조건을 재확인합니다.

### 실행계획 승인 게이트 (plan gate)

위의 EXPLAIN 분석을 권장 순서에만 맡기지 않고 **실행 자체에 강제**하는
게이트입니다. `Execute`는 쿼리 실행 직전에 동일한 방언별 JSON EXPLAIN을
수행하고, 추정 위험도가 프로파일 임계값 이상이면 실행하지 않고 분석된
플랜을 담은 `PlanGateError`로 거부합니다.

- **정책 필드** (프로파일 `policy`): `plan_gate`(bool, 기본 `true`),
  `plan_gate_risk`(`low`|`medium`|`high`, 기본 `high` — 이 값 *이상*이면 차단)
- **MCP `run_sql_safely`**: 게이트에 걸리면 `status=plan_approval_required`와
  `live_plan`(risk_factors/suggestions 포함)·`threshold`·`notice`를 반환합니다.
  우선 기간·LIMIT 조건을 좁혀 재생성하고, 그래도 그대로 실행해야 한다면
  **사용자 승인을 받은 뒤** `approve_plan=true`로 재호출합니다
- **REST**: `POST /api/query/execute`/`/api/query/preview`도 `approve_plan`
  필드를 받으며, 차단 시 동일한 `plan_approval_required` 응답 형태를
  반환합니다
- **preview는 게이트를 거치지 않습니다** — 이미 `default_max_rows`로 행이
  캡되어 있기 때문입니다
- **EXPLAIN 실패는 차단하지 않습니다**(fail-open) — 실제 쿼리 실행이
  올바른 오류 분류와 함께 원인을 표면화합니다
- 게이트 차단도 감사 로그(`audit/query-*.jsonl`)에 기록됩니다

## 장애 대응

| 상황 | 동작 |
| --- | --- |
| Failover/네트워크 단절 중 접속 실패 | 접속 타임아웃(5s) 빠른 실패 + 브레이커 |
| 죽은 커넥션 재사용 | `conn_max_lifetime/idle_time`으로 자동 폐기 |
| 쿼리 중 단절/타임아웃 | 오류 분류(TIMEOUT/CANCELED/PG-코드/MY-코드) + 감사 기록 |
| 반복 실패 | 서킷브레이커 30초 오픈 (`CIRCUIT_OPEN`) |
| 대량 조회 | maxRows+1 → `truncated=true`, 바이트 캡 |
| 실수로 쓰기 시도 | SQL 가드 차단 + read-only 세션이 이중 차단 (PG-25006 / MY-1290) |

## 오류 코드

드라이버 오류는 안정적인 코드로 분류되어 감사·이력·힌트에 쓰입니다:

| 코드 | 의미 |
| --- | --- |
| `PG-42P01` / `MY-1146` | 테이블 없음 |
| `PG-42703` / `MY-1054` | 컬럼 없음 |
| `PG-42601` / `MY-1064` | 문법 오류 (방언 확인) |
| `PG-42883` / `MY-1305` | 함수 없음 (타 방언 함수 사용) |
| `PG-28P01` / `MY-1045` | 인증 실패 |
| `PG-25006` / `MY-1290` | read-only 세션 위반 |
| `PG-57014` / `MY-1317` / `MY-3024` | 취소/시간 초과 |
| `TIMEOUT` / `CANCELED` | 컨텍스트 타임아웃/취소 |

## 감사·모니터링

- 실행 감사: `audit/query-*.jsonl` — trace_id, execution_id, user,
  profile, sql_hash, sql(4KB 절단), 시작/소요, row_count, truncated,
  success, error_code(PG-*/MY-*/TIMEOUT/CANCELED), 정제 메시지
- 이력 API: `GET /api/query/history` (최근 200건 + 실행 중 목록)
- 메트릭: `GET /api/metrics`(JSON) / `GET /metrics`(Prometheus) —
  `db_query_total/success/failure/slow`, `db_connection_ping_failure_total`,
  프로파일별 `db_pool_open/in_use/idle/wait_count/wait_duration_ms`
  (db.Stats()), 브레이커 상태

## 실행 이력 → 자동 학습

`learn_from_feedback`은 피드백뿐 아니라 **실행 감사(`audit/query-*.jsonl`)도
스캔**합니다 (기본 3회 이상 반복 시 승격):

- 특정 테이블 조회가 반복적으로 느림(≥5초) → `slow_query` 룰 — 이후 그
  테이블을 쓰는 SQL 검증 시 `LEARNED_SLOW_QUERY` 경고(평균 소요·보강안 포함)
- 특정 테이블에서 동일 실행 오류(PG-42P01, MY-1045, TIMEOUT 등) 반복 →
  `recurring_exec_error` 룰 — `LEARNED_EXEC_ERROR` 경고로 코드별 원인 안내
- CANCELED(사용자 취소)는 학습하지 않음

룰은 `learned_rules.json`에서 검토·수정·삭제 가능합니다.

## 자동 메타데이터 수집

이 커넥터의 read-only 풀·서킷브레이커는 대상 DB의 **물리 메타데이터 자동
수집**(스키마·테이블·컬럼·제약·인덱스 스냅숏 + 증분 변경감지)에도 그대로
재사용됩니다. 시스템 카탈로그 조회는 사용자 SQL이 아니라 서버 내부 질의로,
SQL 가드·플랜 게이트·소량 행 제한을 거치지 않지만 세션은 여전히 DSN으로
read-only가 강제되고 풀·서킷브레이커·행 상한이 적용됩니다. SELECT 전용
계정으로도 제약을 읽도록 PostgreSQL은 `pg_constraint`, MySQL/MariaDB는
`information_schema.statistics`를 사용합니다. 자세한 내용은
[metadata-sync.md](metadata-sync.md)를 참고하세요.

## 보안 계층 요약

1. 카탈로그 검증 (미존재 식별자·PII·금지조인 차단) — 실행 전 필수
2. 커넥터 SQL 가드 (읽기전용·다중문·방언별 위험 함수 — 키워드 기반)
3. 방언 AST 가드 (실제 파서로 read-only SELECT/WITH 단일문만 허용,
   fail-closed — 우회 구문 차단)
4. 실행계획 승인 게이트 (실측 EXPLAIN 위험도 ≥ `plan_gate_risk`면
   `plan_approval_required`로 거부 — 승인 시에만 실행)
5. DB 세션 read-only (DSN 강제: postgres/mysql/mariadb 각각의 세션 변수)
6. read-only 전용 DB 계정 (운영자 책임)
7. admin token — execute/preview/metadata/cancel 및 프로파일 변경 보호
8. 감사 로그 + 결과 행/바이트 제한

## 문제 해결

| 증상 | 원인/조치 |
| --- | --- |
| `unsupported db type` | 프로파일 `type`이 postgres/mysql/mariadb가 아님 |
| `connect_string must be host:port/dbname ...` | 접속 문자열 형식 오류 — 축약형 또는 URL/DSN 사용 |
| `CIRCUIT_OPEN` | 연속 실패로 차단 중 — 30초 후 재시도, 원인(접속 정보/네트워크) 해결 |
| `environment variable X is not set` | password_ref의 env 변수 미주입 |
| `PG-28P01` / `MY-1045` | 계정/비밀번호 불일치 |
| `PG-25006` / `MY-1290` | 쓰기 시도 — 이 커넥터는 설계상 read-only |
| `MY-1064`에 FETCH FIRST 언급 | MySQL은 `LIMIT n`만 지원 — SQL 재생성 |
| TIMEOUT | query_timeout 초과 — 기간 조건/limit 보강 후 재시도 |
