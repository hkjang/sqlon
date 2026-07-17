# SQLON 문서

SQLON은 PostgreSQL·MySQL·MariaDB·Oracle을 관찰하고 진단하며 승인된 변경을
통제하는 AI DBA 운영 플랫폼입니다. 기존 메타데이터 기반 NL2SQL은 SQL Lab으로
유지됩니다. 이 디렉터리는 대상 독자별 전문 문서를 제공합니다.

## 문서 목록

| 문서 | 대상 | 내용 |
| --- | --- | --- |
| [architecture.md](architecture.md) | 아키텍트, 개발자 | 시스템 구조, 카탈로그 컴파일 파이프라인, 핫스왑, 트랜스포트 |
| [mcp-tools-reference.md](mcp-tools-reference.md) | MCP 클라이언트 개발자, LLM 통합 | MCP 도구 전체 레퍼런스 (파라미터·응답·사용 시점) |
| [sql-generation-workflow.md](sql-generation-workflow.md) | LLM 통합, 프롬프트 엔지니어 | 질문→SQL 표준 워크플로 10단계와 엔드투엔드 예시 |
| [validation-rules.md](validation-rules.md) | LLM 통합, 운영자 | validate_sql 오류/경고 코드 35종 카탈로그와 수정 방법 |
| [datasets.md](datasets.md) | 운영자, 데이터 관리자 | 18개 데이터셋 스키마·작성 규칙·예시 (라이브: `list_datasets`) |
| [rest-api.md](rest-api.md) | 관리도구 개발자, 운영자 | REST 관리 API, 인증, curl 예시 (라이브: `/docs` Swagger) |
| [db-connector.md](db-connector.md) | 운영자, DBA | 4개 엔진 DB 프로파일, 읽기 전용 실행 파이프라인, 장애 대응, 메트릭과 Oracle 별도 배포판 |
| [fleet.md](fleet.md) | DBA, SRE, 운영자 | 첫 화면의 DB 플릿 인벤토리, 위험 순위, 접근 권한, 증거·최신성 응답과 현재 관찰 범위 |
| [observability.md](observability.md) | DBA, SRE, 운영자 | 대상 DB 워크로드·대기·Top SQL·용량 시계열 수집, 보존 정책과 엔진별 원천 |
| [change-control.md](change-control.md) | DBA, 운영자 | 승인 기반 변경 통제: 변경계획 상태 모델, 위험도·승인 정책, 실행·보상 경로, 영속화와 REST/MCP 표면 |
| [metadata-sync.md](metadata-sync.md) | 운영자, 데이터 관리자 | 자동 메타데이터 수집(Phase 1-2): 물리 자동 수집·스냅숏·구조 해시·증분 변경감지, 스냅숏→카탈로그 자동 반영, read-only 카탈로그 접근, MCP/REST 인터페이스 |
| [profile-catalogs.md](profile-catalogs.md) | 운영자, 데이터 관리자 | DB 프로파일별 카탈로그 워크스페이스: 프로파일마다 독립 메타데이터 JSON을 라이브 DB로 구축·조회·관리(`list/get/build_profile_catalog`, `get/put_profile_dataset`) |
| [onboarding.md](onboarding.md) | 신규 사용자 | 빠른 시작 온보딩 가이드 — 관리 콘솔 좌측 **📖 온보딩 가이드**로 화면 모달에서도 동일 내용 제공 |
| [metadata-quality.md](metadata-quality.md) | 메타데이터 품질 점수(A–E)와 릴리스 차단 게이트 |
| [metadata-enrich.md](metadata-enrich.md) | 운영자, 데이터 관리자, LLM 통합 | 규칙 기반 의미 메타데이터 보강(Phase 5): 논리명·의미타입·설명 **검토 후보** 생성, 근거·신뢰도, overrides.json 스니펫, `suggest_semantic_metadata` |
| [metadata-candidates.md](metadata-candidates.md) | 운영자, 데이터 관리자, LLM 통합 | 규칙 기반 모델 후보(Phase 6): 코드사전·지표·관계 **검토 후보** 생성, `suggest_model_candidates` |
| [metadata-impact.md](metadata-impact.md) | 운영자, 데이터 관리자 | 계보/영향도 분석(Phase 7): 테이블/컬럼 변경 전 의존 지표·관계·조인·골든셋·용어집·하위 테이블 역추적, impact_level, `analyze_impact` |
| [metadata-review.md](metadata-review.md) | 운영자, 데이터 관리자, LLM 통합 | 후보 승인 워크플로(Phase 9): 검토 큐·승인/반려 이력·적용 스니펫, `/admin/reviews` UI, `review_candidates`/`decide_candidates`/`get_approved_overrides` |
| [openmetadata.md](openmetadata.md) | 운영자, 데이터 관리자 | OpenMetadata 양방향 연동: import(설명·PII·용어집→빈 필드 후보), export(설명 push), 자동화. `import_openmetadata`/`export_to_openmetadata`/`openmetadata_status` |
| [auth.md](auth.md) | 운영자, 보안 담당 | 인증·권한·MCP 키: Postgres 메타 DB, 로컬/Keycloak SSO 로그인, 역할·프로파일 권한, MCP 키 라이프사이클 |
| [operations.md](operations.md) | 운영자, SRE | 배포, 모니터링, 백업/복원, 피드백 학습 루프, 트러블슈팅 |
| [migration.md](migration.md) | 운영자, SRE | 기존 JAMYPG 데이터의 백업 우선 자동 마이그레이션과 복구 |
| [evaluation.md](evaluation.md) | QA, 운영자 | 골든셋 평가 체계, 지표 정의, CI 통합, 골든셋 확장 |
| [security.md](security.md) | 보안 담당자 | 읽기전용 정책, PII 차단, 인증, 감사 로그, 위협 모델 |
| [development.md](development.md) | 개발자 | 빌드/테스트, 코드 구조, 확장 포인트 (도구·데이터셋·룰 추가법) |
| [json-data-tool-map.md](json-data-tool-map.md) | (레거시) | v0.1 시점 스냅샷 — [datasets.md](datasets.md)로 대체됨 |

## 빠른 시작별 진입점

- **MCP 클라이언트를 붙이려면**: [mcp-tools-reference.md](mcp-tools-reference.md) → [sql-generation-workflow.md](sql-generation-workflow.md)
- **메타데이터를 관리하려면**: 웹 콘솔 `/admin` → [datasets.md](datasets.md)
- **운영 배포하려면**: 릴리즈의 `DEPLOY-OFFLINE.md` → [operations.md](operations.md)
- **정확도를 검증하려면**: [evaluation.md](evaluation.md)
- **코드를 고치려면**: [architecture.md](architecture.md) → [development.md](development.md)

## 살아있는 문서 (서버가 직접 제공)

정적 문서와 별개로, 실행 중인 서버가 항상 최신 상태를 제공합니다:

| 경로/도구 | 내용 |
| --- | --- |
| `GET /docs` | Swagger UI (REST API 문서 + Try it out) |
| `GET /admin` | 데이터셋 관리 콘솔 (사용 가이드 내장) |
| MCP `list_datasets` | 데이터셋 레지스트리 + 라이브 상태 |
| MCP `get_catalog_health` | 카탈로그 컴파일 상태·이슈 |
| MCP `tools/list` | 도구 스키마 원본 |
