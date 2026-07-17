package mcp

import (
	"context"
	"fmt"

	"sqlon/internal/change"
)

// approvedChangeRunner is deliberately not exported: the only write bridge is
// the API handler that has already authenticated an approver and obtained an
// approved ChangePlan ID. Step commands never arrive as an execute request.
type approvedChangeRunner struct{ server *Server }

func (r approvedChangeRunner) Revalidate(ctx context.Context, p change.Plan) error {
	if ok, err := r.server.DB.AdminAvailable(ctx, p.ProfileID); err != nil || !ok {
		if err != nil {
			return err
		}
		return fmt.Errorf("profile %q has no approved DBA executor credentials", p.ProfileID)
	}
	ping := r.server.DB.Ping(ctx, p.ProfileID)
	if !ping.OK {
		return fmt.Errorf("pre-execution connection check failed: %s", ping.Error)
	}
	return nil
}
func (r approvedChangeRunner) Execute(ctx context.Context, p change.Plan, step change.Step) error {
	if _, err := r.server.DB.AdminExec(ctx, p.ProfileID, step.Command, 60); err != nil {
		return err
	}
	// Verification is part of the immutable approved plan and is issued through
	// the read-only pool. It cannot reuse the privileged executor.
	if _, err := r.server.DB.SystemQuery(ctx, p.ProfileID, step.Verification); err != nil {
		return fmt.Errorf("step %d verification failed: %w", step.Order, err)
	}
	return nil
}
func (r approvedChangeRunner) Verify(context.Context, change.Plan) error { return nil }
