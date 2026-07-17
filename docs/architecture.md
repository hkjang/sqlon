# 아키텍처

## 개요

JAMYPG는 **LLM에게 스키마 전체를 던지지 않는다**는 원칙 위에 설계되었습니다.
JSON 메타데이터를 서버 시작 시 **내부 카탈로그로 컴파일**하고, 검색·조인
계획·지표 조회·검증·실행계획 분석을 **구조화된 도구**로 분리해 LLM이 매
단계에서 검증 가능한 근거만 사용하게 합니다.

```text
┌─────────────────────────────────────────────────────────────────┐
│                        MCP 클라이언트 (LLM)                      │
│   Claude / qwen-code / opencode / 자체 에이전트                  │
└───────────────┬─────────────────────────────────┬───────────────┘
                │ stdio (JSON-RPC)                │ Streamable HTTP
┌───────────────▼─────────────────────────────────▼───────────────┐
│  internal/mcp — 트랜스포트 & 표면                                │
│  ├─ stdio.go        : NDJSON JSON-RPC 루프                       │
│  ├─ server.go       : HTTP(단일 /mcp 엔드포인트), 세션(관대),    │
│  │                    28개 도구 디스패치, 감사 로그, 핫스왑      │
│  ├─ admin.go        : REST /api/*, /admin, /admin/editor, /docs  │
│  ├─ openapi.go      : OpenAPI 3.0 스펙                           │
│  └─ webui/ (embed)  : 관리 콘솔·테이블 편집기·Swagger UI 자산    │
└───────────────┬─────────────────────────────────┬───────────────┘
                │ atomic.Pointer[Catalog] (핫스왑) │ 읽기 전용 실행
┌───────────────▼─────────────────────────────────┼───────────────┐
│  internal/catalog — 도메인 로직 (DB 연결 없음, 순수 메모리)      │
│  ├─ load.go       : JSON → 카탈로그 컴파일, semantic type 추론   │
│  ├─ types.go      : Catalog/Table/Column/Relation 모델           │
│  ├─ datasets.go   : 데이터셋 레지스트리(19종)·교체·백업·복원     │
│  ├─ search.go     : 다중 신호 스키마 검색, 컨텍스트 압축         │
│  ├─ analyze.go    : 질문 분해, intent 시그니처                   │
│  ├─ join.go       : 조인 그래프 BFS, confidence                  │
│  ├─ joinsuggest.go: 누락 엣지 발굴(운영자 승인용)                │
│  ├─ metrics.go    : 지표 사전 조회(사전 우선/추정 분리)          │
│  ├─ glossary.go   : 업무 용어 사전(전 단계 공용)                 │
│  ├─ patterns.go   : 다단계 SQL 패턴 사전                         │
│  ├─ timeparse.go  : 시간 표현 → semantic type별 SQL 조건         │
│  ├─ skeleton.go   : 검증된 부품으로 SQL 골격 조립                │
│  ├─ validate.go   : 정적 SQL 검증(33종 룰), fix_hints            │
│  ├─ rank.go       : 후보 SQL 객관 점수 랭킹                      │
│  ├─ stats.go      : 컬럼 통계, 값→필터컬럼 매핑                  │
│  ├─ overrides.go  : 운영자 보정(PII/조인 정책/기본 필터)         │
│  ├─ feedback.go   : 피드백 적재→검색 부스트·few-shot 재사용      │
│  ├─ learn.go      : 반복 실패 패턴 → 학습 룰 승격                │
│  ├─ eval.go       : 골든셋 평가                                  │
│  └─ health.go     : 컴파일 품질 리포트                           │
└───────────────────────────────┬─────────────────┼───────────────┘
                                │ 파일 I/O        │
┌───────────────────────────────▼───────────────┐ │
│  data/metadb — JSON 데이터셋 19종                │ │
│  물리/논리 모델, 코드사전, 조인관계, 인덱스,  │ │
│  SQL예제, 주제영역, 프롬프트, DB정의,         │ │
│  용어사전, 지표사전, overrides, 패턴, 통계,   │ │
│  골든셋, 학습룰, DB프로파일(db_profiles),     │ │
│  feedback/(JSONL), audit/(JSONL), backups/    │ │
└───────────────────────────────────────────────┘ │
┌─────────────────────────────────────────────────▼───────────────┐
│  internal/dbconn — 읽기 전용 DB 커넥터                           │
│  ├─ profile.go  : 접속 프로파일(type/pool/policy, password_ref)  │
│  ├─ sqlguard.go : SELECT/WITH 단일문 + 금지 키워드 가드          │
│  ├─ dialect.go  : postgres│mysql│mariadb DSN·LIMIT 래핑·금지어   │
│  ├─ manager.go  : 풀·타임아웃·행 제한·서킷 브레이커·감사·메트릭  │
│  ├─ explain.go  : 실 EXPLAIN(JSON) 리스크 분석                   │
│  └─ errors.go   : PG-<SQLSTATE> / MY-<errno> 오류 코드 분류      │
│  → 대상 DB: PostgreSQL · MySQL · MariaDB                         │
│    (pgx / go-sql-driver — 순수 Go 드라이버, CGO 없음)            │
└─────────────────────────────────────────────────────────────────┘
```

