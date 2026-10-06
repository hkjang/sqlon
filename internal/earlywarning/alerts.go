package earlywarning

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// book holds every alert — firing and resolved — and owns their lifecycle.
// It is not safe for concurrent use; the Engine serializes access.
type book struct {
	Alerts []*Alert `json:"alerts"`
}

func (b *book) firing(key string) *Alert {
	for _, a := range b.Alerts {
		if a.State == StateFiring && a.Key == key {
			return a
		}
	}
	return nil
}

// reconcile applies one check cycle for a profile: new conditions fire,
// changed ones update (escalation clears an acknowledgement — the situation
// is no longer the one that was acknowledged), and firing standing alerts
// whose check ran but no longer reports them resolve. Alerts owned by a
// check that did not run are left exactly as they were.
func (b *book) reconcile(profileID string, ran map[string]bool, conds []Condition, now time.Time) {
	merged := map[string]Condition{}
	var order []string
	for _, c := range conds {
		c.ProfileID = profileID
		c.Key = profileID + "|" + c.Key
		if prev, ok := merged[c.Key]; ok {
			if Rank(c.Severity) > Rank(prev.Severity) {
				merged[c.Key] = c
			}
			continue
		}
		merged[c.Key] = c
		order = append(order, c.Key)
	}
	for _, key := range order {
		c := merged[key]
		a := b.firing(key)
		if a == nil {
			a = &Alert{ID: newAlertID(), Key: key, ProfileID: profileID, State: StateFiring, FirstSeen: now, PeakSeverity: c.Severity}
			b.Alerts = append(b.Alerts, a)
		} else if Rank(c.Severity) > Rank(a.Severity) && a.AckedAt != nil {
			a.AckedAt, a.AckedBy, a.AckNote = nil, "", ""
		}
		a.Check, a.Rule, a.Severity, a.Object = c.Check, c.Rule, c.Severity, c.Object
		a.Title, a.Detail, a.Recommendation = c.Title, c.Detail, c.Recommendation
		a.Value, a.Threshold, a.Attributes = c.Value, c.Threshold, c.Attributes
		a.Event, a.QuietResolve, a.LastSeen = c.Event, c.QuietResolve, now
		switch {
		case c.Severity == SevCritical && a.CriticalSince == nil:
			at := now
			a.CriticalSince = &at
		case c.Severity != SevCritical:
			a.CriticalSince = nil
		}
		if Rank(c.Severity) > Rank(a.PeakSeverity) {
			a.PeakSeverity = c.Severity
		}
	}
	for _, a := range b.Alerts {
		if a.State != StateFiring || a.ProfileID != profileID || a.Event || !ran[a.Check] {
			continue
		}
		if _, still := merged[a.Key]; !still {
			b.resolve(a, now, "조건 해소")
		}
	}
}

func (b *book) resolve(a *Alert, now time.Time, reason string) {
	at := now
	a.State, a.ResolvedAt, a.ResolveReason = StateResolved, &at, reason
}

// resolveProfile resolves everything a removed profile still has firing,
// without a notification: nobody is watching that database any more.
func (b *book) resolveProfile(profileID string, now time.Time) {
	for _, a := range b.Alerts {
		if a.State == StateFiring && a.ProfileID == profileID {
			b.resolve(a, now, "프로파일 삭제됨")
			a.QuietResolve = true
		}
	}
}

// expire ages events out after ttl and drops resolved history past
// retention (keeping at most maxHistory resolved alerts).
func (b *book) expire(now time.Time, ttl, retention time.Duration, maxHistory int) {
	for _, a := range b.Alerts {
		if a.State == StateFiring && a.Event && now.Sub(a.FirstSeen) >= ttl {
			b.resolve(a, now, "이벤트 보존 기간 경과")
		}
	}
	kept := b.Alerts[:0]
	var resolved []*Alert
	for _, a := range b.Alerts {
		switch {
		case a.State == StateFiring:
			kept = append(kept, a)
		case a.ResolvedAt != nil && now.Sub(*a.ResolvedAt) < retention:
			resolved = append(resolved, a)
		}
	}
	sort.SliceStable(resolved, func(i, j int) bool { return resolved[i].ResolvedAt.After(*resolved[j].ResolvedAt) })
	if len(resolved) > maxHistory {
		resolved = resolved[:maxHistory]
	}
	b.Alerts = append(kept, resolved...)
}

