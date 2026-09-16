package mail

import (
	"fmt"
	"strings"
	"time"
)

// Notification is the content of one event mail before recipients are
// resolved. Ref identifies the object (change id, job id, sync source) and is
// recorded with the delivery so the log can be joined back to the event.
type Notification struct {
	Event   string
	Subject string
	Lines   []string
	Path    string // app path appended to mail.base_url for the "open" line
	Ref     string
}

// Render produces the plain-text body: the lines, an optional link, and a
// footer that says why the mail arrived.
func (n Notification) Render(cfg Config) string {
	lines := append([]string{}, n.Lines...)
	if link := cfg.Link(n.Path); link != "" {
		lines = append(lines, "", "바로 열기: "+link)
	}
	lines = append(lines, "", "—", "이 메일은 sqlon 관리자 설정(메일 알림)에 따라 자동으로 발송되었습니다.")
	return strings.Join(lines, "\n")
}

// ChangeReviewRequired tells approvers that a plan is waiting on them.
func ChangeReviewRequired(actor, planID, profile, target, risk, reason string, required int) Notification {
	return Notification{
		Event:   EventChangeReviewRequired,
		Subject: fmt.Sprintf("[sqlon] 변경 계획 승인 요청: %s (%s)", target, risk),
		Ref:     planID,
		Path:    "/admin/changes",
		Lines: []string{
			fmt.Sprintf("%s 님이 변경 계획 %s 을(를) 제출했습니다. 승인 %d건이 모이면 실행할 수 있습니다.", label(actor), planID, required),
			"프로파일: " + profile,
			"대상: " + target,
			"위험도: " + risk,
			"사유: " + clip(reason, 300),
		},
	}
}

// ChangeApproved tells the DBA group a plan has every approval and can run.
func ChangeApproved(actor, planID, profile, target, risk string) Notification {
	return Notification{
		Event:   EventChangeApproved,
		Subject: fmt.Sprintf("[sqlon] 변경 계획 실행 가능: %s", target),
		Ref:     planID,
		Path:    "/admin/changes",
		Lines: []string{
			fmt.Sprintf("%s 님의 승인으로 변경 계획 %s 의 승인이 모두 모였습니다. 유지보수 창 안에서 실행하세요.", label(actor), planID),
			"프로파일: " + profile,
			"대상: " + target,
			"위험도: " + risk,
		},
	}
}

// ChangeFailed reports an execution or rollback that stopped with an error and
// now needs a person. The error text is clipped; SQL is never included.
func ChangeFailed(actor, planID, profile, target, state, cause string) Notification {
	return Notification{
		Event:   EventChangeFailed,
		Subject: fmt.Sprintf("[sqlon] 변경 실행 실패 (%s): %s", state, target),
		Ref:     planID,
		Path:    "/admin/changes",
		Lines: []string{
			fmt.Sprintf("%s 님이 실행한 변경 계획 %s 이(가) 실패로 멈췄습니다. 현재 상태: %s", label(actor), planID, state),
			"프로파일: " + profile,
			"대상: " + target,
			"오류: " + clip(cause, 300),
			"",
			"rollback_required 상태이면 변경 관리 화면에서 롤백을 실행하세요.",
		},
	}
}

// QueryFinished tells the person who submitted a long async query that it is
// done. The SQL is not repeated — it may carry filter values — only the
// outcome and where to fetch the result.
func QueryFinished(jobID, profile, status string, took time.Duration, rows int, cause string) Notification {
	took = took.Round(time.Second)
	if status == "done" {
		return Notification{
			Event:   EventQueryFinished,
			Subject: fmt.Sprintf("[sqlon] 비동기 쿼리 완료: %s (%s)", profile, took),
			Ref:     jobID,
			Path:    "/admin/db",
			Lines: []string{
				fmt.Sprintf("프로파일 %s 에 제출한 비동기 쿼리 %s 이(가) %s 만에 끝났습니다.", profile, jobID, took),
				fmt.Sprintf("반환 행: %d", rows),
				"결과는 GET /api/query/job/" + jobID + " 또는 DB 화면의 실행 이력에서 확인하세요.",
			},
		}
	}
	return Notification{
		Event:   EventQueryFinished,
		Subject: fmt.Sprintf("[sqlon] 비동기 쿼리 실패: %s (%s)", profile, took),
		Ref:     jobID,
		Path:    "/admin/db",
		Lines: []string{
			fmt.Sprintf("프로파일 %s 에 제출한 비동기 쿼리 %s 이(가) %s 뒤 실패했습니다.", profile, jobID, took),
			"오류: " + clip(cause, 300),
		},
	}
}

// SchedulerFailed is sent once when the scheduled metadata sync starts
// failing, not on every tick.
func SchedulerFailed(source, cause string) Notification {
	return Notification{
		Event:   EventSchedulerFailed,
		Subject: "[sqlon] 예약 메타데이터 동기화 실패: " + source,
		Ref:     source,
		Path:    "/admin",
		Lines: []string{
			fmt.Sprintf("프로파일 %s 의 예약 동기화가 실패했습니다. 회복될 때까지 다시 알리지 않습니다.", source),
			"오류: " + clip(cause, 300),
		},
	}
}

// SchedulerRecovered closes the loop opened by SchedulerFailed.
func SchedulerRecovered(source string) Notification {
	return Notification{
		Event:   EventSchedulerRecovered,
		Subject: "[sqlon] 예약 메타데이터 동기화 회복: " + source,
		Ref:     source,
		Path:    "/admin",
		Lines:   []string{fmt.Sprintf("프로파일 %s 의 예약 동기화가 다시 성공했습니다.", source)},
	}
}

// TestMessage is what the admin's test button sends.
func TestMessage() Notification {
	return Notification{
		Event:   EventTest,
		Subject: "[sqlon] SMTP 시험 발송",
		Lines:   []string{"sqlon 서버 설정 화면에서 보낸 시험 메일입니다.", "이 메일을 받았다면 SMTP 릴레이 설정이 정상입니다."},
	}
}

func label(actor string) string {
	if strings.TrimSpace(actor) == "" {
		return "알 수 없는 사용자"
	}
	return strings.TrimSpace(actor)
}

func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(없음)"
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
