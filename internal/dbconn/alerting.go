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
	// EscalationRef pages this database's on-call channel (same schemes)
	// instead of the server's for critical alerts left unacknowledged.
	EscalationRef string `json:"escalation_ref,omitempty"`
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
	for name, ref := range map[string]string{"webhook_ref": a.WebhookRef, "escalation_ref": a.EscalationRef} {
		ref = strings.TrimSpace(ref)
		if ref == "" || ref == maskedPlain {
			continue
		}
		scheme, err := parsePasswordRef(ref)
		if err != nil {
			return fmt.Errorf("alerting.%s: %w", name, err)
		}
		if scheme == "plain" {
			if _, err := ParseWebhookURL(ref[len("plain:"):]); err != nil {
				return fmt.Errorf("alerting.%s: %w", name, err)
			}
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
	if a == nil {
		return "", nil
	}
	return resolveURLRef("webhook_ref", a.WebhookRef)
}

// ResolveEscalation materializes the profile's on-call URL ("" when none).
func (a *AlertingConfig) ResolveEscalation() (string, error) {
	if a == nil {
		return "", nil
	}
	return resolveURLRef("escalation_ref", a.EscalationRef)
}

func resolveURLRef(name, ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", nil
	}
	v, err := ResolvePassword(strings.TrimSpace(ref))
	if err != nil {
		return "", fmt.Errorf("alerting.%s: %w", name, err)
	}
	if _, err := ParseWebhookURL(v); err != nil {
		return "", fmt.Errorf("alerting.%s: %w", name, err)
	}
	return strings.TrimSpace(v), nil
}

func (a *AlertingConfig) masked() map[string]any {
	return map[string]any{"webhook_ref": maskedRefOrEmpty(a.WebhookRef), "min_severity": a.MinSeverity, "escalation_ref": maskedRefOrEmpty(a.EscalationRef)}
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
	if p.Alerting == nil {
		return nil
	}
	for _, f := range []struct {
		name     string
		incoming *string
		stored   func(*AlertingConfig) string
	}{
		{"webhook_ref", &p.Alerting.WebhookRef, func(a *AlertingConfig) string { return a.WebhookRef }},
		{"escalation_ref", &p.Alerting.EscalationRef, func(a *AlertingConfig) string { return a.EscalationRef }},
	} {
		if strings.TrimSpace(*f.incoming) != maskedPlain {
			continue
		}
		if existing == nil || existing.Alerting == nil || !strings.HasPrefix(f.stored(existing.Alerting), "plain:") {
			return fmt.Errorf("alerting.%s: enter the webhook again (the stored value is not available)", f.name)
		}
		*f.incoming = f.stored(existing.Alerting)
	}
	return nil
}
