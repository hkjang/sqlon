# Changelog

## Unreleased

## v0.3.0 — 2026-10-06

### 예방 경보 강화

- **DB 서버 디스크 실측 보고**: `scripts/sqlon-disk-report.sh`(POSIX sh + df, curl 또는
  BusyBox wget)를 DB 서버 cron 에서 돌리면 `POST /api/early-warning/disk` 로 볼륨 사용량이
  들어와, SQL로는 보이지 않는 DB 밖 파일(덤프 백업·외부 로그)과 볼륨의 실제 크기로 고갈을
  예측합니다. 경보에 "DB 밖 파일 약 N GiB" 를 함께 보여주고, DB에 접속할 수 없어도 평가하며,
  보고가 15분 넘게 끊기면 경고합니다. ext4 root 예약 블록은 한도에서 뺍니다.
- **MySQL·MariaDB 점유량**: 연결된 DB 하나가 아니라 모든 스키마 + binlog 를 수집합니다.
  binlog 권한(REPLICATION CLIENT·BINLOG MONITOR)은 `SHOW GRANTS` 로 먼저 확인해 권한
  오류가 회로 차단기를 열지 않게 합니다.
- **무음(silence)**: 계획 작업 동안 DB·규칙(접두어 가능) 단위로 알림만 멈춥니다. 경보는
  계속 표시되고 끝날 때 여전히 유효한 것을 보냅니다. 콘솔에서 추가·해제합니다.
- **DB별 알림 채널**: 프로파일 `alerting.webhook_ref`(env:/file:/plain:, 화면에서 가림)로 팀
  채널을 지정하고 `alerting.min_severity` 로 DB별 최소 위험도를 정합니다. 채널마다 전달 상태와
  재시도를 따로 관리하고, 해석할 수 없는 채널은 기본 채널로 대신 보냅니다.
- **일일 용량 리포트**: 매일 09:00(`SQLON_ALERT_DIGEST_AT`) 기본 채널로 DB별 사용률·추세·
  가득 차는 날짜를 보냅니다.

### 성능·정리

- 수집기가 매 주기 "직전 스냅숏"을 찾으려고 저장소 전체(보존 30일)를 읽던 것을 메모리
  캐시로 바꿨습니다. 7일치 저장소 기준 조회 1회 0.47초 → 메모리 조회이며, 그동안 저장소
  잠금을 잡지 않습니다. 플릿 상태·운영 화면 조회도 같은 경로를 씁니다.
- v0.1.0 직후 커밋에서 0바이트로 잘렸던 `docs/` 문서 28개를 복원하고 기본 포트 변경(6767)을
  반영했습니다.

## v0.2.0 — 2026-10-05

### 예방 경보 (Early Warning)

PostgreSQL 볼륨이 가득 차 DB가 멈추는 장애를 사전에 알리는 체계를 추가했습니다.
지금까지 PostgreSQL 용량 수집에는 한도가 없어 사용률·고갈 예측 경보가 구조적으로
한 번도 울릴 수 없었고, WAL을 붙잡는 슬롯·아카이브 점검은 주기적으로 돌지 않았습니다.

- **저장공간 점유량**: PostgreSQL에서 연결된 데이터베이스뿐 아니라 인스턴스의 모든
  데이터베이스 + `pg_wal` + 임시파일 + 로그 디렉터리를 `storage:footprint` 로
  수집합니다. `pg_ls_*` 실행 권한을 먼저 조회해, 권한이 없으면 호출하지 않고
  `pg_monitor` 부여를 안내합니다(권한 오류가 회로 차단기를 열지 않도록).
- **용량 한도 선언**: DB 프로파일에 `capacity.storage_limit`(예: `500GiB`)와
  사용률·남은 일수 임계값을 선언합니다. 화면(`/admin/db`)에서 입력할 수 있습니다.
