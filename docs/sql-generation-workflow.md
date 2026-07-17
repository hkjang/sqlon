# SQL 생성 워크플로

LLM(MCP 클라이언트)이 질문을 받았을 때 따라야 하는 표준 10단계입니다.
서버의 `initialize.instructions`와 `text2sql_workflow` 프롬프트에도 같은
순서가 내장되어 있습니다.

## 원칙 (절대 규칙)

1. 카탈로그(도구 응답)에 없는 테이블·컬럼을 **만들지 않는다**
2. 조인 조건은 `get_join_paths`/`build_sql_skeleton`에서만 가져온다
3. 업무 지표 계산식은 지표 사전(`source: dictionary`)을 우선 사용한다
4. PII 컬럼(`pii: true`)은 SELECT에 노출하지 않는다
5. 검증 실패 SQL은 실행하지 않는다; 자동 수정은 **2회**까지만
6. 탐색성 쿼리에는 항상 row bound(`LIMIT n`)를 둔다
7. 기본값을 적용했다면 최종 응답에 **가정으로 명시**한다

## 10단계

```text
 1. analyze_question ──── 모호성? ──┬─ 기본값 있음 → 적용 + 가정 표시
                                    └─ 없음 → 사용자에게 되묻고 중단
 2. search_schema (+find_filter_columns, resolve_time 필요 시)
 3. get_metric_definition  (질문의 모든 지표 용어에 대해)
 4. get_schema_context     (선택 테이블)
 5. get_join_paths         (2개 테이블 이상일 때; 경로없음/저신뢰 → 되묻기)
 6. SQL 작성:
      복잡·다중 테이블 → build_sql_skeleton 후 SLOT만 채움
      단순 단일 테이블 → 컨텍스트 기반 직접 작성
 7. validate_sql (expected_outputs, metrics 전달)
      실패 → fix_hints 반영 재작성 → 재검증 (최대 2회)
      2회 후에도 실패 → 실패 원인 + 수정 제안을 사용자에게 반환
      난이도 높음 → 후보 2~3개 생성 후 rank_candidates로 best_sql 선택
 8. explain_sql (DB 프로파일이 있으면 profile 전달 → 실측 EXPLAIN(JSON 플랜))
      risk=high → 기간/limit/파티션 조건 추가해 재생성 (7로 복귀)
      이후 run_sql_safely {profile}로 실행; 0행이면 hint 따라 조건 재확인
 9. 구조화 JSON 응답 반환 (아래 형식)
10. record_feedback (성공/실패/교정/채택 여부)
```

## 최종 응답 형식 (9단계)

UI가 활용할 수 있도록 SQL만 반환하지 말고 JSON으로 반환합니다:

```json
{
  "sql": "SELECT ...",
  "used_tables": ["dw_snapshot.customer_snapshot"],
  "used_columns": ["credit_grade", "credit_score", "base_month", "excl_code_1"],
  "applied_metrics": [{"name": "평균 신용점수", "expression": "AVG(credit_score)"}],
  "applied_join_paths": [],
  "applied_filters": ["base_month = '202506'", "excl_code_1 IS NULL"],
  "assumptions": ["기간 미지정 → 최신 기준월(202506) 사용"],
  "cautions": ["dw_snapshot 스냅샷 데이터 — dw_history 이력 조건 미적용"],
  "validation_result": {"valid": true, "warnings": 0},
  "executable": true
}
```

## 엔드투엔드 예시

질문: **"2025년 6월 기준 평균 신용점수를 등급별로 보여줘"**

### 1) analyze_question

```json
{"question": "2025년 6월 기준 평균 신용점수를 등급별로 보여줘"}
```

응답 요지: `intent: ["aggregation.avg", "group_by"]`,
`target_metrics: [{term: "평균 신용점수", source: "dictionary", ...}]`,
`dimensions: ["등급", "월"]`, `time_range: [{start: "20250601", end:
"20250630", granularity: "month"}]`, `expected_output_columns: ["평균
신용점수", "등급"]`, `ambiguities: []` → 되물을 것 없음.

### 2) search_schema

top1 = `dw_snapshot.customer_snapshot` (K-Score 2.0), 사유: 지표 사전 매핑 +12,
설명/도메인 토큰 매칭. `excluded[]`에 컷된 유사 스코어 뷰들이 사유와 함께
표시됨.

### 3) get_metric_definition("평균 신용점수")

