package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"sqlon/internal/change"
	"sqlon/internal/mail"
	"sqlon/internal/meta"
)

// Event mail wiring. The mail service lives only when the meta DB is enabled:
// it needs the settings table for its configuration and the user table for
// addresses. Standalone mode has neither, so it sends nothing — which is also
// the default with a meta DB until an admin turns mail.enabled on.
//
// Events were chosen by one rule — if this mail does not arrive, somebody
// loses time or keeps refreshing a screen:
//   - change.review_required / change.approved → dba/admin users (approvers,
//     then executors) minus the actor
//   - change.failed → dba/admin users minus the actor
//   - query.finished → the user who submitted an async query that ran ≥ 1 min
//   - scheduler.failed / recovered → admin users, on the transition only

// asyncMailAfter is the minimum async-query runtime before a completion mail
// is worth sending; shorter jobs are still on screen. Var so tests can lower it.
var asyncMailAfter = time.Minute

// metaSettings adapts the meta service to mail.Settings.
type metaSettings struct{ s *Server }

func (m metaSettings) Values(ctx context.Context) (map[string]string, error) {
	return m.s.Meta.EffectiveSettings(ctx, m.s.bootDefaults)
}

// metaDirectory adapts the existing user table to mail.Directory. Only active
// users with an address resolve; usernames are matched case-insensitively.
type metaDirectory struct{ svc *meta.Service }

func (d metaDirectory) Emails(ctx context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	for _, id := range ids {
		u, err := d.svc.Store.GetUserByUsername(ctx, id)
		if err != nil || u == nil {
			u, err = d.svc.Store.GetUserByID(ctx, id)
		}
		if err != nil || u == nil || !u.IsActive || strings.TrimSpace(u.Email) == "" {
			continue
		}
		out[strings.ToLower(id)] = strings.TrimSpace(u.Email)
	}
	return out, nil
}

// enableMail is called from EnableMeta once the meta service is attached.
func (s *Server) enableMail(svc *meta.Service) {
	s.Mail = mail.New(metaSettings{s: s}, metaDirectory{svc: svc}, s.opDir())
}

// notifyMail is the single entry point every event uses. Nil-safe so callers
// never check the mode; Notify itself never blocks the request.
func (s *Server) notifyMail(ctx context.Context, n mail.Notification, actor string, recipients []string) {
	if s.Mail == nil || len(recipients) == 0 {
		return
	}
	s.Mail.Notify(context.WithoutCancel(ctx), n, actor, recipients)
}

// restActor names the caller of a REST change handler. Those handlers gate on
// requireDBA, which authenticates but does not put the user in the context,
// so ask again; the name only drives the "not to yourself" rule.
func (s *Server) restActor(r *http.Request) string {
	if u := userFrom(r.Context()); u != nil {
		return u.Username
	}
	if s.Meta == nil {
		return ""
	}
	u, _ := s.authenticate(r)
	return actorName(u)
}

// usersWithRole lists active usernames that pass the filter — the approver
// (dba/admin) and admin audiences. The user table is the only roster.
func (s *Server) usersWithRole(ctx context.Context, pass func(*meta.User) bool) []string {
	if s.Meta == nil {
		return nil
	}
	users, err := s.Meta.Store.ListUsers(ctx)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(users))
	for _, u := range users {
		if u != nil && u.IsActive && pass(u) {
			out = append(out, u.Username)
		}
	}
	return out
}

