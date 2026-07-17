# SQL 검증 룰 카탈로그

`validate_sql`이 검사하는 전체 룰입니다. **오류(error)는 실행 금지**이며
`fix_hints[]`에 구조화 수정 지침이 담깁니다. **경고(warning)는 메타데이터
기반 안전 점검**으로, 대부분 정책 필터·조인 신뢰도에 관한 것입니다.

- 자동 수정 루프: `fix_hints` 반영 → 재검증, **최대 2회**. 이후에도
  실패하면 실패 원인과 수정 제안을 사용자에게 반환합니다.
- 검증은 정적(regex + 카탈로그 대조)이며 CTE/인라인뷰 스코프를 인식합니다:
  CTE 이름은 UNKNOWN_TABLE로 오탐하지 않고, CTE 본문 안의 실제 테이블·
  컬럼은 계속 검증됩니다.

## 오류 (15종) — 실행 차단

| 코드 | 조건 | 수정 방법 |
| --- | --- | --- |
| `EMPTY_SQL` | SQL이 비어 있음 | SQL 생성 후 호출 |
| `NOT_SELECT` | SELECT/WITH로 시작하지 않음 | 조회 전용 정책 — DML/DDL 제거 |
| `BLOCKED_KEYWORD` | INSERT/UPDATE/DELETE/MERGE/DROP/ALTER/TRUNCATE/CREATE/GRANT/REVOKE/EXECUTE 포함 | 해당 구문 제거 (읽기 전용) |
| `DIALECT_FUNCTION` (오류) | Oracle 전용(NVL/NVL2/DECODE/ADD_MONTHS/MONTHS_BETWEEN/LISTAGG/ROWNUM) 또는 SQL Server 전용(ISNULL/GETDATE/TOP n) 함수·구문 — 모든 지원 방언에서 무효 | 표준으로 교체 (NVL→COALESCE, DECODE→CASE, ROWNUM→LIMIT, GETDATE→CURRENT_TIMESTAMP, TOP n→LIMIT n) |
| `DIALECT_LIMIT` | mysql 방언에서 `FETCH FIRST` 사용 | `LIMIT n`으로 교체 |
| `DIALECT_BACKTICK` | postgres 방언에서 백틱 식별자 | 제거 또는 큰따옴표 |
| `UNKNOWN_TABLE` | FROM/JOIN의 테이블이 카탈로그에 없음 | `search_schema`로 올바른 테이블 확인, 스키마-한정 이름 사용 |
| `UNKNOWN_COLUMN` | alias.컬럼이 해당 테이블에 없음 | `get_schema_context`의 컬럼 목록에서 정확한 이름 선택 |
| `PII_COLUMN` | PII 지정 컬럼이 참조됨 | SELECT에서 제거하거나 마스킹(SUBSTR/해시), 집계만 반환 |
| `PII_SELECT_STAR` | PII 보유 테이블에 `SELECT *` | 필요한 컬럼만 명시 |
| `FORBIDDEN_JOIN` | 운영자 금지 조인 조합 | 사유 확인 후 다른 조합/단일 테이블로 재구성 |
| `JOIN_WITHOUT_ON` | JOIN 수 > ON/USING 수 (카티션 위험) | `get_join_paths`의 condition을 ON에 사용 |
| `COMMA_JOIN_NO_WHERE` | 콤마 FROM + WHERE 없음 (카티션) | 명시적 JOIN ... ON으로 변환 |
| `CODE_VALUE_UNKNOWN` | 코드 컬럼 리터럴(=, !=, IN)이 코드사전에 없음 | 힌트의 유효 코드 목록에서 선택 (예: `'77'` → `'00'`) |
| `SCHEMA_HISTORY_PREDICATE_MISMATCH` | `overrides.json`의 `schema_hints` 룰에 `note`가 있는 스키마의 테이블에 `segment_history_column_pairs` 이력 조건(예: start_date/end_date) 적용 | 룰의 `note` 안내를 따름 (해당 스키마의 올바른 기준일 컬럼 사용) — 설정이 없으면 발생하지 않음 |

## 경고 (20종) — 검토 필요

### 방언·구문

