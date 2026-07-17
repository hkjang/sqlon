# Changelog

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
