// Package mail sends event notifications through a company SMTP relay.
//
// The common in-house relay listens on port 25 with no credentials and no
// TLS, so that is the default; STARTTLS/implicit TLS and AUTH are negotiated
// only when configured or advertised. Nothing in this package blocks a
// request: the Service delivers in the background and records every attempt
// (subject and recipient, never the body) so an administrator can answer
// "did it go out?" without reading mail server logs.
package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"
)

var (
	// ErrDisabled is returned by SendNow when mail.enabled is off.
	ErrDisabled = errors.New("메일 발송이 꺼져 있습니다 (mail.enabled=false)")
	// ErrInvalid wraps every configuration or addressing problem.
	ErrInvalid = errors.New("메일 설정 오류")
)

// Message is one outbound mail after recipients were resolved.
type Message struct {
	To      string
	Subject string
	Body    string
}

// Deliver opens a relay session and sends one message. Exported so the
// settings screen can prove the relay works before anything depends on it.
func Deliver(ctx context.Context, cfg Config, msg Message) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(msg.To) == "" {
		return fmt.Errorf("%w: 수신자가 없습니다", ErrInvalid)
	}
	c, err := dial(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if err := startSession(c, cfg); err != nil {
		return err
	}
	if err := c.Mail(cfg.FromAddress); err != nil {
		return fmt.Errorf("MAIL FROM 실패: %w", err)
	}
	if err := c.Rcpt(strings.TrimSpace(msg.To)); err != nil {
		return fmt.Errorf("RCPT TO 실패: %w", err)
	}
	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("DATA 실패: %w", err)
	}
	if _, err := wc.Write([]byte(compose(cfg, msg, time.Now()))); err != nil {
		return fmt.Errorf("본문 전송 실패: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("본문 종료 실패: %w", err)
	}
	return c.Quit()
}

// Verify runs the handshake (and AUTH when configured) without sending.
func Verify(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	c, err := dial(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if err := startSession(c, cfg); err != nil {
		return err
	}
	return c.Quit()
}

func dial(ctx context.Context, cfg Config) (*smtp.Client, error) {
	d := &net.Dialer{Timeout: cfg.Timeout}
	var conn net.Conn
	var err error
	if cfg.Security == SecurityTLS {
		td := &tls.Dialer{NetDialer: d, Config: cfg.tlsConfig()}
		conn, err = td.DialContext(ctx, "tcp", cfg.endpoint())
	} else {
		conn, err = d.DialContext(ctx, "tcp", cfg.endpoint())
	}
	if err != nil {
		return nil, fmt.Errorf("SMTP 연결 실패(%s): %w", cfg.endpoint(), err)
	}
	// The whole session, not just the dial, is bounded by the timeout so a
	// relay that accepts the connection and then stalls cannot pin a goroutine.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(cfg.Timeout))
	}
	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("SMTP 세션 시작 실패: %w", err)
	}
	return c, nil
}

