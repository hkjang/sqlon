# 변경 통제 (Change Control)

SQLON의 모든 데이터베이스 쓰기 작업은 **승인 기반 변경계획(ChangePlan)** 을
거칩니다. AI와 클라이언트는 임의의 권한 SQL을 직접 실행할 수 없으며,
구조화된 계획을 생성·제출·승인받은 뒤에만 내부 실행기가 승인된 단계를
실행합니다. (원칙: Plan Before Change, Human Controlled Execution)

## 상태 모델

```text
draft → analyzing → review_required → approved → (scheduled) → executing
  → verifying → completed
  → failed / rollback_required → rolling_back → rolled_back
draft/analyzing/review_required/approved/scheduled → cancelled
```

전이 규칙은 서버가 강제합니다(`internal/change/plan.go`). `executing` 진입
시 필요 승인 수를 다시 검사하므로 클라이언트가 승인 정책을 우회할 수
없습니다.

## 위험도와 승인 정책

| 위험도 | 필요 승인 |
| --- | --- |
| low | 0 |
| medium, high | 1 |
| critical | 2 (서로 다른 승인자) |
| emergency | 1 |

`required_approvals`는 서버 정책 값으로, 요청 JSON에 더 작은 값을 넣어도
생성 시 덮어써집니다. 동일 승인자의 중복 승인은 거부됩니다.

## 계획 필수 필드

계획의 모든 단계(step)는 `order`(1부터 연속), `command`(실행 문장),
`verification`(사후 검증 문장), `compensation`(보상/역작업 문장)을 모두
포함해야 검증을 통과합니다. 보상 작업이 없는 변경은 생성 자체가 거부됩니다.

## 안전 단계 템플릿 (구조화된 권한 작업)

되돌릴 수 있고 검증 가능한 권한 작업은 손으로 SQL을 쓰지 않고 **구조화된
액션**에서 단계를 생성할 수 있습니다. `build_change_step`(MCP) /
`POST /api/changes/template`(REST)는 `{profile, action, args}`를 받아
방언별로 **안전하게 인용된**(SEC-011) `command`·`verification`·
`compensation` 3종을 반환합니다. 이 호출은 DB를 변경하지 않습니다.

| 액션 | command | 보상(compensation) |
| --- | --- | --- |
| `create_user` | CREATE ROLE/USER (NOLOGIN·기본 호스트) | DROP ROLE/USER IF EXISTS |
| `create_database` | CREATE DATABASE (owner·encoding) | DROP DATABASE IF EXISTS |
| `grant` | GRANT … TO grantee | REVOKE … FROM grantee |
| `revoke` | REVOKE … FROM grantee | GRANT … TO grantee |

규칙:

- 객체명은 방언별 인용 함수로 감싸므로 식별자 주입이 불가능하며, 제어
  문자가 든 식별자와 문장 구분자(`;`)가 든 권한/객체는 거부됩니다.
- **비밀번호는 계획에 저장할 수 없습니다** — 비밀번호 없이 계정을 만들고
  secret 참조로 별도 설정하세요(SEC-002/SEC-007).
- 되돌릴 수 없는 작업(DROP, 파괴적 파라미터 변경 등)은 템플릿을 제공하지
  않습니다. 운영자가 실행·검증·보상을 직접 작성해 자동 복구에 대한 잘못된
  기대를 갖지 않도록 합니다.
- 이것이 레거시 `dba_*` 안전 SQL 빌더의 유일한 사용처입니다 — 직접 실행이
  아니라 승인 흐름을 타는 단계 생성기로만 살아 있습니다(요건 §14).
- 웹 콘솔의 변경계획 작성 폼에서 단계별 "안전 템플릿"으로 사용할 수
  있습니다.

## 유지보수 창 (실행 시간 제약)

`maintenance_window`는 표시용 자유 텍스트이고, 실행을 실제로 제약하려면
구조화된 `maintenance_windows`(배열)를 사용합니다.

```json
"maintenance_windows": [{ "days": ["sat", "sun"], "start": "17:00", "end": "19:00" }]
```

