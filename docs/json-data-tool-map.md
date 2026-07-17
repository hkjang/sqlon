# JSON Data to MCP Tool Map

> **참고 (레거시)**: 이 문서는 초기 구현(v0.1) 기준의 정적 스냅샷입니다.
> 최신 데이터셋 가이드는 **[datasets.md](datasets.md)**, 라이브 상태는
> MCP `list_datasets` 도구 또는 웹 콘솔 `/admin`을 사용하세요. 레지스트리
> 정의는 `internal/catalog/datasets.go`에 있습니다. 문서 전체 목록:
> [README.md](README.md)

이 문서는 현재 `main` 브랜치 구현 기준으로 `data/metadb` 아래 JSON 데이터가 어떤 로더에서 읽히고, 어떤 MCP 도구/리소스/프롬프트에서 사용되는지 정리합니다.

대상 구현:

- 서버 진입점: `cmd/jamypg-mcp/main.go`
- 카탈로그 로더: `internal/catalog/load.go`
- MCP 도구/리소스/프롬프트 핸들러: `internal/mcp/server.go`
- stdio/http transport는 같은 MCP 핸들러를 공유하므로 데이터 사용 방식은 동일합니다.

## 1. 전체 로딩 흐름

서버 시작 시 `catalog.Load(dataDir)`가 `data/metadb` 폴더를 읽어 메모리 카탈로그를 만듭니다.

필수 로딩 파일:

| JSON 파일 | 로더 함수 | 실패 시 |
|---|---|---|
| `meta_physical_models.json` | `loadPhysical` | 서버 시작 실패 |
| `meta_logical_models.json` | `loadLogical` | 서버 시작 실패 |

선택 로딩 파일:

| JSON 파일 | 로더/대상 필드 | 실패 시 |
|---|---|---|
| `meta_code_dict.json` | `loadCodeDict` | 무시하고 계속 시작 |
| `topology_indexes.json` | `loadIndexes` | 무시하고 계속 시작 |
| `topology_relations.json` | `loadRelations` | 무시하고 계속 시작 |
| `sql_datasets.json` | `Catalog.Samples` | 무시하고 계속 시작 |
| `meta_subject_areas.json` | `Catalog.Subjects` | 무시하고 계속 시작 |
| `prompts.json` | `Catalog.Prompts` | 무시하고 계속 시작 |
| `databases.json` | `Catalog.Databases` | 무시하고 계속 시작 |

컴파일 결과는 주로 아래 내부 모델에 저장됩니다.

| 내부 모델 | 주요 원천 JSON | 용도 |
|---|---|---|
| `Catalog.Tables` | `meta_physical_models.json`, `meta_logical_models.json`, `meta_code_dict.json`, `topology_indexes.json` | 테이블/컬럼 검색, SQL 검증, 스키마 컨텍스트 생성 |
| `Catalog.ByName` | 컴파일된 `Catalog.Tables` | 스키마 없는 테이블명 해석 |
| `Table.SearchText` | 테이블명, 논리명, 설명, 컬럼명, 코드사전 | `search_schema` 점수 계산 |
| `Catalog.Relations` | `topology_relations.json` | 조인 그래프와 리소스 노출 |
| `Catalog.Adjacency` | `topology_relations.json` | `get_join_paths`, 조인 검증 경고 |
| `Catalog.Samples` | `sql_datasets.json` | few-shot 검색, 스키마 검색 보정 |
| `Catalog.Prompts` | `prompts.json` | MCP prompt 목록/조회 |

## 2. JSON 파일별 상세 사용처

### `meta_physical_models.json`

물리 스키마의 기준 데이터입니다. 서버 시작 시 반드시 필요합니다.

읽는 코드:

- `internal/catalog/load.go`
- `catalog.Load`
- `loadPhysical`

주요 입력 필드:

