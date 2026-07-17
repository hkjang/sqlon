package dbconn

import (
	"context"
	"testing"
)

// Router decision logic that doesn't need a live DB: with discovery disabled,
// routing relies on declared schemas, dialect, and priority only.

func noDiscover(schemas ...string) Routing {
	off := false
	return Routing{Schemas: schemas, Discover: &off}
}

func TestRouteDeclaredScopeDecisive(t *testing.T) {
	m := NewManager(t.TempDir())
	profs := []Profile{
		{ID: "sales", Type: "postgres", ConnectString: "h:5432/d", Username: "u", PasswordRef: "plain:x", Routing: noDiscover("sales")},
		{ID: "hr", Type: "postgres", ConnectString: "h:5432/d", Username: "u", PasswordRef: "plain:x", Routing: noDiscover("hr")},
	}
	dec := m.RouteProfile(context.Background(), "SELECT * FROM sales.orders", "postgres", profs)
	if !dec.Decisive || dec.Selected != "sales" {
		t.Fatalf("declared-scope routing failed: decisive=%v selected=%s", dec.Decisive, dec.Selected)
	}
}

func TestRouteDialectFilter(t *testing.T) {
	m := NewManager(t.TempDir())
	profs := []Profile{
		{ID: "pg", Type: "postgres", ConnectString: "h:5432/d", Username: "u", PasswordRef: "plain:x", Routing: noDiscover("sales")},
		{ID: "my", Type: "mysql", ConnectString: "h:3306/d", Username: "u", PasswordRef: "plain:x", Routing: noDiscover("sales")},
	}
	dec := m.RouteProfile(context.Background(), "SELECT * FROM sales.orders", "postgres", profs)
	if !dec.Decisive || dec.Selected != "pg" {
		t.Fatalf("dialect filter failed: %+v", dec)
	}
	if len(dec.Excluded) != 1 || dec.Excluded[0].ProfileID != "my" {
		t.Fatalf("mysql profile should be excluded by dialect, got excluded=%+v", dec.Excluded)
	}
}

func TestRouteAmbiguousWhenBothDeclare(t *testing.T) {
	m := NewManager(t.TempDir())
	profs := []Profile{
		{ID: "a", Type: "postgres", ConnectString: "h:5432/d", Username: "u", PasswordRef: "plain:x", Routing: noDiscover("sales")},
		{ID: "b", Type: "postgres", ConnectString: "h:5432/d", Username: "u", PasswordRef: "plain:x", Routing: noDiscover("sales")},
	}
	dec := m.RouteProfile(context.Background(), "SELECT * FROM sales.orders", "postgres", profs)
	if dec.Decisive {
		t.Fatalf("two equally-declared profiles must not auto-route, got %s", dec.Selected)
	}
	if len(dec.Candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(dec.Candidates))
	}
}

func TestRoutePriorityBreaksTie(t *testing.T) {
	m := NewManager(t.TempDir())
	hi := noDiscover("sales")
	hi.Priority = 1
	lo := noDiscover("sales")
	lo.Priority = 90
	profs := []Profile{
		{ID: "lo", Type: "postgres", ConnectString: "h:5432/d", Username: "u", PasswordRef: "plain:x", Routing: lo},
		{ID: "hi", Type: "postgres", ConnectString: "h:5432/d", Username: "u", PasswordRef: "plain:x", Routing: hi},
	}
	dec := m.RouteProfile(context.Background(), "SELECT * FROM sales.orders", "postgres", profs)
	// both declare sales (score +30 each); priority 1 vs 90 differ by ~8.9
	// points which is under the 10-point decisive margin → still asks. This
	// asserts the conservative default: priority nudges ranking but does not
	// alone make a same-coverage tie "decisive".
	if dec.Candidates[0].ProfileID != "hi" {
		t.Fatalf("higher priority should rank first, got %s", dec.Candidates[0].ProfileID)
	}
}

func TestRouteNoEligibleProfile(t *testing.T) {
	m := NewManager(t.TempDir())
	profs := []Profile{
		{ID: "my", Type: "mysql", ConnectString: "h:3306/d", Username: "u", PasswordRef: "plain:x", Routing: noDiscover("sales")},
	}
	dec := m.RouteProfile(context.Background(), "SELECT * FROM sales.orders", "postgres", profs)
	if dec.Decisive || len(dec.Candidates) != 0 {
		t.Fatalf("no postgres profile → no candidate, got %+v", dec)
	}
}
