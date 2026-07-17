# 프로파일별 카탈로그 워크스페이스

기본 jamypg는 `-data`가 가리키는 **단일 전역 카탈로그** 하나를 NL2SQL에
사용합니다. 이와 별개로, 등록된 **DB 프로파일마다 독립적인 카탈로그 메타데이터
JSON 세트**를 `<data>/profiles/<profile>/` 아래에 두고 조회·관리할 수 있습니다.
연결된 DB별로 물리/논리 모델·오버라이드·용어집·지표 등을 따로 구축·관리하는
용도입니다.

## 디렉터리 구조

```
<data>/                         # 전역(활성) 카탈로그 — NL2SQL 기본
  meta_physical_models.json
  overrides.json  ...
  profiles/
    pg-prod/                    # 프로파일 pg-prod 의 워크스페이스
      meta_physical_models.json
      meta_logical_models.json
      overrides.json  glossary.json  metrics.json ...
    mysql-dw/
      ...
```

워크스페이스는 `.gitignore`(`data/*/profiles/`) 대상이라 커밋되지 않습니다.

## 도구 / API

| 작업 | MCP 도구 | REST |
| --- | --- | --- |
| 프로파일별 워크스페이스 목록 | `list_profile_catalogs` | `GET /api/profile-catalogs` |
| 워크스페이스 조회(요약·데이터셋·헬스) | `get_profile_catalog` | `GET /api/profile-catalogs/{profile}` |
| 라이브 DB로 구축/갱신 (관리자) | `build_profile_catalog` | `POST /api/profile-catalogs/{profile}/build` |
| 데이터셋 JSON 조회 | `get_profile_dataset` | `GET /api/profile-catalogs/{profile}/dataset/{name}` |
| 데이터셋 JSON 관리 (관리자) | `put_profile_dataset` | `PUT /api/profile-catalogs/{profile}/dataset/{name}` |

## 사용 흐름

```sh
# 1) 프로파일의 라이브 스키마로 워크스페이스 구축 (물리 모델 + FK 관계)
curl -s -X POST http://127.0.0.1:9797/api/profile-catalogs/pg-prod/build \
  -H 'Content-Type: application/json' -d '{}'            # 관리자

# 2) 조회 — 카탈로그 요약, 데이터셋 인벤토리(파일별 건수/이슈), 헬스
curl -s http://127.0.0.1:9797/api/profile-catalogs/pg-prod

# 3) 업무 메타데이터 관리 — 논리명(overrides)/용어집/지표 등 JSON 편집
curl -s -X PUT http://127.0.0.1:9797/api/profile-catalogs/pg-prod/dataset/overrides \
  -H 'Content-Type: application/json' \
  -d '{"columns":[{"table":"public.orders","column":"id","logical_name":"주문번호"}]}'   # 관리자
```

## 원칙 / 안전장치

- **물리는 자동, 업무 의미는 보존**: `build_profile_catalog`는 라이브 스키마의
  물리 모델을 워크스페이스에 병합하되, 워크스페이스에 이미 있는 설명은
  덮어쓰지 않고(신규만 DB 코멘트로 채움) 삭제분은 폐기 후보로만 표시합니다
  (`prune=true` 시에만 제거). 내부적으로
  [메타데이터 싱크 반영](metadata-sync.md)과 같은 로직을 씁니다.
- **검증·백업·롤백**: `put_profile_dataset`은 데이터셋 스키마를 검증하고, 반영
  전 파일을 백업하며, 워크스페이스가 컴파일되지 않으면 자동 롤백합니다.
- **권한**: 조회는 읽기 전용, 구축·쓰기는 관리자(+프로파일 ACL)만 가능합니다.

## 활성 카탈로그 전환

### 무재기동 핫스왑 (`set_active_catalog`, 관리자·단독 모드)

`set_active_catalog{profile}`(REST `POST /api/profile-catalogs/active`)로 재기동
없이 활성 NL2SQL 카탈로그를 프로파일 워크스페이스로 전환합니다. 이후 검색·
`prepare_sql_context`·검증이 그 워크스페이스 메타데이터를 사용합니다. `profile`을
비우면 기본(`-data`) 카탈로그로 되돌립니다.

```sh
curl -s -X POST http://127.0.0.1:9797/api/profile-catalogs/active \
  -H 'Content-Type: application/json' -d '{"profile":"pg-prod"}'    # 관리자
curl -s http://127.0.0.1:9797/api/profile-catalogs/active           # 현재 활성 조회
```

**중요 — 운영 데이터는 고정**: 전환은 NL2SQL 카탈로그(테이블/검색/조인/검증)만
바꿉니다. DB 프로파일 레지스트리·쿼리 실행(`run_sql_safely`)·감사 로그·프로파일
워크스페이스는 부팅 시 `-data`에 고정된 **운영 디렉터리**에 그대로 남아 전환의
영향을 받지 않습니다. 전환은 프로세스 메모리 상태이며 **재기동 시 `-data`로
복귀**합니다. 데이터셋을 Postgres에서 관리하는 메타 DB 모드에서는 지원하지
않습니다.

### 영구 승격 (기동 시)

전환을 영구화하려면 서버를 해당 워크스페이스로 기동합니다:

```sh
go run ./cmd/sqlon -data ./data/metadb/profiles/pg-prod -addr 127.0.0.1:9797
```
