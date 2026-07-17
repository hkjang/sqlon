# REST 관리 API

HTTP 모드에서 제공되는 데이터셋 관리 API입니다. **살아있는 문서는
`/docs`(Swagger UI, Try it out 지원)와 `/openapi.json`** 이며, 이 문서는
개념·인증·스크립팅 예시를 보완합니다.

REST와 MCP 도구(`put_dataset` 등)는 **동일한 서버 코드**를 호출하므로
검증·백업·핫스왑·롤백 동작이 완전히 같습니다.

## 엔드포인트 요약

| 메서드/경로 | 인증 | 설명 |
| --- | --- | --- |
| `GET /api/health` | — | 카탈로그 헬스 (이슈, 커버리지, PII, 사전 크기) |
| `GET /api/datasets` | — | 19종 레지스트리 + 라이브 상태 |
| `GET /api/datasets/{name}` | — | 상세 + 샘플 (`?sample_rows=N`, 최대 50) |
| `GET /api/datasets/{name}/content` | — | 원본 파일 그대로 (없으면 204) |
| `GET /api/datasets/{name}/backups` | — | 백업 목록 (최신순) |
| `PUT /api/datasets/{name}` | 토큰 | 교체 (`?force=true`로 오류 무시 적용) |
| `DELETE /api/datasets/{name}` | 토큰 | 제거 (선택+편집가능만) |
| `POST /api/datasets/{name}/restore` | 토큰 | 백업으로 복원 `{"backup":"..."}` |
| `POST /api/reload` | 토큰 | 디스크에서 재컴파일 + 핫스왑 |

정적 페이지: `GET /admin`(관리 콘솔), `GET /admin/editor`(테이블 편집기),
`GET /docs`(Swagger), `GET /healthz`(liveness).

승인 기반 변경 통제 API(`/api/changes*`, DBA 권한 필요)는
[change-control.md](change-control.md)를 참고하세요.

## 인증

서버를 `-admin-token <값>`(또는 환경변수 `JAMYPG_ADMIN_TOKEN`)으로 기동하면
**변경 API(PUT/DELETE/POST)** 에 다음 중 하나가 필요합니다:

```text
X-Admin-Token: <값>
Authorization: Bearer <값>
```

- 조회(GET)는 항상 개방입니다.
- 토큰 미설정 시 변경 API도 개방되며 기동 로그에 경고가 출력됩니다.
  **내부망 외 노출 시 반드시 설정하세요.**
- 401 응답에는 설정 방법 힌트가 포함됩니다.

## 변경 의미론

`PUT`의 요청 본문은 **파일의 새 전체 내용**입니다 (부분 병합 아님).

```text
PUT 흐름:
  본문 JSON 유효성 + 형식(array/object) 검사
  → 기존 파일을 backups/<file>.<ts>로 백업
  → 파일 쓰기 (pretty-print 정규화)
  → 전체 카탈로그 재컴파일
  → 이 파일 귀속 신규 오류?
       없음: 핫스왑, {"applied":true, "backup":..., "loaded":...}
       있음(+force 아님): 파일 복원 + 이전 카탈로그 유지,
             {"applied":false, "reason":..., "issues":[...], "hint":...}
  → HTTP는 둘 다 200 — applied 필드로 판정 (요청 자체가 잘못되면 400)
```

- `DELETE`: 백업 후 삭제 + 핫스왑. `required`(물리/논리 모델)·
  `editable:false`(feedback/audit)는 400.
- `restore`: 백업 이름은 해당 데이터셋 파일 접두사여야 하며 경로 구분자
  금지(경로 탈출 차단). 복원 전 현재 파일도 백업됩니다.
- 모든 변경은 `audit/*.jsonl`에 `admin:put_dataset` 형태로 기록됩니다
  (원격 주소 포함).
- 동시 변경은 서버에서 직렬화됩니다.

## curl 예시

```sh
BASE=http://127.0.0.1:9797
TOKEN='변경용-비밀토큰'   # -admin-token 값

# 1) 전체 목록과 상태
curl -s $BASE/api/datasets | jq '.datasets[] | {name, present, loaded, issues: (.issues|length)}'

# 2) 지표 사전 내려받기 → 수정 → 반영
curl -s $BASE/api/datasets/metrics/content -o metrics.json
$EDITOR metrics.json
curl -s -X PUT $BASE/api/datasets/metrics \
  -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' \
  --data-binary @metrics.json | jq '{applied, backup, reason, issues}'

# 3) 롤백됐다면 사유 확인 후 강제 적용(주의)
curl -s -X PUT "$BASE/api/datasets/metrics?force=true" \
  -H "X-Admin-Token: $TOKEN" --data-binary @metrics.json | jq .applied

# 4) 백업 목록 → 특정 시점 복원
curl -s $BASE/api/datasets/metrics/backups | jq '.backups[].name'
curl -s -X POST $BASE/api/datasets/metrics/restore \
  -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"backup":"metrics.json.20260703T112104"}' | jq .applied

# 5) 볼륨에서 파일 직접 교체 후 리로드
curl -s -X POST $BASE/api/reload -H "X-Admin-Token: $TOKEN" | jq '{reloaded, errors, warnings}'

# 6) 헬스
curl -s $BASE/api/health | jq '{status, error_count, warning_count}'
```

## 오류 응답

| 코드 | 의미 |
| --- | --- |
| 400 | 알 수 없는 데이터셋 이름, 형식 위반, 보호 대상 변경 시도, 잘못된 백업 이름 |
| 401 | 토큰 필요/불일치 |
| 404 | 알 수 없는 데이터셋(조회) |
| 500 | reload 실패 (이전 카탈로그 유지됨) |

본문은 `{"error": "..."}` 형식입니다. `PUT`의 검증 롤백은 400이 아니라
**200 + `applied:false`** 임에 유의하세요 (요청은 유효했고 내용이 거부된
것이므로).