| JSON 필드 | 내부 반영 위치 |
|---|---|
| `schema_name` | `Table.Schema` |
| `table_name` | `Table.Name`, `Table.FQN` |
| `column_name` | `Column.Name` |
| `column_order` | `Column.Order` |
| `data_type` | `Column.DataType` |
| `length_precision` | `Column.LengthPrecision` |
| `null_constraint` | `Column.Nullable` |
| `is_pk` | `Column.IsPK`, `Table.PrimaryKeys` |
| `is_fk` | `Column.IsFK`, `Table.ForeignKeys` |
| `description` | `Table.Description`, 컬럼 설명 fallback |
| `version` | `Table.Version` |

직접/간접 사용 도구:

| MCP 도구 | 사용 방식 |
|---|---|
| `search_schema` | 테이블명, 컬럼명, 타입, PK/FK, 설명 기반 검색 |
| `get_schema_context` | SQL 생성용 테이블/컬럼 정의 반환 |
| `get_column_stats` | 컬럼 타입, 길이, null 제약, PK/FK 반환 |
| `validate_sql` | SQL에 나온 테이블/컬럼 존재 여부 검증 |
| `explain_sql` | `validate_sql` 결과와 인덱스 힌트 기반 정적 위험 평가 |
| `run_sql_safely` | 실행 전 `validate_sql` 호출 |
| `get_metric_definition` | 컬럼명/타입/설명 기반 지표 후보 추론 |
| `analyze_question` | 내부적으로 `search_schema`를 호출하므로 간접 사용 |

리소스 사용처:

| MCP 리소스 | 사용 방식 |
|---|---|
| `metadata://catalog/summary` | 테이블/컬럼 수 계산 |
| `metadata://catalog/tables` | 테이블 목록과 컬럼 수, PK/FK 반환 |
| `metadata://catalog/table/{schema}.{table}` | 특정 테이블 상세 반환 |

### `meta_logical_models.json`

논리명과 업무 설명을 물리 모델에 보강합니다. 서버 시작 시 반드시 필요합니다.

읽는 코드:

- `internal/catalog/load.go`
- `catalog.Load`
- `loadLogical`

주요 입력 필드:

| JSON 필드 | 내부 반영 위치 |
|---|---|
| `schema_name` | 테이블 resolve key |
| `entity_name_en` | 물리 테이블명 매칭 |
| `entity_name_ko` | `Table.LogicalName` |
| `attribute_name_en` | 물리 컬럼명 매칭 |
| `attribute_name_ko` | `Column.LogicalName` |
| `description` | `Column.Description` |
| `note` | `Column.Note` |
| `is_pk`, `is_fk` | `Column.IsPK`, `Column.IsFK` 보강 |
| `entity_order` | `Column.Order` 보강 |
| `version` | `Table.Version` 보강 |

직접/간접 사용 도구:

| MCP 도구 | 사용 방식 |
|---|---|
| `search_schema` | 한글 논리 테이블명/컬럼명/업무 설명 검색 |
| `get_schema_context` | SQL 생성용 업무명과 설명 반환 |
| `get_metric_definition` | 지표명과 논리 컬럼명 매칭 |
| `get_column_stats` | 컬럼 논리명/설명 반환 |
| `validate_sql` | 물리 컬럼 존재 검증은 물리명 기준이지만, 힌트/컨텍스트에 논리 설명 제공 |
| `analyze_question` | `search_schema` 간접 호출 |

리소스 사용처:

| MCP 리소스 | 사용 방식 |
|---|---|
| `metadata://catalog/tables` | 논리 테이블명 반환 |
| `metadata://catalog/table/{schema}.{table}` | 논리 테이블명/논리 컬럼명 포함 상세 반환 |

### `meta_code_dict.json`

코드 컬럼의 값 사전을 컬럼 메타에 붙입니다.

읽는 코드:

- `internal/catalog/load.go`
- `catalog.Load`
- `loadCodeDict`

주요 입력 필드:

| JSON 필드 | 내부 반영 위치 |
|---|---|
| `schema_name` | 테이블 매칭 |
| `table_name` | 테이블 매칭 |
| `column_name` | 컬럼 매칭 |
| `common_division_code` | `Column.CommonCode` |
| `code_dict_txt` | `Column.CodeDict` |

직접/간접 사용 도구:

