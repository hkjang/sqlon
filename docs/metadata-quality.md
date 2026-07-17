# 메타데이터 품질 점수 & 릴리스 게이트

자동 메타데이터 관리 스펙의 **Phase 4**(FR-META-023/024/025) 구현입니다.
`get_catalog_health`가 로드 오류·경고를 보고하는 것과 달리, 이 기능은 **모든
테이블의 메타데이터 품질을 정량 점수화**하고 A–E 등급을 매기며, 카탈로그
릴리스를 차단해야 하는 조건을 평가합니다.

구현: `internal/catalog/quality.go`.

## 품질 점수 (FR-META-023/024)

테이블마다 측정 가능한 7개 차원을 0–100으로 채점하고 가중 합산합니다.
(스펙의 나머지 차원 — 사용자 신뢰도·리니지 — 은 해당 서브시스템이 생기면
연결됩니다.)

| 차원 | 가중치 | 측정 내용 |
| --- | ---: | --- |
| 완전성(completeness) | 25 | 테이블 논리명·설명·도메인, 컬럼 논리명·설명 커버리지 |
| 일관성(consistency) | 15 | 모든 컬럼이 데이터 타입을 가짐 |
| 관계성(relationship) | 20 | 기본키 존재 + 조인 그래프 연결성 |
| 프로파일링(profiling) | 15 | 컬럼 통계(column_stats) 보유율 |
| 지표 연결(metric_link) | 5 | 지표 사전이 이 테이블을 참조 |
| 사용성(usability) | 10 | 골든 예제·피드백에서 사용됨 |
| 보안성(security) | 10 | PII로 보이는 컬럼이 모두 분류(pii=true)됨 |

### 등급 (FR-META-024)

| 등급 | 점수 | 운영 정책 |
| --- | ---: | --- |
| A | 90+ | NL2SQL 우선 검색 |
| B | 75+ | 일반 사용 |
| C | 60+ | 경고와 함께 사용 |
| D | 40+ | 검색 순위 하향 |
| E | <40 | 기본 검색 제외 |

리포트는 전체 점수·등급, 스키마별·도메인별 집계, 등급 분포, 그리고 **개선
대상 상위 목록**(점수 낮고 구체적 개선안이 있는 테이블)을 반환합니다. 각
테이블에는 issues(문제)와 suggestions(개선안: "논리명 추가", "기본키 선언",
"profile_metadata_assets 실행" 등)가 붙습니다.

## 릴리스 게이트 (FR-META-025)

카탈로그 릴리스를 차단해야 하는 조건을 평가해 `pass`(가능/금지)와 위반
목록을 반환합니다. `severity=block` 위반이 하나라도 있으면 릴리스가
금지됩니다.

| 코드 | 차단 조건 |
| --- | --- |
| `LOAD_ERROR` | 카탈로그 로드 오류(참조 오류·필수 모델 문제) |
| `NO_TABLES` | 컴파일된 테이블이 0개 |
| `METRIC_BROKEN` | 지표가 존재하지 않는 테이블을 참조 |
| `JOIN_BROKEN` | 인증(preferred) 조인이 존재하지 않는 테이블을 참조 |
| `PII_UNCLASSIFIED` | PII로 보이는 컬럼이 미분류(pii=false) |
| `QUALITY_FLOOR` | 전체 품질 점수가 릴리스 하한(기본 60) 미만 |

> **원칙**: 지표·인증 조인·PII는 무결성/컴플라이언스 항목이라 자동으로
> 차단합니다. 이는 Phase 1-2의 "삭제=폐기 후보" 원칙과 이어지는데, 삭제된
> 테이블을 참조하던 지표·조인이 릴리스를 막아 잘못된 SQL 생성을 예방합니다.

## MCP 도구 & REST

- **MCP `get_metadata_quality {gate?}`** — 기본은 전체 품질 리포트(테이블별
  점수·등급·차원·집계·개선 대상). `gate=true`면 릴리스 게이트 평가(pass +
  차단 위반 목록)로 전환.
- **REST `GET /api/metadata/quality`** (쿼리 `?gate=true`로 게이트 평가).

DB를 건드리지 않는 카탈로그 전용 도구라 별도 프로파일 권한이 필요 없습니다.

## 사용 예시

```jsonc
// 품질 리포트
get_metadata_quality {}
→ { "overall_score": 76.3, "overall_grade": "B", "table_count": 7,
    "grade_counts": {"B":4,"C":3}, "by_schema": {"PUBLIC":76.3},
    "tables": [ { "table":"PUBLIC.JAMYPG_SETTINGS", "score":60, "grade":"C",
                  "dimensions":{...}, "suggestions":["add a table description", ...] } ],
    "top_improvement_targets": [ ... ] }

// 릴리스 게이트
get_metadata_quality { "gate": true }
→ { "pass": true, "overall_score": 76.3, "overall_grade": "B",
    "violations": [], "note": "품질 게이트 통과 — 릴리스 가능" }

// 지표가 삭제된 테이블을 참조하면
→ { "pass": false, "violations": [
      { "code":"METRIC_BROKEN", "severity":"block",
        "message":"metric 'X' references a missing table", "detail":"..." } ] }
```

## 참고

- [metadata-sync.md](metadata-sync.md) — Phase 1-2 수집·스냅숏·프로파일링
- [mcp-tools-reference.md](mcp-tools-reference.md) — 전체 MCP 도구
- 코드: `internal/catalog/quality.go`, `internal/mcp/{server,admin}.go`
