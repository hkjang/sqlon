package earlywarning

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Chat actions put buttons on alert messages — acknowledge, silence for two
// hours, propose a fix — so the person who reads the alert in Mattermost can
// act without opening the console. Mattermost calls back an integration URL
// with a context we choose; it cannot carry SQLON credentials, so the
// context holds a token bound to one alert and one action, signed with a
// key that never leaves this server, and expiring after a week.

const (
	ActionAck     = "ack"
	ActionSilence = "silence"
	ActionFix     = "fix"
	actionTTL     = 7 * 24 * time.Hour
	// ChatActionPath is where chat servers call back.
	ChatActionPath = "/api/early-warning/chat-action"
)

// FixableRules are the rules propose_early_warning_fix can turn into a plan.
var FixableRules = map[string]bool{"maint_replication_slot": true, "maint_vacuum_blocker": true, "maint_bloat": true, "maint_config_risk": true, RuleMonitorPrivilege: true}

func (e *Engine) loadActionKeyLocked() error {
	if len(e.actionKey) > 0 {
		return nil
	}
	if e.cfg.Dir != "" {
		path := filepath.Join(e.cfg.Dir, "action.key")
		if b, err := os.ReadFile(path); err == nil {
			if key, err := hex.DecodeString(strings.TrimSpace(string(b))); err == nil && len(key) >= 32 {
				e.actionKey = key
				return nil
			}
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return err
		}
		if err := os.MkdirAll(e.cfg.Dir, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(hex.EncodeToString(key)), 0o600); err != nil {
			return err
		}
		e.actionKey = key
		return nil
	}
	key := make([]byte, 32)
	_, err := rand.Read(key)
	e.actionKey = key
	return err
}

type actionClaims struct {
	Alert  string `json:"a"`
	Action string `json:"x"`
	Expiry int64  `json:"e"`
}

func (e *Engine) signActionLocked(alertID, action string, now time.Time) string {
	payload, _ := json.Marshal(actionClaims{Alert: alertID, Action: action, Expiry: now.Add(actionTTL).Unix()})
	body := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, e.actionKey)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyAction checks a token and returns the alert and action it allows.
func (e *Engine) VerifyAction(token string) (alertID, action string, err error) {
	e.mu.Lock()
	key, enabled := e.actionKey, e.ChatActions == "mattermost"
	now := e.now()
	e.mu.Unlock()
	if !enabled {
		// turning chat actions off disarms buttons already posted
		return "", "", errors.New("chat actions are off — use the console")
	}
	body, sig, ok := strings.Cut(token, ".")
	if !ok || len(key) == 0 {
		return "", "", errors.New("invalid action token")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(body))
	want := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, want) {
		return "", "", errors.New("invalid action token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return "", "", errors.New("invalid action token")
	}
	var c actionClaims
	if json.Unmarshal(raw, &c) != nil {
		return "", "", errors.New("invalid action token")
	}
	if now.Unix() > c.Expiry {
		return "", "", errors.New("this button has expired — use the console")
	}
	return c.Alert, c.Action, nil
}

// actionBaseLocked is where the chat server calls back: ActionURL, else
// the console URL's origin.
func (e *Engine) actionBaseLocked() string {
	if e.ActionURL != "" {
		return strings.TrimRight(e.ActionURL, "/")
	}
	if u, err := url.Parse(e.ConsoleURL); err == nil && u.Scheme != "" && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return ""
}

var severityColor = map[string]string{SevCritical: "#d32f2f", SevWarning: "#f59e0b", SevInfo: "#0ea5e9"}

// ChatAttachment returns a Mattermost message attachment with action
// buttons for a notification, or nil when chat actions are off or the
// notification is not something to act on.
func (e *Engine) ChatAttachment(n Notification) map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	base := e.actionBaseLocked()
	if e.ChatActions != "mattermost" || base == "" || len(e.actionKey) == 0 {
		return nil
	}
	switch n.Kind {
	case KindFiring, KindEscalated, KindReminder, KindPage:
	default:
		return nil
	}
	a := n.Alert
	now := e.now()
	callback := base + ChatActionPath
	button := func(action, label string) map[string]any {
		return map[string]any{
			// ids must be unique within a post and alphanumeric
			"id": action + shortHash(a.ID)[:8], "name": label, "type": "button",
			"integration": map[string]any{"url": callback, "context": map[string]any{"token": e.signActionLocked(a.ID, action, now)}},
		}
	}
	actions := []map[string]any{button(ActionAck, "✅ 확인"), button(ActionSilence, "🔕 2시간 무음")}
	if FixableRules[a.Rule] {
		actions = append(actions, button(ActionFix, "🛠 수정안 만들기"))
	}
	return map[string]any{
		"fallback": fmt.Sprintf("[%s] %s", strings.ToUpper(a.Severity), a.Title),
		"color":    severityColor[a.Severity],
		"text":     fmt.Sprintf("**%s** — %s", a.ProfileID, a.Title),
		"actions":  actions,
	}
}
