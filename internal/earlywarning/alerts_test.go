package earlywarning

import (
	"testing"
	"time"
)

func cond(key, check, sev string) Condition {
	return Condition{Key: key, Check: check, Rule: key, Severity: sev, Title: key}
}

func kinds(notes []Notification) []string {
	var out []string
	for _, n := range notes {
		out = append(out, n.Kind+":"+n.Alert.Rule)
	}
	return out
}

func TestLifecycleFireRemindEscalateResolve(t *testing.T) {
	var b book
	ran := map[string]bool{CheckCapacity: true}
	now := t0
	b.reconcile("p", ran, []Condition{cond("disk", CheckCapacity, SevWarning)}, now)
	notes := b.pending(now, SevWarning, 6*time.Hour)
	if got := kinds(notes); len(got) != 1 || got[0] != "firing:disk" {
		t.Fatalf("first sighting must fire once: %v", got)
	}
	b.markDelivered(notes, now)

	now = now.Add(time.Hour)
	b.reconcile("p", ran, []Condition{cond("disk", CheckCapacity, SevWarning)}, now)
	if got := b.pending(now, SevWarning, 6*time.Hour); len(got) != 0 {
		t.Fatalf("a standing alert must not repeat inside the renotify interval: %v", kinds(got))
	}

	now = now.Add(6 * time.Hour)
	b.reconcile("p", ran, []Condition{cond("disk", CheckCapacity, SevWarning)}, now)
	notes = b.pending(now, SevWarning, 6*time.Hour)
	if got := kinds(notes); len(got) != 1 || got[0] != "reminder:disk" {
		t.Fatalf("after the interval a reminder is due: %v", got)
	}
	b.markDelivered(notes, now)

	alert := b.firing("p|disk")
	if _, err := b.ack(alert.ID, "dba", "volume order placed", now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(7 * time.Hour)
	b.reconcile("p", ran, []Condition{cond("disk", CheckCapacity, SevWarning)}, now)
	if got := b.pending(now, SevWarning, 6*time.Hour); len(got) != 0 {
		t.Fatalf("an acknowledged alert sends no reminders: %v", kinds(got))
	}

	b.reconcile("p", ran, []Condition{cond("disk", CheckCapacity, SevCritical)}, now)
	notes = b.pending(now, SevWarning, 6*time.Hour)
	if got := kinds(notes); len(got) != 1 || got[0] != "escalated:disk" {
		t.Fatalf("escalation must notify even after an ack: %v", got)
	}
	if b.firing("p|disk").AckedAt != nil {
		t.Fatalf("escalation clears the acknowledgement")
	}
	b.markDelivered(notes, now)

	b.reconcile("p", ran, nil, now.Add(time.Minute))
	a := b.Alerts[0]
	if a.State != StateResolved || a.PeakSeverity != SevCritical {
		t.Fatalf("absent from a check that ran → resolved, keeping the peak: %+v", a)
	}
	notes = b.pending(now, SevWarning, 6*time.Hour)
	if got := kinds(notes); len(got) != 1 || got[0] != "resolved:disk" {
		t.Fatalf("a notified alert announces its resolution: %v", got)
	}
	b.markDelivered(notes, now)
	if got := b.pending(now, SevWarning, 6*time.Hour); len(got) != 0 {
		t.Fatalf("resolution is announced once: %v", kinds(got))
	}
}

func TestAlertSurvivesACheckThatDidNotRun(t *testing.T) {
	var b book
	b.reconcile("p", map[string]bool{CheckMaintenance: true}, []Condition{cond("slot", CheckMaintenance, SevCritical)}, t0)
	// Next cycle the DB is unreachable: only the collection check ran.
	b.reconcile("p", map[string]bool{CheckCollection: true}, []Condition{cond("down", CheckCollection, SevCritical)}, t0.Add(time.Minute))
	if a := b.firing("p|slot"); a == nil {
		t.Fatalf("an alert must not resolve because its check could not run")
	}
}

func TestAlertsAreScopedToTheirProfile(t *testing.T) {
	var b book
	ran := map[string]bool{CheckCapacity: true}
	b.reconcile("a", ran, []Condition{cond("disk", CheckCapacity, SevWarning)}, t0)
	b.reconcile("b", ran, nil, t0)
	if b.firing("a|disk") == nil {
		t.Fatalf("profile b's clean cycle resolved profile a's alert")
	}
}

func TestBelowMinimumSeverityIsNeverSent(t *testing.T) {
	var b book
	b.reconcile("p", map[string]bool{CheckCapacity: true}, []Condition{cond("hint", CheckCapacity, SevInfo)}, t0)
	if got := b.pending(t0, SevWarning, time.Hour); len(got) != 0 {
		t.Fatalf("info is shown on the console, not sent: %v", kinds(got))
	}
	b.reconcile("p", map[string]bool{CheckCapacity: true}, nil, t0.Add(time.Minute))
	if got := b.pending(t0, SevWarning, time.Hour); len(got) != 0 {
		t.Fatalf("an alert never sent must not announce its resolution: %v", kinds(got))
	}
}

func TestEventsNotifyOnceAndCloseOnAckOrTTL(t *testing.T) {
	var b book
	ev := cond("schema_change:h2", CheckSchema, SevCritical)
	ev.Event = true
	b.reconcile("p", map[string]bool{CheckSchema: true}, []Condition{ev}, t0)
	notes := b.pending(t0, SevWarning, time.Hour)
	b.markDelivered(notes, t0)
	b.reconcile("p", map[string]bool{CheckSchema: true}, nil, t0.Add(15*time.Minute))
	if b.firing("p|schema_change:h2") == nil {
		t.Fatalf("an event stays firing until acknowledged, its absence resolves nothing")
	}
	if got := b.pending(t0.Add(10*time.Hour), SevWarning, time.Hour); len(got) != 0 {
		t.Fatalf("events are not re-sent: %v", kinds(got))
	}
	b.expire(t0.Add(25*time.Hour), 24*time.Hour, 30*24*time.Hour, 100)
	if b.firing("p|schema_change:h2") != nil {
		t.Fatalf("an event closes after its TTL")
	}
	if got := b.pending(t0.Add(25*time.Hour), SevWarning, time.Hour); len(got) != 0 {
		t.Fatalf("an event's expiry is silent: %v", kinds(got))
	}

	ev2 := cond("schema_change:h3", CheckSchema, SevWarning)
	ev2.Event = true
	b.reconcile("p", map[string]bool{CheckSchema: true}, []Condition{ev2}, t0)
	a, err := b.ack(b.firing("p|schema_change:h3").ID, "dba", "deploy 1.4", t0)
	if err != nil || a.State != StateResolved {
		t.Fatalf("acknowledging an event closes it: %+v %v", a, err)
	}
}

func TestHistoryRetention(t *testing.T) {
	var b book
	ran := map[string]bool{CheckCapacity: true}
	for i := 0; i < 5; i++ {
		at := t0.Add(time.Duration(i) * time.Hour)
		b.reconcile("p", ran, []Condition{cond("disk", CheckCapacity, SevWarning)}, at)
		b.reconcile("p", ran, nil, at.Add(time.Minute))
	}
	b.expire(t0.Add(10*time.Hour), 24*time.Hour, 30*24*time.Hour, 3)
	if len(b.Alerts) != 3 {
		t.Fatalf("history must be capped at 3, got %d", len(b.Alerts))
	}
	b.expire(t0.Add(40*24*time.Hour), 24*time.Hour, 30*24*time.Hour, 3)
	if len(b.Alerts) != 0 {
		t.Fatalf("resolved alerts past retention must be dropped, got %d", len(b.Alerts))
	}
}
