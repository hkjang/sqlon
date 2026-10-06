package earlywarning

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Runtime settings let an administrator change how early warning notifies
// (channels, minimum severity, reminder and escalation timing, daily report,
// heartbeat, chat buttons) and how often the heavier checks run, without a
// restart — from the console, REST, or MCP. Flags and environment variables
// stay the defaults; a runtime value overrides its default until it is
// reset. Overrides persist in <Dir>/settings.json.

// Settings is a partial override: empty fields keep the default.
type Settings struct {
	// WebhookRef is the default channel: env:NAME | file:PATH | plain:URL.
	WebhookRef         string `json:"webhook_ref,omitempty"`
	ConsoleURL         string `json:"console_url,omitempty"`
	MinNotifySeverity  string `json:"min_notify_severity,omitempty"`
	RenotifyInterval   string `json:"renotify_interval,omitempty"` // "6h"; "off" never re-sends
	DigestAt           string `json:"digest_at,omitempty"`         // "09:00"; "off"
	MaintenanceEvery   string `json:"maintenance_interval,omitempty"`
	SchemaEvery        string `json:"schema_interval,omitempty"` // "off" disables schema watch
	HostDiskStaleAfter string `json:"host_disk_stale_after,omitempty"`
	// HeartbeatRef is pinged after every healthy cycle (dead man's switch).
	HeartbeatRef string `json:"heartbeat_ref,omitempty"`
	// EscalationRef pages an on-call channel for critical alerts left
	// unacknowledged for EscalateAfter ("30m"; "0m" pages at once; "off").
	EscalationRef string `json:"escalation_ref,omitempty"`
	EscalateAfter string `json:"escalate_after,omitempty"`
	// ChatActions adds buttons to messages ("mattermost" | "off"); ActionURL
	// is the base URL the chat server calls back (default: console origin).
	ChatActions string `json:"chat_actions,omitempty"`
	ActionURL   string `json:"action_url,omitempty"`
}

var settingNames = []string{"webhook_ref", "console_url", "min_notify_severity", "renotify_interval", "digest_at", "maintenance_interval", "schema_interval", "host_disk_stale_after", "heartbeat_ref", "escalation_ref", "escalate_after", "chat_actions", "action_url"}

func (s *Settings) field(name string) *string {
	switch name {
	case "webhook_ref":
		return &s.WebhookRef
	case "console_url":
		return &s.ConsoleURL
	case "min_notify_severity":
		return &s.MinNotifySeverity
	case "renotify_interval":
		return &s.RenotifyInterval
	case "digest_at":
		return &s.DigestAt
	case "maintenance_interval":
		return &s.MaintenanceEvery
	case "schema_interval":
		return &s.SchemaEvery
	case "host_disk_stale_after":
		return &s.HostDiskStaleAfter
	case "heartbeat_ref":
		return &s.HeartbeatRef
	case "escalation_ref":
		return &s.EscalationRef
	case "escalate_after":
		return &s.EscalateAfter
	case "chat_actions":
		return &s.ChatActions
	case "action_url":
		return &s.ActionURL
	}
	return nil
}

// secretFields hold URLs that carry their secret; echoes of the masked
// value keep the stored one.
var secretFields = map[string]bool{"webhook_ref": true, "heartbeat_ref": true, "escalation_ref": true}

// SettingsView is what callers see: every effective value, where it came
// from, and channels without their secrets.
type SettingsView struct {
	Webhook            string            `json:"webhook"` // masked target, "" = none
	WebhookRef         string            `json:"webhook_ref,omitempty"`
	ConsoleURL         string            `json:"console_url,omitempty"`
	MinNotifySeverity  string            `json:"min_notify_severity"`
	RenotifyInterval   string            `json:"renotify_interval"`
	DigestAt           string            `json:"digest_at"`
	MaintenanceEvery   string            `json:"maintenance_interval"`
	SchemaEvery        string            `json:"schema_interval"`
	HostDiskStaleAfter string            `json:"host_disk_stale_after"`
	Heartbeat          string            `json:"heartbeat"`
	HeartbeatRef       string            `json:"heartbeat_ref,omitempty"`
	Escalation         string            `json:"escalation"`
	EscalationRef      string            `json:"escalation_ref,omitempty"`
	EscalateAfter      string            `json:"escalate_after"`
	ChatActions        string            `json:"chat_actions"`
	ActionURL          string            `json:"action_url,omitempty"`
	Source             map[string]string `json:"source"` // field → default | runtime
	UpdatedAt          *time.Time        `json:"updated_at,omitempty"`
	UpdatedBy          string            `json:"updated_by,omitempty"`
}

