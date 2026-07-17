# 운영 가이드

## 배포

### 오프라인망 (Docker, 권장)

릴리즈 자산 `jamypg-mcp-<ver>-docker.tar.gz` 반입 → 검증 → 로드 → 실행.
상세 절차는 릴리즈의 `DEPLOY-OFFLINE.md` 참조.

```sh
docker run -d --name jamypg-mcp --restart unless-stopped \
  -p 9797:9797 \
  -v jamypg-data:/app/data/metadb \
  -e JAMYPG_ADMIN_TOKEN='변경용-비밀토큰' \
  jamypg-mcp:<ver>
```

- **볼륨 필수 권장**: feedback/audit/learned_rules/backups가 재기동 후에도
  유지됩니다. 최초 마운트 시 이미지 내장 메타데이터가 볼륨으로 복사됩니다.
- 이미지에는 `jamypg-eval`, `jamypg-goldgen` CLI도 포함됩니다.
- 단일 Dockerfile(CGO_ENABLED=0, 순수 Go 드라이버)입니다 — Oracle Instant
  Client 같은 별도 클라이언트 라이브러리나 전용 이미지 변형이 없습니다.

### 단독 바이너리

```sh
# HTTP (웹 콘솔 포함)
./jamypg-mcp -transport http -addr 0.0.0.0:9797 -data ./data/metadb -admin-token ...
# stdio (데스크톱 MCP 클라이언트용 — 웹 UI 없음)
./jamypg-mcp -transport stdio -data ./data/metadb
```

주요 플래그: `-data`(메타 디렉터리), `-addr`, `-endpoint`(기본 /mcp),
`-allow-origin`(추가 허용 Origin), `-stateless`, `-sse-post`,
`-admin-token`(`JAMYPG_ADMIN_TOKEN`), `-meta-db`(`JAMYPG_META_DB` —
로그인/사용자/MCP 키/사용자별 프로파일 활성화, [auth.md](auth.md)),
`-bootstrap-admin`(`JAMYPG_BOOTSTRAP_ADMIN`),
`-oidc-*`(`JAMYPG_OIDC_ISSUER/CLIENT_ID/CLIENT_SECRET/REDIRECT_URL`).

대상 DB(PostgreSQL/MySQL/MariaDB) 접속 프로파일은 `db_profiles.json`으로
관리합니다 — [db-connector.md](db-connector.md) 참조.

## 기동 확인 체크리스트

```sh
curl -s http://HOST:9797/healthz | jq .catalog
# table_count/relation_count/sample_count가 기대값인지

docker logs jamypg-mcp | head -20
# "loaded catalog: N tables, ..." 와
# catalog ERROR 라인들 — 원천 메타의 알려진 문제(존재하지 않는 컬럼을
# 참조하는 조인 관계 등)가 여기 나열되는 것은 정상 동작입니다.
# 새로운 ERROR가 늘었는지가 관건입니다.
```

`/admin` 접속 → 헤더의 오류/경고 칩 확인 → 이슈가 있는 데이터셋은 행에
`이슈 N` 뱃지가 표시됩니다.

## 모니터링

| 신호 | 위치 | 보는 법 |
| --- | --- | --- |
| liveness | `GET /healthz` | 200 + catalog 요약 (Docker HEALTHCHECK 내장) |
| 컴파일 품질 | `GET /api/health` / MCP `get_catalog_health` | `status`, `error_count` 추이 |
| 사용 추적 | `data/metadb/audit/audit-YYYYMMDD.jsonl` | 도구별 호출량·소요·오류율 집계 |
| DB 실행 추적 | `data/metadb/audit/query-YYYYMMDD.jsonl` | `db:execute` 항목 — SQL·행 수·소요·오류코드(`PG-*`/`MY-*`/`TIMEOUT`) |
| 쿼리·도구·품질 메트릭 | `GET /metrics` (Prometheus) | DB: `db_query_total`/`db_query_failure_total`/`db_query_slow_total`, `db_pool_*`. 서버: `jamypg_up`, `jamypg_build_info`, `jamypg_tool_calls_total{tool,status}`, `jamypg_tool_duration_ms_sum{tool}`, `jamypg_catalog_tables`/`_relations`, `jamypg_metadata_quality_score`, `jamypg_metadata_quality_gate_pass` |
| 메타 품질 대시보드 | `/admin/quality` | 종합 점수·등급, 릴리스 게이트 위반, 테이블별 품질(점수 낮은 순), 감사 로그 무결성 검증 |
| 정확도 | `docker exec jamypg-mcp jamypg-eval -data /app/data/metadb` | 지표 하락 여부 |