- 시각은 **UTC 기준 HH:MM**이며, `end`가 `start`보다 크지 않으면 자정을
  넘겨 다음 날로 이어집니다(창의 시작 요일 기준).
- `days`가 비면 매일입니다.
- 창이 설정된 비-비상(non-emergency) 계획은 창 밖에서 실행할 수 없습니다.
  차단은 상태를 바꾸지 않는 **재시도 가능한 거부**이므로, 창이 열리면 같은
  승인된 계획이 그대로 실행됩니다.
- **emergency 위험도는 창을 우회**합니다 — 장애 대응이 유지보수 일정에
  막히지 않도록 합니다.

## 실행 경로

- 실행은 `execute_approved_change`(MCP) 또는 `POST /api/changes/{id}/execute`
  (REST, 승인 필요 위험도에는 `X-Approval-ID` 헤더 필수)만 가능합니다.
- 실행 직전 재검증(Revalidate): DBA 실행 자격 증명 존재 + 연결 확인, 그리고
  구조화된 유지보수 창 게이트 통과.
- 각 단계의 `command`는 쓰기 가능 관리 풀로, `verification`은 읽기 전용
  풀로 실행됩니다.
- 실패 시 `rollback_required`로 전이되며, `rollback_change`가 보상 작업을
  역순 실행합니다.
- 레거시 `dba_create_user`/`dba_grant`/`dba_set_parameter`/`dba_execute` 등
  직접 실행 도구는 MCP 도구 목록에서 제거되었고 호출 시 deprecated 오류를
  반환합니다(내부 실행기 전용).

## 영속화

변경계획·승인·실행 결과는 `<data>/changes/` 아래에 계획당 하나의 JSON
파일로 저장되어 **프로세스 재시작 후에도 유지**됩니다
(`internal/change/store.go`).

- 쓰기는 원자적(temp 파일 + rename)이며, 파일명은 계획 ID를 새니타이즈하고
  해시를 붙여 만들므로 경로 이탈이나 ID 충돌이 불가능합니다.
- 멱등키(`Idempotency-Key`)는 `_idempotency.json`에 함께 저장되어 재시작
  후에도 같은 키의 재요청이 기존 계획을 반환합니다.
- 순수 상태 전이(생성·제출·승인·취소·실행 개시)는 **디스크 저장 성공 후에만
  메모리에 반영**됩니다 — 디스크 오류 시 상태가 변하지 않아 재시도할 수
  있습니다.
- 권한 SQL이 이미 실행된 뒤의 결과 상태(completed/failed/rollback_required/
  rolled_back)는 메모리에 반영하고 저장 오류를 함께 반환합니다 — DB에서
  일어난 사실이 항상 우선합니다(No Silent Failure).
- 시작 시 일부 파일이 손상되어 복구 불가하면 복구 가능한 계획만 로드하고
  손실을 로그로 보고합니다. 취소·롤백된 계획도 삭제하지 않고 변경 이력으로
  보존합니다.

## API 요약

| 인터페이스 | 표면 |
| --- | --- |
| 웹 콘솔 | `/admin/changes` — 변경계획 목록·작성·제출·승인·실행·롤백·취소, 단계별 실행/검증/보상 문장 표시 |
| MCP | `create_change_plan`, `evaluate_change_risk`, `build_change_step`, `submit_change`, `approve_change`, `execute_approved_change`, `verify_change`, `rollback_change`, `cancel_change` |
| REST | `GET /api/changes`(목록, 최신순), `POST /api/changes`, `POST /api/changes/template`, `GET /api/changes/{id}`, `POST /api/changes/{id}/submit`, `/approve`, `/execute`, `/rollback`, `/cancel` |

세 표면 모두 동일한 `internal/change.Service`를 호출하므로 정책이 갈라질
수 없습니다. REST 변경 API는 DBA 권한(`requireDBA`)을 요구하며, 웹 콘솔의
실행 버튼도 승인 ID(`X-Approval-ID`)와 실행 전 대상 재확인을 거칩니다.
