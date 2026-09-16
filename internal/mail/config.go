package mail

import (
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"strconv"
	"strings"
	"time"
)

// Setting keys. The names are shared with every in-house service that follows
// the mail standard so an operator learns them once.
const (
	KeyEnabled       = "mail.enabled"
	KeyHost          = "mail.smtp_host"
	KeyPort          = "mail.smtp_port"
	KeySecurity      = "mail.security"
	KeySkipTLSVerify = "mail.skip_tls_verify"
	KeyUsername      = "mail.username"
	KeyPassword      = "mail.password"
	KeyFromAddress   = "mail.from_address"
	KeyFromName      = "mail.from_name"
	KeyBaseURL       = "mail.base_url"
	KeyTimeout       = "mail.timeout_seconds"

	// Per-event switches. Each covers the events listed in EventSwitches.
	KeyNotifyChangeReview  = "mail.notify_change_review"
	KeyNotifyChangeFailed  = "mail.notify_change_failed"
	KeyNotifyQueryFinished = "mail.notify_query_finished"
	KeyNotifyScheduler     = "mail.notify_scheduler"
)

// Security modes for mail.security.
const (
	SecurityAuto     = "auto"
	SecurityNone     = "none"
	SecurityStartTLS = "starttls"
	SecurityTLS      = "tls"
)

// Events this app sends. Each is something a person is actually waiting for —
// an approval to give, a turn to act, a job that finished, a loop that broke —
// never a bare "something changed".
const (
	EventChangeReviewRequired = "change.review_required" // a plan awaits approval
	EventChangeApproved       = "change.approved"        // approvals complete, ready to execute
	EventChangeFailed         = "change.failed"          // execution/rollback failed, operator action needed
	EventQueryFinished        = "query.finished"         // a long async query finished (done or failed)
	EventSchedulerFailed      = "scheduler.failed"       // scheduled metadata sync started failing
	EventSchedulerRecovered   = "scheduler.recovered"    // ...and works again
	EventTest                 = "test"                   // the admin's test button
)

// EventSwitches maps each event to the setting that turns it off. Events
// without an entry (test) are always allowed.
var EventSwitches = map[string]string{
	EventChangeReviewRequired: KeyNotifyChangeReview,
	EventChangeApproved:       KeyNotifyChangeReview,
	EventChangeFailed:         KeyNotifyChangeFailed,
	EventQueryFinished:        KeyNotifyQueryFinished,
	EventSchedulerFailed:      KeyNotifyScheduler,
	EventSchedulerRecovered:   KeyNotifyScheduler,
}

const (
	DefaultPort    = 25
	DefaultTimeout = 10 * time.Second
	DefaultFrom    = "sqlon"
)

// Config is the effective relay configuration. Username and Password are
// deliberately excluded from JSON so a Config can never leak through an API.
type Config struct {
	Enabled     bool          `json:"enabled"`
	Host        string        `json:"host"`
	Port        int           `json:"port"`
	Security    string        `json:"security"`
	SkipVerify  bool          `json:"skip_tls_verify"`
	Username    string        `json:"-"`
	Password    string        `json:"-"`
	FromAddress string        `json:"from_address"`
	FromName    string        `json:"from_name"`
	BaseURL     string        `json:"base_url"`
	Timeout     time.Duration `json:"-"`
	// Events holds explicit per-event switches; absent means on.
	Events map[string]bool `json:"-"`
}

// FromSettings reads a Config from the string→string settings map the meta DB
// stores. Missing or blank keys fall back to the relay-on-port-25 defaults.
func FromSettings(values map[string]string) Config {
	get := func(key string) string { return strings.TrimSpace(values[key]) }
	cfg := Config{
		Port:        DefaultPort,
		Security:    SecurityAuto,
		Timeout:     DefaultTimeout,
		Enabled:     parseBool(get(KeyEnabled), false),
		Host:        get(KeyHost),
		Username:    get(KeyUsername),
		Password:    values[KeyPassword], // passwords may legitimately have spaces
		FromAddress: get(KeyFromAddress),
		FromName:    get(KeyFromName),
		BaseURL:     strings.TrimRight(get(KeyBaseURL), "/"),
		SkipVerify:  parseBool(get(KeySkipTLSVerify), false),
		Events:      map[string]bool{},
	}
	if v := strings.ToLower(get(KeySecurity)); v != "" {
		cfg.Security = v
	}
	if n, err := strconv.Atoi(get(KeyPort)); err == nil && n > 0 {
		cfg.Port = n
	}
	if n, err := strconv.Atoi(get(KeyTimeout)); err == nil && n > 0 {
		cfg.Timeout = time.Duration(n) * time.Second
	}
	if cfg.FromName == "" {
		cfg.FromName = DefaultFrom
	}
	if cfg.FromAddress == "" && cfg.Host != "" {
		cfg.FromAddress = DefaultFrom + "@" + cfg.Host
	}
	// The implicit-TLS port needs no extra setting.
	if cfg.Security == SecurityAuto && cfg.Port == 465 {
		cfg.Security = SecurityTLS
	}
	for event, key := range EventSwitches {
		if v := get(key); v != "" {
			cfg.Events[event] = parseBool(v, true)
		}
	}
	return cfg
}

// Allows reports whether an event may be sent. Unknown events are allowed so
// adding a notification never requires a settings change first.
func (c Config) Allows(event string) bool {
	if on, known := c.Events[event]; known {
		return on
	}
	return true
}

// Validate checks that the relay can be addressed at all. It does not touch
// the network.
func (c Config) Validate() error {
	if c.Host == "" {
		return fmt.Errorf("%w: %s 가 필요합니다", ErrInvalid, KeyHost)
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("%w: %s 는 1~65535 사이여야 합니다", ErrInvalid, KeyPort)
	}
	if !strings.Contains(c.FromAddress, "@") {
		return fmt.Errorf("%w: %s 는 메일 주소여야 합니다", ErrInvalid, KeyFromAddress)
	}
	switch c.Security {
	case SecurityAuto, SecurityNone, SecurityStartTLS, SecurityTLS:
	default:
		return fmt.Errorf("%w: %s 는 auto·none·starttls·tls 중 하나여야 합니다", ErrInvalid, KeySecurity)
	}
	return nil
}

func (c Config) endpoint() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

// helloName is the EHLO argument. Relays that inspect the greeting are happier
// with the sender domain than with a container hostname.
func (c Config) helloName() string {
	if i := strings.LastIndex(c.FromAddress, "@"); i >= 0 && i+1 < len(c.FromAddress) {
		return c.FromAddress[i+1:]
	}
	return "localhost"
}

func (c Config) fromHeader() string {
	if c.FromName == "" {
		return c.FromAddress
	}
	return mime.QEncoding.Encode("utf-8", c.FromName) + " <" + c.FromAddress + ">"
}

func (c Config) tlsConfig() *tls.Config {
	return &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.SkipVerify} //nolint:gosec // opt-in for relays with private certificates
}

// Link joins BaseURL and an app path for the "open" line in a mail body.
// Without a base URL the line is omitted rather than guessed.
func (c Config) Link(path string) string {
	if c.BaseURL == "" || path == "" {
		return ""
	}
	return c.BaseURL + "/" + strings.TrimLeft(path, "/")
}

func parseBool(v string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "y":
		return true
	case "0", "false", "no", "off", "n":
		return false
	}
	return fallback
}
