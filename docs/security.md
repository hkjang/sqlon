# 보안 모델

## 원칙

카탈로그 자체는 **정적 JSON 메타데이터 서비스**이지만, `run_sql_safely`·
`explain_sql`은 접속 프로파일이 주어지면 대상 DB(PostgreSQL/MySQL/MariaDB)에
**읽기 전용으로 실제 접속**합니다. 생성 SQL이 실행될 수 있고 메타데이터
자체가 내부 자산이므로 다층 방어를 둡니다.

## 1. 읽기 전용 SQL 정책

- `validate_sql`이 SELECT/WITH 이외를 차단하고 DML/DDL 키워드(INSERT,
  UPDATE, DELETE, MERGE, DROP, ALTER, TRUNCATE, CREATE, GRANT, REVOKE,
  EXECUTE)를 오류로 표시합니다 (`NOT_SELECT`, `BLOCKED_KEYWORD`).
- `run_sql_safely`는 프로파일 없이 호출하면 dry-run 가드(LIMIT을 강제한
  `bounded_sql` 반환)이고, 프로파일 지정 시 **3중 읽기 전용 방어** 아래
  실행합니다:
  1. **SQL 가드**(`internal/dbconn/sqlguard.go`): SELECT/WITH 단일문만 허용,
     공통 금지 키워드(DML/DDL/트랜잭션/세션 변경 — SET, USE, LOCK, CALL 등)
     + 방언별 추가 금지어(postgres: DO, COPY, VACUUM, pg_sleep, pg_read_*,
     pg_ls_*, lo_import/lo_export, dblink, pg_terminate_backend 등;
     mysql/mariadb: REPLACE, HANDLER, LOAD_FILE, OUTFILE, DUMPFILE, SLEEP,
     BENCHMARK, GET_LOCK, KILL, FLUSH 등). 주석·문자열 리터럴을 마스킹한
     뒤 검사하므로 주석 속 키워드로 우회/오탐되지 않습니다.
  2. **DB 세션 read-only**: DSN에 항상 주입 — postgres
     `default_transaction_read_only=on`, mysql `transaction_read_only=1`,
     mariadb `tx_read_only=1`.
  3. **SELECT 전용 DB 계정** 사용 권장 — 최종 방어선.
- 행 수 제한: 쿼리를 `SELECT * FROM (<sql>) AS jamypg_q LIMIT <n>`으로
  래핑합니다(3개 엔진 공통; mysql/mariadb의 WITH 쿼리는 래핑 대신 말미에
  `LIMIT n` 부가). 쿼리 타임아웃·응답 바이트 상한도 함께 적용됩니다.
- 검증 실패 SQL은 실행되지 않습니다(`executed=false`) — 클라이언트도 검증
  실패 SQL을 실행하지 않는 것이 계약입니다 (`retry_guidance`에 명시).

## 2. PII 보호

- PII 지정: `overrides.json`의 `pii_columns` — `SCHEMA.TABLE.COLUMN` 또는
  `*.COLUMN` 와일드카드 (예: `*.ssn_hash`).
- 차단 지점:
  - `validate_sql`: PII 컬럼 참조 → `PII_COLUMN` **오류**, PII 보유 테이블에
    `SELECT *` → `PII_SELECT_STAR` **오류**
  - `run_sql_safely`: 실행 결과의 PII 값 마스킹
  - `get_schema_context`: 컬럼에 `pii: true` 플래그 + 생성 규칙에 노출 금지
    명시
  - `get_catalog_health`: 현재 PII 컬럼 전체 목록 노출(감사용)
- 한계: 파생 가공(SUBSTR 등)을 통한 우회는 정적 분석 한계상 완전 차단이
  아니므로 DB 계정 권한에서 최종 방어하십시오.

## 3. 조인·필터 정책

- `forbidden_joins`(overrides): 금지 조합은 `get_join_paths`가 경로를
  반환하지 않고 `validate_sql`이 `FORBIDDEN_JOIN` 오류로 차단
- `default_filters`: 누락 시 경고로 강제 유도
- 카티션 위험(`JOIN_WITHOUT_ON`, `COMMA_JOIN_NO_WHERE`)은 오류

## 4. 관리 표면 인증

| 표면 | 보호 |
| --- | --- |
| MCP 도구 (stdio/HTTP) | 단독 모드: 트랜스포트 신뢰 기반 (아래 네트워크 절 참조) |
| REST 조회 (GET) | 개방 |
| REST 변경 (PUT/DELETE/POST) | `-admin-token`/`JAMYPG_ADMIN_TOKEN` 설정 시 `X-Admin-Token` 또는 `Bearer` 필수 |
| 웹 콘솔 | 토큰을 브라우저 localStorage에 저장해 전송 |