| 코드 | 조건 | 조치 |
| --- | --- | --- |
| `DIALECT_FUNCTION` (경고) | 카탈로그 방언과 어긋나는 함수 — postgres에서 MySQL 전용(IFNULL/DATE_FORMAT/CURDATE/STR_TO_DATE/GROUP_CONCAT), mysql/mariadb에서 PostgreSQL 전용(TO_CHAR/TO_DATE/TO_NUMBER/DATE_TRUNC/STRING_AGG/SYSDATE) | 방언 대응 함수로 교체 (IFNULL↔COALESCE, DATE_FORMAT↔TO_CHAR, GROUP_CONCAT↔STRING_AGG 등) |
| `DIALECT_CONCAT` | mysql/mariadb에서 `\|\|` 사용 (기본 모드에서는 논리 OR) | `CONCAT(a, b)` 사용 |
| `UNKNOWN_ALIAS` | 알 수 없는 컬럼 한정자 (CTE/인라인뷰 alias는 제외됨) | 오타 확인 또는 FROM/JOIN에 alias 정의 |
| `AMBIGUOUS_COLUMN` | 한정자 없는 컬럼이 참조 테이블 2곳 이상에 존재 | `T1.컬럼`처럼 alias로 한정 |
| `GROUP_BY_MISMATCH` | SELECT의 비집계 컬럼이 GROUP BY에 없음 | GROUP BY에 추가 또는 집계로 감싸기 |
| `MISSING_GROUP_BY` | 집계+비집계 혼재인데 GROUP BY 없음 | GROUP BY 추가 |

### 정책 필터 (도메인 정책)

