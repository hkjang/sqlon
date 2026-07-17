# OpenMetadata 연동

jamypg를 [OpenMetadata](https://open-metadata.org)와 양방향 연동해 메타데이터
관리를 자동화합니다. OpenMetadata는 조직이 큐레이션한 업무 메타데이터(설명·
displayName·PII 태그·용어집·오너십)의 중앙 저장소이고, jamypg는 이를 NL2SQL에
활용합니다. 두 시스템을 연결하면 **논리명·설명·PII 분류를 손수 입력하지 않고
전사 카탈로그에서 자동 수급**하고, 반대로 jamypg가 생성한 메타데이터를 다시
채워 넣을 수 있습니다.

## 원칙

- **물리 사실은 자동, 업무 의미는 검토 후 반영** — OpenMetadata의 업무 메타데이터
  는 jamypg에 **후보**로 들어오며 기본은 미리보기입니다. 실제 반영(overrides/
  glossary 병합)은 `apply=true`라는 명시적 두 번째 행위로만 일어나고, 각 파일은
  자동 백업되며 **운영자가 이미 채운 값은 절대 덮어쓰지 않습니다**(빈 필드만).
- **읽기 전용 존중** — export도 OpenMetadata에 **이미 설명이 있는 컬럼은 건드리지
  않고** 빈 필드만 채웁니다. 기본은 dry-run입니다.

## 설정

```sh
export JAMYPG_OPENMETADATA_URL=http://openmetadata:8585   # 콘솔/URL 또는 .../api
export JAMYPG_OPENMETADATA_TOKEN=<bot-jwt>                 # OpenMetadata bot JWT
go run ./cmd/sqlon -data ./data/metadb -addr 127.0.0.1:9797
# 또는 플래그: -openmetadata-url ... -openmetadata-token ...
```

봇 토큰은 OpenMetadata의 *Settings → Bots*에서 발급합니다(예: `ingestion-bot`).

### 런타임 설정 (무재기동)

플래그/환경변수 대신 **관리 콘솔에서 접속 정보를 바로 설정**할 수 있습니다.
`/admin/openmetadata`의 *연결 설정* 또는 REST로 저장하면 `<data>/openmetadata.json`
(`0600`)에 영속되고 즉시 적용됩니다(재기동 불필요). 파일 설정이 플래그/환경변수보다
우선합니다.

```sh
# 저장 (관리자) — token을 비우면 기존 토큰 유지, url을 비우면 설정 삭제(플래그로 복귀)
curl -s -X PUT http://127.0.0.1:9797/api/openmetadata/config \
  -H 'Content-Type: application/json' -d '{"url":"http://openmetadata:8585","token":"<bot-jwt>"}'
# 조회 — 토큰은 has_token 플래그로만 노출(값 미반환)
curl -s http://127.0.0.1:9797/api/openmetadata/config
```

## 관리 콘솔 (curl 없이)

좌측 내비 **🔗 OpenMetadata** (`/admin/openmetadata`)에서 GUI로 운영할 수 있습니다:

- **연결 상태 확인** — 설정·연결·서버 버전 표시
- **Import**: scope/max/용어집 포함 선택 → *미리보기*(후보 테이블) → *반영 + 리로드*
  (확인 다이얼로그, 컬럼 후보·PII 배지·skipped 표시)
- **Export**: *계획(dry-run)* → *실제 반영*(변경 계획·기록 상태 표)
- 하단에 원본 JSON 응답 표시

REST/MCP를 그대로 호출하므로 아래 API와 동작·안전장치가 동일합니다.

## Import (OpenMetadata → jamypg)

OpenMetadata의 테이블/컬럼 메타데이터를 가져와 jamypg 카탈로그의 **빈 필드에만**
후보로 매핑합니다.

| OpenMetadata | → jamypg |
| --- | --- |
| column `displayName` | 컬럼 논리명(logical_name) |
| column `description` | 컬럼 설명(description) |
| column tag `PII.Sensitive` | `pii: true` + semantic_type `PII` |
| table `displayName` / `description` | 테이블 오버라이드 |
| glossaryTerms | glossary.json 용어 |

테이블은 OpenMetadata FQN(`service.database.schema.table`)의 뒤 2개 세그먼트를
jamypg의 `schema.table`로 축약해 매칭합니다. 카탈로그에 없는 테이블은
`skipped_tables`로 보고됩니다.

```sh
# 미리보기 (기본): 후보만 반환, 파일 미변경
curl -s -X POST http://127.0.0.1:9797/api/openmetadata/import \
  -H 'Content-Type: application/json' -d '{"scope":"svc.metadb","max_tables":500}'

# 반영 (관리자): overrides.json/glossary.json 병합 + 백업 + 카탈로그 리로드
curl -s -X POST http://127.0.0.1:9797/api/openmetadata/import \
  -H 'Content-Type: application/json' -d '{"apply":true}'
```

MCP: `import_openmetadata{scope?, max_tables?, include_glossary?, apply?, to_review?}`.
멱등 — 재실행 시 이미 채워진 값은 건너뜁니다.

#### 검토 큐로 스테이징 (`to_review`)

바로 반영(`apply`) 대신 **논리명·설명 gap을 검토 큐로 보내** 담당자가 승인 후
반영하는 경로입니다. OpenMetadata import가 [승인 워크플로](metadata-review.md)와
하나로 이어집니다.

```sh
curl -s -X POST http://127.0.0.1:9797/api/openmetadata/import \
  -H 'Content-Type: application/json' -d '{"to_review":true}'   # 관리자
# → 리뷰 큐(source=openmetadata) 에 스테이징
```

이후 `review_candidates`(또는 `/admin/reviews`)에서 검토 → `decide_candidates`로
승인 → `apply_approved_candidates`로 반영합니다. 스테이징은
`<data>/reviews/imported.json`에 멱등 저장되며, PII·테이블 오버라이드·충돌은 이
경로가 아니라 `apply`/`drift`로 처리합니다.

## Export (jamypg → OpenMetadata)

jamypg가 가진 컬럼 설명(명시적 설명, 없으면 논리명으로 조합)을 OpenMetadata의
**설명이 비어 있는 컬럼**에 JSON Patch로 씁니다.

```sh
# 계획 (기본): 무엇을 쓸지만 반환
curl -s -X POST http://127.0.0.1:9797/api/openmetadata/export \
  -H 'Content-Type: application/json' -d '{"dry_run":true}'

# 실제 반영 (관리자)
curl -s -X POST http://127.0.0.1:9797/api/openmetadata/export \
  -H 'Content-Type: application/json' -d '{"dry_run":false}'
```

MCP: `export_to_openmetadata{scope?, max_tables?, dry_run?}`.

### Lineage export (관계 → OpenMetadata lineage)

`export_lineage_to_openmetadata`(REST `POST /api/openmetadata/lineage`)는 jamypg의
관계 그래프를 OpenMetadata 테이블 lineage 엣지로 push합니다
(`PUT /api/v1/lineage`). **from=참조(부모), to=기준(자식)** 테이블이며, jamypg의
FK 스타일 관계를 OpenMetadata의 **관계형 lineage**로 매핑하는 것입니다(ETL
데이터흐름이 아님). 엔티티 id는 fetch한 테이블 목록에서 해석하고, OpenMetadata에
없는 테이블이 걸린 엣지는 건너뛰고 보고합니다.

```sh
# 계획 (기본)
curl -s -X POST http://127.0.0.1:9797/api/openmetadata/lineage -d '{"dry_run":true}'
# 실제 반영 (관리자)
curl -s -X POST http://127.0.0.1:9797/api/openmetadata/lineage -d '{"dry_run":false}'
```

## Drift — 대조/조정 리포트

`openmetadata_drift`(REST `POST /api/openmetadata/drift`)는 아무것도 쓰지 않고
jamypg와 OpenMetadata의 논리명·설명·PII를 대조해 세 부류로 분류합니다:

| 분류 | 의미 | 조치 |
| --- | --- | --- |
| `jamypg_gap` | jamypg 비어 있음, OM에 값 있음 | **import** 후보 |
| `conflict` | 양쪽 다 값 있고 서로 다름 | 사람이 어느 쪽 채택할지 결정 |
| `ext_gap` | jamypg에 값 있음, OM 비어 있음 | **export** 후보 |

```sh
curl -s -X POST http://127.0.0.1:9797/api/openmetadata/drift \
  -H 'Content-Type: application/json' -d '{"scope":"svc.db"}'
# → { counts:{jamypg_gaps,conflicts,ext_gaps}, jamypg_gaps:[…], conflicts:[…], ext_gaps:[…] }
```

두 카탈로그를 정기적으로 대조해 거버넌스 상 불일치를 조기에 잡고, gap은
import/export로, conflict는 검토로 해소합니다.

## 자동화 (스케줄러 연계)

내장 스케줄러(`-sync-interval`)가 매 틱마다 **DB sync(물리) → OpenMetadata
import(업무 의미, 빈 필드만) → 다이제스트 웹훅(통지)** 순서로 실행합니다(cron
불필요). 설정된 단계만 동작합니다.

```sh
go run ./cmd/sqlon -data ./data/metadb -addr 127.0.0.1:9797 \
  -openmetadata-url http://openmetadata:8585 -openmetadata-token <bot-jwt> \
  -sync-interval 24h \
  -sync-source pg-prod \          # 물리 구조 증분 수집(선택)
  -openmetadata-sync \            # OpenMetadata 업무 메타데이터 자동 import+반영
  -openmetadata-scope svc.db \    # import 스코프(선택)
  -digest-webhook https://hooks.slack.com/...   # 결과 통지(선택)
```

- `-openmetadata-sync`는 매 틱 `import_openmetadata apply`(증분·빈 필드만)를 수행하고
  결과를 로그·감사 로그에 기록합니다. OpenMetadata 미설정 시 조용히 건너뜁니다.
- 각 단계는 독립적이라 원하는 조합만 켤 수 있습니다(예: OM import + 웹훅만).

`openmetadata_status`(MCP/`GET /api/openmetadata/status`)로 연결·인증·버전을
먼저 확인하세요.

## 매핑 세부 / 안전장치

- **PII**: OpenMetadata 기본 분류 `PII.Sensitive`만 민감으로 취급하며
  (`PII.NonSensitive`/`Tier.*`는 제외), import 시 `pii=true`로 표시되어 jamypg의
  PII 마스킹·차단 정책이 자동 적용됩니다.
- **덮어쓰기 금지**: import는 jamypg가 비어 있는 필드에만, export는 OpenMetadata가
  비어 있는 컬럼에만 씁니다. 양쪽의 사람 큐레이션을 보존합니다.
- **백업**: import apply는 `overrides.json`/`glossary.json`을 반영 전
  `<data>/backups/`에 백업합니다.
- **감사**: import apply / export 쓰기는 감사 로그(해시 체인)에 기록됩니다.
