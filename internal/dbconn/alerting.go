package dbconn

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// AlertingConfig routes one database's early-warning notifications.
type AlertingConfig struct {
	// WebhookRef sends this database's alerts to its own channel (a team's
	// Mattermost/Slack incoming webhook) instead of the server default.
	// Same reference schemes as password_ref: env:NAME | file:PATH |
	// plain:URL. Webhook URLs carry their secret in the path, so API
	// responses mask a plain value like a password.
	WebhookRef string `json:"webhook_ref,omitempty"`
	// MinSeverity overrides the server's lowest notified severity for this
	// database (info | warning | critical), e.g. critical-only for dev.
	MinSeverity string `json:"min_severity,omitempty"`
}

const maskedPlain = "plain:****"

func (a *AlertingConfig) validate() error {
	if a == nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(a.MinSeverity)) {
	case "", "info", "warning", "critical":
	default:
		return errors.New("alerting.min_severity must be info, warning, or critical")
	}
	ref := strings.TrimSpace(a.WebhookRef)
	if ref == "" || ref == maskedPlain {
		return nil
	}
	scheme, err := parsePasswordRef(ref)
	if err != nil {
		return fmt.Errorf("alerting.webhook_ref: %w", err)
	}
	if scheme == "plain" {
		if _, err := ParseWebhookURL(ref[len("plain:"):]); err != nil {
			return fmt.Errorf("alerting.webhook_ref: %w", err)
		}
	}
	return nil
}

// ParseWebhookURL accepts only absolute http(s) URLs.
func ParseWebhookURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("webhook must be an absolute http(s) URL")
	}
	return u, nil
}

// ResolveWebhook materializes the profile's webhook URL ("" when none).
func (a *AlertingConfig) ResolveWebhook() (string, error) {
	if a == nil || strings.TrimSpace(a.WebhookRef) == "" {
		return "", nil
	}
	v, err := ResolvePassword(strings.TrimSpace(a.WebhookRef))
	if err != nil {
		return "", fmt.Errorf("alerting.webhook_ref: %w", err)
	}
	if _, err := ParseWebhookURL(v); err != nil {
		return "", fmt.Errorf("alerting.webhook_ref: %w", err)
	}
	return strings.TrimSpace(v), nil
}

func (a *AlertingConfig) masked() map[string]any {
	return map[string]any{"webhook_ref": maskedRefOrEmpty(a.WebhookRef), "min_severity": a.MinSeverity}
}

func maskedRefOrEmpty(ref string) string {
	if strings.TrimSpace(ref) == "" {
		return ""
	}
	return MaskedRef(ref)
}

// PreserveMaskedSecrets puts back secrets a client could only echo masked:
// a webhook submitted as "plain:****" keeps the stored value. It fails when
// there is nothing to keep, so a masked placeholder is never stored.
func PreserveMaskedSecrets(p *Profile, existing *Profile) error {
	if p.Alerting == nil || strings.TrimSpace(p.Alerting.WebhookRef) != maskedPlain {
		return nil
	}
	if existing == nil || existing.Alerting == nil || !strings.HasPrefix(existing.Alerting.WebhookRef, "plain:") {
		return errors.New("alerting.webhook_ref: enter the webhook again (the stored value is not available)")
	}
	p.Alerting.WebhookRef = existing.Alerting.WebhookRef
	return nil
}