이 그룹은 컬럼명이 Go 코드에 하드코딩돼 있지 않고 **`overrides.json`의
도메인 정책 필드로 운영자가 설정**합니다([datasets.md](datasets.md#overrides--overridesjson-json-object)
참조). 해당 필드를 설정하지 않은 데이터셋에서는 발생하지 않습니다.
아래 컬럼명(start_date, is_active 등)은 은행형 설정 예시입니다.

| 코드 | 조건 (설정 필드) | 조치 |
| --- | --- | --- |
| `MISSING_PIT_FILTER` | `segment_history_column_pairs`의 {start,end} 쌍(예: start_date/end_date) 보유 테이블에 시점 조건 없음 | `start_date <= 기준일 AND end_date > 기준일` 추가 |
| `MISSING_VALIDITY_FILTER` | `validity_flag_columns`의 컬럼(예: is_active) 존재하나 미사용 (구 `MISSING_is_active`) | `COALESCE(is_active,'Y') <> 'N'` (유효 행 조회 시) |
| `MISSING_DEL_FILTER` | `soft_delete_columns`의 컬럼(예: status_code) 존재하나 미사용 | `status_code IS NULL` (삭제 제외 시) |
| `MISSING_EXCL_FILTER` | `exclusion_column_prefixes` 접두사 컬럼(예: excl_code*) 존재하나 미사용 | `excl_code_n IS NULL` (분석 제외 반영) |
| `DEFAULT_FILTER_MISSING` | overrides의 기본 필터 누락 | 힌트의 사유 확인 후 조건 추가 |

같은 필드들이 `build_sql_skeleton`에서는 해당 조건을 **자동 주입**하므로,
스켈레톤을 그대로 채우면 이 경고들은 발생하지 않습니다. 오류 표의
`SCHEMA_HISTORY_PREDICATE_MISMATCH`도 `schema_hints`(note 포함) 설정에서
파생됩니다.

### 의미 정합성

| 코드 | 조건 | 조치 |
| --- | --- | --- |
| `DATE_TYPE_MISMATCH` | 문자열형 날짜 컬럼에 DATE 리터럴 (또는 반대) | semantic_type에 맞는 리터럴 형태 사용 |
| `EXPECTED_OUTPUT_MISSING` | `expected_outputs`의 항목이 SELECT에 없음 | 힌트의 후보 컬럼을 SELECT(+GROUP BY)에 포함 |
| `METRIC_MISMATCH` | `metrics`로 지정한 지표의 사전 expression이 SQL에 없음 | 사전 계산식을 그대로 사용 |
| `NO_ROW_BOUND` | row bound 없음 (COUNT 단독 제외) | `bounded_sql` 사용 또는 `LIMIT n` 추가 |

### 조인 신뢰도

| 코드 | 조건 | 조치 |
| --- | --- | --- |
| `UNVERIFIED_JOIN` | 인접 JOIN 테이블 간 카탈로그 경로 없음 — 단, 두 테이블이 `overrides.json` `join_key_candidate_columns`의 공통 컬럼을 가지면 경고 억제 | `get_join_paths` 재확인; 임의 조인 금지 |
| `LOW_CONFIDENCE_JOIN` | 경로는 있으나 confidence < 0.7 | caution/description 검토, 업무 타당성 확인 |

### 학습 룰 (피드백·실행 이력 승격)

| 코드 | 조건 | 조치 |
| --- | --- | --- |
| `LEARNED_TABLE_CORRECTION` | 과거 반복 교정에서 다른 테이블로 교체된 테이블 사용 | 힌트의 대체 테이블 우선 검토 |
| `LEARNED_COLUMN_CORRECTION` | 반복 교체된 컬럼 사용 | 힌트의 대체 컬럼 우선 검토 |
| `LEARNED_RECURRING_ERROR` | 과거 반복 검증 오류 패턴과 동일 대상 사용 | 사용 전 `get_schema_context`로 재확인 |
| `LEARNED_SLOW_QUERY` | 이 테이블 조회가 반복적으로 느림(≥5초, 실행 감사 기반) | 기간/파티션 조건 보강, 실행 전 `explain_sql` 필수 |
| `LEARNED_EXEC_ERROR` | 이 테이블에서 동일 실행 오류(PG-*/MY-*/TIMEOUT) 반복 | 힌트의 코드별 원인(권한/인증/타임아웃) 해소 |

학습 룰의 원천과 승격 조건은 [operations.md](operations.md#피드백-학습-루프)
참조. `learned_rules.json`을 편집해 룰을 다듬거나 삭제할 수 있습니다.

## 생성 전 차단: 테이블 후보 near-tie 재질문 (참고)

검증 이전 단계에도 차단이 있습니다. `prepare_sql_context`(및 질문만으로
호출된 `build_sql_skeleton`의 자동 검색 경로)는 스키마 검색 상위 후보가
**near-tie**(1위 점수의 8% 이내)이면 — domain/grain 메타데이터가 같거나
비어 있어도 예외 없이 — 순위 1위를 임의로 고르지 않고 **blocking
`table_choice` 재질문**을 반환합니다. 동점 후보 전부가 논리명·grain·domain·
설명 라벨과 함께 선택지로 나열되며, `build_sql_skeleton`은 이 경우
스켈레톤 대신 `status=needs_clarification`을 반환합니다. 답변을 받은 뒤
테이블을 명시해 재호출해야 합니다.

## 실행 계층: 방언 AST read-only 가드 (참고)

카탈로그 정적 검증(`validate_sql`)과 **별개로**, 실행 계층(`internal/dbconn`)
에는 SQL을 대상 방언의 실제 문법 파서로 파싱해 read-only SELECT/WITH
단일문만 허용하는 AST 가드가 있습니다 — postgres는 go-pgquery(libpg_query
WASM), mysql/mariadb는 TiDB 파서(MariaDB 모드). 기존 키워드 가드에 더해
Execute/CountRows/Metadata/ExplainPlan과 `POST /api/query/validate`에서
수행됩니다.

- **fail-closed**: 방언 문법으로 파싱되지 않는 SQL은 거부
- 차단하는 우회 클래스: data-modifying CTE(`WITH x AS (DELETE ...) SELECT`),
  `SELECT INTO` / `INTO OUTFILE`/`DUMPFILE`, 잠금 읽기(`FOR UPDATE`,
  `LOCK IN SHARE MODE`), 세션 변수 할당(`@v := ...`), `DO` 블록,
  `PREPARE`/`EXECUTE`, 다중 statement 밀반입

상세는 [db-connector.md](db-connector.md#ast-기반-read-only-검증-방언-파서)
참조.

## explain_sql 리스크 요인 (참고)

검증과 별개로 `explain_sql`은 다음을 정적 점수화합니다:

- WHERE 없음(+25, full scan), row bound 없음(+15), ORDER BY 무제한(+10)
- 상세(grain=상세/…D)·대용량(row_count>1천만) 테이블에 기간 조건 없음(+20)
- 술어에 인덱스 컬럼 없음(+10), 조인 수 ×8, 경고 수 ×12
- 검증 실패 시 `risk: blocked`

`risk_score ≥ 70 → high`: 실행하지 말고 `suggestions`(기간 조건·limit·인덱스
컬럼)를 반영해 재생성하십시오.

## 한계

- 정적 검증입니다. DB 파서가 아니므로 문법 전체를 보장하지 않고, 실행
  결과의 의미(0행, 스케일 오류)는 잡지 못합니다. 문법 보장은 실행 계층의
  방언 AST 가드(위 참조)가, 비용 보장은 DB 프로파일이 있을 때
  `explain_sql`/실행계획 승인 게이트의 실측 EXPLAIN(postgres는
  `EXPLAIN (FORMAT JSON)`, mysql/mariadb는 `EXPLAIN FORMAT=JSON`)이
  보강합니다.
- GROUP BY 검사는 휴리스틱이라 복잡한 표현식에서 과소 탐지될 수 있습니다
  (그래서 경고 수준입니다).
