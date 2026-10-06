package earlywarning

import (
	"sort"
	"time"
)

// Escalation pages an on-call channel when a critical alert stays
// unacknowledged: the team channel can be muted, busy, or asleep, and a
// volume that fills overnight is exactly the case for a second, louder
// path. Paging is derived from bookkeeping like other notifications, so a
// failed page is retried, an acknowledgement or a silence stops it, and the
// on-call channel is told when a paged alert resolves.

const (
	KindPage         = "page"
	KindPageResolved = "page_resolved"
)

// pendingPages lists pages due now. escalateAfter < 0 disables paging;
// anything up to a second means "at once" (Config stores it as 1ns).
func (b *book) pendingPages(now time.Time, escalateAfter time.Duration, silenced func(*Alert) bool) []Notification {
	if escalateAfter < 0 {
		return nil
	}
	var out []Notification
	for _, a := range b.Alerts {
		switch {
		case a.State == StateResolved:
			if a.PagedAt != nil && !a.PageResolveNotified {
				out = append(out, Notification{Kind: KindPageResolved, Alert: *a})
			}
		case a.Severity == SevCritical && a.PagedAt == nil && a.AckedAt == nil && a.CriticalSince != nil &&
			(escalateAfter <= time.Second || now.Sub(*a.CriticalSince) >= escalateAfter) && !silenced(a):
			out = append(out, Notification{Kind: KindPage, Alert: *a})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Kind == KindPage && out[j].Kind != KindPage })
	return out
}

func (b *book) markPaged(notes []Notification, now time.Time) {
	byID := map[string]*Alert{}
	for _, a := range b.Alerts {
		byID[a.ID] = a
	}
	for _, n := range notes {
		if a := byID[n.Alert.ID]; a != nil {
			if n.Kind == KindPageResolved {
				a.PageResolveNotified = true
			} else {
				at := now
				a.PagedAt = &at
			}
		}
	}
}
