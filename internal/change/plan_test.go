package change

import (
	"testing"
	"time"
)

func validPlan() Plan {
	return Plan{ID: "c1", ProfileID: "prod", Reason: "analyze", State: Draft, Steps: []Step{{Order: 1, Command: "ANALYZE", Verification: "check", Compensation: "document"}}, RequiredApprovals: 1}
}
func TestPlanBlocksExecutionWithoutApproval(t *testing.T) {
	p := validPlan()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_ = p.Transition(Analyzing, now)
	_ = p.Transition(ReviewRequired, now)
	_ = p.Transition(Approved, now)
	if err := p.Transition(Executing, now); err == nil {
		t.Fatal("unapproved execution allowed")
	}
}
