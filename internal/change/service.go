package change

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Runner is the only bridge from an approved plan to a write-capable database
// connection. It performs just-in-time revalidation and post-change checks.
type Runner interface {
	Revalidate(context.Context, Plan) error
	Execute(context.Context, Plan, Step) error
	Verify(context.Context, Plan) error
	Compensate(context.Context, Plan, Step) error
}

// Rollback executes compensations in reverse order. It is allowed only after
// an execution/verification failure has explicitly marked rollback_required.
func (s *Service) Rollback(ctx context.Context, id string, runner Runner) (Plan, error) {
	if runner == nil {
		return Plan{}, errors.New("change runner is required")
	}
	s.mu.Lock()
	p, ok := s.plans[id]
	if !ok {
		s.mu.Unlock()
		return Plan{}, fmt.Errorf("change plan %q not found", id)
	}
	if p.State != RollbackRequired {
		s.mu.Unlock()
		return Plan{}, fmt.Errorf("plan %q does not require rollback", id)
	}
	if err := p.Transition(RollingBack, s.now()); err != nil {
		s.mu.Unlock()
		return Plan{}, err
	}
	s.plans[id] = p
	s.mu.Unlock()
	for i := len(p.Steps) - 1; i >= 0; i-- {
		if err := runner.Compensate(ctx, p, p.Steps[i]); err != nil {
			return s.failRollback(id, err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p = s.plans[id]
	if err := p.Transition(RolledBack, s.now()); err != nil {
		return Plan{}, err
	}
	s.plans[id] = p
	return p, nil
}

func (s *Service) failRollback(id string, cause error) (Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.plans[id]
	_ = p.Transition(Failed, s.now())
	s.plans[id] = p
	return p, cause
}

// Service is the shared API/MCP change-control service. It stores plans in
// memory for the standalone mode; storage adapters can implement persistence
// without changing approval or transition policy.
type Service struct {
	mu          sync.RWMutex
	plans       map[string]Plan
	idempotency map[string]string
	now         func() time.Time
}

func NewService() *Service {
	return &Service{plans: map[string]Plan{}, idempotency: map[string]string{}, now: time.Now}
}
func (s *Service) Create(p Plan, key string) (Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key != "" {
		if id, ok := s.idempotency[key]; ok {
			return s.plans[id], nil
		}
	}
	if p.ID == "" {
		return Plan{}, errors.New("change plan id is required")
	}
	if _, exists := s.plans[p.ID]; exists {
		return Plan{}, fmt.Errorf("change plan %q already exists", p.ID)
	}
	if p.State == "" {
		p.State = Draft
	}
	required, err := ApprovalRequirement(p.Risk)
	if err != nil {
		return Plan{}, err
	}
	p.RequiredApprovals = required
	p.Approvals = nil
	if err := p.Validate(); err != nil {
		return Plan{}, err
	}
	p.CreatedAt = s.now().UTC()
	p.UpdatedAt = p.CreatedAt
	s.plans[p.ID] = p
	if key != "" {
		s.idempotency[key] = p.ID
	}
	return p, nil
}

// Submit freezes the plan for review. Low-risk plans can become executable
// without human approval; every other risk class must enter review_required.
func (s *Service) Submit(id string) (Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[id]
	if !ok {
		return Plan{}, fmt.Errorf("change plan %q not found", id)
	}
	if p.State != Draft {
		return Plan{}, fmt.Errorf("plan %q is not a draft", id)
	}
	if err := p.Transition(Analyzing, s.now()); err != nil {
		return Plan{}, err
	}
	next := ReviewRequired
	if p.RequiredApprovals == 0 {
		next = Approved
	}
	if err := p.Transition(next, s.now()); err != nil {
		return Plan{}, err
	}
	s.plans[id] = p
	return p, nil
}

func (s *Service) Get(id string) (Plan, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.plans[id]
	return p, ok
}
func (s *Service) Approve(id, actor string) (Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[id]
	if !ok {
		return Plan{}, fmt.Errorf("change plan %q not found", id)
	}
	if actor == "" {
		return Plan{}, errors.New("approver is required")
	}
	if p.State != ReviewRequired {
		return Plan{}, fmt.Errorf("plan %q is not awaiting approval", id)
	}
	for _, approval := range p.Approvals {
		if approval.Actor == actor && approval.Decision == "approved" {
			return Plan{}, fmt.Errorf("approver %q has already approved plan %q", actor, id)
		}
	}
	now := s.now().UTC()
	p.Approvals = append(p.Approvals, Approval{ID: fmt.Sprintf("%s-a%d-%d", p.ID, len(p.Approvals)+1, now.UnixNano()), Actor: actor, Decision: "approved", At: now})
	if len(p.Approvals) >= p.RequiredApprovals {
		if err := p.Transition(Approved, s.now()); err != nil {
			return Plan{}, err
		}
	}
	s.plans[id] = p
	return p, nil
}

func (s *Service) Cancel(id string) (Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[id]
	if !ok {
		return Plan{}, fmt.Errorf("change plan %q not found", id)
	}
	if err := p.Transition(Cancelled, s.now()); err != nil {
		return Plan{}, err
	}
	s.plans[id] = p
	return p, nil
}

// Execute accepts no raw SQL. It only runs the immutable steps of a plan that
// has reached Approved (or Scheduled) through the normal approval workflow.
func (s *Service) Execute(ctx context.Context, id string, runner Runner) (Plan, error) {
	if runner == nil {
		return Plan{}, errors.New("change runner is required")
	}
	s.mu.Lock()
	p, ok := s.plans[id]
	if !ok {
		s.mu.Unlock()
		return Plan{}, fmt.Errorf("change plan %q not found", id)
	}
	if p.State != Approved && p.State != Scheduled {
		s.mu.Unlock()
		return Plan{}, fmt.Errorf("plan %q is not approved for execution", id)
	}
	if len(p.Approvals) < p.RequiredApprovals {
		s.mu.Unlock()
		return Plan{}, errors.New("required approvals are missing")
	}
	if err := p.Transition(Executing, s.now()); err != nil {
		s.mu.Unlock()
		return Plan{}, err
	}
	s.plans[id] = p
	s.mu.Unlock()
	if err := runner.Revalidate(ctx, p); err != nil {
		return s.fail(id, err)
	}
	for _, step := range p.Steps {
		if err := runner.Execute(ctx, p, step); err != nil {
			return s.fail(id, err)
		}
	}
	if err := runner.Verify(ctx, p); err != nil {
		return s.fail(id, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p = s.plans[id]
	if err := p.Transition(Verifying, s.now()); err != nil {
		return Plan{}, err
	}
	if err := p.Transition(Completed, s.now()); err != nil {
		return Plan{}, err
	}
	s.plans[id] = p
	return p, nil
}

func (s *Service) fail(id string, cause error) (Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.plans[id]
	if p.State == Executing {
		_ = p.Transition(RollbackRequired, s.now())
	}
	s.plans[id] = p
	return p, cause
}
