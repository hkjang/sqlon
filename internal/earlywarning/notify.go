package earlywarning

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	_ "time/tzdata" // container images may ship without a zoneinfo database
)

// Notifier delivers one cycle's notifications in a single call.
type Notifier interface {
	Notify(ctx context.Context, notes []Notification, profileNames map[string]string) error
	// NotifyText posts a prepared message (the daily report, a test).
	NotifyText(ctx context.Context, kind, text string) error
	// Target describes the destination without secrets, for status pages.
	Target() string
	// ID identifies the destination (stable, secret-free).
	ID() string
}

// WebhookNotifier POSTs JSON whose "text" field is Markdown, so the same URL
// works as a Mattermost or Slack incoming webhook; the structured
// "notifications" array is there for anything else that consumes it.
type WebhookNotifier struct {
	URL    string
	Client *http.Client
	// ConsoleURL, when set, is linked from the message footer.
	ConsoleURL string
}

func (w *WebhookNotifier) Target() string { return MaskURL(w.URL) }

func (w *WebhookNotifier) ID() string { return "webhook:" + shortHash(w.URL) }

func (w *WebhookNotifier) Notify(ctx context.Context, notes []Notification, names map[string]string) error {
	return w.post(ctx, map[string]any{
		"source":        "sqlon_early_warning",
		"kind":          "alerts",
		"ts":            time.Now().UTC().Format(time.RFC3339),
		"text":          FormatText(notes, names, w.ConsoleURL),
		"notifications": notes,
	})
}

func (w *WebhookNotifier) NotifyText(ctx context.Context, kind, text string) error {
	if w.ConsoleURL != "" {
		text += fmt.Sprintf("\n\n[예방 경보 콘솔 열기](%s)", w.ConsoleURL)
	}
	return w.post(ctx, map[string]any{"source": "sqlon_early_warning", "kind": kind, "ts": time.Now().UTC().Format(time.RFC3339), "text": text})
}

func (w *WebhookNotifier) post(ctx context.Context, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("webhook returned %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	return nil
}

// MaskURL keeps scheme and host and hides the path, where incoming-webhook
// URLs carry their secret.
func MaskURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "(invalid url)"
	}
	if u.Path != "" && u.Path != "/" {
		return u.Scheme + "://" + u.Host + "/…"
	}
	return u.Scheme + "://" + u.Host
}

var displayLoc atomic.Pointer[time.Location]

// SetDisplayLocation sets the zone used for times in messages (default
// Asia/Seoul). Unknown names are reported and leave the zone unchanged.
func SetDisplayLocation(name string) error {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return err
	}
	displayLoc.Store(loc)
	return nil
}

func displayLocation() *time.Location {
	if loc := displayLoc.Load(); loc != nil {
		return loc
	}
	if loc, err := time.LoadLocation("Asia/Seoul"); err == nil {
		displayLoc.Store(loc)
		return loc
	}
	return time.UTC
}

const maxMessageItems = 15

// FormatText renders notifications as one Markdown message: the newest and
// worst first, the recommendation under each firing item.
func FormatText(notes []Notification, names map[string]string, consoleURL string) string {
	counts := map[string]int{}
	for _, n := range notes {
		counts[n.Kind]++
	}
	var head []string
	for _, k := range []struct{ kind, label string }{{KindFiring, "새 경보"}, {KindEscalated, "격상"}, {KindReminder, "지속"}, {KindResolved, "해소"}} {
		if counts[k.kind] > 0 {
			head = append(head, fmt.Sprintf("%s %d", k.label, counts[k.kind]))
		}
	}
	var b strings.Builder
	icon := "🛡️"
	for _, n := range notes {
		if n.Kind != KindResolved && n.Alert.Severity == SevCritical {
			icon = "🚨"
			break
		}
	}
	fmt.Fprintf(&b, "#### %s SQLON 예방 경보 — %s\n", icon, strings.Join(head, " · "))
	for i, n := range notes {
		if i == maxMessageItems {
			fmt.Fprintf(&b, "\n… 외 %d건", len(notes)-i)
			break
		}
		a := n.Alert
		name := names[a.ProfileID]
		if name == "" {
			name = a.ProfileID
		} else if name != a.ProfileID {
			name = fmt.Sprintf("%s (%s)", name, a.ProfileID)
		}
		switch n.Kind {
		case KindResolved:
			fmt.Fprintf(&b, "\n✅ **해소** · %s · %s", name, a.Title)
			continue
		case KindEscalated:
			fmt.Fprintf(&b, "\n%s **%s 격상** · %s · %s", sevIcon(a.Severity), strings.ToUpper(a.Severity), name, a.Title)
		case KindReminder:
			fmt.Fprintf(&b, "\n🔁 **%s 지속** (%s부터) · %s · %s", strings.ToUpper(a.Severity), a.FirstSeen.In(displayLocation()).Format("01-02 15:04"), name, a.Title)
		default:
			fmt.Fprintf(&b, "\n%s **%s** · %s · %s", sevIcon(a.Severity), strings.ToUpper(a.Severity), name, a.Title)
		}
		if a.Detail != "" {
			for _, line := range strings.Split(a.Detail, "\n") {
				fmt.Fprintf(&b, "\n> %s", line)
			}
		}
		if a.Recommendation != "" && n.Kind != KindReminder {
			fmt.Fprintf(&b, "\n> 조치: %s", a.Recommendation)
		}
	}
	if consoleURL != "" {
		fmt.Fprintf(&b, "\n\n[예방 경보 콘솔 열기](%s) — 확인(ack)하면 지속 알림이 멈춥니다.", consoleURL)
	}
	return b.String()
}

func sevIcon(severity string) string {
	switch severity {
	case SevCritical:
		return "🔴"
	case SevWarning:
		return "🟠"
	}
	return "🔵"
}