- 토큰 미설정이면 변경 API가 개방되고 **기동 로그에 경고**가 남습니다.
  내부망 밖 노출 시 반드시 설정하십시오.
- 메타 DB 인증 모드(`-meta-db`/`JAMYPG_META_DB`)에서는 로그인 세션·MCP 키
  기반 인증과 역할(admin) 검사가 활성화되며, DB 프로파일도 소유/공유 단위로
  격리됩니다. Keycloak SSO는 `JAMYPG_OIDC_*`로 설정 —
  상세는 [auth.md](auth.md).
- 단독 모드에서 MCP의 `put_dataset` 등은 토큰 검사를 받지 않습니다 — MCP
  채널 자체가 신뢰 경계입니다(로컬 stdio 또는 통제된 HTTP). MCP를 외부에
  노출할 경우 메타 DB 인증 모드를 켜거나 게이트웨이에서 인증을 부과하십시오.

## 5. 네트워크 경계

- 기본 바인딩 `127.0.0.1:9797` (Docker는 0.0.0.0 — 포트 공개 범위로 통제)
- Origin 검증: 빈 Origin(비브라우저), localhost 계열, `-allow-origin`
  등록분만 허용. 브라우저발 교차 출처 접근을 차단합니다.
- TLS는 내장하지 않습니다 — 리버스 프록시에서 종단하십시오.

## 6. 파일 시스템 안전

- 데이터셋 쓰기는 **레지스트리 등록 이름만** 허용 — 임의 경로 불가
- 백업 복원 이름은 `해당파일명.` 접두사 + 경로 구분자 금지 (경로 탈출 차단)
- 요청 본문 상한: MCP POST 16MB, 데이터셋 PUT 32MB, restore 1MB
- 시스템 관리 데이터셋(feedback/audit)은 교체·제거 불가

## 7. 감사 추적

- 모든 MCP tool call: `audit/audit-YYYYMMDD.jsonl` — 도구명, 인자(4KB 절단),
  소요, 오류 여부
- 모든 DB 실행: `audit/query-YYYYMMDD.jsonl` — 도구명 `db:execute`, SQL,
  프로파일, 행 수, 소요, 오류 코드(`PG-<SQLSTATE>`/`MY-<errno>`/`TIMEOUT`)
- 모든 REST 변경: audit 파일에 `admin:put_dataset` 등으로 기록 + 원격 주소
- 데이터 변경의 이전 상태: `backups/` (타임스탬프)
- 권장: audit/backups 볼륨을 로그 수집기로 흡수, 보존 정책 적용

## 8. 자격증명 취급

- `databases.json`(방언 판별용 메타)의 host/username/password는 **암호화된
  토큰 형태**로 저장되어 있으며 서버는 이를 복호화하지 않습니다(`dbms`만
  사용). 복호화 키는 이 저장소/이미지에 존재하지 않습니다.
- 실제 접속용 `db_profiles.json`의 비밀번호는 `password_ref`로 간접
  참조합니다 — `env:NAME` | `file:PATH` | `plain:VALUE`. `plain:`은
  비권장이며, 비밀번호는 API 응답·감사 로그에 노출되지 않고 접속 대상은
  마스킹되어 표시됩니다.
- 릴리즈 이미지에는 `data/metadb`가 내장되므로, 이미지 배포 범위 = 메타데이터
  공개 범위임을 인지하고 배포 채널을 통제하십시오.

## 9. 위협 모델 요약

| 위협 | 완화 |
| --- | --- |
| LLM이 파괴적 SQL 생성 | 정적 검증 + SQL 가드(단일 SELECT/WITH·금지어) + DB 세션 read-only + SELECT 전용 계정 |
| 장시간·과대 쿼리 DoS | 쿼리 타임아웃, LIMIT 래핑·행 상한, 응답 바이트 상한, 연속 장애 서킷 브레이커 |
| PII 유출 SQL | PII 오류 차단 + 결과 값 마스킹 + 스키마 컨텍스트 플래그 + DB 권한 최종 방어 |
| 무단 메타데이터 변조 | admin token/역할 검사 + 감사 로그 + 자동 백업/롤백 |
| 경로 탈출/임의 파일 쓰기 | 레지스트리 화이트리스트 + 백업 이름 검증 |
| 브라우저 CSRF성 접근 | Origin 검증(localhost/allowlist) + 토큰 헤더(쿠키 미사용) |
| 대용량 본문 DoS | 크기 상한, 변경 직렬화 |