// An alert that keeps firing and resolving (usage hovering at a threshold)
// trains people to ignore the channel. Once a key has fired flapThreshold
// times within flapWindow it is "flapping": new/reminder/resolved
// notifications for it are held until it settles; escalations still go out.
const (
	flapWindow    = time.Hour
	flapThreshold = 3
)

// flapCounts counts occurrences per key that began within the window.
func (b *book) flapCounts(now time.Time) map[string]int {
	counts := map[string]int{}
	for _, a := range b.Alerts {
		if !a.Event && now.Sub(a.FirstSeen) < flapWindow {
			counts[a.Key]++
		}
	}
	return counts
}

// notifyPolicy says, per alert, the lowest severity worth sending and
// whether a silence currently holds its notifications back.
type notifyPolicy func(a *Alert) (minSeverity string, silenced bool)

func fixedPolicy(minSeverity string) notifyPolicy {
	return func(*Alert) (string, bool) { return minSeverity, false }
}

// pending derives what still has to be said. Deriving it from delivery
// bookkeeping (rather than queuing messages) means a failed webhook call is
// simply retried on the next cycle, and an alert held back by a silence is
// sent — as new, escalated, or resolved — once the silence ends.
func (b *book) pending(now time.Time, policy notifyPolicy, renotify time.Duration) []Notification {
	var out []Notification
	flaps := b.flapCounts(now)
	for _, a := range b.Alerts {
		minSeverity, silenced := policy(a)
		if silenced {
			continue
		}
		flapping := flaps[a.Key] >= flapThreshold
		if a.State == StateResolved {
			if a.LastNotifiedAt != nil && !a.ResolveNotified && !a.QuietResolve && !a.Event && !flapping {
				out = append(out, Notification{Kind: KindResolved, Alert: *a})
			}
			continue
		}
		if Rank(a.Severity) < Rank(minSeverity) {
			continue
		}
		switch {
		case a.LastNotifiedAt == nil && !flapping:
			out = append(out, Notification{Kind: KindFiring, Alert: *a})
		case a.LastNotifiedAt != nil && Rank(a.Severity) > Rank(a.NotifiedSeverity):
			out = append(out, Notification{Kind: KindEscalated, Alert: *a})
		case renotify > 0 && !flapping && a.LastNotifiedAt != nil && !a.Event && a.AckedAt == nil && now.Sub(*a.LastNotifiedAt) >= renotify:
			out = append(out, Notification{Kind: KindReminder, Alert: *a})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := kindOrder(out[i].Kind), kindOrder(out[j].Kind)
		if ri != rj {
			return ri < rj
		}
		return Rank(out[i].Alert.Severity) > Rank(out[j].Alert.Severity)
	})
	return out
}

func kindOrder(kind string) int {
	switch kind {
	case KindEscalated:
		return 0
	case KindFiring:
		return 1
	case KindReminder:
		return 2
	}
	return 3
}

func (b *book) markDelivered(notes []Notification, now time.Time) {
	byID := map[string]*Alert{}
	for _, a := range b.Alerts {
		byID[a.ID] = a
	}
	for _, n := range notes {
		a := byID[n.Alert.ID]
		if a == nil {
			continue
		}
		if n.Kind == KindResolved {
			a.ResolveNotified = true
			continue
		}
		at := now
		a.LastNotifiedAt, a.NotifiedSeverity = &at, n.Alert.Severity
		a.NotifyCount++
	}
}

var errAlertNotFound = errors.New("alert not found")

// ack acknowledges an alert: a standing one stops sending reminders until it
// escalates or resolves; an event (it already happened) is closed.
func (b *book) ack(id, actor, note string, now time.Time) (Alert, error) {
	for _, a := range b.Alerts {
		if a.ID != id {
			continue
		}
		if a.State != StateFiring {
			return *a, fmt.Errorf("alert %s is already resolved", id)
		}
		at := now
		a.AckedAt, a.AckedBy, a.AckNote = &at, strings.TrimSpace(actor), strings.TrimSpace(note)
		if a.Event {
			b.resolve(a, now, "확인 처리됨")
		}
		return *a, nil
	}
	return Alert{}, errAlertNotFound
}

func newAlertID() string {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("ew-%d", time.Now().UnixNano())
	}
	return "ew-" + hex.EncodeToString(raw[:])
}

func shortHash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:6])
}