실행 파일은 3개입니다: `jamypg-mcp`(서버), `jamypg-eval`(평가 CLI),
`jamypg-goldgen`(골든셋 생성 CLI). 모두 같은 `internal/catalog`를 사용합니다.

## 엔진 어댑터 계층

DBA 운영 기능(워크로드·용량 수집, 세션·잠금 관찰)의 엔진별 구현은
`internal/engine/<엔진>` 패키지에 격리되어 있습니다.

```text
internal/engine/
  capability.go, registry.go   엔진별 CapabilitySet 선언 (기능 지원 여부)
  postgres/  workload.go, sessions.go, locks.go
  mysql/     workload.go, sessions.go, locks.go   (MariaDB가 공유)
  mariadb/   mariadb.go   (MySQL 위임 + InnoDB 잠금 뷰 차이만 구현)
  oracle/    workload.go, sessions.go, locks.go   (base 라이선스 뷰만 사용)
  adapters/  adapters.go   단일 배선 지점: 엔진 이름 → 구현 등록
```

규칙:

- 역할 인터페이스(`collector.Provider`, `observability.Provider`)는 결과
  모델을 소유한 서비스 패키지에 있고, 엔진 패키지가 이를 구현합니다.
- 서비스는 엔진 이름으로 분기하지 않습니다. 기능 활성화는
  `engine.CapabilitySet`, 구현 조회는 `adapters`가 만든 레지스트리를
  사용합니다.
- 새 엔진 추가 = 엔진 패키지 신설 + `adapters` 등록 + Capability 선언.
  `internal/engine/adapters/contract_test.go`가 Capability 선언과 구현
  등록의 일치를 강제하고, 모든 엔진에 동일한 읽기 전용·라이선스 계약
  테스트를 적용합니다.

## 카탈로그 컴파일 파이프라인

`catalog.Load(dataDir)`는 다음 순서로 실행됩니다 (`load.go`):

```text
1. loadPhysical   meta_physical_models.json   ← 실패 시 서버 기동 중단
2. loadLogical    meta_logical_models.json    ← 실패 시 서버 기동 중단
   (테이블 0개면 기동 중단)
3. 선택 파일 로드(실패는 LoadIssue로 축적, 기동은 계속):
   code_dict → indexes → relations → sql_datasets → subject_areas
   → prompts → databases
4. Dialect 결정 (databases.json DBMS 또는 overrides.json dialect,
   기본 postgres — postgres | mysql | mariadb)
5. 사전 로드: glossary(없으면 내장 기본값) → metrics → overrides → patterns
6. finalize:
   - 컬럼 정렬, PK/FK 집계
   - semantic type 추론 (DATE / TIMESTAMP / DATE_YYYYMMDD /
     MONTH_YYYYMM / DATETIME_YYYYMMDDHH24MISS) ← 이름+타입+길이 규칙
   - 도메인 추론 (설명의 [태그] 또는 논리명 중간 세그먼트)
   - grain 추론 (테이블명 접미사 × subject_areas 규칙)
7. applyOverrides: 설명/도메인/PII/동의어/권장·금지 조인/기본 필터 반영
8. loadColumnStats: 프로파일 통계 부착, row count/최신성 승격
9. SearchText 재구성 (overrides 반영분 포함)
10. validateRelations: 조인 엣지의 테이블/컬럼 실존 교차 검증 → LoadIssue
11. validateMetrics: 지표 사전의 테이블/컬럼 실존 교차 검증 → LoadIssue
12. loadFeedback: 성공 피드백 → 테이블 사용 빈도(검색 부스트)
13. loadLearnedRules: 학습 룰 → 검색 패널티 + 검증 경고 등록
```

핵심 설계 결정:

- **필수 vs 선택**: 물리/논리 모델만 필수. 나머지는 없으면 해당 기능이
  축소되거나 내장 기본값으로 대체되며, 모든 문제는 `LoadIssue`로 축적되어
  `get_catalog_health`와 기동 로그에 노출됩니다. *조용한 실패 없음*.
- **정규화**: 모든 식별자는 `cleanIdent`(공백/제로폭 문자 제거, 대문자화)로
  통일. 원천 JSON의 `" transactions"` 같은 오염을 흡수합니다.
