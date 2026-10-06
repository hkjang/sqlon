package earlywarning

import (
	"context"
	"sort"
	"time"

	"sqlon/internal/dbconn"
)

// Routing: every alert goes to exactly one destination — its database's own
// channel when the profile declares one (alerting.webhook_ref), otherwise the
// server default. One destination per alert keeps delivery bookkeeping
// (and retries) per alert exact; the daily report and the console still
// cover every database.

const defaultDestination = "default"

type route struct {
	id       string
	notifier Notifier
	notes    []Notification
	profiles map[string]bool
	page     bool // escalation: bookkeeping goes to PagedAt
}

const escalationDestination = "escalation"

// pageRoutesLocked routes due pages: a database's own escalation channel
// when it declares one, else the server's.
func (e *Engine) pageRoutesLocked(profiles map[string]dbconn.Profile, silences []Silence, now time.Time) []*route {
	pages := e.st.pendingPages(now, e.cfg.EscalateAfter, func(a *Alert) bool { return silencedUntil(silences, a, now) != nil })
	byID := map[string]*route{}
	var order []string
	for _, n := range pages {
		var target Notifier
		id := escalationDestination
		if p, ok := profiles[n.Alert.ProfileID]; ok && e.RouteEscalation != nil {
			custom, err := e.RouteEscalation(p)
			if err != nil {
				st := e.channelLocked("escalation:"+p.ID, "(호출 채널 설정 오류)")
				st.LastError, st.Profiles = err.Error(), []string{p.ID}
			} else if custom != nil {
				target, id = custom, "escalation:"+custom.ID()
			}
		}
		if target == nil {
			target = e.Escalation
		}
		if target == nil {
			continue
		}
		r := byID[id]
		if r == nil {
			r = &route{id: id, notifier: target, profiles: map[string]bool{}, page: true}
			byID[id] = r
			order = append(order, id)
		}
		r.notes = append(r.notes, n)
		r.profiles[n.Alert.ProfileID] = true
	}
	var out []*route
	for _, id := range order {
		st := e.statusLocked(id, byID[id].notifier)
		if st.NextRetryAt != nil && now.Before(*st.NextRetryAt) {
			continue
		}
		out = append(out, byID[id])
	}
	return out
}

// routeLocked groups notifications by destination, skipping destinations
// still in backoff. A profile whose channel cannot be resolved (missing env
// var, bad URL) falls back to the default so the alert is not lost, and the
// failure shows on that channel's status.
func (e *Engine) routeLocked(notes []Notification, profiles map[string]dbconn.Profile, now time.Time) []*route {
	byID := map[string]*route{}
	var order []string
	add := func(id string, n Notifier, note Notification) {
		r := byID[id]
		if r == nil {
			r = &route{id: id, notifier: n, profiles: map[string]bool{}}
			byID[id] = r
			order = append(order, id)
		}
		r.notes = append(r.notes, note)
		r.profiles[note.Alert.ProfileID] = true
	}
	for _, note := range notes {
		if p, ok := profiles[note.Alert.ProfileID]; ok && e.Route != nil {
			custom, err := e.Route(p)
			if err != nil {
				st := e.channelLocked("profile:"+p.ID, "(설정 오류)")
				st.LastError = err.Error()
				st.Profiles = []string{p.ID}
			} else if custom != nil {
				add(custom.ID(), custom, note)
				continue
			}
		}
		if e.Notifier != nil {
			add(defaultDestination, e.Notifier, note)
		}
	}
	var out []*route
	for _, id := range order {
		st := e.statusLocked(id, byID[id].notifier)
		if st.NextRetryAt != nil && now.Before(*st.NextRetryAt) {
			continue
		}
		out = append(out, byID[id])
	}
	return out
}

func (e *Engine) statusLocked(id string, n Notifier) *DeliveryStatus {
	if id == escalationDestination {
		e.st.Escalation.Configured = true
		e.st.Escalation.Target = n.Target()
		return &e.st.Escalation
	}
	if id == defaultDestination {
		e.st.Delivery.Configured = true
		e.st.Delivery.Target = n.Target()
		return &e.st.Delivery
	}
	return e.channelLocked(id, n.Target())
}

func (e *Engine) channelLocked(id, target string) *DeliveryStatus {
	if e.st.Channels == nil {
		e.st.Channels = map[string]*DeliveryStatus{}
	}
	st := e.st.Channels[id]
	if st == nil {
		st = &DeliveryStatus{Configured: true}
		e.st.Channels[id] = st
	}
	st.Target = target
	return st
}

// deliver sends each route's notifications and records the outcome. It is
// called without the engine lock held.
func (e *Engine) deliver(ctx context.Context, routes []*route, names map[string]string, report *CycleReport) {
	for _, r := range routes {
		dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := r.notifier.Notify(dctx, r.notes, names)
		cancel()
		e.mu.Lock()
		at := e.now()
		st := e.statusLocked(r.id, r.notifier)
		st.LastAttemptAt = &at
		st.Profiles = sortedKeys(r.profiles)
		report.Notifications += len(r.notes)
		if err != nil {
			st.Failed++
			st.ConsecutiveFailures++
			st.LastError = err.Error()
			backoff := time.Duration(1<<min(st.ConsecutiveFailures-1, 5)) * time.Minute // 1,2,4…32 min
			next := at.Add(backoff)
			st.NextRetryAt = &next
			if report.DeliveryError == "" {
				report.DeliveryError = err.Error()
			}
			e.logf("early-warning: delivery to %s failed (%d pending, retry after %s): %v", st.Target, len(r.notes), backoff, err)
		} else {
			st.Delivered += int64(len(r.notes))
			st.ConsecutiveFailures, st.LastError, st.NextRetryAt = 0, "", nil
			st.LastSuccessAt = &at
			if r.page {
				e.st.markPaged(r.notes, at)
			} else {
				e.st.markDelivered(r.notes, at)
			}
			report.Delivered = true
		}
		e.mu.Unlock()
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
