# 의미 메타데이터 보강 (Phase 5)

`suggest_semantic_metadata`는 **논리명·의미타입·설명이 비어 있는 컬럼**에
대해 검토 가능한 후보(candidate)를 생성한다. 규칙 기반이며 오프라인으로
동작하고, **운영 카탈로그에 자동 반영하지 않는다** — 모든 결과는
`review_status: "suggested"` 상태의 제안일 뿐이며, LLM 클라이언트나 담당자가
다듬어 `overrides.json`으로 승인해야 실제 반영된다.

관련 요건: FR-META-009(논리명 제안), FR-META-010(설명 생성),
FR-META-011(의미타입 분류), FR-META-012(근거·신뢰도 동반).

## 설계 원칙

- **물리 사실은 자동 수집, 업무 의미는 후보만.** 컬럼 타입·PK·NULL 여부 등
  물리 사실은 [metadata-sync](metadata-sync.md)가 자동 수집하지만, 논리명·설명
  같은 업무 의미는 결코 자동 확정하지 않는다.
- **결정적(deterministic)·오프라인.** 외부 LLM 호출 없이 용어집·기존 카탈로그·
  약어 사전·이름/타입 패턴만으로 후보를 만든다. 같은 카탈로그면 항상 같은 결과.
- **근거·신뢰도 동반.** 모든 후보는 `evidence`(왜 이렇게 제안했는지)와
  `confidence`(0–1)를 함께 반환한다. 낮은 신뢰도는 사람이 우선 검토하라는 신호다.

## 제안 종류(kind)

| kind | 설명 | 근거 우선순위 / 신뢰도 |
| --- | --- | --- |
| `logical_name` | 빈 논리명 후보 | 컬럼 코멘트(0.85) → 용어집 동의어(0.9) → 타 테이블 동일 컬럼명 재사용(0.8) → 약어 확장(0.6) |
| `semantic_type` | 의미타입 분류 | 이름 접미사 + 데이터 타입 패턴 (0.7–0.9) |
| `description` | 구조화 설명 문장 | 논리명·의미타입·NULL/PII 사실 조합 템플릿 (0.55) |

의미타입 분류값: `IDENTIFIER · NAME · CODE · AMOUNT · RATIO · SCORE · COUNT ·
DATE · FLAG · PII`. 예: `*_CD/_TYPE/_DIV`→CODE, `*_YN`→FLAG, `*_AMT/_BAL`→AMOUNT,
`*_RT/RATIO`→RATIO, `*SCORE*`→SCORE, `*_CNT/_QTY`→COUNT, `*_NM/_NAME`→NAME,
`*_NO/_ID/_SEQ`·PK→IDENTIFIER, date 타입·`*_DT`→DATE, `pii=true`→PII.

## 사용

### MCP

```json
{
  "name": "suggest_semantic_metadata",
  "arguments": {
    "tables": ["public.customer"],   // 생략 시 전체 카탈로그
    "kinds": ["logical_name", "semantic_type"]  // 생략 시 3종 모두
  }
}
```

### REST

```sh
curl -s -X POST http://127.0.0.1:9797/api/metadata/suggest \
  -H 'Content-Type: application/json' \
  -d '{"kinds":["semantic_type"]}'
```

카탈로그만 읽으므로 DB 접속·인증 게이트가 필요 없다.

## 응답

```jsonc
{
  "count": 25,
  "generator": "rule",
  "suggestions": [
    {
      "kind": "semantic_type",
      "table": "PUBLIC.CUSTOMER",
      "column": "STATUS_CD",
      "suggested": "CODE",
      "confidence": 0.85,
      "evidence": ["code-column naming/dictionary"],
      "generator": "rule",
      "review_status": "suggested"
    }
  ],
  "overrides_snippet": [        // confidence >= 0.7 항목만, 컬럼별 병합
    { "table": "PUBLIC.CUSTOMER", "column": "STATUS_CD", "semantic_type": "CODE" }
  ],
  "how_to_apply": "검토 후 overrides.json 의 columns[] 에 붙여넣고 재적재하세요.",
  "note": "review_status=suggested. 운영 카탈로그에 자동 반영하지 않습니다."
}
```

이미 채워진 필드는 건너뛴다 — 후보는 **빈 값에 대해서만** 생성된다.

## 승인 흐름

1. `suggest_semantic_metadata` 호출 → 후보 검토.
2. 고신뢰(≥0.7) 항목은 `overrides_snippet`을 그대로 `overrides.json`의
   `columns[]`에 붙여넣거나, 저신뢰 항목은 사람이 문구를 수정.
3. 데이터셋 재적재(카탈로그 재컴파일) → [metadata-quality](metadata-quality.md)로
   품질 점수 재확인.
