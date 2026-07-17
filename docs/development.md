# 개발자 가이드

## 빌드·테스트

```sh
go build ./...                     # 전체 빌드 (순수 Go — pgx·go-sql-driver/mysql, CGO 없음)
go vet ./...
go test ./...                      # 골든셋 평가 포함 (~17초)
go test ./internal/mcp/ -run TestAdmin -v   # 부분 실행

# 실행
go run ./cmd/jamypg-mcp -transport http -data ./data/metadb
go run ./cmd/jamypg-eval -verbose
go run ./cmd/jamypg-goldgen -n 80

# 통합 테스트 (postgres:16 / mysql:8.4 / mariadb:11.4 실 DB —
# deploy/test/gen_testenv.py가 생성한 환경)
docker compose -f deploy/test/docker-compose.yml up -d
go test -tags integration ./test/integration -v

# 크로스 빌드 (정적, CGO_ENABLED=0 — scripts/build.sh가 windows-amd64 /
# linux-amd64 / linux-arm64 3종을 생성)
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/jamypg-mcp-windows-amd64.exe ./cmd/jamypg-mcp
docker build -t jamypg-mcp:dev .
```

Go 1.25+ (ServeMux 메서드/와일드카드 패턴, `atomic.Pointer` 사용).
빌드 태그는 통합 테스트용 `integration`뿐입니다 — DB 드라이버는 항상
컴파일에 포함되며 조건부 빌드(구 `-tags oracle`)는 없습니다.

## 코드 구조

```text
cmd/
  jamypg-mcp/      서버 진입점 (플래그, 기동 로그)
  jamypg-eval/     평가 CLI
  jamypg-goldgen/  골든셋 생성 CLI
internal/catalog/  도메인 로직 전부 (mcp에 비의존, DB 비연결)
internal/dbconn/   읽기 전용 DB 커넥터 (postgres/mysql/mariadb — 순수 Go)
internal/meta/     메타 DB(PostgreSQL) 저장소 — 사용자/MCP 키/프로파일/
                   데이터셋 동기화 (jamypg_datasets 등 jamypg_* 테이블)
internal/mcp/      트랜스포트·도구 디스패치·REST·웹 UI
  webui/           go:embed 정적 자산 (admin/editor/docs/swagger)
data/metadb/          데이터셋 (테스트도 이 실데이터를 사용)
deploy/test/       통합 테스트용 3-DB docker-compose (gen_testenv.py 생성물)
docs/              본 문서들
```

의존 방향: `cmd → internal/mcp → internal/catalog · internal/dbconn ·
internal/meta`. catalog는 mcp를 모릅니다.

### 파일별 책임 (internal/catalog)

[architecture.md](architecture.md)의 다이어그램 참조. 대략적 규칙:

- **컴파일**: types.go(모델) / load.go(로더·추론) / overrides.go / stats.go /
  datasets.go(레지스트리·파일 관리)
- **조회**: search.go / analyze.go / join.go / metrics.go / glossary.go /
  patterns.go / timeparse.go / skeleton.go / stats.go
- **판정**: validate.go / rank.go / health.go / eval.go
- **환류**: feedback.go / learn.go / joinsuggest.go

DB 실행 경로(프로파일, SQL 가드, 방언별 DSN/LIMIT, 실 EXPLAIN, 오류 코드)는
`internal/dbconn` 소관입니다 — [db-connector.md](db-connector.md) 참조.

### 테스트 배치

| 파일 | 커버 |
| --- | --- |
| catalog_test.go | 로드·검색·조인·검증 기본 |
| eval_test.go | 골든셋 게이트(임계값), 지표 사전, 시간 파싱, PII/방언, 헬스 |
| complex_test.go | intent 시그니처, 코드값, 결과 스키마, CTE 스코프, 패턴, 스켈레톤 |
| learn_test.go | 룰 승격·패널티·경고 (임시 디렉터리 픽스처) |
| rank_test.go | 후보 랭킹, 조인 제안 |
| internal/dbconn/dbconn_test.go | SQL 가드, 방언별 DSN 조립, LIMIT 래핑, 오류 코드 |
| internal/mcp/stdio_test.go | stdio 왕복, **도구 수 단정(현재 28)** |
| internal/mcp/server_test.go | 세션 관대 정책 |
| internal/mcp/datasets_test.go | 데이터셋 도구 수명주기 (픽스처 데이터 디렉터리) |
| internal/mcp/admin_test.go | REST 수명주기, 토큰, 정적 페이지 |
| test/integration/ | 실 DB 3종 왕복 (`-tags integration`, compose 필요) |

관례: 실데이터 검증은 `loadTestCatalog`(data/metadb), **변경 동작 테스트는
반드시 `t.TempDir()` 픽스처** (`newFixtureServer` 참조 — 실데이터 무손상).

---

## 확장 포인트

### 새 MCP 도구 추가

1. `internal/catalog`에 순수 함수/메서드로 로직 구현 (+ 단위 테스트)
2. `internal/mcp/server.go`:
   - `tools()`에 `tool(name, description, objectSchema(...))` 등록
   - `callTool()` switch에 디스패치 추가 (인자 구조체 + `decodeArgs`)