`source: "dictionary"` — `expression: AVG(credit_score)`, `required_filters:
["base_month = 조회 기준월", "excl_code_1 IS NULL (컬럼 존재 시)"]`,
`recommended_group_by: ["base_month", "credit_grade"]`. **이 계산식을 그대로 사용.**

### 4) get_schema_context(tables=["dw_snapshot.customer_snapshot"])

컬럼 목록(credit_grade, credit_score, base_month semantic_type=MONTH_YYYYMM, ...),
`time_conditions`: `base_month = '202506'`, policy hint: dw_snapshot는 스냅샷 —
start_date/end_date 발명 금지.

### 5) get_join_paths — 단일 테이블이므로 생략

### 6) SQL 작성

```sql
SELECT T1.credit_grade, AVG(T1.credit_score) AS "평균 신용점수"
FROM dw_snapshot.customer_snapshot T1
WHERE T1.base_month = '202506'
  AND T1.excl_code_1 IS NULL
GROUP BY T1.credit_grade
ORDER BY T1.credit_grade
LIMIT 100
```

### 7) validate_sql

```json
{"sql": "...", "metrics": ["평균 신용점수"], "expected_outputs": ["등급"]}
```

→ `valid: true`, 경고 0. (만약 `excl_code_1 IS NULL`을 빼먹었다면
`MISSING_EXCL_FILTER` 경고 + 힌트가 왔을 것)

### 8) explain_sql → risk: low → 진행

### 9) 구조화 응답 반환 → 10) record_feedback(outcome: "success", adopted: true)

## 실패 루프 예시

7단계에서 `NVL(excl_code_1, 'N')`을 썼다면 (Oracle 습관):

```json
"errors": [{"code": "DIALECT_FUNCTION", "hint": "COALESCE, CASE, 표준 날짜 연산으로 바꾸세요 (NVL→COALESCE, DECODE→CASE, ROWNUM→LIMIT)."}],
"fix_hints": [{"code": "DIALECT_FUNCTION", "issue": "...", "suggestion": "COALESCE, CASE, 표준 날짜 연산으로 바꾸세요 (NVL→COALESCE, ...)."}],
"retry_guidance": "fix_hints를 반영해 SQL을 수정한 뒤 validate_sql을 다시 호출하세요. 자동 수정은 최대 2회..."
```

→ `fix_hints.suggestion`대로 고쳐 1회 재검증 → 통과.

## 복잡 질문 분기 (6단계)

질문: "전월 대비 2025년 상반기 성별 카드 이용금액 합계 증감"

- `analyze_question.patterns`에 `mom_change`(LAG 기반 CTE 템플릿) 포함
- 2개 테이블(customers ⋈ customer_transactions) → `build_sql_skeleton` 호출:

```sql
SELECT /* SLOT: 출력 컬럼 (차원 먼저, 지표 다음) */
       SUM(amount) AS "카드 이용금액" /* 지표사전: ... */
FROM dw_history.customers T1
INNER JOIN dw_history.customer_transactions T2 ON T1.cust_id = T2.cust_id
WHERE T2.trans_date >= '20250101' AND T2.trans_date <= '20250630'
  AND T2.start_date <= '{기준일:YYYYMMDD}' AND T2.end_date > '{기준일:YYYYMMDD}' /* SLOT */
  AND T2.status_code IS NULL
  AND /* SLOT: 추가 필터 조건 */
/* SLOT: GROUP BY 차원 */
/* SLOT: ORDER BY */
LIMIT 100
```

LLM은 구조를 유지한 채 SLOT만 채우고(gender 차원, 기준일, GROUP BY),
`mom_change` 템플릿의 주의("비교월까지 기간 확장")를 반영합니다. 이후
후보 2개를 만들어 `rank_candidates`로 확정 → 7단계 계속.

## 되묻기 기준 (중단 조건)

| 상황 | 근거 필드 | 행동 |
| --- | --- | --- |
| 지표가 사전에 없고 후보가 여럿 | `target_metrics[].source == "unknown"` | 후보 계산식 나열 + 확인 요청 |
| 조인 경로 없음 | `get_join_paths.found == false` | 연결 기준 질문 또는 단일 테이블 대안 제시 |
| 조인 confidence < 0.7 | `confidence`, `guidance` | 업무 타당성 확인 요청 |
| 기간 없음 + 기본값 부적절 | `ambiguities` | 기준월/기간 확인 요청 |
| 금지 조인 | `FORBIDDEN_JOIN` | 사유 전달, 대안 재구성 |
