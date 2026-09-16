package mail

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRelay is a minimal plaintext SMTP server: enough of the dialogue for
// EHLO/AUTH PLAIN/MAIL/RCPT/DATA/QUIT to run and capture what was sent.
type fakeRelay struct {
	ln       net.Listener
	mu       sync.Mutex
	messages []string
	auths    []string
	wantAuth bool
}

func newFakeRelay(t *testing.T, wantAuth bool) *fakeRelay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeRelay{ln: ln, wantAuth: wantAuth}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go r.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return r
}

func (r *fakeRelay) addr() (string, int) {
	a := r.ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port
}

func (r *fakeRelay) serve(c net.Conn) {
	defer c.Close()
	rd := bufio.NewReader(c)
	w := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
	w("220 relay.test ESMTP")
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			if r.wantAuth {
				w("250-relay.test")
				w("250 AUTH PLAIN LOGIN")
			} else {
				w("250 relay.test")
			}
		case strings.HasPrefix(cmd, "AUTH"):
			r.mu.Lock()
			r.auths = append(r.auths, strings.TrimSpace(line))
			r.mu.Unlock()
			w("235 ok")
		case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
			w("250 ok")
		case strings.HasPrefix(cmd, "DATA"):
			w("354 go")
			var body strings.Builder
			for {
				l, err := rd.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				body.WriteString(l)
			}
			r.mu.Lock()
			r.messages = append(r.messages, body.String())
			r.mu.Unlock()
			w("250 queued")
		case strings.HasPrefix(cmd, "QUIT"):
			w("221 bye")
			return
		default:
			w("250 ok")
		}
	}
}

func (r *fakeRelay) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.messages...)
}

func relayConfig(r *fakeRelay) Config {
	host, port := r.addr()
	return Config{Enabled: true, Host: host, Port: port, Security: SecurityNone, FromAddress: "sqlon@example.test",
		FromName: "sqlon", Timeout: 3 * time.Second, Events: map[string]bool{}}
}

func TestDeliverThroughAnonymousRelay(t *testing.T) {
	relay := newFakeRelay(t, false)
	cfg := relayConfig(relay)
	err := Deliver(context.Background(), cfg, Message{To: "dba@example.test", Subject: "승인 요청", Body: "첫 줄\n.점으로 시작하는 줄\n끝"})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	msgs := relay.got()
	if len(msgs) != 1 {
		t.Fatalf("relay got %d messages", len(msgs))
	}
	m := msgs[0]
	for _, want := range []string{"To: dba@example.test\r\n", "Subject: =?utf-8?q?", "X-SQLON-Notification: 1\r\n", "\r\n..점으로 시작하는 줄\r\n", "Content-Type: text/plain; charset=UTF-8"} {
		if !strings.Contains(m, want) {
			t.Errorf("message lacks %q:\n%s", want, m)
		}
	}
	if strings.Contains(m, "\n.점") {
		t.Error("leading dot was not stuffed")
	}
}

func TestDeliverAuthenticatesOnlyWhenUsernameSet(t *testing.T) {
	relay := newFakeRelay(t, true)
	cfg := relayConfig(relay)
	if err := Deliver(context.Background(), cfg, Message{To: "a@example.test", Subject: "s", Body: "b"}); err != nil {
		t.Fatalf("anonymous deliver: %v", err)
	}
	if len(relay.auths) != 0 {
		t.Fatalf("no AUTH expected without username, got %v", relay.auths)
	}
	// PLAIN over a plaintext connection is refused by net/smtp unless the host
	// is localhost — the relay here is 127.0.0.1 so it is allowed.
	cfg.Username, cfg.Password = "svc", "pw"
	if err := Deliver(context.Background(), cfg, Message{To: "a@example.test", Subject: "s", Body: "b"}); err != nil {
		t.Fatalf("authenticated deliver: %v", err)
	}
	if len(relay.auths) != 1 || !strings.HasPrefix(strings.ToUpper(relay.auths[0]), "AUTH PLAIN") {
		t.Fatalf("expected one AUTH PLAIN, got %v", relay.auths)
	}
}

func TestDeliverDeadRelayFailsWithinTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // nothing listens here any more
	cfg := Config{Enabled: true, Host: "127.0.0.1", Port: port, Security: SecurityNone, FromAddress: "sqlon@example.test", Timeout: 2 * time.Second}
	start := time.Now()
	if err := Deliver(context.Background(), cfg, Message{To: "a@example.test", Subject: "s", Body: "b"}); err == nil {
		t.Fatal("expected connection failure")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took too long: %s", time.Since(start))
	}
}