3. `stdio_test.go`의 도구 수 단정 갱신 (28 → 29)
4. 필요 시 `initialize.instructions`·`text2sql_workflow` 프롬프트에 사용
   시점 반영, README 도구 목록 갱신

설계 규칙: 도구는 **LLM이 검증 가능한 근거**를 반환해야 합니다 — 점수에는
사유(reasons)를, 차단에는 힌트를, 추정에는 `source: inferred` 표시를.

### 새 데이터셋 추가

1. `internal/catalog/datasets.go`의 `DatasetRegistry`에 항목 추가
   (name/file/required/editable/format/description/schema/used_by)
2. 로더 작성 후 `load.go`의 `Load()`에 배선 — 실패는 `LoadIssue`로 축적
   (선택 파일이면 기동 중단 금지)
3. `datasetLoadedCount()`에 로드 건수 케이스 추가 (콘솔 표시용)
4. 이것만으로 `list_datasets`/`put_dataset`/웹 콘솔/REST가 자동 인식합니다
5. [datasets.md](datasets.md)에 스키마 문서화

### 검증 룰 추가

1. `internal/catalog/validate.go`의 `ValidateSQL` 흐름에 검사 추가 —
   `addErr(code, ...)`(실행 차단) 또는 `addWarn(code, ...)`
2. **코드는 대문자 스네이크** + `Hint`에 구체적 수정법(한국어) 필수
   (fix_hints·rank_candidates가 코드/힌트에 의존)
3. `rank.go`의 경고 가중치 테이블에 코드별 감점 추가 검토
4. [validation-rules.md](validation-rules.md) 표에 등록
5. 오탐 위험이 있으면 경고로 시작해 운영 데이터로 확인 후 오류로 승격
6. 방언 의존 룰(예: mysql에서 `FETCH FIRST` 금지, postgres에서 `IFNULL`
   경고)은 `Catalog.Dialect` 분기로 작성 — postgres|mysql|mariadb 3종 고려

### 검색 신호 추가

`search.go > SearchSchema`의 점수 루프에 신호 추가. 반드시
`reasons = appendReason(...)`으로 **사유를 남길 것** — 설명 불가능한 점수는
디버깅 불가. 이후 `jamypg-eval`로 회귀 확인.

### 시간 표현 추가

`timeparse.go > ParseTimeExpressions`에 패턴 추가 + `eval_test.go >
TestParseTimeExpressions`에 케이스 추가. 비교 표현(전월 대비류)은 독립
기간을 만들지 않도록 주의 (기존 `standalonePrevMonth` 참조).

### 학습 룰 타입 추가

`learn.go`: 집계 로직 + `LearnedRule.Type` 추가 → `applyLearnedRules`
(검색 반영)와 `learnedRuleWarnings`(검증 반영)에 케이스 추가.

### REST 엔드포인트 추가

`internal/mcp/admin.go > registerAdmin`에 패턴 등록. 변경성이면
`requireAdmin` + `adminAudit` 필수. `openapi.go` 스펙과 `admin_test.go`
동기화. 데이터 변경은 반드시 `dataMu` 잠금 + 실패 시 롤백 패턴 준수.

---

## 핵심 불변식 (깨뜨리면 안 되는 것)

1. **catalog는 로드 후 불변** — 변경은 새 카탈로그 컴파일 + `setCatalog`
   원자 교체로만. 기존 스냅샷을 절대 제자리 수정하지 않는다
2. **부분 적용 금지** — 데이터셋 변경은 백업→쓰기→전체 재컴파일→검증→스왑,
   실패 시 복원. 중간 상태가 관측되면 안 된다
3. **식별자 정규화 일원화** — 비교 전 반드시 `cleanIdent` (대문자·공백 제거)
4. **조용한 실패 금지** — 선택 데이터 문제는 `LoadIssue`로 축적해
   health에 노출
5. **사전 vs 추정 구분** — 확정 근거(dictionary)와 추정(inferred)을 응답에서
   섞지 않는다
6. **테스트에서 실데이터 변경 금지** — 변경 테스트는 TempDir 픽스처
7. **DB 실행은 읽기 전용 3중 방어를 우회하지 않는다** — SQL 가드
   (`dbconn/sqlguard.go`) + DSN 세션 read-only + SELECT 전용 계정 권장.
   새 실행 경로는 반드시 `ValidateReadOnlySQL`을 통과해야 한다

## 릴리즈 절차

1. `internal/mcp/server.go`의 serverInfo version + `openapi.go` version 범프
2. `go test ./...` + `jamypg-eval` 지표 확인
3. 크로스 빌드(`scripts/build.sh`) + `docker build` → `docker save | gzip`
   + sha256
4. `dist/DEPLOY-OFFLINE.md` 버전 치환
5. 커밋 → 태그 `vX.Y.Z` → push → `gh release create` (tar.gz, sha256,
   DEPLOY-OFFLINE.md, 플랫폼 바이너리 첨부)

주의: 실행 중인 Windows exe는 잠겨 있어 덮어쓸 수 없음 — 새 이름으로 빌드
후 사용자에게 교체 안내.