type storedSettings struct {
	Settings
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

// NotifierFactory builds a channel from a resolved webhook URL.
type NotifierFactory func(webhookURL, consoleURL string) Notifier

// WebhookResolver turns a webhook reference into a URL (env:/file:/plain:).
type WebhookResolver func(ref string) (string, error)

type settingsBase struct {
	cfg          Config
	notifier     Notifier
	consoleURL   string
	heartbeatURL string
	escalation   Notifier
	chatActions  string
	actionURL    string
}

func (e *Engine) captureBase() *settingsBase {
	return &settingsBase{cfg: e.cfg, notifier: e.Notifier, consoleURL: e.ConsoleURL, heartbeatURL: e.HeartbeatURL, escalation: e.Escalation, chatActions: e.ChatActions, actionURL: e.ActionURL}
}

func fmtDuration(d time.Duration, zero string) string {
	if d <= 0 {
		return zero
	}
	return shortDuration(d)
}

// shortDuration writes 6h, 2m, 1h30m instead of 6h0m0s.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func parseSettingDuration(name, v string, allowOff bool) (time.Duration, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	if allowOff && v == "off" {
		return -1, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < time.Minute || d > 30*24*time.Hour {
		return 0, fmt.Errorf("%s must be a duration between 1m and 720h%s", name, map[bool]string{true: ", or off", false: ""}[allowOff])
	}
	return d, nil
}