func TestFromSettingsDefaultsAndSwitches(t *testing.T) {
	cfg := FromSettings(nil)
	if cfg.Enabled || cfg.Port != 25 || cfg.Security != SecurityAuto || cfg.Timeout != 10*time.Second {
		t.Fatalf("defaults: %+v", cfg)
	}
	if err := cfg.Validate(); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty host should be invalid, got %v", err)
	}
	cfg = FromSettings(map[string]string{
		KeyEnabled: "true", KeyHost: "relay.corp", KeyPort: "465", KeyTimeout: "3",
		KeyNotifyChangeReview: "false", KeyPassword: " sp ace ", KeyBaseURL: "https://sqlon.corp/",
	})
	if !cfg.Enabled || cfg.Security != SecurityTLS || cfg.Timeout != 3*time.Second {
		t.Fatalf("parsed: %+v", cfg)
	}
	if cfg.FromAddress != "sqlon@relay.corp" || cfg.FromName != "sqlon" {
		t.Fatalf("from defaults: %q %q", cfg.FromAddress, cfg.FromName)
	}
	if cfg.Password != " sp ace " {
		t.Fatalf("password must not be trimmed: %q", cfg.Password)
	}
	if cfg.Allows(EventChangeReviewRequired) || cfg.Allows(EventChangeApproved) {
		t.Fatal("change_review switch should cover both review events")
	}
	if !cfg.Allows(EventChangeFailed) || !cfg.Allows(EventTest) || !cfg.Allows("future.event") {
		t.Fatal("unset switches and unknown events must stay on")
	}
	if got := cfg.Link("/admin/changes"); got != "https://sqlon.corp/admin/changes" {
		t.Fatalf("link: %q", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	cfg.Security = "weird"
	if err := cfg.Validate(); err == nil {
		t.Fatal("bad security must fail validation")
	}
}

// memSettings and memDirectory stand in for the meta DB.
type memSettings map[string]string

func (m memSettings) Values(context.Context) (map[string]string, error) { return m, nil }

type memDirectory map[string]string

func (m memDirectory) Emails(_ context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	for _, id := range ids {
		if e, ok := m[strings.ToLower(id)]; ok {
			out[strings.ToLower(id)] = e
		}
	}
	return out, nil
}

type capture struct {
	mu   sync.Mutex
	sent []Message
	fail error
}

func (c *capture) send(_ context.Context, _ Config, m Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail != nil {
		return c.fail
	}
	c.sent = append(c.sent, m)
	return nil
}

func enabledSettings() memSettings {
	return memSettings{KeyEnabled: "true", KeyHost: "relay.test", KeyFromAddress: "sqlon@relay.test"}
}

func TestServiceDisabledSendsNothing(t *testing.T) {
	sink := &capture{}
	svc := New(memSettings{KeyHost: "relay.test"}, memDirectory{"bob": "bob@x"}, "")
	svc.SetSender(sink.send)
	svc.Notify(context.Background(), TestMessage(), "", []string{"bob"})
	svc.Wait()
	if len(sink.sent) != 0 || svc.Deliveries("", 10).Total != 0 {
		t.Fatal("disabled mail must neither send nor record")
	}
	if err := svc.SendNow(context.Background(), TestMessage(), "admin", "bob@x"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("SendNow when disabled: %v", err)
	}
}

func TestServiceIncompleteConfigIsQuietNoop(t *testing.T) {
	sink := &capture{}
	svc := New(memSettings{KeyEnabled: "true"}, memDirectory{"bob": "bob@x"}, "")
	svc.SetSender(sink.send)
	svc.Notify(context.Background(), TestMessage(), "", []string{"bob"})
	svc.Wait()
	if len(sink.sent) != 0 || svc.Deliveries("", 10).Total != 0 {
		t.Fatal("missing host must not send or record")
	}
	err := svc.SendNow(context.Background(), TestMessage(), "admin", "bob@x")
	if err == nil || !strings.Contains(err.Error(), KeyHost) {
		t.Fatalf("SendNow should name the missing key, got %v", err)
	}
}

func TestServiceSkipsActorDedupesAndRecords(t *testing.T) {
	sink := &capture{}
	dir := t.TempDir()
	svc := New(enabledSettings(), memDirectory{"alice": "alice@x", "bob": "Bob@X", "carol": "bob@x"}, dir)
	svc.SetSender(sink.send)
	n := ChangeReviewRequired("alice", "chg-1", "prod", "public.orders", "medium", "index", 1)
	svc.Notify(context.Background(), n, "alice", []string{"alice", "bob", "carol", "", "noaddr", "direct@x"})
	svc.Wait()
	if len(sink.sent) != 2 {
		t.Fatalf("expected bob + direct address only, got %d: %+v", len(sink.sent), sink.sent)
	}
	for _, m := range sink.sent {
		if m.To == "alice@x" {
			t.Fatal("actor must not be mailed about their own action")
		}
		if !strings.Contains(m.Body, "chg-1") || !strings.Contains(m.Body, "자동으로 발송") {
			t.Fatalf("body: %q", m.Body)
		}
	}
	page := svc.Deliveries("", 10)
	if page.Total != 2 || page.Status["sent"] != 2 {
		t.Fatalf("log: %+v", page)
	}
	for _, d := range page.Items {
		if d.Event != EventChangeReviewRequired || d.Ref != "chg-1" || d.Actor != "alice" || d.Attempts != 1 {
			t.Fatalf("delivery: %+v", d)
		}
	}
	// The log survives a restart and never carries the body.
	files, _ := filepath.Glob(filepath.Join(dir, "mail", "deliveries-*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("expected one log file, got %v", files)
	}
	raw, _ := os.ReadFile(files[0])
	if strings.Contains(string(raw), "자동으로 발송") || strings.Contains(string(raw), "public.orders\n") {
		t.Fatal("log file must not contain the body")
	}
	reloaded := New(enabledSettings(), nil, dir)
	if got := reloaded.Deliveries("sent", 10); got.Total != 2 || len(got.Items) != 2 {
		t.Fatalf("reloaded log: %+v", got)
	}
}

func TestServiceRecordsFailuresAndRetriesOnce(t *testing.T) {
	sink := &capture{fail: errors.New("connection refused")}
	svc := New(enabledSettings(), memDirectory{"bob": "bob@x"}, "")
	svc.SetSender(sink.send)
	svc.Notify(context.Background(), ChangeFailed("ai", "chg-2", "prod", "t", "rollback_required", "boom"), "ai", []string{"bob"})
	svc.Wait()
	page := svc.Deliveries("failed", 10)
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("log: %+v", page)
	}
	d := page.Items[0]
	if d.Status != "failed" || d.Attempts != 2 || !strings.Contains(d.Error, "connection refused") {
		t.Fatalf("failed delivery: %+v", d)
	}
}

func TestServiceEventSwitchStopsOnlyThatEvent(t *testing.T) {
	sink := &capture{}
	settings := enabledSettings()
	settings[KeyNotifyQueryFinished] = "false"
	svc := New(settings, memDirectory{"bob": "bob@x"}, "")
	svc.SetSender(sink.send)
	svc.Notify(context.Background(), QueryFinished("job1", "prod", "done", 90*time.Second, 3, ""), "", []string{"bob"})
	svc.Notify(context.Background(), SchedulerFailed("prod", "down"), "", []string{"bob"})
	svc.Wait()
	if len(sink.sent) != 1 || !strings.Contains(sink.sent[0].Subject, "동기화 실패") {
		t.Fatalf("only the scheduler mail should go out: %+v", sink.sent)
	}
}

func TestSendNowRecordsOutcome(t *testing.T) {
	relay := newFakeRelay(t, false)
	host, port := relay.addr()
	settings := memSettings{KeyEnabled: "true", KeyHost: host, KeyPort: strconv.Itoa(port), KeySecurity: "none", KeyFromAddress: "sqlon@example.test"}
	svc := New(settings, nil, "")
	if err := svc.SendNow(context.Background(), TestMessage(), "admin", "ops@example.test"); err != nil {
		t.Fatalf("send now: %v", err)
	}
	if len(relay.got()) != 1 {
		t.Fatal("relay did not receive the test mail")
	}
	page := svc.Deliveries("", 5)
	if page.Total != 1 || page.Items[0].Event != EventTest || page.Items[0].Status != "sent" || page.Items[0].Actor != "admin" {
		t.Fatalf("log: %+v", page)
	}
}

func TestLogKeepsMostRecent(t *testing.T) {
	l := NewLog("")
	for i := 0; i < logKeep+20; i++ {
		d := l.Add(Delivery{Event: "e", Recipient: "r", Status: "queued"})
		d.Status = "sent"
		l.Finish(d)
	}
	page := l.List("", 10)
	if page.Total != logKeep || len(page.Items) != 10 {
		t.Fatalf("ring: total=%d items=%d", page.Total, len(page.Items))
	}
}