`/metrics`는 DB 커넥터 메트릭과 서버/카탈로그/품질 게이지를 한 엔드포인트에서
Prometheus 텍스트 포맷으로 노출합니다(외부 클라이언트 라이브러리 없음).

감사 로그 예시 집계:

```sh
jq -r .tool audit-*.jsonl | sort | uniq -c | sort -rn      # 도구별 호출량
jq 'select(.is_error==true)' audit-*.jsonl                  # 오류만
jq -r .entry.error_code query-*.jsonl | sort | uniq -c      # DB 오류코드 분포
```

### 감사 로그 무결성 (해시 체인)

모든 감사 엔트리는 단조 증가 `seq`와 이전 엔트리에 연결되는 `hash`를 갖습니다
(`hash_n = sha256(prev_hash ‖ canonical_json(entry))`). 한 줄이라도 삭제·수정·
재배열되면 체인이 깨지고 위치가 특정됩니다.

```sh
curl -s "http://127.0.0.1:9797/api/audit/verify?day=YYYYMMDD"   # 관리자
# → {"valid":true,"entries":N,"tip_hash":"..."} 또는
#   {"valid":false,"verified_entries":k,"broken_at_line":i,"reason":"..."}
```

`/admin/quality` 하단에서도 검증할 수 있습니다. 이 기능 도입 이전에 기록된
로그는 해시가 없어 `valid:false`로 표시됩니다(정상 — 신규 엔트리부터 보증).

### 메타데이터 다이제스트

`GET /api/metadata/digest`(MCP `get_metadata_digest`)는 품질 점수·릴리스 게이트,
검토 큐 백로그(대기/승인/반려), 골든 승격 후보 수, 카탈로그 규모·로드 경고를
한 줄 헤드라인과 함께 반환합니다. 일일 점검·알림 트리거에 사용합니다.

### 메타데이터 스케줄러 · 웹훅

`-sync-source <profile-id> -sync-interval <duration>`(예: `24h`)로 주기적
증분 메타데이터 동기화를 서버 내장으로 실행합니다(cron 불필요). 각 실행은
변경 건수·품질 점수·게이트 통과 여부를 로그와 감사 로그에 남깁니다. 증분·폐기
후보 원칙은 유지되어 업무 의미를 자동 변경하지 않습니다. 최소 간격 1분.

`-digest-webhook <url>`(또는 `JAMYPG_DIGEST_WEBHOOK`)을 설정하면 매 틱마다
다이제스트 JSON을 해당 URL로 POST합니다(Slack 등 알림 연동). `-sync-source`
없이 웹훅만 설정해도 스케줄에 맞춰 다이제스트만 전송됩니다.

`-openmetadata-sync`(+선택 `-openmetadata-scope`)를 켜면 매 틱마다 OpenMetadata
증분 import를 반영합니다. 틱 실행 순서는 **DB sync → OpenMetadata import → 다이제스트
웹훅**이며 설정된 단계만 동작합니다. 자세한 내용은
[openmetadata.md](openmetadata.md)를 참고하세요.

## 데이터셋 운영

일상 작업은 `/admin`(JSON)·`/admin/editor`(표) 사용 —
[datasets.md](datasets.md) 참조. 원칙:

1. 변경 전 `get_dataset`/콘솔로 현재 내용 확인
2. 적용 결과의 `applied` 확인 — `false`면 자동 롤백된 것 (사유·이슈 확인)
3. 반영 후 `run_evaluation`으로 정확도 회귀 확인
4. 대량/정기 변경은 REST 스크립트화 ([rest-api.md](rest-api.md))

### 백업·복원

- 자동 백업: `data/<set>/backups/<파일>.<타임스탬프>` (교체·제거·복원 직전)
- 수동 스냅샷: 볼륨/디렉터리 전체를 주기 백업 권장 (backups/는 무한 증가
  하므로 보존 정책에 따라 오래된 항목 정리)
- 복원: 콘솔의 [이 백업으로 복원] 또는 `POST /api/datasets/{name}/restore`

## 피드백 학습 루프

