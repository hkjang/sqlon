//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"sqlon/internal/dbconn"
)

// fixedStore serves one hand-built profile so the test can set a PlanGateRisk
// threshold low enough that even the tiny test tables trip the gate.
type fixedStore struct{ p dbconn.Profile }

func (s fixedStore) GetProfileByID(_ context.Context, id string) (dbconn.Profile, error) {
	return s.p, nil
}
func (s fixedStore) ListAllProfiles(_ context.Context) ([]dbconn.Profile, error) {
	return []dbconn.Profile{s.p}, nil
}

// loadProfile reads a dataset db_profiles.json entry by id.
func loadProfile(t *testing.T, dataset, id string) dbconn.Profile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "data", dataset, "db_profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var profs []dbconn.Profile
	if err := json.Unmarshal(b, &profs); err != nil {
		t.Fatal(err)
	}
	for _, p := range profs {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("profile %s not found in %s", id, dataset)
	return dbconn.Profile{}
}

func managerWithProfile(t *testing.T, p dbconn.Profile) *dbconn.Manager {
	t.Helper()
	m := dbconn.NewManager(filepath.Join(repoRoot(t), "data", "metadb"))
	m.SetProfileStore(fixedStore{p: dbconn.ApplyDefaults(p)})
	t.Cleanup(m.Close)
	return m
}

func TestPlanGateBlocksThenApproves(t *testing.T) {
	q := "SELECT COUNT(*) FROM public.jamypg_users"
	for _, dialectProfile := range []string{"pg-meta", "mysql-meta", "mariadb-meta"} {
		p := loadProfile(t, "metadb", dialectProfile)
		low := "low"
		on := true
		p.Policy.PlanGate = &on
		p.Policy.PlanGateRisk = low // any analyzable plan trips the gate
		m := managerWithProfile(t, p)

		// 1) without approval → blocked with a PlanGateError carrying the plan
		_, err := m.Execute(ctxT(t), p.ID, q, dbconn.ExecOptions{})
		if err == nil {
			t.Fatalf("%s: expected plan gate to block", dialectProfile)
		}
		var gate *dbconn.PlanGateError
		if !asPlanGate(err, &gate) {
			t.Fatalf("%s: expected *PlanGateError, got %T: %v", dialectProfile, err, err)
		}
		if gate.Plan == nil || gate.Plan.Dialect == "" {
			t.Fatalf("%s: plan gate error missing plan detail: %+v", dialectProfile, gate)
		}

		// 2) with explicit approval → executes
		res, err := m.Execute(ctxT(t), p.ID, q, dbconn.ExecOptions{ApprovePlan: true})
		if err != nil {
			t.Fatalf("%s: approved execution failed: %v", dialectProfile, err)
		}
		if res.RowCount != 1 {
			t.Fatalf("%s: expected 1 row, got %d", dialectProfile, res.RowCount)
		}

		// 3) preview path bypasses the gate (already row-capped)
		if _, err := m.Execute(ctxT(t), p.ID, q, dbconn.ExecOptions{Preview: true}); err != nil {
			t.Fatalf("%s: preview should bypass the gate: %v", dialectProfile, err)
		}
	}
}

func TestPlanGateDisabledExecutes(t *testing.T) {
	p := loadProfile(t, "metadb", "pg-meta")
	off := false
	p.Policy.PlanGate = &off
	m := managerWithProfile(t, p)
	if _, err := m.Execute(ctxT(t), p.ID, "SELECT COUNT(*) FROM public.jamypg_users", dbconn.ExecOptions{}); err != nil {
		t.Fatalf("gate disabled but execution blocked: %v", err)
	}
}

func TestPlanGateDefaultHighAllowsSmallQueries(t *testing.T) {
	// default threshold "high": the tiny seed tables are low-risk, so normal
	// queries run without approval — the gate only bites genuinely heavy plans.
	m := newManager(t)
	for _, id := range profiles {
		if _, err := m.Execute(ctxT(t), id, "SELECT COUNT(*) FROM public.jamypg_users", dbconn.ExecOptions{}); err != nil {
			t.Fatalf("%s: small query should not trip the default-high gate: %v", id, err)
		}
	}
}

func asPlanGate(err error, target **dbconn.PlanGateError) bool {
	for e := err; e != nil; {
		if g, ok := e.(*dbconn.PlanGateError); ok {
			*target = g
			return true
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := e.(unwrapper); ok {
			e = u.Unwrap()
		} else {
			break
		}
	}
	return false
}
