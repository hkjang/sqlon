// Package change implements the approval-gated change-control domain.
package change

import (
	"errors"
	"fmt"
	"time"
)

type State string

const (
	Draft            State = "draft"
	Analyzing        State = "analyzing"
	ReviewRequired   State = "review_required"
	Approved         State = "approved"
	Scheduled        State = "scheduled"
	Executing        State = "executing"
	Verifying        State = "verifying"
	Completed        State = "completed"
	Failed           State = "failed"
	RollbackRequired State = "rollback_required"
	RollingBack      State = "rolling_back"
	RolledBack       State = "rolled_back"
	Cancelled        State = "cancelled"
)

type Risk string

const (
	Low       Risk = "low"
	Medium    Risk = "medium"
	High      Risk = "high"
	Critical  Risk = "critical"
	Emergency Risk = "emergency"
)

// Plan is structured before it reaches a write-capable executor. SQL fields
// remain evidence only; execution must use the approved plan ID and steps.
type Plan struct {
	ID                string   `json:"id"`
	ProfileID         string   `json:"profile_id"`
	Target            string   `json:"target"`
	State             State    `json:"state"`
	Risk              Risk     `json:"risk"`
	Reason            string   `json:"reason"`
	PreState          any      `json:"pre_state,omitempty"`
	Impact            any      `json:"impact,omitempty"`
	ExpectedLock      string   `json:"expected_lock,omitempty"`
	EstimatedDuration string   `json:"estimated_duration,omitempty"`
	Preconditions     []string `json:"preconditions,omitempty"`
	MaintenanceWindow string   `json:"maintenance_window,omitempty"`
	// MaintenanceWindows, when set, are enforced at execution time: a
	// non-emergency plan cannot execute outside them. MaintenanceWindow above
	// remains a free-text human note.
	MaintenanceWindows []Window   `json:"maintenance_windows,omitempty"`
	Steps              []Step     `json:"steps"`
	RequiredApprovals  int        `json:"required_approvals"`
	Approvals          []Approval `json:"approvals,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}
type Step struct {
	Order        int    `json:"order"`
	Command      string `json:"command"`
	Verification string `json:"verification"`
	Compensation string `json:"compensation"`
}
type Approval struct {
	ID       string    `json:"id"`
	Actor    string    `json:"actor"`
	At       time.Time `json:"at"`
	Decision string    `json:"decision"`
}

func (p *Plan) Validate() error {
	if p.ID == "" || p.ProfileID == "" || p.Target == "" || p.Reason == "" {
		return errors.New("id, profile_id, target, and reason are required")
	}
	if _, err := ApprovalRequirement(p.Risk); err != nil {
		return err
	}
	if len(p.Steps) == 0 {
		return errors.New("at least one change step is required")
	}
	for i, step := range p.Steps {
		if step.Order != i+1 || step.Command == "" || step.Verification == "" || step.Compensation == "" {
			return fmt.Errorf("step %d must be ordered and include command, verification, and compensation", i+1)
		}
	}
	if p.RequiredApprovals < 0 {
		return errors.New("required_approvals cannot be negative")
	}
	for i, w := range p.MaintenanceWindows {
		if err := w.Validate(); err != nil {
			return fmt.Errorf("maintenance window %d: %w", i+1, err)
		}
	}
	return nil
}

// ApprovalRequirement is server policy. A caller cannot lower it by putting a
// smaller required_approvals value in JSON.
func ApprovalRequirement(r Risk) (int, error) {
	switch r {
	case Low:
		return 0, nil
	case Medium, High:
		return 1, nil
	case Critical:
		return 2, nil
	case Emergency:
		return 1, nil
	default:
		return 0, fmt.Errorf("unsupported change risk %q", r)
	}
}

func (p *Plan) CanTransition(to State) bool {
	allowed := map[State][]State{Draft: {Analyzing, Cancelled}, Analyzing: {ReviewRequired, Approved, Cancelled}, ReviewRequired: {Approved, Cancelled}, Approved: {Scheduled, Executing, Cancelled}, Scheduled: {Executing, Cancelled}, Executing: {Verifying, Failed, RollbackRequired}, Verifying: {Completed, RollbackRequired, Failed}, Failed: {RollbackRequired}, RollbackRequired: {RollingBack}, RollingBack: {RolledBack, Failed}}
	for _, next := range allowed[p.State] {
		if next == to {
			return true
		}
	}
	return false
}

func (p *Plan) Transition(to State, now time.Time) error {
	if !p.CanTransition(to) {
		return fmt.Errorf("invalid change state transition: %s -> %s", p.State, to)
	}
	if to == Executing {
		required, err := ApprovalRequirement(p.Risk)
		if err != nil {
			return err
		}
		if len(p.Approvals) < required {
			return errors.New("required approvals are missing")
		}
	}
	p.State, p.UpdatedAt = to, now
	return nil
}
