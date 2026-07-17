package change

import (
	"context"
	"errors"
	"testing"
)

type testRunner struct{ fail bool }

func (r testRunner) Revalidate(context.Context, Plan) error { return nil }
func (r testRunner) Execute(context.Context, Plan, Step) error {
	if r.fail {
		return errors.New("write failed")
	}
	return nil
}
func (r testRunner) Verify(context.Context, Plan) error           { return nil }
func (r testRunner) Compensate(context.Context, Plan, Step) error { return nil }

func TestCreateIdempotencyAndApproval(t *testing.T) {
	s := NewService()
	p := validPlan()
	a, err := s.Create(p, "request-1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(p, "request-1")
	if err != nil || a.CreatedAt != b.CreatedAt {
		t.Fatal("idempotency failed")
	}
	if _, err := s.Submit(p.ID); err != nil {
		t.Fatal(err)
	}
	approved, err := s.Approve(p.ID, "dba1")
	if err != nil {
		t.Fatal(err)
	}
	if approved.State != Approved {
		t.Fatalf("got %s", approved.State)
	}
}

func TestExecuteRequiresApprovalAndRecordsFailure(t *testing.T) {
	s := NewService()
	p := validPlan()
	if _, err := s.Create(p, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(context.Background(), p.ID, testRunner{}); err == nil {
		t.Fatal("unapproved plan executed")
	}
	if _, err := s.Submit(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(p.ID, "dba1"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Execute(context.Background(), p.ID, testRunner{fail: true})
	if err == nil || got.State != RollbackRequired {
		t.Fatalf("got plan=%+v err=%v", got, err)
	}
}

func TestCreateEnforcesRiskApprovalPolicy(t *testing.T) {
	s := NewService()
	p := validPlan()
	p.Risk = Critical
	p.RequiredApprovals = 0
	got, err := s.Create(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.RequiredApprovals != 2 {
		t.Fatalf("critical plan approvals = %d, want 2", got.RequiredApprovals)
	}
	if _, err := s.Submit(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(p.ID, "dba1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(p.ID, "dba1"); err == nil {
		t.Fatal("duplicate approver accepted")
	}
}
