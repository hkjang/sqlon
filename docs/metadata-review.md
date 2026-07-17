# 후보 승인 워크플로 (Phase 9)

의미 보강([Phase 5](metadata-enrich.md))과 모델 후보([Phase 6](metadata-candidates.md))
엔진이 만든 **검토 후보**를 담당자가 한 곳에서 승인/반려하는 워크플로다. 이로써
"물리 사실은 자동 수집, 업무 의미는 승인 후에만 반영"이라는 원칙이 **끝단까지**
지켜진다 — 승인해도 카탈로그는 자동 수정되지 않고, 적용용 스니펫만 생성된다.

관련 요건: FR-META-018(검토 큐), FR-META-019(승인/반려 이력),
FR-META-020(승인분 적용 스니펫).

## 구성 요소

- **검토 큐** — 두 엔진의 현재 후보를 저장된 결정과 조인해 보여준다. 각 후보에는
  내용 기반 안정 `id`가 부여된다. 제안 값이 바뀌면 id도 바뀌어 **재검토 대상**이
  된다.
- **결정 저장** — 승인/반려는 검토자·시각·메모와 함께
  `<data>/reviews/decisions.json`에 영속 저장된다(원자적 write).
- **적용 스니펫** — 승인된 후보를 목적 파일별 조각으로 컴파일한다:
  `overrides.json`의 `columns[]`, `metrics.json`, `relations.json`, 코드사전 바인딩.

## 관리자 UI

좌측 내비 **🧾 메타 검토** (`/admin/reviews`):

- 상태(대기/승인/반려)·종류·테이블로 필터
- 행별 승인/반려, 또는 체크박스 다중 선택 후 일괄 처리
- 검토자 이름·메모 입력
- **승인 스니펫 보기**로 적용용 JSON을 확인·복사

## API

### 큐 조회

```sh
curl -s "http://127.0.0.1:9797/api/reviews?status=pending&kinds=metric,relation"
```

```jsonc
{
  "items": [ { "id": "mt-…", "source": "model", "kind": "metric",
               "table": "…", "column": "…", "suggested": {…},
               "confidence": 0.7, "status": "pending" } ],
  "summary": { "pending": 26, "approved": 1, "rejected": 0 },
  "stale_count": 0
}
```

### 결정 (관리자 권한)

```sh
curl -s -X POST http://127.0.0.1:9797/api/reviews/decide \
  -H 'Content-Type: application/json' \
  -d '{"decisions":[{"id":"mt-…","decision":"approved","notes":"확인"}],"reviewer":"hkjang"}'
# → {"applied":1,"reviewer":"hkjang"}
```

`reviewer`를 생략하면 인증 사용자명(→ `X-Reviewer` 헤더 → `admin`) 순으로 결정된다.

### 승인분 적용 스니펫 (수동 반영)

```sh
curl -s "http://127.0.0.1:9797/api/reviews/apply"
# → { overrides_columns:[…], metrics:[…], relations:[…], code_dicts:[…], counts:{…} }
```

### 원클릭 반영 (자동 병합 + 리로드, 관리자)

승인분을 데이터셋 파일에 직접 병합하고 카탈로그를 핫리로드한다. 각 파일은
반영 전 자동 백업된다.

```sh
curl -s -X POST http://127.0.0.1:9797/api/reviews/apply
# → { "applied":3, "written":{"overrides.json":2,"meta_code_dict.json":1},
#     "backups":[…], "reloaded":{…} }
```

- 병합 대상: `overrides.json`(columns[]), `metrics.json`,
  `topology_relations.json`, `meta_code_dict.json`
- **멱등**: 반영된 결정은 `applied_at`이 찍혀 재실행 시 건너뛰고, 병합 시
  파일 내용과도 중복 제거한다.
- **운영자 수기 값 보호**: overrides.json에 이미 채워진 필드는 후보로 덮어쓰지
  않는다(사람 큐레이션 우선).
- 관리 UI의 **승인분 반영 + 리로드** 버튼, MCP `apply_approved_candidates`도
  동일 동작.

승인이 사람의 명시적 게이트이고, 반영은 그 뒤의 두 번째 명시적 행위이므로
"자동 미반영" 원칙과 충돌하지 않는다.

## MCP 도구

- `review_candidates{tables?, kinds?, status?}` — 검토 큐 조회
- `decide_candidates{decisions:[{id, decision, notes?}], reviewer?}` — 승인/반려
- `get_approved_overrides` — 승인분 적용 스니펫
- `apply_approved_candidates` — 승인분을 데이터셋 파일에 병합 + 리로드(원클릭)

LLM 클라이언트가 후보를 다듬어 제시하고, 담당자가 승인하는 협업 흐름을 그대로
도구로 노출한다.

## 적용 흐름

1. `/admin/reviews` 또는 `review_candidates`로 대기 후보 검토.
2. 승인/반려 (`decide_candidates` / UI 버튼).
3. `get_approved_overrides`(또는 **승인 스니펫 보기**)로 조각을 받아
   `overrides.json`·`metrics.json`·`relations.json`에 반영.
4. 데이터셋 재적재/재기동 → 승인된 메타데이터가 라이브.
5. [품질 점수](metadata-quality.md)·[영향도](metadata-impact.md)로 반영 결과 확인.