| MCP 도구 | 사용 방식 |
|---|---|
| `search_schema` | 코드값/코드명 텍스트가 질문과 매칭되면 컬럼 점수 증가 |
| `get_schema_context` | SQL 생성 컨텍스트에 `code_dict` 포함 |
| `get_column_stats` | `common_code`, `code_dict` 반환 |
| `get_metric_definition` | 컬럼 스코어링 시 코드사전 텍스트도 검색 대상 |
| `analyze_question` | `search_schema` 간접 호출 |

현재 `validate_sql`은 코드값의 업무 의미까지 검증하지는 않습니다. 컬럼 존재와 정책 중심의 정적 검증만 수행합니다.

### `topology_indexes.json`

인덱스 정보를 테이블/컬럼 메타에 붙입니다.

읽는 코드:

- `internal/catalog/load.go`
- `catalog.Load`
- `loadIndexes`

주요 입력 필드:

| JSON 필드 | 내부 반영 위치 |
|---|---|
| `schema_name` | 테이블 매칭 |
| `table_name` | 테이블 매칭 |
| `index_name` | `Table.Indexes[].IndexName` |
| `column_name` | `Table.Indexes[].ColumnName`, `Column.Indexed` |
| `seq` | `Table.Indexes[].Seq` |
| `index_type` | `Table.Indexes[].IndexType` |
| `description` | `Table.Indexes[].Description` |

직접/간접 사용 도구:

| MCP 도구 | 사용 방식 |
|---|---|
| `search_schema` | 인덱스 컬럼에 약간의 점수 보정 |
| `get_schema_context` | 테이블별 `indexes` 배열 반환 |
| `get_column_stats` | `indexed` 여부 반환 |
| `explain_sql` | SQL에 인덱스 컬럼명이 포함되면 `index_hints` 반환 |

리소스 사용처:

| MCP 리소스 | 사용 방식 |
|---|---|
| `metadata://catalog/table/{schema}.{table}` | 테이블 상세에 인덱스 배열 포함 |

### `topology_relations.json`

테이블 간 조인 관계의 기준 데이터입니다.

읽는 코드:

- `internal/catalog/load.go`
- `catalog.Load`
- `loadRelations`

주요 입력 필드:

| JSON 필드 | 내부 반영 위치 |
|---|---|
| `base_schema`, `base_table`, `base_column` | 조인 edge 출발 테이블/컬럼 |
| `reference_schema`, `reference_table`, `reference_column` | 조인 edge 대상 테이블/컬럼 |
| `cardinality` | `Relation.Cardinality` |
| `join_type` | `Relation.JoinType` |
| `provision_type` | `Relation.ProvisionType` |
| `description` | `Relation.Description` |
| `meta_version` | `Relation.MetaVersion` |

컴파일 결과:

- `Catalog.Relations`
- `Catalog.Adjacency`
- 양방향 edge 생성

직접/간접 사용 도구:

| MCP 도구 | 사용 방식 |
|---|---|
| `get_join_paths` | BFS로 테이블 간 추천 조인 경로 탐색 |
| `validate_sql` | 인접 JOIN 테이블 사이에 관계 경로가 없으면 경고 |
| `explain_sql` | `validate_sql` 결과를 포함하므로 조인 경고 간접 포함 |
| `run_sql_safely` | `validate_sql` 간접 호출 |

리소스 사용처:

| MCP 리소스 | 사용 방식 |
|---|---|
| `metadata://catalog/relations` | 전체 관계 배열 반환 |
| `metadata://catalog/summary` | 관계 수 반환 |

### `sql_datasets.json`

질문-정답 SQL few-shot 예제입니다.

읽는 코드:

- `internal/catalog/load.go`
- `catalog.Load`
- `readJSON(..., &Catalog.Samples)`

주요 입력 필드:

| JSON 필드 | 내부 반영 위치 |
|---|---|
| `question` | `Sample.Question` |
| `target_sql` | `Sample.TargetSQL` |
| `target_domain` | `Sample.TargetDomain` |
| `target_table` | `Sample.TargetTable` |
| `target_column` | `Sample.TargetColumn` |
| `target_intent` | `Sample.TargetIntent` |
| `target_difficulty` | `Sample.TargetDifficulty` |

