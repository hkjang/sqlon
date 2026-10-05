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
// (channel, minimum severity, reminder interval, daily report time) and how
// often the heavier checks run, without a restart — from the console, REST,
// or MCP. Flags and environment variables stay the defaults; a runtime value
// overrides its default until it is reset. Overrides persist in
// <Dir>/settings.json.

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
}

// SettingsView is what callers see: every effective value, where it came
// from, and the channel without its secret.
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
	Source             map[string]string `json:"source"` // field → default | runtime
	UpdatedAt          *time.Time        `json:"updated_at,omitempty"`
	UpdatedBy          string            `json:"updated_by,omitempty"`
}

type storedSettings struct {
	Settings
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

// NotifierFactory builds the default channel from a resolved webhook URL.
type NotifierFactory func(webhookURL, consoleURL string) Notifier

// WebhookResolver turns a webhook reference into a URL (env:/file:/plain:).
type WebhookResolver func(ref string) (string, error)

type settingsBase struct {
	cfg        Config
	notifier   Notifier
	consoleURL string
}

func fmtDuration(d time.Duration, zero string) string {
	if d <= 0 {
		return zero
	}
	return d.String()
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

// validate checks every non-empty field.
func (s Settings) validate(resolve WebhookResolver) error {
	if s.WebhookRef != "" && strings.TrimSpace(s.WebhookRef) != "plain:****" && resolve != nil {
		if _, err := resolve(s.WebhookRef); err != nil {
			return err
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
	if s.ConsoleURL != "" && !strings.HasPrefix(s.ConsoleURL, "http://") && !strings.HasPrefix(s.ConsoleURL, "https://") {
		return errors.New("console_url must be an http(s) URL")
	}
	return nil
}

// LoadSettings captures the current configuration as the defaults and
// applies stored runtime overrides. Call once after the default notifier,
// Factory and Resolve are set.
func (e *Engine) LoadSettings() error {
	e.evalMu.Lock()
	defer e.evalMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.base = &settingsBase{cfg: e.cfg, notifier: e.Notifier, consoleURL: e.ConsoleURL}
	if e.cfg.Dir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(e.cfg.Dir, "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
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

// applyLocked recomputes cfg and the default notifier from base + stored.
func (e *Engine) applyLocked() error {
	if e.base == nil {
		e.base = &settingsBase{cfg: e.cfg, notifier: e.Notifier, consoleURL: e.ConsoleURL}
	}
	cfg, notifier, console := e.base.cfg, e.base.notifier, e.base.consoleURL
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
		if s.ConsoleURL != "" {
			console = s.ConsoleURL
		}
		if s.WebhookRef != "" && e.Resolve != nil && e.Factory != nil {
			url, err := e.Resolve(s.WebhookRef)
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
	e.cfg, e.Notifier, e.ConsoleURL = cfg, notifier, console
	return nil
}

// Settings returns the effective settings.
func (e *Engine) Settings() SettingsView {
	e.mu.Lock()
	defer e.mu.Unlock()
	v := SettingsView{
		MinNotifySeverity: e.cfg.MinNotifySeverity, RenotifyInterval: fmtDuration(e.cfg.RenotifyInterval, "off"),
		DigestAt: e.cfg.DigestAt, MaintenanceEvery: e.cfg.MaintenanceEvery.String(), SchemaEvery: fmtDuration(e.cfg.SchemaEvery, "off"),
		HostDiskStaleAfter: e.cfg.HostDiskStaleAfter.String(), ConsoleURL: e.ConsoleURL, Source: map[string]string{},
	}
	if v.DigestAt == "" {
		v.DigestAt = "off"
	}
	if e.Notifier != nil {
		v.Webhook = e.Notifier.Target()
	}
	fields := map[string]string{}
	if e.stored != nil {
		s := e.stored.Settings
		fields = map[string]string{"webhook_ref": s.WebhookRef, "console_url": s.ConsoleURL, "min_notify_severity": s.MinNotifySeverity, "renotify_interval": s.RenotifyInterval, "digest_at": s.DigestAt, "maintenance_interval": s.MaintenanceEvery, "schema_interval": s.SchemaEvery, "host_disk_stale_after": s.HostDiskStaleAfter}
		if s.WebhookRef != "" {
			v.WebhookRef = maskRef(s.WebhookRef)
		}
		at := e.stored.UpdatedAt
		v.UpdatedAt, v.UpdatedBy = &at, e.stored.UpdatedBy
	}
	for _, name := range []string{"webhook_ref", "console_url", "min_notify_severity", "renotify_interval", "digest_at", "maintenance_interval", "schema_interval", "host_disk_stale_after"} {
		v.Source[name] = "default"
		if fields[name] != "" {
			v.Source[name] = "runtime"
		}
	}
	return v
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
	next := storedSettings{}
	if e.stored != nil {
		next = *e.stored
	}
	for _, name := range reset {
		switch strings.TrimSpace(name) {
		case "all":
			next.Settings = Settings{}
		case "webhook_ref":
			next.WebhookRef = ""
		case "console_url":
			next.ConsoleURL = ""
		case "min_notify_severity":
			next.MinNotifySeverity = ""
		case "renotify_interval":
			next.RenotifyInterval = ""
		case "digest_at":
			next.DigestAt = ""
		case "maintenance_interval":
			next.MaintenanceEvery = ""
		case "schema_interval":
			next.SchemaEvery = ""
		case "host_disk_stale_after":
			next.HostDiskStaleAfter = ""
		default:
			e.mu.Unlock()
			e.evalMu.Unlock()
			return SettingsView{}, fmt.Errorf("unknown setting %q", name)
		}
	}
	if strings.TrimSpace(patch.WebhookRef) == "plain:****" {
		patch.WebhookRef = "" // the masked echo of the stored value: keep it
	}
	merge := func(dst *string, v string) {
		if strings.TrimSpace(v) != "" {
			*dst = strings.TrimSpace(v)
		}
	}
	merge(&next.WebhookRef, patch.WebhookRef)
	merge(&next.ConsoleURL, patch.ConsoleURL)
	merge(&next.MinNotifySeverity, strings.ToLower(patch.MinNotifySeverity))
	merge(&next.RenotifyInterval, patch.RenotifyInterval)
	merge(&next.DigestAt, patch.DigestAt)
	merge(&next.MaintenanceEvery, patch.MaintenanceEvery)
	merge(&next.SchemaEvery, patch.SchemaEvery)
	merge(&next.HostDiskStaleAfter, patch.HostDiskStaleAfter)
	next.UpdatedAt, next.UpdatedBy = e.now(), actor
	prev := e.stored
	e.stored = &next
	if err := e.applyLocked(); err != nil {
		e.stored = prev
		_ = e.applyLocked()
		e.mu.Unlock()
		e.evalMu.Unlock()
		return SettingsView{}, err
	}
	var saveErr error
	if e.cfg.Dir != "" {
		saveErr = writeJSONAtomic(filepath.Join(e.cfg.Dir, "settings.json"), next)
	}
	e.mu.Unlock()
	e.evalMu.Unlock()
	if saveErr != nil {
		return SettingsView{}, saveErr
	}
	return e.Settings(), nil
}