// notifyChange mails the DBA group when a plan transition is one somebody is
// waiting on. Called after Submit/Approve/Execute/Rollback from both the REST
// handlers and the MCP tools so neither path can forget it. Transitions that
// only the actor cares about (cancel, completed, refused by the maintenance
// window) send nothing.
func (s *Server) notifyChange(ctx context.Context, p change.Plan, actor string, callErr error) {
	if s.Mail == nil || p.ID == "" {
		return
	}
	var n mail.Notification
	switch {
	// review_required with no approval yet is the submission. A plan that needs
	// several approvals (critical) stays in this state after the first one; that
	// is not a new request, and the approver is not its submitter, so nothing
	// goes out until every approval is in.
	case callErr == nil && p.State == change.ReviewRequired && len(p.Approvals) == 0:
		n = mail.ChangeReviewRequired(actor, p.ID, p.ProfileID, p.Target, string(p.Risk), p.Reason, p.RequiredApprovals)
	case callErr == nil && p.State == change.Approved && p.RequiredApprovals > 0:
		n = mail.ChangeApproved(actor, p.ID, p.ProfileID, p.Target, string(p.Risk))
	case callErr != nil && (p.State == change.RollbackRequired || p.State == change.Failed):
		n = mail.ChangeFailed(actor, p.ID, p.ProfileID, p.Target, string(p.State), callErr.Error())
	default:
		return
	}
	s.notifyMail(ctx, n, actor, s.usersWithRole(ctx, (*meta.User).IsDBA))
}

// notifyAsyncFinished tells the submitter when a long-running async query is
// done. Short jobs are skipped: the person is still looking at the screen.
func (s *Server) notifyAsyncFinished(job *asyncJob) {
	if s.Mail == nil || job == nil || job.FinishedAt == nil || strings.TrimSpace(job.User) == "" {
		return
	}
	took := job.FinishedAt.Sub(job.StartedAt)
	if took < asyncMailAfter {
		return
	}
	rows := 0
	if job.Result != nil {
		rows = job.Result.RowCount
	}
	n := mail.QueryFinished(job.ID, job.ProfileID, job.Status, took, rows, job.Error)
	s.notifyMail(context.Background(), n, "", []string{job.User})
}

// notifySchedulerState mails admins when the scheduled sync flips between
// working and failing — once per transition, never per tick.
func (s *Server) notifySchedulerState(ctx context.Context, source string, failed bool, cause string) {
	if s.Mail == nil {
		return
	}
	s.mailMu.Lock()
	if s.schedulerFailing == nil {
		s.schedulerFailing = map[string]bool{}
	}
	was := s.schedulerFailing[source]
	s.schedulerFailing[source] = failed
	s.mailMu.Unlock()
	if was == failed {
		return
	}
	n := mail.SchedulerRecovered(source)
	if failed {
		n = mail.SchedulerFailed(source, cause)
	}
	s.notifyMail(ctx, n, "", s.usersWithRole(ctx, (*meta.User).IsAdmin))
}

// registerMailAPI adds the admin endpoints: the delivery log and the test
// button. Both require the meta DB and the admin role.
func (s *Server) registerMailAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/mail/deliveries", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMeta(w) || !s.requireAdmin(w, r) {
			return
		}
		if s.Mail == nil {
			writeJSON(w, http.StatusOK, mail.Page{Items: []mail.Delivery{}, Status: map[string]int{}})
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		writeJSON(w, http.StatusOK, s.Mail.Deliveries(r.URL.Query().Get("status"), limit))
	})
	mux.HandleFunc("POST /api/mail/test", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMeta(w) || !s.requireAdmin(w, r) {
			return
		}
		actor, _ := s.authenticate(r)
		var req struct {
			Recipient string `json:"recipient"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil && err != io.EOF {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		to := strings.TrimSpace(req.Recipient)
		if to == "" && actor != nil {
			to = strings.TrimSpace(actor.Email)
		}
		if !strings.Contains(to, "@") {
			writeAPIError(w, http.StatusBadRequest, errEmpty("recipient must be an email address (계정에 메일 주소가 없으면 직접 입력)"))
			return
		}
		if s.Mail == nil {
			writeAPIError(w, http.StatusServiceUnavailable, errEmpty("mail service unavailable"))
			return
		}
		err := s.Mail.SendNow(r.Context(), mail.TestMessage(), actorName(actor), to)
		s.adminAudit(r, "mail_test", to, err)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "recipient": to, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "recipient": to})
	})
}
