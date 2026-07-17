package dbconn

import (
	"errors"
	"testing"
)

func TestCostCeilingError(t *testing.T) {
	plan := &PlanResult{TotalCost: 250_000, MaxCardinality: 5_000_000}

	// disabled by default (0 caps) → no error
	if e := costCeilingError(plan, Policy{}); e != nil {
		t.Fatalf("disabled ceiling should not block, got %v", e)
	}

	// cost cap tripped
	e := costCeilingError(plan, Policy{MaxPlanCost: 100_000})
	if e == nil || e.Measure != "cost" || e.Actual != 250_000 || e.Limit != 100_000 {
		t.Fatalf("cost ceiling not tripped correctly: %+v", e)
	}

	// rows cap tripped when cost is fine
	e = costCeilingError(plan, Policy{MaxPlanRows: 1_000_000})
	if e == nil || e.Measure != "rows" {
		t.Fatalf("rows ceiling not tripped: %+v", e)
	}

	// within caps → no error
	if e := costCeilingError(plan, Policy{MaxPlanCost: 500_000, MaxPlanRows: 9_000_000}); e != nil {
		t.Fatalf("within caps should pass, got %v", e)
	}

	// cost takes precedence over rows when both trip
	e = costCeilingError(plan, Policy{MaxPlanCost: 1, MaxPlanRows: 1})
	if e == nil || e.Measure != "cost" {
		t.Fatalf("cost should be reported first: %+v", e)
	}
}

func TestPlanCostErrorIsDistinctType(t *testing.T) {
	var err error = &PlanCostError{Plan: &PlanResult{}, Measure: "cost", Actual: 9, Limit: 1}
	var ce *PlanCostError
	if !errors.As(err, &ce) {
		t.Fatal("errors.As should match PlanCostError")
	}
	var ge *PlanGateError
	if errors.As(err, &ge) {
		t.Fatal("PlanCostError must not match PlanGateError")
	}
	if ce.Error() == "" {
		t.Fatal("error message should be non-empty")
	}
}