// ParseEscalateAfter accepts a delay ("30m"), "0"/"0m" (page at once), or
// "off" (never page; returned as -1).
func ParseEscalateAfter(v string) (time.Duration, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	switch v {
	case "off":
		return -1, nil
	case "0", "0m", "0s", "immediate":
		return time.Nanosecond, nil // 0 in Config means "default"
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < time.Minute || d > 24*time.Hour {
		return 0, errors.New("escalate_after must be 0 (at once), a duration between 1m and 24h, or off")
	}
	return d, nil
}

// validate checks every non-empty field.
func (s Settings) validate(resolve WebhookResolver) error {
	for name := range secretFields {
		ref := strings.TrimSpace(*s.field(name))
		if ref != "" && ref != "plain:****" && resolve != nil {
			if _, err := resolve(ref); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	if s.MinNotifySeverity != "" && Rank(strings.ToLower(s.MinNotifySeverity)) == 0 {
		return errors.New("min_notify_severity must be info, warning, or critical")
	}
	if s.RenotifyInterval != "" {
		if _, err := parseSettingDuration("renotify_interval", s.RenotifyInterval, true); err != nil {
			return err
		}
	}
	if s.DigestAt != "" {
		if _, err := ParseDigestTime(s.DigestAt); err != nil {
			return err
		}
	}
	for name, v := range map[string]string{"maintenance_interval": s.MaintenanceEvery, "host_disk_stale_after": s.HostDiskStaleAfter} {
		if v != "" {
			if _, err := parseSettingDuration(name, v, false); err != nil {
				return err
			}
		}
	}
	if s.SchemaEvery != "" {
		if _, err := parseSettingDuration("schema_interval", s.SchemaEvery, true); err != nil {
			return err
		}
	}
	if s.EscalateAfter != "" {
		if _, err := ParseEscalateAfter(s.EscalateAfter); err != nil {
			return err
		}
	}
	switch strings.ToLower(strings.TrimSpace(s.ChatActions)) {
	case "", "off", "mattermost":
	default:
		return errors.New("chat_actions must be mattermost or off")
	}
	for name, v := range map[string]string{"console_url": s.ConsoleURL, "action_url": s.ActionURL} {
		if v != "" && !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
			return fmt.Errorf("%s must be an http(s) URL", name)
		}
	}
	return nil
}

// LoadSettings captures the current configuration as the defaults and
// applies stored runtime overrides. Call once after the default channels,
// Factory and Resolve are set.
func (e *Engine) LoadSettings() error {
	e.evalMu.Lock()
	defer e.evalMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.base = e.captureBase()
	if e.cfg.Dir == "" {
		return e.applyLocked()
	}
	b, err := os.ReadFile(filepath.Join(e.cfg.Dir, "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return e.applyLocked()
	}
	if err != nil {
		return err
	}
	var stored storedSettings
	if err := json.Unmarshal(b, &stored); err != nil {
		return fmt.Errorf("settings.json: %w", err)
	}
	e.stored = &stored
	return e.applyLocked()
}

// applyLocked recomputes the effective configuration from base + stored.
func (e *Engine) applyLocked() error {
	if e.base == nil {
		e.base = e.captureBase()
	}
	b := *e.base
	cfg, notifier, console, heartbeat, escalation, chat, action := b.cfg, b.notifier, b.consoleURL, b.heartbeatURL, b.escalation, b.chatActions, b.actionURL
	resolve := func(ref string) (string, error) {
		if e.Resolve == nil {
			return "", errors.New("no webhook resolver")
		}
		return e.Resolve(ref)
	}
	if e.stored != nil {
		s := e.stored.Settings
		if s.MinNotifySeverity != "" {
			cfg.MinNotifySeverity = strings.ToLower(s.MinNotifySeverity)
		}
		if s.RenotifyInterval != "" {
			cfg.RenotifyInterval, _ = parseSettingDuration("", s.RenotifyInterval, true)
		}
		if s.DigestAt != "" {
			cfg.DigestAt, _ = ParseDigestTime(s.DigestAt)
		}
		if s.MaintenanceEvery != "" {
			cfg.MaintenanceEvery, _ = parseSettingDuration("", s.MaintenanceEvery, false)
		}
		if s.SchemaEvery != "" {
			cfg.SchemaEvery, _ = parseSettingDuration("", s.SchemaEvery, true)
		}
		if s.HostDiskStaleAfter != "" {
			cfg.HostDiskStaleAfter, _ = parseSettingDuration("", s.HostDiskStaleAfter, false)
		}
		if s.EscalateAfter != "" {
			cfg.EscalateAfter, _ = ParseEscalateAfter(s.EscalateAfter)
		}
		if s.ConsoleURL != "" {
			console = s.ConsoleURL
		}
		if s.ChatActions != "" {
			chat = strings.ToLower(s.ChatActions)
		}
		if s.ActionURL != "" {
			action = s.ActionURL
		}
		if s.HeartbeatRef != "" {
			url, err := resolve(s.HeartbeatRef)
			if err != nil {
				return err
			}
			heartbeat = url
		}
		if s.EscalationRef != "" && e.Factory != nil {
			url, err := resolve(s.EscalationRef)
			if err != nil {
				return err
			}
			escalation = e.Factory(url, console)
		}
		if s.WebhookRef != "" && e.Factory != nil {
			url, err := resolve(s.WebhookRef)
			if err != nil {
				return err
			}
			notifier = e.Factory(url, console)
		} else if s.ConsoleURL != "" && notifier != nil && e.Factory != nil {
			if w, ok := notifier.(*WebhookNotifier); ok {
				notifier = e.Factory(w.URL, console)
			}
		}
	}
	if chat == "mattermost" {
		// the signing key exists only once buttons are on
		if err := e.loadActionKeyLocked(); err != nil {
			return err
		}
	}
	e.cfg, e.Notifier, e.ConsoleURL = cfg, notifier, console
	e.HeartbeatURL, e.Escalation, e.ChatActions, e.ActionURL = heartbeat, escalation, chat, action
	return nil
}

// Settings returns the effective settings.
func (e *Engine) Settings() SettingsView {
	e.mu.Lock()
	defer e.mu.Unlock()
	v := SettingsView{
		MinNotifySeverity: e.cfg.MinNotifySeverity, RenotifyInterval: fmtDuration(e.cfg.RenotifyInterval, "off"),
		DigestAt: e.cfg.DigestAt, MaintenanceEvery: shortDuration(e.cfg.MaintenanceEvery), SchemaEvery: fmtDuration(e.cfg.SchemaEvery, "off"),
		HostDiskStaleAfter: shortDuration(e.cfg.HostDiskStaleAfter), ConsoleURL: e.ConsoleURL, Source: map[string]string{},
		EscalateAfter: fmtEscalate(e.cfg.EscalateAfter), ChatActions: e.ChatActions, ActionURL: e.actionBaseLocked(),
	}
	if v.DigestAt == "" {
		v.DigestAt = "off"
	}
	if v.ChatActions == "" {
		v.ChatActions = "off"
	}
	if e.Notifier != nil {
		v.Webhook = e.Notifier.Target()
	}
	if e.Escalation != nil {
		v.Escalation = e.Escalation.Target()
	}
	if e.HeartbeatURL != "" {
		v.Heartbeat = MaskURL(e.HeartbeatURL)
	}
	stored := Settings{}
	if e.stored != nil {
		stored = e.stored.Settings
		at := e.stored.UpdatedAt
		v.UpdatedAt, v.UpdatedBy = &at, e.stored.UpdatedBy
	}
	v.WebhookRef, v.HeartbeatRef, v.EscalationRef = maskRef(stored.WebhookRef), maskRef(stored.HeartbeatRef), maskRef(stored.EscalationRef)
	for _, name := range settingNames {
		v.Source[name] = "default"
		if *stored.field(name) != "" {
			v.Source[name] = "runtime"
		}
	}
	return v
}

func fmtEscalate(d time.Duration) string {
	switch {
	case d < 0:
		return "off"
	case d < time.Second:
		return "0m"
	}
	return shortDuration(d)
}

func maskRef(ref string) string {
	if strings.HasPrefix(strings.TrimSpace(ref), "plain:") {
		return "plain:****"
	}
	return ref
}

// UpdateSettings merges a patch into the runtime overrides (empty fields
// unchanged) and applies it at once; reset lists fields to return to their
// defaults ("all" resets everything).
func (e *Engine) UpdateSettings(patch Settings, reset []string, actor string) (SettingsView, error) {
	if err := patch.validate(e.Resolve); err != nil {
		return SettingsView{}, err
	}
	e.evalMu.Lock()
	e.mu.Lock()
	unlock := func() { e.mu.Unlock(); e.evalMu.Unlock() }
	next := storedSettings{}
	if e.stored != nil {
		next = *e.stored
	}
	for _, name := range reset {
		name = strings.TrimSpace(name)
		if name == "all" {
			next.Settings = Settings{}
			continue
		}
		f := next.field(name)
		if f == nil {
			unlock()
			return SettingsView{}, fmt.Errorf("unknown setting %q", name)
		}
		*f = ""
	}
	for _, name := range settingNames {
		v := strings.TrimSpace(*patch.field(name))
		if v == "" || secretFields[name] && v == "plain:****" {
			continue // empty keeps; a masked echo keeps the stored secret
		}
		if name == "min_notify_severity" || name == "chat_actions" {
			v = strings.ToLower(v)
		}
		*next.field(name) = v
	}
	next.UpdatedAt, next.UpdatedBy = e.now(), actor
	prev := e.stored
	e.stored = &next
	if err := e.applyLocked(); err != nil {
		e.stored = prev
		_ = e.applyLocked()
		unlock()
		return SettingsView{}, err
	}
	var saveErr error
	if e.cfg.Dir != "" {
		saveErr = writeJSONAtomic(filepath.Join(e.cfg.Dir, "settings.json"), next)
	}
	unlock()
	if saveErr != nil {
		return SettingsView{}, saveErr
	}
	return e.Settings(), nil
}
