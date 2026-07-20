# Changelog

## Unreleased

## v0.59.0 — 2026-07-20

- 4개 엔진의 실제 누적 워크로드·대기·Top SQL·용량 통계를 읽기 전용 Provider로
  수집하고 append-only 운영 저장소에 보존하는 주기 수집기를 추가했습니다.
  이전 스냅숏에서 QPS/TPS와 일간 용량 증가량을 계산하며 REST/MCP와
  워크로드·용량 화면이 동일 서비스 계층을 사용합니다.
- PostgreSQL·MySQL·MariaDB·Oracle의 읽기 전용 시스템 뷰를 공통 Provider로
  정규화하는 세션·잠금 관찰 서비스를 추가했습니다. REST/MCP/UI에서 장기 SQL과
  장기 트랜잭션, 대기 이벤트, RAC 안전 세션 키, 보호 세션, 루트 블로커와 영향
  세션 수를 근거·수집 시각과 함께 확인할 수 있습니다.
- 권한 범위 내 PostgreSQL·MySQL·MariaDB·Oracle 프로파일을 공통 서비스에서
  병렬 점검하고 위험 순위, 수집 상태, 근거, 최신성, 엔진 Capability를 반환하는
  플릿 REST/MCP API와 첫 화면을 추가했습니다. DB 프로파일 UI에는 업무 서비스,
  환경, 중요도, 역할, 담당 조직과 Oracle 연결·Pack 정책 입력을 추가했습니다.
- 운영 환경 프로파일의 조회·DBA 자격증명은 `plain:` 저장을 거부하고 `env:` 또는
  `file:` Secret 참조를 강제합니다.
- 기본 시작 시 기존 `data/metadb`의 프로파일, 카탈로그와 감사 로그를 완전
  백업한 뒤 `data/sqlon`으로 충돌 없이 원자적 병합하는 마이그레이션을
  추가했습니다. 명시적인 데이터 경로는 이동하지 않으며 기존 환경변수 별칭을
  계속 지원합니다.
- PostgreSQL 메타 저장소를 `sqlon_meta` 스키마로 전환하고 기존
  `public.jamypg_*` 테이블과 데이터를 트랜잭션 안에서 자동 이동합니다. 신규
  인증 쿠키와 MCP 키는 `sqlon_session`, `ssk_`를 사용하며 기존 쿠키와
  `jsk_` 키는 한 릴리스 동안 계속 인증됩니다.
- Prometheus 제품 지표를 `sqlon_*`로 전환하고 기존 `jamypg_*` 이름을
  deprecated 별칭으로 제공합니다.

## v0.58.0 — 2026-07-15

- 데이터셋 관리 화면에 검색, 상태 필터, 요약 지표와 변경 영향 중심의 작업 흐름을 추가했습니다.
- 테이블 편집기에 데이터셋 요약, 단계 안내, 변경 상태 기반 저장과 단축키를 추가했습니다.
- 데이터셋 조회 실패를 빈 데이터로 오인하지 않도록 오류를 명확히 표시하고 편집을 차단합니다.
- 객체·배열 셀의 잘못된 JSON 입력을 저장 전에 검증해 데이터 타입 손상을 방지합니다.

## v0.57.0 — 2026-07-15

### OpenMetadata integration reliability

- Added connection testing before configuration save with URL validation and
  failure-stage diagnostics for authentication, DNS, network, timeout, and API
  path errors.
- Hardened table and glossary pagination against repeated cursors and exposed
  partial-fetch warnings instead of silently treating incomplete imports as
  successful.
- Split imports into preview, review-queue, and direct-apply workflows, and
  added lineage planning and publishing to the administration UI.

### Work area UX

- Natural-language queries can now select a DB profile and use its dedicated
  catalog workspace, with the effective catalog source shown in the result.
- History adds full-text filtering, activity summaries, prompt reuse, and SQL
  handoff to the DB console.
- Statistics now emphasize SQL validity rate, interpret quality signals, show
  refresh state, and link directly to relevant follow-up actions.
- Refreshed responsive layouts, guided empty states, example queries, and
  in-product help across query, history, statistics, and OpenMetadata screens.

## v0.56.0 — 2026-07-15

### Profile catalog reliability and workflow

- Added live schema discovery and selectable collection scope before building
  a profile catalog workspace.
- Profile catalog APIs now enforce profile access checks for direct workspace
  and dataset reads.
- Workspace load failures and per-profile batch build failures are surfaced
  with actionable detail instead of being hidden behind aggregate counts.
- The UI now reflects the actual activation model: standalone mode supports a
  temporary global switch, while meta-DB mode automatically uses the workspace
  selected by each request's profile.
- Redesigned the profile catalog screen around the select, discover, build,
  review, and enrich workflow with search, readiness state, guided empty
  states, responsive controls, and clearer dataset counts.

## v0.55.0 — 2026-07-15

### Database connection UX and diagnostics

- Added an engine-first DB connection wizard for PostgreSQL, MySQL, and
  MariaDB. The selected engine is now persisted explicitly instead of falling
  back to PostgreSQL.
- Profile saves now run an immediate connection test and show actionable
  diagnostics for DNS, network, authentication, database, TLS, secret-mount,
  and server compatibility failures.
- The DB screen now reports the drivers actually compiled into the binary;
  MySQL/MariaDB use the bundled pure-Go `go-sql-driver/mysql` and require no
  native client library in the runtime image.

### DBA console

- Redesigned the privileged DBA console around the operator workflow and added
  detailed PostgreSQL/MySQL/MariaDB capability guides.
- Improved profile context, status visibility, edit-from-list actions, safety
  guidance, and responsive presentation.

## v0.2.0 — 2026-07-11

### Security and DBA controls

- Standalone HTTP remains loopback-only by default. A non-loopback bind now
  requires either meta-DB authentication or both `-public-mcp` and an admin
  token.
- All profile-backed MCP operations (`run_sql_safely`, live `explain_sql`, and
  execution-based `run_evaluation`) share one authorization registry and
  standalone admin-token gate.
- Feedback is server-scoped and quarantined as `pending/untrusted`; only an
  administrator-approved record may influence few-shot examples, retrieval
  priors, or learned rules. Size/rate limits and duplicate suppression are
  included.
- Operator default filters support `enforcement: error` for execution-blocking
  policy, with dialect AST validation and query-block/alias-aware predicate
  checks.

### AI grounding

- Metric resolution now combines exact name/business name/alias priority with
  glossary synonyms and conservative token coverage/proximity scoring.
- Metric lookup, question analysis, and schema retrieval use the same resolver;
  responses include confidence and match evidence.

### MCP and engineering

- Added the administrator-only `review_feedback` tool (29 registered tools).
- Tool registry, dispatcher, and README drift is now detected by tests.
- OpenAPI and MCP server versions share one source of truth.
- Added GitHub Actions checks for module verification, vetting, tests, and all
  CLI builds.

### Upgrade notes

- Existing v1 feedback JSONL records remain audit data but are not trusted or
  learned automatically. Submit new feedback and approve it through
  `review_feedback`.
- `record_feedback` no longer changes retrieval or learning immediately.
- Existing externally bound standalone commands must add `-public-mcp` and
  configure `-admin-token`, or migrate to authenticated `-meta-db` mode.
- Default-filter behavior remains warning-only unless the entry explicitly sets
  `"enforcement": "error"`.