```text
클라이언트 record_feedback ──▶ feedback/*.jsonl
        │ (재기동/리로드 시 자동)                (주기 실행 권장)
        ├─ 성공/교정 → 검색 부스트 + few-shot   learn_from_feedback
        │                                            │
        └────────────◀ learned_rules.json ◀──────────┘
             검색 패널티 + LEARNED_* 검증 경고
```

운영 절차:

1. 클라이언트가 `record_feedback`을 성실히 호출하도록 통합 (outcome,
   final_sql, adopted가 핵심 필드)
2. 주 1회 등 주기로 `learn_from_feedback` 호출 (`min_occurrences` 기본 3) —
   feedback뿐 아니라 DB 실행 감사(query-*.jsonl)의 반복 지연·오류 패턴도
   룰로 승격됩니다
3. `learned_rules.json`을 리뷰 — 잘못 승격된 룰은 항목 삭제 후
   `reload_catalog`
4. 반복되는 `recurring_error`는 근본 원인(메타 설명 부족, 동의어 누락)을
   glossary/overrides로 해결하는 것이 정석

## 정확도 운영

- 골든셋 평가: [evaluation.md](evaluation.md). 데이터셋 변경 후·릴리즈 전
  필수 실행. 프로파일을 지정하면 expected_sql 실 실행 성공률도 측정됩니다
- 조인 그래프 확충: `suggest_joins` 실행 → 근거 검토 → `suggested_override`를
  overrides.json `preferred_joins`에 추가 → 평가 재실행
- 검색 미스 분석: `jamypg-eval -verbose`의 MISS 케이스 → 대부분 논리명/동의어
  부재가 원인 → glossary·overrides 보강

## 트러블슈팅

| 증상 | 원인 | 조치 |
| --- | --- | --- |
| `/admin`·`/docs` 404 | 구버전 바이너리/이미지 실행 중 | v0.3.0+ 재배포. 실행 중 exe는 잠겨 덮어쓰기 불가 — 종료 후 교체 |
| `missing or unknown Mcp-Session-Id` | v0.2.0 이하 | v0.2.1+ (세션 관대 정책) |
| 기동 실패 `load catalog` | 물리/논리 모델 없음·파손, 테이블 0개 | `-data` 경로와 두 필수 파일 확인 |
| `catalog ERROR ... relation reference column not in table` | 원천 메타의 깨진 조인 정의 | 알려진 데이터 이슈 — relations 데이터셋에서 해당 엣지 수정 |
| put_dataset이 계속 롤백 | 새 내용이 로드 오류 유발 | 응답 `issues` 확인 → 스키마 준수. 의도적 선등록이면 `force` |
| 변경 API 401 | admin token 불일치 | `-admin-token` 값과 `X-Admin-Token` 헤더 대조 |
| 브라우저에서 API 차단 (403 forbidden origin) | 비-로컬 Origin | `-allow-origin https://...` 추가 |
| run_sql_safely 오류 `PG-28P01` / `MY-1045` | 대상 DB 인증 실패 | 프로파일 username·`password_ref`(env:/file:) 확인 |
| 오류 `PG-25006` / `MY-1290` (read-only) | 세션 read-only 위반 — 정상 방어 | 생성 SQL에 쓰기 구문 포함 여부 점검 |
| 오류 `TIMEOUT` / `MY-3024` / `PG-57014` | 쿼리 타임아웃 초과 | 기간/LIMIT 축소, `explain_sql`로 플랜 확인, 필요 시 `query_timeout_seconds` 조정 |
| 검색이 특정 테이블을 계속 상위 노출 | feedback 부스트 편향 | feedback 파일 정리 또는 learned rule로 패널티 |
| 평가 지표 급락 | 데이터셋 변경 부작용 | backups로 복원 → 변경분 재검토 |
| 대용량 편집 후 저장 지연 | 카탈로그 재컴파일(수 초) + 파일 크기 | 정상 — sql_datasets(5MB)급은 수 초 소요 |

## 업그레이드

1. 새 이미지/바이너리 반입 (릴리즈 자산 + sha256 검증)
2. 볼륨 사용 중이면 데이터는 유지됨 — 새 컨테이너로 교체 기동
3. `healthz` + `run_evaluation`으로 회귀 확인
4. 데이터 스키마가 바뀌는 릴리즈는 릴리즈 노트의 마이그레이션 절 참조
