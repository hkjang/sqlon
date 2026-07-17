package change

import (
	"context"
	"testing"
	"time"
)

func at(day time.Weekday, hh, mm int) time.Time {
	// 2026-07-13 is a Monday; offset to the requested weekday.
	base := time.Date(2026, 7, 13, hh, mm, 0, 0, time.UTC)
	return base.AddDate(0, 0, (int(day)-int(time.Monday)+7)%7)
}

func TestWindowContainsSameDayAndWrap(t *testing.T) {
	day := Window{Days: []string{"sat"}, Start: "02:00", End: "04:00"}
	if !day.Contains(at(time.Saturday, 3, 0)) {
		t.Fatal("saturday 03:00 should be inside the window")
	}
	if day.Contains(at(time.Saturday, 4, 0)) {
		t.Fatal("end is exclusive: 04:00 must be outside")
	}
	if day.Contains(at(time.Sunday, 3, 0)) {
		t.Fatal("sunday must be outside a saturday-only window")
	}

	// Wraps midnight: Sat 22:00 → Sun 02:00, keyed to the start day (sat).
	wrap := Window{Days: []string{"sat"}, Start: "22:00", End: "02:00"}
	if !wrap.Contains(at(time.Saturday, 23, 0)) {
		t.Fatal("saturday 23:00 should be inside the wrapping window")
	}
	if !wrap.Contains(at(time.Sunday, 1, 0)) {
		t.Fatal("sunday 01:00 belongs to the saturday-start wrapping window")
	}
	if wrap.Contains(at(time.Sunday, 23, 0)) {
		t.Fatal("sunday 23:00 must be outside a saturday-start window")
	}
}

func TestWindowEmptyDaysMeansEveryDay(t *testing.T) {
	w := Window{Start: "01:00", End: "05:00"}
	for d := time.Sunday; d <= time.Saturday; d++ {
		if !w.Contains(at(d, 2, 0)) {
			t.Fatalf("empty-days window should include %s 02:00", d)
		}
	}
}

func TestMaintenanceGate(t *testing.T) {
	plan := Plan{Risk: High, MaintenanceWindows: []Window{{Days: []string{"sun"}, Start: "01:00", End: "03:00"}}}
	if ok, _ := plan.MaintenanceGate(at(time.Sunday, 2, 0)); !ok {
		t.Fatal("in-window execution should be allowed")
	}
	if ok, reason := plan.MaintenanceGate(at(time.Monday, 2, 0)); ok || reason == "" {
		t.Fatalf("out-of-window execution should be refused with a reason, got ok=%v", ok)
	}
	// Emergency bypasses the schedule entirely.
	emergency := Plan{Risk: Emergency, MaintenanceWindows: plan.MaintenanceWindows}
	if ok, _ := emergency.MaintenanceGate(at(time.Monday, 2, 0)); !ok {
		t.Fatal("emergency change must bypass the maintenance window")
	}
	// No windows configured = always allowed.
	if ok, _ := (&Plan{Risk: High}).MaintenanceGate(at(time.Monday, 12, 0)); !ok {
		t.Fatal("plan without windows should always be allowed")
	}
}

func TestExecuteRefusesOutsideMaintenanceWindowButStaysApproved(t *testing.T) {
	s := NewService()
	fixed := at(time.Monday, 12, 0) // outside the sunday window below
	s.now = func() time.Time { return fixed }
	p := validPlan()
	p.Risk = High
	p.MaintenanceWindows = []Window{{Days: []string{"sun"}, Start: "01:00", End: "03:00"}}
	if _, err := s.Create(p, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(p.ID, "dba1"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Execute(context.Background(), p.ID, testRunner{})
	if err == nil {
		t.Fatal("execution outside the maintenance window must be refused")
	}
	if got.State != Approved {
		t.Fatalf("refused plan must stay Approved (retryable), got %s", got.State)
	}
	// Once the window opens, the same approved plan executes.
	s.now = func() time.Time { return at(time.Sunday, 2, 0) }
	done, err := s.Execute(context.Background(), p.ID, testRunner{})
	if err != nil || done.State != Completed {
		t.Fatalf("in-window execution failed: state=%s err=%v", done.State, err)
	}
}

func TestValidateRejectsMalformedWindow(t *testing.T) {
	p := validPlan()
	p.MaintenanceWindows = []Window{{Start: "25:00", End: "26:00"}}
	if err := p.Validate(); err == nil {
		t.Fatal("malformed window time must fail validation")
	}
	p.MaintenanceWindows = []Window{{Days: []string{"funday"}, Start: "01:00", End: "02:00"}}
	if err := p.Validate(); err == nil {
		t.Fatal("invalid weekday must fail validation")
	}
}
