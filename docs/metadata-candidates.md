# 모델 후보 생성 (Phase 6)

`suggest_model_candidates`는 물리 스키마와 프로파일링 결과로부터 **코드사전·
지표·관계** 후보를 규칙 기반으로 생성한다. [의미 보강](metadata-enrich.md)과
같은 원칙 — 결정적·오프라인·검토 후보만, 운영 카탈로그에 자동 반영하지 않음
(`review_status: suggested`). 모든 후보는 `evidence`+`confidence`를 동반한다.

관련 요건: FR-META-013(코드사전 후보), FR-META-014(지표 후보),
FR-META-015(관계 후보).

## 후보 종류(kind)

### code_dict — 코드사전 스켈레톤

저카디널리티 코드 컬럼(의미타입 CODE, `*_CD/_TYPE/_DIV/_YN/_STATUS`, 또는
distinct ≤ 20)이면서 아직 코드사전이 없는 컬럼에 대해, 프로파일 top-value로
코드 목록 스켈레톤을 만든다. **label은 비워** 둔다 — 각 코드의 업무 의미는
담당자가 채운다.

- 신뢰도: semantic_type=CODE(0.75) → 코드형 이름(0.7) → 저카디널리티만(0.6)
- **개인정보 컬럼(pii=true)은 값 목록을 만들지 않는다.**
- 프로파일 top-value가 없으면 생성하지 않는다.

### metric — 집계 지표

측정값 컬럼(의미타입 AMOUNT/COUNT/RATIO/SCORE)이면서 기존 지표가 참조하지
않는 컬럼에 대해 집계 지표를 제안한다.

| 의미타입 | 집계 |
| --- | --- |
| AMOUNT | SUM |
| COUNT | SUM |
| RATIO / SCORE | AVG |

같은 테이블의 날짜·코드 컬럼을 `recommended_group_by` 차원으로,
`expression`·`example_sql`을 함께 제안한다. 신뢰도 0.7.

### relation — FK 관계 추론

식별자 컬럼(`*_ID/_NO/_CD/_SEQ/_KEY`, PK 자신 제외)의 이름으로 참조 테이블을
추론한다.

1. 다른 테이블의 **단일 PK 컬럼명과 정확히 일치**(예: `customer_id` ↔
   customer.customer_id) → 신뢰도 0.8
2. 이름 어간이 **테이블명과 일치**(예: `customer_id` → customer) → 신뢰도 0.7

기준 컬럼과 참조 PK의 **타입 패밀리(numeric/text/temporal)가 다르면** 신뢰도를
0.2 낮추고 경고 근거를 남긴다. 이미 `relations.json`에 존재하는 관계는
재제안하지 않는다.

## 사용

### MCP

```json
{
  "name": "suggest_model_candidates",
  "arguments": {
    "tables": ["public.orders"],       // 생략 시 전체
    "kinds": ["metric", "relation"]     // 생략 시 3종 모두
  }
}
```

### REST

```sh
curl -s -X POST http://127.0.0.1:9797/api/metadata/candidates \
  -H 'Content-Type: application/json' \
  -d '{"kinds":["code_dict"]}'
```

카탈로그·프로파일만 읽으므로 DB 접속·인증 게이트가 필요 없다.

## 응답

```jsonc
{
  "count": 3,
  "count_bykind": { "code_dict": 1, "metric": 1, "relation": 1 },
  "generator": "rule",
  "candidates": [
    {
      "kind": "relation",
      "table": "S.ORDERS",
      "column": "customer_id",
      "target": "S.CUSTOMER",
      "suggested": {
        "base_table": "S.ORDERS", "base_column": "customer_id",
        "reference_table": "S.CUSTOMER", "reference_column": "customer_id",
        "cardinality": "many-to-one", "join_type": "inner",
        "provision_type": "inferred", "preferred": false
      },
      "confidence": 0.8,
      "evidence": ["customer_id matches PK of S.CUSTOMER"],
      "generator": "rule",
      "review_status": "suggested"
    }
  ],
  "how_to_apply": "...",
  "note": "review_status=suggested. 운영 카탈로그에 자동 반영하지 않습니다."
}
```

## 승인 흐름

1. `suggest_model_candidates` 호출 → 후보 검토.
2. 확정: code_dict → `code_dictionary.json`/컬럼 `code_dict`, metric →
   `metrics.json`, relation → `relations.json`에 반영.
3. 데이터셋 재적재 → [metadata-quality](metadata-quality.md)로 품질 재확인
   (관계성·지표연결 차원 점수 상승).