직접/간접 사용 도구:

| MCP 도구 | 사용 방식 |
|---|---|
| `search_examples` | 질문과 유사한 few-shot 예제 반환 |
| `search_schema` | 예제의 `target_table`이 현재 테이블과 일치하면 검색 점수 보정 |
| `analyze_question` | `SearchSamples`로 `fewshot_hits` 반환 |
| `get_column_stats` | 해당 컬럼이 예제 SQL/타깃 컬럼에 등장한 횟수 계산 |
| `get_schema_context` | 테이블 자동 선택 시 `search_schema`를 통해 간접 사용 |

리소스 사용처:

| MCP 리소스 | 사용 방식 |
|---|---|
| `metadata://catalog/examples` | 앞 100개 예제 반환 |
| `metadata://catalog/summary` | 예제 수 반환 |

### `meta_subject_areas.json`

주제영역/테이블 명명 규칙 데이터입니다.

읽는 코드:

- `internal/catalog/load.go`
- `catalog.Load`
- `readJSON(..., &Catalog.Subjects)`

현재 상태:

| 항목 | 상태 |
|---|---|
| 서버 시작 시 로드 | 예 |
| MCP 도구에서 직접 사용 | 아니오 |
| MCP 리소스로 직접 노출 | 아니오 |
| SQL 검증에 사용 | 아니오 |

참고: `Catalog.Subjects`에는 저장되지만 현재 `search_schema`, `analyze_question`, `prompts/get` 등에서 직접 참조하지 않습니다. 향후 dw_history 주제영역 라우팅 점수에 연결할 수 있습니다.

### `prompts.json`

저장된 SQL 프롬프트 템플릿입니다.

읽는 코드:

- `internal/catalog/load.go`
- `catalog.Load`
- `readJSON(..., &Catalog.Prompts)`

주요 입력 필드:

| JSON 필드 | 내부 반영 위치 |
|---|---|
| `name` | `PromptDef.Name` |
| `role` | `PromptDef.Role` |
| `category` | `PromptDef.Category` |
| `content` | `PromptDef.Content` |
| `description` | `PromptDef.Description` |
| `is_active` | `PromptDef.IsActive` |

MCP prompt 사용처:

| MCP 메서드 | 사용 방식 |
|---|---|
| `prompts/list` | 활성 SQL prompt를 `ds_...` 이름으로 목록에 추가 |
| `prompts/get` | 저장 prompt content 반환, `{argument}` placeholder 치환 |

리소스 사용처:

| MCP 리소스 | 사용 방식 |
|---|---|
| `metadata://catalog/prompts` | 저장 prompt의 이름, role, category, 설명, 활성 여부 반환 |

주의: 현재 도구 호출에서 `prompts.json`을 자동으로 SQL 생성에 주입하지는 않습니다. MCP 클라이언트가 `prompts/list`/`prompts/get`으로 명시적으로 가져가서 사용합니다.

### `databases.json`

DBMS 종류/연결 설정 데이터입니다. `dbms` 값(POSTGRES/MYSQL/MARIADB)이
카탈로그 방언(생성/검증 SQL dialect)을 결정합니다.

읽는 코드:

- `internal/catalog/load.go`
- `catalog.Load`
- `readJSON(..., &Catalog.Databases)`

현재 상태:

| 항목 | 상태 |
|---|---|
| 서버 시작 시 로드 | 예 |
| 카탈로그 방언(`Catalog.Dialect`) 결정 | 예 — `dbms` 값 사용, 미지정 시 postgres (overrides.json `dialect`로 재정의 가능) |
| MCP 리소스로 직접 노출 | 아니오 |
| 실제 DB 접속에 사용 | 아니오 |

주의:

- 이 파일로는 DB에 접속하지 않습니다. 실제 접속은 `db_profiles.json`의
  프로파일(postgres/mysql/mariadb, 순수 Go 드라이버 pgx·go-sql-driver/mysql)로
  수행됩니다.
- `run_sql_safely`는 profile 미지정 시 dry-run guard이며, profile 지정 시
  대상 DB에서 read-only로 실제 실행합니다.
