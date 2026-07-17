# 계보/영향도 분석 (Phase 7)

`analyze_impact`는 테이블/컬럼을 **변경하거나 폐기하기 전에** 그 자산에
의존하는 모든 카탈로그 요소를 역추적해 변경 영향 범위(blast radius)를
보여준다. 카탈로그에 대한 **읽기 전용 분석**이며 아무것도 수정하지 않는다.

관련 요건: FR-META-016(계보 추적), FR-META-017(영향도 평가).

## 추적하는 의존 종류(kind)

| kind | 의미 |
| --- | --- |
| `metric` | 대상 테이블/컬럼을 tables·columns·expression으로 참조하는 지표 |
| `relation` | 대상이 base 또는 reference로 참여하는 관계(FK) |
| `preferred_join` | 대상을 사용하는 선호 조인(overrides) |
| `forbidden_join` | 대상이 걸린 금지 조인(overrides) |
| `golden_query` | 대상 테이블/컬럼을 참조하는 골든셋 질의 |
| `override` | 대상에 대한 테이블/컬럼 오버라이드 |
| `glossary` | 대상(컬럼명·논리명)을 동의어로 매핑한 용어집 항목 |
| `downstream_table` | 대상에서 1홉으로 조인 가능한 하위 테이블 |

컬럼 정확 매칭은 **토큰 경계**를 지킨다 — `customer_id`를 분석할 때 `customer`
부분 문자열만으로는 매칭되지 않는다.

## impact_level

- **high** — 지표(`metric`) 또는 선호 조인(`preferred_join`)이 의존. 깨지면
  NL2SQL 산출물이 즉시 잘못될 수 있음.
- **medium** — 관계(`relation`) 또는 골든셋(`golden_query`)이 의존.
- **low** — 오버라이드·용어집·하위 테이블만 의존.
- **none** — 의존 자산 없음.

## 사용

### MCP

```json
{ "name": "analyze_impact", "arguments": { "table": "sakila.customer", "column": "customer_id" } }
```

`column`을 생략하면 테이블 전체를 분석한다.

### REST

```sh
curl -s "http://127.0.0.1:9797/api/metadata/impact?table=sakila.customer&column=customer_id"
```

## 응답 예

```jsonc
{
  "target": "SAKILA.CUSTOMER",
  "impact_level": "medium",
  "total": 9,
  "count_bykind": { "downstream_table": 2, "glossary": 1, "golden_query": 3, "override": 1, "relation": 2 },
  "dependents": [
    { "kind": "relation", "name": "SAKILA.PAYMENT → SAKILA.CUSTOMER", "detail": "customer_id = customer_id", "via": "CUSTOMER_ID" },
    { "kind": "golden_query", "name": "결제 금액 합계가 가장 큰 고객은 누구야?", "via": "amount|customer_id" }
  ],
  "note": "읽기 전용 분석입니다. 변경 전 여기 나열된 자산을 함께 검토/회귀 테스트하세요."
}
```

## 활용

- **컬럼 폐기 전** `analyze_impact`로 이 컬럼을 쓰는 지표·골든셋·조인을 먼저
  파악하고, 대체 컬럼으로 마이그레이션.
- **[증분 변경 감지](metadata-sync.md)** 가 `retire_candidate`로 표시한 테이블을
  실제로 내리기 전, `analyze_impact`로 하위 영향이 없는지 확인.
- **[품질 게이트](metadata-quality.md)** 의 JOIN_BROKEN/METRIC_BROKEN 위반이
  뜨면, 원인 테이블에 `analyze_impact`를 걸어 어떤 지표·조인이 끊겼는지 추적.