- **컴파일 산출물**: `Tables`(FQN 인덱스), `ByName`(스키마 생략 해석),
  `Adjacency`(조인 그래프), `SearchText`(검색 코퍼스) 등 조회 전용 구조.
- **방언 반영**: 결정된 dialect는 `validate_sql`의 방언 룰(Oracle 전용
  문법 오류 처리, 교차 방언 함수 경고)과 스켈레톤·행 제한 제안(LIMIT n)에
  일관되게 적용됩니다.

## 카탈로그 핫스왑

```go
type Server struct {
    catalogPtr atomic.Pointer[catalog.Catalog]  // 읽기: s.cat()
    dataMu     sync.Mutex                       // 변경 직렬화
}
```

- 모든 요청 처리기는 `s.cat()`로 **일관된 스냅샷**을 읽습니다.
- `put_dataset`/`remove_dataset`/`reload_catalog`(MCP)와 REST 변경 API는
  `dataMu`로 직렬화된 뒤 **새 카탈로그를 통째로 컴파일**하고 성공 시에만
  포인터를 교체합니다. 진행 중인 요청은 이전 스냅샷으로 끝까지 처리됩니다.
- 변경 실패(컴파일 오류·신규 LoadIssue) 시 백업을 복원하고 이전 카탈로그를
  유지합니다 — **부분 적용 상태가 존재하지 않습니다**.

## 트랜스포트

| | stdio | Streamable HTTP |
| --- | --- | --- |
| 용도 | 데스크톱 MCP 클라이언트(서브프로세스) | 게이트웨이, 원격, 웹 콘솔 |
| 프로토콜 | NDJSON JSON-RPC 2.0 | POST/GET/DELETE `/mcp` |
| 세션 | 없음 | `Mcp-Session-Id` 발급하되 **강제하지 않음** (관대 정책 — qwen-code/opencode 호환) |
| 웹 UI | 없음 | `/admin`, `/admin/editor`, `/docs`, `/api/*`, `/healthz` |

두 트랜스포트는 동일한 `handleRequest`를 공유하므로 도구 동작이 같습니다.

## 정확도를 만드는 5개 축

1. **그라운딩** — LLM은 카탈로그가 반환한 식별자만 사용. `validate_sql`이
   위반을 오류로 차단.
2. **사전(Dictionary) 우선** — 지표 계산식·용어·패턴·조인 조건은 사전에서
   취득. 사전에 없으면 "추정"임을 구분해 반환.
3. **압축 컨텍스트** — `get_schema_context`/`build_sql_skeleton`이 Top-N
   테이블·Top-K 컬럼·필수 조인·정책 필터만 전달.
4. **다층 검증** — 정적 룰 33종 + 리스크 추정(프로파일 지정 시 실
   EXPLAIN 기반) + 후보 랭킹 + (골든셋) 평가.
5. **환류 학습** — 피드백 → 검색 부스트/패널티, few-shot 재사용, 룰 승격.

## 데이터 흐름: 질문 → SQL (요약)

상세는 [sql-generation-workflow.md](sql-generation-workflow.md) 참조.

```text
질문 ─ analyze_question ─ search_schema ─ get_metric_definition
     └ find_filter_columns / resolve_time (필요 시)
     → get_schema_context + get_join_paths
     → (복잡) build_sql_skeleton → LLM이 SLOT만 채움
     → validate_sql (fix_hints 루프 ≤2회) → (선택) rank_candidates
     → explain_sql (프로파일 지정 시 실 EXPLAIN; risk=high면 재생성)
     → (선택) run_sql_safely — 읽기 전용 실행 → 구조화 JSON 응답
     → record_feedback → (주기) learn_from_feedback
```

## 관측 가능성

- `logs/`: 표준 로그 (기동 시 카탈로그 요약 + LoadIssue 오류 전체 출력)
- `data/<set>/audit/audit-YYYYMMDD.jsonl`: 모든 MCP tool call + REST 변경
  (도구명, 인자 4KB 절단, 소요 ms, 오류)
- `data/<set>/audit/query-YYYYMMDD.jsonl`: 모든 DB 실행 (`db:execute` 항목 —
  SQL, 프로파일, 행 수, 소요, `PG-*`/`MY-*` 오류 코드)
- `GET /healthz`: 카탈로그 요약 (liveness)
- `GET /metrics`: Prometheus 텍스트 — `db_query_total`,
  `db_query_success_total`, `db_query_failure_total`, `db_query_slow_total`,
  `db_connection_ping_failure_total`, `db_query_elapsed_ms_sum`, `db_pool_*`
  (open/in_use/idle connections, wait_count, wait_duration_ms)
- MCP `get_catalog_health` / `GET /api/health`: 이슈 상세, 커버리지 갭,
  PII 목록, 사전 크기
