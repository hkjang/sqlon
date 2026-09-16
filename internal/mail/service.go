package mail

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Settings yields the effective settings map (stored over bootstrap). It is
// read on every send so a change in the admin screen applies at once.
type Settings interface {
	Values(ctx context.Context) (map[string]string, error)
}

// Directory turns account identifiers (usernames) into mail addresses using
// the user table the app already has. Mail keeps no roster of its own.
type Directory interface {
	Emails(ctx context.Context, ids []string) (map[string]string, error)
}

// Delivery is one attempt to send one mail. The body is intentionally absent:
// the log must let an admin see what left the building without becoming a
// second copy of it.
type Delivery struct {
	ID        string    `json:"id"`
	Event     string    `json:"event"`
	Recipient string    `json:"recipient"`
	Subject   string    `json:"subject"`
	Ref       string    `json:"ref,omitempty"`
	Actor     string    `json:"actor,omitempty"`
	Status    string    `json:"status"` // queued | sent | failed
	Attempts  int       `json:"attempts"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Page is the admin view of the delivery log.
type Page struct {
	Items   []Delivery     `json:"items"`
	Total   int            `json:"total"`
	Status  map[string]int `json:"status"`
	Skipped []string       `json:"skipped,omitempty"`
}

// Service resolves recipients, sends in the background, and records every
// attempt. Zero value is unusable; use New.
type Service struct {
	settings  Settings
	directory Directory
	log       *Log
	send      func(context.Context, Config, Message) error
	now       func() time.Time
	wg        sync.WaitGroup
}

// New wires a service. dataDir may be empty, in which case the delivery log
// lives only in memory.
func New(settings Settings, directory Directory, dataDir string) *Service {
	return &Service{
		settings:  settings,
		directory: directory,
		log:       NewLog(dataDir),
		send:      Deliver,
		now:       func() time.Time { return time.Now().UTC() },
	}
}

// SetSender swaps the transport so tests can drive the service without a relay.
func (s *Service) SetSender(f func(context.Context, Config, Message) error) { s.send = f }

// Wait blocks until background deliveries finish (tests and shutdown).
func (s *Service) Wait() { s.wg.Wait() }

// Config returns the current effective configuration.
func (s *Service) Config(ctx context.Context) (Config, error) {
	if s.settings == nil {
		return FromSettings(nil), nil
	}
	values, err := s.settings.Values(ctx)
	if err != nil {
		return Config{}, err
	}
	return FromSettings(values), nil
}

// Notify sends one event mail to each recipient in the background. It never
// returns an error: mail failure is not the caller's failure. The actor is
// dropped from the recipients so nobody is told about their own action, and a
// disabled or incomplete configuration is a quiet no-op (the reason is logged
// once per call, never per recipient).
func (s *Service) Notify(ctx context.Context, n Notification, actor string, recipients []string) {
	if s == nil {
		return
	}
	cfg, err := s.Config(ctx)
	if err != nil {
		log.Printf("mail: settings unavailable, %s not sent: %v", n.Event, err)
		return
	}
	if !cfg.Enabled || !cfg.Allows(n.Event) {
		return
	}
	if err := cfg.Validate(); err != nil {
		log.Printf("mail: %s not sent: %v", n.Event, err)
		return
	}
	addresses := s.resolve(ctx, recipients, actor)
	if len(addresses) == 0 {
		return
	}
	body := n.Render(cfg)
	for _, addr := range addresses {
		d := s.log.Add(Delivery{Event: n.Event, Recipient: addr, Subject: n.Subject, Ref: n.Ref, Actor: actor,
			Status: "queued", CreatedAt: s.now(), UpdatedAt: s.now()})
		s.wg.Add(1)
		go func(d Delivery) {
			defer s.wg.Done()
			s.deliver(d, cfg, Message{To: addr, Subject: n.Subject, Body: body})
		}(d)
	}
}

// SendNow delivers synchronously and reports the outcome — what the admin's
// test button needs. The attempt is recorded like any other.
func (s *Service) SendNow(ctx context.Context, n Notification, actor, recipient string) error {
	cfg, err := s.Config(ctx)
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		return ErrDisabled
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	d := s.log.Add(Delivery{Event: n.Event, Recipient: recipient, Subject: n.Subject, Ref: n.Ref, Actor: actor,
		Status: "queued", CreatedAt: s.now(), UpdatedAt: s.now()})
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.Timeout+5*time.Second)
	defer cancel()
	err = s.send(sendCtx, cfg, Message{To: recipient, Subject: n.Subject, Body: n.Render(cfg)})
	s.complete(d, 1, err)
	return err
}

// deliver tries twice: a relay that briefly refuses a connection is common and
// losing the notification costs more than a two-second wait.
func (s *Service) deliver(d Delivery, cfg Config, msg Message) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*cfg.Timeout+15*time.Second)
	defer cancel()
	var err error
	attempts := 0
	for attempts < 2 {
		attempts++
		if err = s.send(ctx, cfg, msg); err == nil {
			break
		}
		if attempts == 1 {
			select {
			case <-ctx.Done():
				attempts = 2
			case <-time.After(2 * time.Second):
			}
		}
	}
	s.complete(d, attempts, err)
}

func (s *Service) complete(d Delivery, attempts int, cause error) {
	d.Attempts = attempts
	d.UpdatedAt = s.now()
	if cause != nil {
		d.Status, d.Error = "failed", clip(cause.Error(), 500)
		log.Printf("mail: %s to %s failed: %v", d.Event, d.Recipient, cause)
	} else {
		d.Status, d.Error = "sent", ""
	}
	s.log.Finish(d)
}

// resolve maps recipients to unique addresses, dropping the actor. A recipient
// that already looks like an address is used as-is when the directory has no
// entry for it.
func (s *Service) resolve(ctx context.Context, recipients []string, actor string) []string {
	actor = strings.TrimSpace(actor)
	wanted := make([]string, 0, len(recipients))
	for _, r := range recipients {
		r = strings.TrimSpace(r)
		if r == "" || strings.EqualFold(r, actor) {
			continue
		}
		wanted = append(wanted, r)
	}
	if len(wanted) == 0 {
		return nil
	}
	emails := map[string]string{}
	if s.directory != nil {
		found, err := s.directory.Emails(ctx, wanted)
		if err != nil {
			log.Printf("mail: recipients not resolved: %v", err)
			return nil
		}
		emails = found
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(wanted))
	for _, id := range wanted {
		addr := strings.TrimSpace(emails[strings.ToLower(id)])
		if addr == "" && strings.Contains(id, "@") {
			addr = id
		}
		if addr == "" || seen[strings.ToLower(addr)] {
			continue
		}
		seen[strings.ToLower(addr)] = true
		out = append(out, addr)
	}
	return out
}

// Deliveries lists the newest attempts with a status breakdown.
func (s *Service) Deliveries(status string, limit int) Page { return s.log.List(status, limit) }

// ---- delivery log ----

const logKeep = 500

// Log keeps the most recent deliveries in memory and appends finished ones to
// a daily JSONL file under <dataDir>/mail so a restart does not erase the
// answer to "was it sent?". In-flight (queued) items are memory only.
type Log struct {
	mu    sync.Mutex
	dir   string
	items []Delivery // oldest first
	now   func() time.Time
}

// NewLog opens the log, restoring up to logKeep finished deliveries from disk.
func NewLog(dataDir string) *Log {
	l := &Log{now: func() time.Time { return time.Now().UTC() }}
	if dataDir != "" {
		l.dir = filepath.Join(dataDir, "mail")
		l.items = loadRecent(l.dir, logKeep)
	}
	return l
}

func (l *Log) Add(d Delivery) Delivery {
	l.mu.Lock()
	defer l.mu.Unlock()
	if d.ID == "" {
		d.ID = newID()
	}
	l.items = append(l.items, d)
	l.trim()
	return d
}

// Finish replaces the queued entry with its outcome and persists it.
func (l *Log) Finish(d Delivery) {
	l.mu.Lock()
	defer l.mu.Unlock()
	replaced := false
	for i := range l.items {
		if l.items[i].ID == d.ID {
			l.items[i] = d
			replaced = true
			break
		}
	}
	if !replaced {
		l.items = append(l.items, d)
		l.trim()
	}
	l.persist(d)
}

func (l *Log) List(status string, limit int) Page {
	if limit < 1 || limit > logKeep {
		limit = 50
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	page := Page{Items: []Delivery{}, Status: map[string]int{}}
	status = strings.TrimSpace(status)
	for i := len(l.items) - 1; i >= 0; i-- {
		d := l.items[i]
		page.Status[d.Status]++
		page.Total++
		if status != "" && d.Status != status {
			continue
		}
		if len(page.Items) < limit {
			page.Items = append(page.Items, d)
		}
	}
	return page
}

func (l *Log) trim() {
	if n := len(l.items) - logKeep; n > 0 {
		l.items = append([]Delivery(nil), l.items[n:]...)
	}
}

func (l *Log) persist(d Delivery) {
	if l.dir == "" {
		return
	}
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		log.Printf("mail: delivery log dir: %v", err)
		return
	}
	path := filepath.Join(l.dir, "deliveries-"+l.now().Format("20060102")+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("mail: delivery log open: %v", err)
		return
	}
	defer f.Close()
	b, _ := json.Marshal(d)
	_, _ = f.Write(append(b, '\n'))
}

// loadRecent reads the newest files first until keep entries are collected and
// returns them oldest first.
func loadRecent(dir string, keep int) []Delivery {
	names, err := filepath.Glob(filepath.Join(dir, "deliveries-*.jsonl"))
	if err != nil || len(names) == 0 {
		return nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	var newestFirst []Delivery
	for _, name := range names {
		var file []Delivery
		f, err := os.Open(name)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			var d Delivery
			if json.Unmarshal(sc.Bytes(), &d) == nil && d.ID != "" {
				file = append(file, d)
			}
		}
		f.Close()
		for i := len(file) - 1; i >= 0 && len(newestFirst) < keep; i-- {
			newestFirst = append(newestFirst, file[i])
		}
		if len(newestFirst) >= keep {
			break
		}
	}
	out := make([]Delivery, 0, len(newestFirst))
	for i := len(newestFirst) - 1; i >= 0; i-- {
		out = append(out, newestFirst[i])
	}
	return out
}

func newID() string {
	var b [9]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "m_" + time.Now().UTC().Format("20060102150405.000000")
	}
	return "m_" + base64.RawURLEncoding.EncodeToString(b[:])
}