- `databases.json`의 host/user/password 등은 노출하지 않습니다.

## 3. MCP 도구별 데이터 사용 매트릭스

| MCP 도구 | 직접 사용하는 JSON | 간접 사용하는 JSON | 설명 |
|---|---|---|---|
| `analyze_question` | 없음 | `meta_physical_models.json`, `meta_logical_models.json`, `meta_code_dict.json`, `topology_indexes.json`, `sql_datasets.json` | 질문 휴리스틱 분석 후 `SearchSchema`, `SearchSamples` 호출 |
| `search_schema` | `meta_physical_models.json`, `meta_logical_models.json`, `meta_code_dict.json`, `topology_indexes.json`, `sql_datasets.json` | 없음 | 테이블/컬럼 검색, few-shot 타깃 테이블 점수 보정 |
| `get_schema_context` | `meta_physical_models.json`, `meta_logical_models.json`, `meta_code_dict.json`, `topology_indexes.json` | `sql_datasets.json` | tables가 없으면 `search_schema`로 자동 선택하므로 few-shot 보정이 간접 적용 |
| `get_join_paths` | `topology_relations.json` | `meta_physical_models.json`, `meta_logical_models.json` | 테이블명 resolve 후 관계 그래프 탐색 |
| `get_metric_definition` | `meta_physical_models.json`, `meta_logical_models.json`, `meta_code_dict.json` | 없음 | 컬럼명/논리명/설명/코드사전 기반 지표 후보 추론 |
| `get_column_stats` | `meta_physical_models.json`, `meta_logical_models.json`, `meta_code_dict.json`, `topology_indexes.json`, `sql_datasets.json` | 없음 | 컬럼 메타와 예제 사용 횟수 반환 |
| `search_examples` | `sql_datasets.json` | 없음 | few-shot 예제 검색 |
| `validate_sql` | `meta_physical_models.json`, `meta_logical_models.json`, `topology_relations.json` | `databases.json` (방언 결정) | 테이블/컬럼 존재, 금지 SQL, 필수 필터, 방언 검사, 조인 경고 |
| `explain_sql` | `topology_indexes.json` | `validate_sql`이 쓰는 모든 JSON | 정적 위험 평가와 인덱스 힌트 (profile 지정 시 실측 EXPLAIN 추가) |
| `run_sql_safely` | 없음 | `validate_sql`이 쓰는 모든 JSON | profile 미지정 시 검증 결과와 bounded SQL 반환(dry-run), 지정 시 대상 DB read-only 실행 |
| `record_feedback` | 없음 | 없음 | `data/metadb/feedback/*.jsonl`에 새 피드백 기록 |

## 4. MCP 리소스별 데이터 사용 매트릭스

| MCP 리소스 URI | 사용하는 JSON | 반환 내용 |
|---|---|---|
| `metadata://catalog/summary` | 로드된 전체 카탈로그 상태 | 테이블 수, 컬럼 수, 관계 수, 예제 수, 스키마별 테이블 수 |
| `metadata://catalog/tables` | `meta_physical_models.json`, `meta_logical_models.json` | 테이블 목록, 논리명, 설명, 컬럼 수, PK/FK |
| `metadata://catalog/relations` | `topology_relations.json` | 전체 조인 관계 |
| `metadata://catalog/policies` | JSON 원천 없음 | 코드에 내장된 read-only, blocked keyword, default limit 정책 |
| `metadata://catalog/prompts` | `prompts.json` | prompt 메타 목록 |
| `metadata://catalog/examples` | `sql_datasets.json` | 앞 100개 few-shot 예제 |
| `metadata://catalog/table/{schema}.{table}` | `meta_physical_models.json`, `meta_logical_models.json`, `meta_code_dict.json`, `topology_indexes.json` | 특정 테이블 상세 메타 |

## 5. MCP 프롬프트별 데이터 사용 매트릭스

