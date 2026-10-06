package earlywarning

import (
	"errors"
	"sort"
	"strings"
	"time"
)

// A Silence holds notifications back during planned work — a migration, a
// volume resize, a failover drill — without hiding anything: alerts keep
// firing on the console, and whatever is still true when the silence ends
// is sent then.
type Silence struct {
	ID        string    `json:"id"`
	ProfileID string    `json:"profile_id,omitempty"` // empty = every database
	Rule      string    `json:"rule,omitempty"`       // empty = every rule; a trailing * matches a prefix (maint_*)
	Reason    string    `json:"reason"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	EndsAt    time.Time `json:"ends_at"`
}

const maxSilence = 7 * 24 * time.Hour

func (s Silence) matches(a *Alert, now time.Time) bool {
	if !now.Before(s.EndsAt) {
		return false
	}
	if s.ProfileID != "" && s.ProfileID != a.ProfileID {
		return false
	}
	switch {
	case s.Rule == "":
		return true
	case strings.HasSuffix(s.Rule, "*"):
		return strings.HasPrefix(a.Rule, strings.TrimSuffix(s.Rule, "*"))
	}
	return s.Rule == a.Rule
}

// AddSilence registers a silence lasting d (at most 7 days).
func (e *Engine) AddSilence(s Silence, d time.Duration) (Silence, error) {
	if d <= 0 || d > maxSilence {
		return Silence{}, errors.New("duration must be between 1m and 168h")
	}
	if strings.TrimSpace(s.Reason) == "" {
		return Silence{}, errors.New("reason is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.addSilenceLocked(s, d)
}

func (e *Engine) addSilenceLocked(s Silence, d time.Duration) (Silence, error) {
	now := e.now()
	s.ID = "sil-" + strings.TrimPrefix(newAlertID(), "ew-")
	s.CreatedAt, s.EndsAt = now, now.Add(d)
	s.Reason, s.Rule, s.ProfileID = strings.TrimSpace(s.Reason), strings.TrimSpace(s.Rule), strings.TrimSpace(s.ProfileID)
	e.st.Silences = append(e.st.Silences, s)
	return s, e.saveStateLocked()
}

// EnsureSilence is AddSilence unless a silence for exactly this database and
// rule is already active — a repeated chat click — which it returns instead
// (created is false).
func (e *Engine) EnsureSilence(s Silence, d time.Duration) (Silence, bool, error) {
	if d <= 0 || d > maxSilence {
		return Silence{}, false, errors.New("duration must be between 1m and 168h")
	}
	if strings.TrimSpace(s.Reason) == "" {
		return Silence{}, false, errors.New("reason is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	for _, x := range e.st.Silences {
		if x.ProfileID == strings.TrimSpace(s.ProfileID) && x.Rule == strings.TrimSpace(s.Rule) && now.Before(x.EndsAt) {
			return x, false, nil
		}
	}
	created, err := e.addSilenceLocked(s, d)
	return created, err == nil, err
}

// EndSilence ends a silence now; held-back notifications go out next cycle.
func (e *Engine) EndSilence(id string) (Silence, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	for i := range e.st.Silences {
		if e.st.Silences[i].ID == id && now.Before(e.st.Silences[i].EndsAt) {
			e.st.Silences[i].EndsAt = now
			return e.st.Silences[i], e.saveStateLocked()
		}
	}
	return Silence{}, errAlertNotFound
}

// Silence returns one active silence.
func (e *Engine) Silence(id string) (Silence, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range e.st.Silences {
		if s.ID == id && e.now().Before(s.EndsAt) {
			return s, true
		}
	}
	return Silence{}, false
}

func (e *Engine) activeSilencesLocked(now time.Time) []Silence {
	var out []Silence
	for _, s := range e.st.Silences {
		if now.Before(s.EndsAt) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EndsAt.Before(out[j].EndsAt) })
	return out
}

// silencedUntil reports the latest end among silences covering the alert.
func silencedUntil(silences []Silence, a *Alert, now time.Time) *time.Time {
	var until *time.Time
	for _, s := range silences {
		if s.matches(a, now) && (until == nil || s.EndsAt.After(*until)) {
			end := s.EndsAt
			until = &end
		}
	}
	return until
}
