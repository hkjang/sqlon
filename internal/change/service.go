package change

import (
	"context"
	"errors"
	"fmt"
	"sort"
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
	if err := s.persistLocked(p); err != nil {
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
	return p, s.persistLocked(p)
}

func (s *Service) failRollback(id string, cause error) (Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.plans[id]
	_ = p.Transition(Failed, s.now())
	s.plans[id] = p
	if perr := s.persistLocked(p); perr != nil {
		return p, errors.Join(cause, perr)
	}
	return p, cause
}

// Service is the shared API/MCP change-control service. In-memory state is
// authoritative for the running process; when a Store is attached, every
// mutation is written through so plans, approvals, and execution outcomes
// survive a restart. Pure state transitions (create, submit, approve, cancel,
// execution start) commit to memory only after a successful persist, so a
// disk error leaves the plan untouched and the call retryable. Outcome states
// recorded after privileged SQL already ran (completed, failed,
// rollback_required, rolled_back) commit to memory regardless and report the
// persistence error alongside — the fact on the database always wins.
type Service struct {
	mu          sync.RWMutex
	plans       map[string]Plan
	idempotency map[string]string
	store       Store
	now         func() time.Time
}

func NewService() *Service {
	return &Service{plans: map[string]Plan{}, idempotency: map[string]string{}, now: time.Now}
}

// NewServiceWithStore restores persisted plans and idempotency keys from the
// store and write-through-persists every subsequent mutation. On a partial
// load failure it returns the service with the recoverable subset alongside
// the error so the caller can surface the loss instead of hiding it.
func NewServiceWithStore(store Store) (*Service, error) {
	s := NewService()
	s.store = store
	if store == nil {
		return s, nil
	}
	plans, idempotency, err := store.Load()
	for _, p := range plans {
		s.plans[p.ID] = p
	}
	for key, id := range idempotency {
		s.idempotency[key] = id
	}
	return s, err
}

// persistLocked writes a plan through to the store. Callers must hold s.mu.
func (s *Service) persistLocked(p Plan) error {
	if s.store == nil {
		return nil
	}
	if err := s.store.SavePlan(p); err != nil {
		return fmt.Errorf("change plan %q was applied in memory but could not be persisted: %w", p.ID, err)
	}
	return nil
}

// persistIdempotencyLocked snapshots the idempotency map to the store.
// Callers must hold s.mu.
func (s *Service) persistIdempotencyLocked() error {
	if s.store == nil {
		return nil
	}
	snapshot := make(map[string]string, len(s.idempotency))
	for key, id := range s.idempotency {
		snapshot[key] = id
	}
	if err := s.store.SaveIdempotency(snapshot); err != nil {
		return fmt.Errorf("change idempotency keys could not be persisted: %w", err)
	}
	return nil
}

// List returns every known plan, newest first.
func (s *Service) List() []Plan {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Plan, 0, len(s.plans))
	for _, p := range s.plans {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
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
	if err := s.persistLocked(p); err != nil {
		return Plan{}, err
	}
	s.plans[p.ID] = p
	if key != "" {
		s.idempotency[key] = p.ID
		if err := s.persistIdempotencyLocked(); err != nil {
			return p, err
		}
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
	if err := s.persistLocked(p); err != nil {
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
	if err := s.persistLocked(p); err != nil {
		return Plan{}, err
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
	if err := s.persistLocked(p); err != nil {
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
	// Persist the executing marker before any privileged SQL runs: if the
	// process dies mid-execution, a restart must show the plan as in-flight
	// rather than silently reverting it to approved-and-runnable.
	if err := s.persistLocked(p); err != nil {
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
	return p, s.persistLocked(p)
}

func (s *Service) fail(id string, cause error) (Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.plans[id]
	if p.State == Executing {
		_ = p.Transition(RollbackRequired, s.now())
	}
	s.plans[id] = p
	if perr := s.persistLocked(p); perr != nil {
		return p, errors.Join(cause, perr)
	}
	return p, cause
}