// startSession upgrades and authenticates only as far as the relay allows,
// so an anonymous port-25 relay and a hosted provider that demands STARTTLS
// plus AUTH both work from the same settings.
func startSession(c *smtp.Client, cfg Config) error {
	if err := c.Hello(cfg.helloName()); err != nil {
		return fmt.Errorf("EHLO 실패: %w", err)
	}
	if cfg.Security == SecurityStartTLS || cfg.Security == SecurityAuto {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(cfg.tlsConfig()); err != nil {
				return fmt.Errorf("STARTTLS 실패: %w", err)
			}
		} else if cfg.Security == SecurityStartTLS {
			return fmt.Errorf("%w: 서버가 STARTTLS를 지원하지 않습니다", ErrInvalid)
		}
	}
	if cfg.Username == "" {
		return nil
	}
	// Credentials never travel over a plaintext session unless the admin chose
	// security=none on purpose. The decision rests on the actual session state
	// (not the host name, which net/smtp always reports as cfg.Host) so a relay
	// that offers AUTH without STARTTLS under security=auto fails here, before
	// any AUTH line is written.
	_, secure := c.TLSConnectionState()
	allowPlaintext := cfg.Security == SecurityNone
	if !secure && !allowPlaintext {
		return fmt.Errorf("%w: STARTTLS 없이 인증 정보를 보낼 수 없습니다. mail.security=starttls/tls 를 쓰거나, 릴레이가 정말 평문 인증만 받으면 none 을 명시하세요", ErrInvalid)
	}
	ok, mechs := c.Extension("AUTH")
	if !ok {
		return fmt.Errorf("%w: 서버가 AUTH를 지원하지 않습니다. mail.username을 비우고 사용하세요", ErrInvalid)
	}
	upper := strings.ToUpper(mechs)
	switch {
	case strings.Contains(upper, "PLAIN"):
		return c.Auth(plainAuth{user: cfg.Username, pass: cfg.Password, allowPlaintext: allowPlaintext})
	case strings.Contains(upper, "LOGIN"):
		return c.Auth(loginAuth{user: cfg.Username, pass: cfg.Password, allowPlaintext: allowPlaintext})
	default:
		return c.Auth(smtp.CRAMMD5Auth(cfg.Username, cfg.Password))
	}
}

// errPlaintextAuth is the second line of defence behind startSession: an Auth
// asked to start on a non-TLS session refuses unless security=none was chosen.
var errPlaintextAuth = errors.New("TLS 없는 세션에서는 인증 정보를 보내지 않습니다 (mail.security=none 을 명시한 경우만 허용)")

// plainAuth is PLAIN with the same plaintext policy as loginAuth. The standard
// library's PlainAuth decides by host name (localhost is exempt), which is not
// the rule this package documents.
type plainAuth struct {
	user, pass     string
	allowPlaintext bool
}

func (a plainAuth) Start(info *smtp.ServerInfo) (string, []byte, error) {
	if !info.TLS && !a.allowPlaintext {
		return "", nil, errPlaintextAuth
	}
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}

func (a plainAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		return nil, fmt.Errorf("PLAIN 인증에 예상하지 않은 서버 응답: %q", fromServer)
	}
	return nil, nil
}

// loginAuth speaks the LOGIN mechanism many corporate relays (Exchange in
// particular) offer instead of PLAIN; the standard library ships only PLAIN
// and CRAM-MD5.
type loginAuth struct {
	user, pass     string
	allowPlaintext bool
}

func (a loginAuth) Start(info *smtp.ServerInfo) (string, []byte, error) {
	if !info.TLS && !a.allowPlaintext {
		return "", nil, errPlaintextAuth
	}
	return "LOGIN", nil, nil
}

func (a loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimRight(string(fromServer), ": ")) {
	case "username":
		return []byte(a.user), nil
	case "password":
		return []byte(a.pass), nil
	}
	return nil, fmt.Errorf("알 수 없는 LOGIN 프롬프트: %q", fromServer)
}

// compose renders a plain-text MIME message. Headers are Q-encoded so Korean
// subjects survive relays that predate UTF-8 headers.
func compose(cfg Config, msg Message, now time.Time) string {
	var b strings.Builder
	b.WriteString("From: " + cfg.fromHeader() + "\r\n")
	b.WriteString("To: " + strings.TrimSpace(msg.To) + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", msg.Subject) + "\r\n")
	b.WriteString("Date: " + now.Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("Auto-Submitted: auto-generated\r\n")
	b.WriteString("X-SQLON-Notification: 1\r\n")
	b.WriteString("\r\n")
	b.WriteString(crlf(msg.Body))
	return b.String()
}

// crlf normalises line endings. Dot-stuffing is left to net/smtp's DATA
// writer, which already escapes leading dots and appends the terminator.
func crlf(body string) string {
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")
	if !strings.HasSuffix(body, "\r\n") {
		body += "\r\n"
	}
	return body
}
