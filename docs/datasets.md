# 데이터셋 가이드 (18종)

서버가 참조하는 모든 JSON 데이터셋의 스키마·작성 규칙·관리 방법입니다.
**라이브 상태는 항상 MCP `list_datasets` 또는 `GET /api/datasets`가
정본**이며, 레지스트리 정의는 `internal/catalog/datasets.go`에 있습니다.

## 관리 방법 4가지 (어느 쪽이든 동작 동일: 백업→검증→핫스왑→실패 시 롤백)

| 방법 | 대상 | 비고 |
| --- | --- | --- |
| 웹 콘솔 `/admin` | JSON 전체 편집 | 사용 가이드 내장 |
| 웹 `/admin/editor` | 표(그리드)로 행/컬럼 단위 편집 | JSON 지식 불필요 |
| MCP `put_dataset` 등 | LLM/에이전트에서 관리 | |
| REST `PUT /api/datasets/{name}` | 스크립트/CI | `-admin-token` 설정 시 토큰 필요 |

파일을 직접 수정(볼륨 마운트 등)한 경우 `reload_catalog`(또는
`POST /api/reload`)를 호출해야 반영됩니다.

## 분류

- **필수** (`required: true`) — 없으면 서버 기동 실패. 제거 불가.
- **선택** — 없으면 기능 축소 또는 내장 기본값 사용.
- **시스템 관리** (`editable: false`) — 서버가 자동 기록. 조회만 가능.

---

## 필수 데이터셋

### physical_models — `meta_physical_models.json`

물리 모델 원장. 카탈로그의 기준 데이터.

```json
[{"schema_name":"dw_history","table_name":"customer_transactions","column_order":"5",
  "column_name":"amount","data_type":"NUMBER","length_precision":"15",
  "null_constraint":"","is_pk":"N","is_fk":"N",
  "description":"[감사] MCP 도구 호출 활동 로그","version":1}]
```

- 컬럼 1행 = JSON 1항목. `is_pk/is_fk`는 `Y/N`.
- `description`의 `[태그]`는 도메인 추론에 사용됩니다.

### logical_models — `meta_logical_models.json`

논리 모델(한글명). **한국어 질문 매핑의 핵심** — 논리명 품질이 검색 정확도를
좌우합니다.

```json
[{"schema_name":"dw_history","entity_name_en":"customer_transactions","entity_name_ko":"IA_카드계좌실적_D",
  "attribute_name_en":"amount","attribute_name_ko":"총이용금액",
  "data_type":"NUMBER","is_pk":"N","is_fk":"N","description":"","note":""}]
```

---

## 스키마 지식 (선택)

### relations — `topology_relations.json`

조인 그래프 엣지. **SQL의 모든 ON 조건의 원천.**

```json
[{"base_schema":"dw_history","base_table":"customers","base_column":"cust_id",
  "reference_schema":"dw_history","reference_table":"customer_transactions","reference_column":"cust_id",
  "cardinality":"1:N","join_type":"INNER","provision_type":"MANUAL",
  "description":"고객 기본정보와 카드 실적을 고객번호로 연결"}]
```

- `provision_type: MANUAL/OPERATOR` → confidence 0.9 + preferred, 그 외 0.6.
- 복합키는 `base_column: "cust_id, STDAY"`처럼 콤마 나열.
- 존재하지 않는 테이블/컬럼을 참조하면 **로드 오류**로 표면화됩니다
  (`get_catalog_health`). 엣지 후보 발굴은 `suggest_joins` 사용.

### code_dict — `meta_code_dict.json`

코드 컬럼의 유효값. 값 리터럴 검증(`CODE_VALUE_UNKNOWN`)과 값→컬럼
매핑(`find_filter_columns`)에 사용.

```json
[{"schema_name":"dw_history","table_name":"transactions","column_name":"D0_TX_STAT_CD",
  "common_division_code":"A2491","code_dict_txt":"00:정상, 09:수정, 89:D0 Prime, 99:삭제"}]
```

`code_dict_txt`는 `코드:라벨` 쌍의 콤마 나열이 원칙입니다. 형식이 일관돼야
값 검증이 "완전 사전"으로 인정됩니다(쌍 2개↑, 파싱률 80%↑).

### indexes — `topology_indexes.json`

```json
[{"schema_name":"dw_history","table_name":"customer_transactions","index_name":"IDX_customer_transactions_cust_id",
  "column_name":"cust_id","seq":1,"index_type":"Non-Unique","description":"조인 최적화"}]
```

### subject_areas — `meta_subject_areas.json`

