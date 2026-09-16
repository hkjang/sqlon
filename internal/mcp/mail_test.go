package mcp

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sqlon/internal/change"
	"sqlon/internal/dbconn"
	"sqlon/internal/mail"
	"sqlon/internal/meta"
)

// mailSink replaces the SMTP transport so tests see what would have left.
type mailSink struct {
	mu   sync.Mutex
	sent []mail.Message
	fail error
}

func (m *mailSink) send(_ context.Context, _ mail.Config, msg mail.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.sent = append(m.sent, msg)
	return nil
}

func (m *mailSink) recipients() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []string{}
	for _, s := range m.sent {
		out = append(out, s.To)
	}
	return out
}

// newMailServer builds an auth-enabled server with addresses on the accounts:
// admin (approver), dan (dba, approver), alice (plain user, never mailed).
func newMailServer(t *testing.T) (*Server, *http.ServeMux, map[string]string, *mailSink) {
	t.Helper()
	s, mux, adminTok, aliceTok := newAuthServer(t)
	ctx := context.Background()
	if _, err := s.Meta.CreateLocalUser(ctx, "dan", "danpass123", meta.RoleDBA, "Dan", "dan@corp.test"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"admin", "alice"} {
		u, err := s.Meta.Store.GetUserByUsername(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		u.Email = name + "@corp.test"
		if err := s.Meta.Store.UpdateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	rec := doReq(t, mux, "POST", "/auth/login", `{"username":"dan","password":"danpass123"}`, nil)
	danTok := ""
	for _, c := range rec.Result().Cookies() {
		if c.Name == meta.SessionCookie {
			danTok = c.Value
		}
	}
	if danTok == "" {
		t.Fatalf("dan login: %d %s", rec.Code, rec.Body.String())
	}
	sink := &mailSink{}
	s.Mail.SetSender(sink.send)
	return s, mux, map[string]string{"admin": adminTok, "alice": aliceTok, "dan": danTok}, sink
}

func putSettings(t *testing.T, mux *http.ServeMux, tok string, kv map[string]string) {
	t.Helper()
	b, _ := json.Marshal(kv)
	rec := doReq(t, mux, "PUT", "/api/settings", string(b), withCookie(tok))
	if rec.Code != 200 {
		t.Fatalf("put settings: %d %s", rec.Code, rec.Body.String())
	}
}

func enableMail(t *testing.T, mux *http.ServeMux, tok string, extra map[string]string) {
	t.Helper()
	kv := map[string]string{mail.KeyEnabled: "true", mail.KeyHost: "relay.corp.test", mail.KeyFromAddress: "sqlon@corp.test",
		mail.KeyBaseURL: "https://sqlon.corp.test"}
	for k, v := range extra {
		kv[k] = v
	}
	putSettings(t, mux, tok, kv)
}

func submitPlan(t *testing.T, mux *http.ServeMux, tok, id string) {
	t.Helper()
	body := strings.Replace(changePlanBody, `"chg-http-1"`, `"`+id+`"`, 1)
	if rec := doReq(t, mux, "POST", "/api/changes", body, withCookie(tok)); rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, mux, "POST", "/api/changes/"+id+"/submit", "", withCookie(tok)); rec.Code != 200 {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
	}
}

func deliveries(t *testing.T, mux *http.ServeMux, tok, query string) mail.Page {
	t.Helper()
	rec := doReq(t, mux, "GET", "/api/mail/deliveries"+query, "", withCookie(tok))
	if rec.Code != 200 {
		t.Fatalf("deliveries: %d %s", rec.Code, rec.Body.String())
	}
	var page mail.Page
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page
}

// A fresh install changes nothing: the keys exist, none is set, no mail is
// sent or recorded, and the test button explains why.
func TestMailDefaultOffOnFreshInstall(t *testing.T) {
	s, mux, toks, sink := newMailServer(t)

	rec := doReq(t, mux, "GET", "/api/settings", "", withCookie(toks["admin"]))
	var view struct {
		Settings []map[string]any `json:"settings"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	seen := map[string]bool{}
	for _, def := range view.Settings {
		key, _ := def["key"].(string)
		if strings.HasPrefix(key, "mail.") {
			seen[key] = true
			if set, _ := def["is_set"].(bool); set {
				t.Fatalf("%s should be unset on a fresh install", key)
			}
		}
	}
	for _, key := range []string{mail.KeyEnabled, mail.KeyHost, mail.KeyPort, mail.KeySecurity, mail.KeySkipTLSVerify, mail.KeyUsername,
		mail.KeyPassword, mail.KeyFromAddress, mail.KeyFromName, mail.KeyBaseURL, mail.KeyTimeout,
		mail.KeyNotifyChangeReview, mail.KeyNotifyChangeFailed, mail.KeyNotifyQueryFinished, mail.KeyNotifyScheduler} {
		if !seen[key] {
			t.Fatalf("setting %s is not defined in meta.SettingDefs", key)
		}
	}

	submitPlan(t, mux, toks["admin"], "chg-off")
	s.Mail.Wait()
	if len(sink.sent) != 0 {
		t.Fatalf("mail sent while disabled: %+v", sink.sent)
	}
	if page := deliveries(t, mux, toks["admin"], ""); page.Total != 0 {
		t.Fatalf("nothing should be recorded while disabled: %+v", page)
	}
	rec = doReq(t, mux, "POST", "/api/mail/test", `{"recipient":"ops@corp.test"}`, withCookie(toks["admin"]))
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "mail.enabled") {
		t.Fatalf("test send while disabled: %d %s", rec.Code, rec.Body.String())
	}
	// Enabled but without a host: still nothing, and the reason names the key.
	putSettings(t, mux, toks["admin"], map[string]string{mail.KeyEnabled: "true"})
	submitPlan(t, mux, toks["admin"], "chg-nohost")
	s.Mail.Wait()
	if len(sink.sent) != 0 || deliveries(t, mux, toks["admin"], "").Total != 0 {
		t.Fatal("incomplete config must not send or record")
	}
	rec = doReq(t, mux, "POST", "/api/mail/test", `{"recipient":"ops@corp.test"}`, withCookie(toks["admin"]))
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), mail.KeyHost) {
		t.Fatalf("test send without host: %d %s", rec.Code, rec.Body.String())
	}
	// Non-admins get neither the log nor the button.
	if rec := doReq(t, mux, "GET", "/api/mail/deliveries", "", withCookie(toks["alice"])); rec.Code != 403 {
		t.Fatalf("alice deliveries: %d", rec.Code)
	}
	if rec := doReq(t, mux, "POST", "/api/mail/test", `{}`, withCookie(toks["dan"])); rec.Code != 403 {
		t.Fatalf("dba test send should be 403, got %d", rec.Code)
	}
}

func TestMailChangeReviewGoesToApproversNotActor(t *testing.T) {
	s, mux, toks, sink := newMailServer(t)
	enableMail(t, mux, toks["admin"], nil)

	// admin submits → dan (dba) is told; admin (actor) and alice (user) are not.
	submitPlan(t, mux, toks["admin"], "chg-review")
	s.Mail.Wait()
	if got := sink.recipients(); len(got) != 1 || got[0] != "dan@corp.test" {
		t.Fatalf("review_required recipients: %v", got)
	}
	m := sink.sent[0]
	if !strings.Contains(m.Subject, "승인 요청") || !strings.Contains(m.Body, "chg-review") ||
		!strings.Contains(m.Body, "https://sqlon.corp.test/admin/changes") || !strings.Contains(m.Body, "admin 님이") {
		t.Fatalf("review mail: %+v", m)
	}
	if strings.Contains(m.Body, "CREATE INDEX") {
		t.Fatal("mail body must not carry the plan SQL")
	}

	// dan approves → admin is told it can run; dan (actor) is not.
	rec := doReq(t, mux, "POST", "/api/changes/chg-review/approve", "", withCookie(toks["dan"]))
	if rec.Code != 200 {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	s.Mail.Wait()
	if got := sink.recipients(); len(got) != 2 || got[1] != "admin@corp.test" {
		t.Fatalf("approved recipients: %v", got)
	}
	if !strings.Contains(sink.sent[1].Subject, "실행 가능") {
		t.Fatalf("approved subject: %q", sink.sent[1].Subject)
	}

	// Cancel is nobody's wait → silence.
	doReq(t, mux, "POST", "/api/changes/chg-review/cancel", "", withCookie(toks["admin"]))
	s.Mail.Wait()
	if len(sink.sent) != 2 {
		t.Fatalf("cancel must not mail: %d", len(sink.sent))
	}

	// The log has both attempts with event, recipient, subject, ref — no body.
	page := deliveries(t, mux, toks["admin"], "?status=sent")
	if page.Total != 2 || page.Status["sent"] != 2 || len(page.Items) != 2 {
		t.Fatalf("deliveries: %+v", page)
	}
	newest := page.Items[0]
	if newest.Event != mail.EventChangeApproved || newest.Ref != "chg-review" || newest.Actor != "dan" || newest.Recipient != "admin@corp.test" {
		t.Fatalf("newest delivery: %+v", newest)
	}
	raw := doReq(t, mux, "GET", "/api/mail/deliveries", "", withCookie(toks["admin"])).Body.String()
	if strings.Contains(raw, "자동으로 발송") || strings.Contains(raw, "바로 열기") {
		t.Fatal("delivery log leaks the mail body")
	}
}

func TestMailEventSwitchAndPasswordMasking(t *testing.T) {
	s, mux, toks, sink := newMailServer(t)
	enableMail(t, mux, toks["admin"], map[string]string{mail.KeyNotifyChangeReview: "false", mail.KeyPassword: "s3cret"})

	submitPlan(t, mux, toks["admin"], "chg-muted")
	s.Mail.Wait()
	if len(sink.sent) != 0 {
		t.Fatalf("change_review switch off, yet sent: %+v", sink.sent)
	}
	// The password reached the config but never the API.
	cfg, err := s.Mail.Config(context.Background())
	if err != nil || cfg.Password != "s3cret" {
		t.Fatalf("config password: %q %v", cfg.Password, err)
	}
	body := doReq(t, mux, "GET", "/api/settings", "", withCookie(toks["admin"])).Body.String()
	if strings.Contains(body, "s3cret") {
		t.Fatal("settings API returned the SMTP password")
	}
	if !strings.Contains(body, `"key":"mail.password"`) || !strings.Contains(body, "••••••(set)") {
		t.Fatalf("password should show as set-but-masked: %s", body)
	}
	if b, _ := json.Marshal(cfg); strings.Contains(string(b), "s3cret") {
		t.Fatal("Config JSON must omit the password")
	}
}

// A dead relay makes the delivery fail — recorded with both attempts — but the
// request that triggered it succeeds at once.
func TestMailDeadRelayDoesNotBlockRequest(t *testing.T) {
	s, mux, toks, _ := newMailServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	s.Mail.SetSender(mail.Deliver) // real transport against a closed port
	enableMail(t, mux, toks["admin"], map[string]string{mail.KeyHost: "127.0.0.1", mail.KeyPort: strconv.Itoa(port),
		mail.KeySecurity: "none", mail.KeyTimeout: "1"})

	start := time.Now()
	submitPlan(t, mux, toks["admin"], "chg-dead")
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("submit waited on the relay: %s", took)
	}
	s.Mail.Wait()
	page := deliveries(t, mux, toks["admin"], "?status=failed")
	if page.Status["failed"] != 1 || len(page.Items) != 1 {
		t.Fatalf("expected one failed delivery: %+v", page)
	}
	d := page.Items[0]
	if d.Attempts != 2 || d.Error == "" || d.Recipient != "dan@corp.test" {
		t.Fatalf("failed delivery: %+v", d)
	}
	// The test button reports the same failure to the admin instead of hiding it.
	rec := doReq(t, mux, "POST", "/api/mail/test", `{"recipient":"ops@corp.test"}`, withCookie(toks["admin"]))
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "SMTP 연결 실패") {
		t.Fatalf("test send against dead relay: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMailTestSendUsesCallerAddressAndAudits(t *testing.T) {
	s, mux, toks, sink := newMailServer(t)
	enableMail(t, mux, toks["admin"], nil)

	rec := doReq(t, mux, "POST", "/api/mail/test", `{}`, withCookie(toks["admin"]))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"recipient":"admin@corp.test"`) {
		t.Fatalf("test send: %d %s", rec.Code, rec.Body.String())
	}
	if got := sink.recipients(); len(got) != 1 || got[0] != "admin@corp.test" || !strings.Contains(sink.sent[0].Subject, "시험 발송") {
		t.Fatalf("test mail: %v %+v", got, sink.sent)
	}
	rec = doReq(t, mux, "POST", "/api/mail/test", `{"recipient":"not-an-address"}`, withCookie(toks["admin"]))
	if rec.Code != 400 {
		t.Fatalf("bad recipient: %d", rec.Code)
	}
	page := deliveries(t, mux, toks["admin"], "")
	if page.Total != 1 || page.Items[0].Event != mail.EventTest || page.Items[0].Actor != "admin" || page.Items[0].Status != "sent" {
		t.Fatalf("test delivery: %+v", page)
	}
	_ = s
}

func TestMailAsyncQueryFinishedGoesToSubmitter(t *testing.T) {
	s, mux, toks, sink := newMailServer(t)
	enableMail(t, mux, toks["admin"], nil)
	old := asyncMailAfter
	asyncMailAfter = 0
	t.Cleanup(func() { asyncMailAfter = old })

	// The stub driver fails fast; the submitter (alice) is told either way.
	job, refuse := s.submitAsyncQuery("dev-01", "SELECT secret FROM t WHERE k = 'v'", "alice", dbconn.ExecOptions{})
	if refuse != "" || job == nil {
		t.Fatalf("submit refused: %s", refuse)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got, ok := s.asyncJobs.jobView(job.ID); ok && got.Status != "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.Mail.Wait()
	if got := sink.recipients(); len(got) != 1 || got[0] != "alice@corp.test" {
		t.Fatalf("async recipients: %v", got)
	}
	m := sink.sent[0]
	if !strings.Contains(m.Subject, "비동기 쿼리 실패") || !strings.Contains(m.Body, job.ID) || strings.Contains(m.Body, "secret FROM") {
		t.Fatalf("async mail: %+v", m)
	}

	// Short jobs stay silent.
	asyncMailAfter = time.Hour
	fin := time.Now()
	s.notifyAsyncFinished(&asyncJob{ID: "j2", User: "alice", Status: "done", StartedAt: fin.Add(-time.Second), FinishedAt: &fin})
	s.Mail.Wait()
	if len(sink.sent) != 1 {
		t.Fatal("short async job must not mail")
	}
}

func TestMailSchedulerMailsOnTransitionOnly(t *testing.T) {
	s, mux, toks, sink := newMailServer(t)
	enableMail(t, mux, toks["admin"], nil)
	ctx := context.Background()

	s.notifySchedulerState(ctx, "prod-pg", true, "connection refused")
	s.notifySchedulerState(ctx, "prod-pg", true, "connection refused")
	s.notifySchedulerState(ctx, "prod-pg", true, "connection refused")
	s.Mail.Wait()
	if got := sink.recipients(); len(got) != 1 || got[0] != "admin@corp.test" {
		t.Fatalf("failure should mail admins once: %v", got)
	}
	if !strings.Contains(sink.sent[0].Subject, "동기화 실패") {
		t.Fatalf("subject: %q", sink.sent[0].Subject)
	}
	s.notifySchedulerState(ctx, "prod-pg", false, "")
	s.notifySchedulerState(ctx, "prod-pg", false, "")
	s.Mail.Wait()
	if len(sink.sent) != 2 || !strings.Contains(sink.sent[1].Subject, "회복") {
		t.Fatalf("recovery should mail once: %+v", sink.sent)
	}
	// A fresh server that never failed stays quiet on success.
	s.notifySchedulerState(ctx, "other", false, "")
	s.Mail.Wait()
	if len(sink.sent) != 2 {
		t.Fatal("first success must not mail")
	}
}

func TestMailStandaloneModeHasNoMailService(t *testing.T) {
	s, _ := newFixtureServer(t)
	if s.Mail != nil {
		t.Fatal("standalone server must not build a mail service")
	}
	// Nil-safe hooks: nothing panics without meta.
	s.notifyChange(context.Background(), change.Plan{ID: "p", State: change.ReviewRequired}, "x", nil)
	s.notifySchedulerState(context.Background(), "p", true, "e")
	s.notifyAsyncFinished(nil)
}