- **고갈 예측**: 최근 6시간·7일 최소제곱 회귀로 증가율을 구해 가득 차는 시점을
  계산합니다. 6시간 추세는 R² ≥ 0.8일 때만 사용해 일회성 대량 적재로 오경보하지
  않습니다. 한도가 없어도 증가 속도 급증은 감지합니다.
- **디스크를 채우는 원인 점검**(예방 점검 확장, 5분 주기): 실패·적체된 WAL 아카이브,
  `archive_mode=on` 인데 비어 있는 아카이브 명령, `max_wal_size` 대비 과다한 `pg_wal`,
  연결됐지만 크게 뒤처진 복제 슬롯과 13+ `wal_status` lost/unreserved, VACUUM 을
  막는 장기·prepared 트랜잭션, `max_slot_wal_keep_size=-1`·`autovacuum=off`.
  스탠바이에서도 슬롯 조회가 실패하지 않도록 LSN 기준을 바꿨습니다.
- **테이블 급증·급감**: 테이블별 24시간 변화를 자기 평소 증가량과 비교합니다.
  다운샘플링이 대량 적재나 TRUNCATE 같은 급변을 지우지 않도록 큰 변화는 별도 점으로 보존합니다.
- **스키마 변경 감지**(15분 주기): 물리 스키마를 기준선과 비교하고, 실행된
  변경계획에 없는 변경을 "계획 외"로 표시합니다. 부분 수집된 스냅숏은 비교하지 않습니다.
- **관측 중단**: 3회 연속 수집 실패 시 경보합니다.
- **경보 수명주기**: 발생·격상·지속 재알림(6시간)·해소·확인(ack). 경보를 만든 점검이
  실제로 다시 실행됐을 때만 해소하고, 상태를 저장해 재시작해도 중복 알림이 없습니다.
- **알림**: Mattermost·Slack incoming webhook 호환 메시지(`SQLON_ALERT_WEBHOOK`).
  한 주기를 한 메시지로 묶고, 실패 시 지수 백오프로 재시도하며 전달 상태를 표시합니다.
  수집기의 기존 경보(쿼리 지연 회귀 등)도 같은 경로로 전달됩니다.
- **조회**: `/admin/alerts` 예방 경보 콘솔, MCP 도구 `get_early_warnings`,
  REST `/api/early-warning`(+ack·evaluate·test-notification), Prometheus
  `sqlon_storage_days_to_full` 등 게이지.
- 운영 스냅숏 조회가 `Since` 이전 날짜 파일을 읽지 않도록 했습니다.
- DB 프로파일 화면에서 프로파일을 수정하면 화면에 없는 `config_baseline` 이
  지워지던 문제를 고쳤습니다.

상세: [docs/early-warning.md](docs/early-warning.md)

## v0.1.5 — 2026-09-14

- PII 노출 리포트(`get_pii_exposure`)의 짧은 ASCII 단서(`pan`·`dob`·`ssn`·
  `card` 등 네 글자 이하)를 부분 문자열이 아닌 낱말 경계로 대조해
  `japan_code`가 신용카드로, `adobe_flag`가 생년월일로 잡히던 오탐을
  제거했습니다. 밑줄이 포함된 단서, 다섯 글자 이상 단서, 한글 단서는 기존
  부분 문자열 대조를 유지하며 `pan`·`dob`·`ssn`·`card_no`·`cust_addr`·
  `iban`·`rrn`은 그대로 탐지됩니다.
- 실행 중 생기는 로컬 데이터(`data/backups`, `data/sqlon`)와 로컬 데모
  스크립트를 `.gitignore`에 추가했습니다.
- GitHub Pages 랜딩 페이지에 한국어 기본·영어 전환, 브라우저 언어 자동
  감지, SEO/AEO 메타데이터와 Schema.org JSON-LD, 모바일 반응형 레이아웃,
  GitHub Sponsor 버튼을 추가했습니다.

## v0.1.0 — 2026-07-20

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