테이블명 접미사 규칙 → grain 추론 (예: `...D` → 상세, `...S` → 요약).

### databases — `databases.json`

DB 정의. `dbms`(`POSTGRES`/`MYSQL`/`MARIADB`)로 카탈로그 방언 결정(기본
postgres; `overrides.json`의 `dialect`로도 지정 가능). 자격증명은 암호화
상태로 저장.

### prompts — `prompts.json`

MCP `prompts/list`/`prompts/get`으로 노출되는 프롬프트 템플릿.

---

## 사전 (선택 — 정확도의 핵심 레버)

### glossary — `glossary.json` (json-object)

업무 용어/동의어. **검색·질문분해·지표조회·검증이 모두 이 사전 하나를
사용**합니다. 없으면 내장 기본값.

```json
{"entries":[
  {"term":"신용점수","synonyms":["평점","점수","score","lst_score","k-score"],
   "category":"metric"},
  {"term":"고객","synonyms":["회원","차주","cust","cust_id"],"category":"entity"}
]}
```

- `category`: entity | metric | dimension | value | time
- 동의어에 **물리 컬럼명 조각**(`lst_score`)을 넣으면 한국어→컬럼 직결 효과.

### metrics — `metrics.json`

지표 사전. **LLM이 계산식을 만들지 않게 하는 1순위 근거.**

```json
[{"name":"연체율","business_name":"연체 고객 비율",
  "aliases":["연체 비율","delinquency rate"],
  "description":"전체 고객 대비 연체중 고객 비율...",
  "expression":"COUNT(DISTINCT CASE WHEN is_delinquent = 'Y' THEN cust_id END) / NULLIF(COUNT(DISTINCT cust_id), 0)",
  "aggregation":"RATIO",
  "tables":["dw_history.customer_loans"],
  "columns":["is_delinquent","cust_id","start_date","end_date","status_code"],
  "allowed_grains":["customer","agency","month"],
  "recommended_group_by":["TX_AGNC_CD"],
  "required_filters":["start_date <= 기준일 AND end_date > 기준일","status_code IS NULL"],
  "exclusions":["삭제 이력 제외"],
  "null_handling":"is_delinquent IS NULL은 미연체로 간주",
  "dedup_key":"cust_id",
  "example_sql":"SELECT ..."}]
```

작성 규칙:
- `expression`은 **완결된 SQL 조각** — 검증(`METRIC_MISMATCH`)이 문자 그대로
  대조하므로 임의 공백 변형만 허용됩니다.
- `tables`/`columns`는 실존해야 하며 아니면 로드 오류/경고로 표면화.
- `aliases`에 질문에 등장할 표현을 최대한 수집 (검색 +12 부스트의 트리거).

### overrides — `overrides.json` (json-object)

운영자 보정 — 원천 메타를 건드리지 않고 패치.

```json
{"dialect":"postgres",
 "tables":[{"table":"dw_history.customers","domain":"고객","grain":"고객당 1행 (마스터)"}],
 "columns":[{"table":"dw_snapshot.customer_snapshot","column":"credit_score",
             "synonyms":["신용점수","평점"],"pii":false}],
 "pii_columns":["dw_history.customers.cust_name","*.ssn_hash"],
 "forbidden_joins":[{"from_table":"...","to_table":"...","reason":"..."}],
  "preferred_joins":[{"from_table":"dw_history.branches","from_column":"branch_code",
    "to_table":"dw_history.transactions","to_column":"branch_code","cardinality":"N:1",
    "join_type":"INNER","description":"거래지점 연결","confidence":0.95}],
 "default_filters":[{"table":"dw_history.customer_transactions","condition":"status_code IS NULL",
   "reason":"삭제 실적 제외가 기본 정책"}]}
```

- `pii_columns`: `SCHEMA.TABLE.COLUMN` 또는 `*.COLUMN`(전 테이블 와일드카드).
- `preferred_joins`는 조인 그래프에 preferred/0.95로 주입 — `suggest_joins`의
  `suggested_override`를 검토 후 붙여넣는 곳이 여기입니다.
- 중첩 구조가 깊어 **테이블 편집기 대상에서 제외** — `/admin` JSON 편집 사용.

#### 도메인 정책 필드 (과거 하드코딩 → 설정)

과거 Go 소스에 하드코딩돼 있던 은행형 컬럼/스키마 관례(is_active, status_code,
start_date/end_date, dw_history/dw_snapshot 라우팅 등)는 전부 아래 필드로 옮겨졌습니다.
**모두 선택이며 기본값은 빈 값 = 동작 없음** — 일반 데이터셋은 아무 영향이
없고, 도메인 관례가 있는 조직만 설정해서 해당 동작을 되살립니다.

