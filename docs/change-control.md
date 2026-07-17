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

## 실행 경로

- 실행은 `execute_approved_change`(MCP) 또는 `POST /api/changes/{id}/execute`
  (REST, 승인 필요 위험도에는 `X-Approval-ID` 헤더 필수)만 가능합니다.
- 실행 직전 재검증(Revalidate): DBA 실행 자격 증명 존재 + 연결 확인.
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
| MCP | `create_change_plan`, `evaluate_change_risk`, `submit_change`, `approve_change`, `execute_approved_change`, `verify_change`, `rollback_change`, `cancel_change` |
| REST | `GET /api/changes`(목록, 최신순), `POST /api/changes`, `GET /api/changes/{id}`, `POST /api/changes/{id}/submit`, `/approve`, `/execute`, `/rollback`, `/cancel` |

세 표면 모두 동일한 `internal/change.Service`를 호출하므로 정책이 갈라질
수 없습니다. REST 변경 API는 DBA 권한(`requireDBA`)을 요구하며, 웹 콘솔의
실행 버튼도 승인 ID(`X-Approval-ID`)와 실행 전 대상 재확인을 거칩니다.