| MCP prompt | 사용하는 JSON | 설명 |
|---|---|---|
| `text2sql_workflow` | 없음 | 코드에 내장된 워크플로우 프롬프트 |
| `db_sql_generation` | 없음 | 코드에 내장된 SQL 생성 프롬프트, 인자로 schema/join/examples context를 받음 |
| `ds_{prompts.json의 name}` | `prompts.json` | 저장 prompt content를 MCP prompt로 반환 |

## 6. 현재 폴더에 있지만 런타임에서 읽지 않는 파일

아래 파일들은 현재 서버 코드에서 읽지 않습니다. 즉, 도구 결과에 직접 반영되지 않습니다.

| 파일 | 현재 상태 | 비고 |
|---|---|---|
| `embedding_settings.json` | 미사용 | 외부 embedding/RAG 설정으로 보이나 현재 Go 서버는 로컬 lexical search만 사용 |
| `llm_model_servers.json` | 미사용 | LLM 서버 설정으로 보이나 MCP 서버는 LLM을 직접 호출하지 않음 |
| `prompt_variables.json` | 미사용 | prompt 변수 생성 스크립트로 보이나 현재 `prompts.json` placeholder 치환만 지원 |
| `topology_indexes_20260421.json` | 미사용 | 버전 백업/스냅샷 파일 |
| `topology_indexes_full_20260512.json` | 미사용 | 버전 백업/스냅샷 파일 |
| `topology_relations_20260421.json` | 미사용 | 버전 백업/스냅샷 파일 |
| `topology_relations_full_20260512.json` | 미사용 | 버전 백업/스냅샷 파일 |
| `COMMON/user_logical.py` | 미사용 | 참고 스크립트 |
| `COMMON/user_logical.json` | 미사용 | 참고 메타 |
| `COMMON/user_entities.sql` | 미사용 | 참고 SQL |
| `COMMON/user_entities.json` | 미사용 | 참고 메타 |
| `COMMON/dw_history_user_cot.txt` | 미사용 | 참고 프롬프트/CoT 자료 |
| `COMMON/dw_history_user_cot.json` | 미사용 | 참고 프롬프트/CoT 자료 |
| `COMMON/dw_history_score_v2.txt` | 미사용 | 참고 프롬프트 자료 |
| `COMMON/dw_history_score_cot.txt` | 미사용 | 참고 프롬프트/CoT 자료 |

## 7. 도구 호출 관점의 추천 순서

NL2SQL 클라이언트는 아래 순서로 호출하는 것이 현재 데이터 구조를 가장 잘 활용합니다.

1. `analyze_question`
   - 질문 의도, 스키마 힌트, 상위 스키마 후보, few-shot 후보 확인
2. `search_schema`
   - 관련 테이블/컬럼 후보 압축
3. `search_examples`
   - 유사 정답 SQL 패턴 확인
4. `get_schema_context`
   - SQL 생성에 필요한 최소 컬럼/정책 힌트 확보
5. `get_join_paths`
   - 두 개 이상 테이블이 필요할 때 조인 조건 확보
6. `validate_sql`
   - 생성 SQL의 테이블/컬럼/정책/조인 위험 검증
7. `explain_sql`
   - 정적 비용/위험 힌트와 인덱스 힌트 확인
8. `record_feedback`
   - 성공/실패/수정 SQL을 `data/metadb/feedback/*.jsonl`에 축적

## 8. 주의사항

- 현재 검색은 embedding 서버를 호출하지 않습니다. `embedding_settings.json`은 읽지 않습니다.
- 현재 서버는 LLM을 직접 호출하지 않습니다. `llm_model_servers.json`은 읽지 않습니다.
- 실제 DB 접속(postgres/mysql/mariadb)은 `db_profiles.json`의 프로파일로 수행합니다. `databases.json`은 `dbms` 값으로 카탈로그 방언을 결정하는 데만 쓰이며 도구/리소스로 노출되지 않습니다.
- `topology_indexes.json`, `topology_relations.json`만 런타임 기준 파일입니다. 날짜가 붙은 full/version 파일은 읽지 않습니다.
- `record_feedback`만 런타임에 새 파일을 씁니다. 위치는 `data/metadb/feedback/feedback-YYYYMMDD.jsonl`입니다.