**정책 컬럼** (스켈레톤 자동 주입 + `validate_sql` 경고):

- `validity_flag_columns`: "행 유효" 플래그 컬럼명 목록.
  스켈레톤이 `COALESCE(col,'Y') <> 'N'`을 자동 주입하고, 누락 시
  `MISSING_VALIDITY_FILTER` 경고
- `soft_delete_columns`: non-null이면 소프트 삭제인 컬럼.
  `col IS NULL` 자동 주입, 누락 시 `MISSING_DEL_FILTER`
- `exclusion_column_prefixes`: 이 접두사로 시작하는 컬럼은 분석 제외
  컬럼으로 취급 — `col IS NULL` 자동 주입, 누락 시 `MISSING_EXCL_FILTER`
- `segment_history_column_pairs`: `{start,end}` 시점 이력 윈도우 쌍.
  `start <= 기준일 AND end > 기준일` 자동 주입, 누락 시 `MISSING_PIT_FILTER`
- `join_key_candidate_columns`: 테이블 간 공유되는 자연 조인 키 후보 —
  공통 보유 시 `UNVERIFIED_JOIN` 경고 억제
- `entity_key_columns`: 고유 엔터티 식별자 컬럼 — 이 컬럼을 언급하는
  지표는 `COUNT(DISTINCT col)`이 기본
- `audit_column_names`: 내장 기본(created_at/updated_at/version류) 외에
  조인 키 제안에서 제외할 감사 컬럼 추가

**날짜 컬럼 휴리스틱** (시간 조건 대상 컬럼 선택):

- `well_known_date_columns`: `*_DT`/`*_MON`/`*_DT_TM` 접미사 규칙을 따르지
  않지만 날짜로 인식하고 시간 조건 대상으로 우선할 정확한 컬럼명
- `date_column_exclude_prefixes` / `date_column_exclude_names` /
  `date_column_exclude_substrings`: 날짜형이어도 시간 조건 대상으로 절대
  선택하지 않을 이름 패턴 (이력/감사/속성성 날짜)
- `date_column_eligible_suffixes`: 시간 조건 대상 자격을 주는 추가 접미사

**스키마 힌트·해석**:

- `schema_hints`: `{"keywords":[...], "schemas":[...], "note":"..."}` —
  질문 키워드→스키마 관련성 룰. 매칭 스키마의 검색 점수 부스트(+8),
  `note`는 policy_hints로 표면화되고 `note` 있는 스키마에 다른 스키마의
  이력 조건을 쓰면 `SCHEMA_HISTORY_PREDICATE_MISMATCH` 오류. 룰 그룹이
  2개 이상 매칭되면 `analyze_question`의 ambiguities로 표면화.
  (과거 하드코딩 dw_snapshot/dw_history 키워드 라우팅의 대체)
- `preferred_schema_order`: 같은 테이블명이 여러 스키마에 있을 때
  스키마 미지정 이름을 이 목록의 첫 매칭 스키마로 해석. **미설정이면
  모호한 이름은 not-found로 처리** — 임의 추측 대신 호출자가
  스키마-한정 이름을 다시 지정해야 합니다

은행형 설정 예:

```json
{"validity_flag_columns":["is_active"],
 "soft_delete_columns":["status_code"],
 "exclusion_column_prefixes":["excl_code"],
 "segment_history_column_pairs":[{"start":"start_date","end":"end_date"}],
 "schema_hints":[{"keywords":["평점","점수","score"],"schemas":["dw_snapshot"],
   "note":"dw_snapshot는 스냅샷 스키마 — start_date/end_date 이력 조건 대신 base_month/score_date를 사용"}],
 "preferred_schema_order":["dw_history","dw_snapshot"]}
```

### patterns — `patterns.json`

다단계 SQL 패턴(2단 집계, 그룹별 top-N, 전월/전년 대비, 비율, 분포).
없으면 내장 기본 6종 사용. `keywords`가 질문에 매칭되면
`analyze_question.patterns`와 `build_sql_skeleton`에 템플릿이 실립니다.

```json
[{"name":"two_stage_agg","keywords":["인별로 집계","집계한 후"],
  "description":"...","template":"WITH per_entity AS (...)...",
  "slots":["entity_key","stage1_agg","stage2_agg","table","filters"],
  "caution":"AVG(SUM(x)) 중첩 불가 — CTE로 분리"}]
```

---

## 예제·통계·평가 (선택)

### sql_examples — `sql_datasets.json`

골든 질문-SQL 예제 (few-shot 원천). `target_intent`(파이프 구분)는 intent
시그니처 매칭에, `target_table`은 검색 부스트·골든셋 생성에 사용됩니다.

### column_stats — `column_stats.json`

프로파일 통계. 있으면 `get_column_stats`가 profiled 모드로 동작하고 값→컬럼
매핑·대용량 판단이 정교해집니다. DB 프로파일링 잡으로 주기 생성 권장.

```json
[{"schema_name":"dw_history","table_name":"customers","column_name":"gender",
  "row_count":48000000,"null_ratio":0.001,"distinct_count":2,
  "top_values":[{"value":"1","ratio":0.51,"label":"남자"},
                {"value":"2","ratio":0.49,"label":"여자"}],
  "format_pattern":"D(1)","last_updated":"20260630"}]
```

### golden_queries — `golden_queries.json`

평가 셋. `jamypg-goldgen`으로 재생성하며 상단 수작업 케이스는 `-keep`으로
보존됩니다. 형식·운영은 [evaluation.md](evaluation.md) 참조.

### db_profiles — `db_profiles.json`

대상 DB(`postgres`/`mysql`/`mariadb`) 접속 프로파일. **`/admin/db` 화면 또는
`/api/db-profiles*`로 관리**(개별 CRUD + 접속 테스트)하는 것을 권장하며,
데이터셋 도구로도 편집 가능합니다. 비밀번호는 `password_ref`
(`env:`/`file:`/`plain:`)로만 참조 — 스킴 없는 평문은 저장 거부.
git/도커 이미지에서 제외됩니다. `policy.plan_gate`(기본 `true`) /
`policy.plan_gate_risk`(기본 `high`)로 실행계획 승인 게이트를 제어합니다.

프로파일이 여러 개일 때의 자동 선택(`route_db_profile` /
`run_sql_safely(profile="auto")`)을 위해 프로파일별 `routing` 객체를
선언할 수 있습니다:

```json
{"routing": {"schemas": ["sales"], "tags": ["prod"], "priority": 10,
             "default": false, "discover": true}}
```

- `schemas`: 이 프로파일이 담당하도록 선언된 스키마 목록 — DB가 일시
  다운이어도 라우팅 매칭에 사용
- `tags`: 임의 라벨 — 라우팅 후보 목록에 표면화(점수 미반영)
- `priority`: 1 = 최우선, 100 = 최하위(기본값) — tie-breaker
- `default`: capability 시그널로 가르지 못할 때 선호할 fallback 여부
- `discover`: 기본 `true` — live `information_schema` 인벤토리 프로빙.
  느리거나 비용이 큰 대상은 `false`로 꺼서 선언 `schemas` 매칭만 사용

동봉된 OSS 데모 데이터셋(sakila/northwind/wordpress)의 프로파일들은
`routing.schemas`가 설정되어 있어 라우터가 세 데이터셋의 대상 DB를
구분할 수 있습니다. 상세: [db-connector.md](db-connector.md)의
[프로파일 라우팅](db-connector.md#프로파일-라우팅-여러-프로파일-자동-선택).

### learned_rules — `learned_rules.json`

`learn_from_feedback` 산출물. 운영자가 검토·수정·삭제할 수 있으며 재기동/
호출 시 자동 적용됩니다.

---

## 시스템 관리 (조회 전용)

### feedback — `feedback/feedback-YYYYMMDD.jsonl`

`record_feedback` 저장소. 성공/교정 레코드는 few-shot·검색 부스트·룰 승격의
원천입니다. put/remove 대상이 아닙니다.

### audit — `audit/audit-YYYYMMDD.jsonl`

모든 MCP tool call과 REST 변경의 감사 로그. 자동 기록.

---

## 백업과 복원

- 모든 교체/제거 직전 파일이 `backups/<파일명>.<타임스탬프>`로 저장됩니다.
- 복원(`POST /api/datasets/{name}/restore` 또는 `/admin`의 복원 버튼)은
  **복원 직전의 현재 파일도 백업**하므로 복원 자체를 되돌릴 수 있습니다.
- `backups/`, `feedback/`, `audit/`는 git/도커 이미지에서 제외됩니다 —
  볼륨으로 영속화하세요.

## 새 데이터셋 추가 (개발자)

[development.md](development.md#새-데이터셋-추가) 참조 — 레지스트리
(`datasets.go`)에 항목을 추가하고 로더를 `load.go`에 배선하면 관리 도구·
웹 콘솔·REST가 자동으로 인식합니다.
