//go:build integration

package integration

import (
	"path/filepath"
	"testing"

	"sqlon/internal/dbconn"
)

// The three test engines each carry the sakila/northwind/wordpress schemas.
// To exercise capability-based routing we register profiles that DECLARE
// (routing.schemas) and physically serve different schema sets, and assert
// the router picks the profile whose DB actually contains the queried tables.

func routeManager(t *testing.T) *dbconn.Manager {
	t.Helper()
	m := dbconn.NewManager(filepath.Join(repoRoot(t), "data", "metadb"))
	t.Cleanup(m.Close)
	return m
}

// build three postgres profiles all pointing at the same test DB but declaring
// different routing.schemas, so declared-scope routing is deterministic.
func routingProfiles() []dbconn.Profile {
	base := dbconn.Profile{
		Type:          "postgres",
		ConnectString: "127.0.0.1:55432/jamypg_meta",
		Username:      "jamypg_ro",
		PasswordRef:   "plain:jamypg_ro_pw",
	}
	sakila := base
	sakila.ID = "pg-sakila-r"
	sakila.Routing = dbconn.Routing{Schemas: []string{"sakila"}, Priority: 10}
	northwind := base
	northwind.ID = "pg-northwind-r"
	northwind.Routing = dbconn.Routing{Schemas: []string{"northwind"}, Priority: 10}
	wordpress := base
	wordpress.ID = "pg-wordpress-r"
	wordpress.Routing = dbconn.Routing{Schemas: []string{"wordpress"}, Priority: 10}
	return []dbconn.Profile{sakila, northwind, wordpress}
}

func TestRouterPicksByCapability(t *testing.T) {
	m := routeManager(t)
	profs := routingProfiles()

	cases := []struct {
		sql, wantProfile string
	}{
		{"SELECT COUNT(*) FROM sakila.film", "pg-sakila-r"},
		{"SELECT COUNT(*) FROM northwind.products", "pg-northwind-r"},
		{"SELECT COUNT(*) FROM wordpress.wp_posts", "pg-wordpress-r"},
		{"SELECT c.name, COUNT(*) FROM sakila.film_category fc JOIN sakila.category c ON fc.category_id = c.category_id GROUP BY c.name", "pg-sakila-r"},
	}
	for _, c := range cases {
		dec := m.RouteProfile(ctxT(t), c.sql, "postgres", profs)
		if !dec.Decisive {
			t.Fatalf("%q: expected decisive routing, got %s (candidates: %+v)", c.sql, dec.Reason, dec.Candidates)
		}
		if dec.Selected != c.wantProfile {
			t.Fatalf("%q: routed to %s, want %s", c.sql, dec.Selected, c.wantProfile)
		}
	}
}

func TestRouterAmbiguousReturnsCandidates(t *testing.T) {
	m := routeManager(t)
	// two profiles that both physically serve sakila (same DB, both declare it)
	base := dbconn.Profile{
		Type: "postgres", ConnectString: "127.0.0.1:55432/jamypg_meta",
		Username: "jamypg_ro", PasswordRef: "plain:jamypg_ro_pw",
	}
	a := base
	a.ID = "pg-a"
	a.Routing = dbconn.Routing{Schemas: []string{"sakila"}, Priority: 50}
	b := base
	b.ID = "pg-b"
	b.Routing = dbconn.Routing{Schemas: []string{"sakila"}, Priority: 50}

	dec := m.RouteProfile(ctxT(t), "SELECT COUNT(*) FROM sakila.film", "postgres", []dbconn.Profile{a, b})
	if dec.Decisive {
		t.Fatalf("two equal profiles must not auto-route, got %s", dec.Selected)
	}
	if len(dec.Candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(dec.Candidates))
	}
}

func TestRouterPriorityBreaksDeclaredTie(t *testing.T) {
	m := routeManager(t)
	base := dbconn.Profile{
		Type: "postgres", ConnectString: "127.0.0.1:55432/jamypg_meta",
		Username: "jamypg_ro", PasswordRef: "plain:jamypg_ro_pw",
	}
	// one profile does NOT serve wordpress (declares only sakila) → capability
	// rules it out; the other declares+serves wordpress → clear winner.
	only := base
	only.ID = "pg-wp"
	only.Routing = dbconn.Routing{Schemas: []string{"wordpress"}, Priority: 5}
	other := base
	other.ID = "pg-sk"
	other.Routing = dbconn.Routing{Schemas: []string{"sakila"}, Priority: 1}

	dec := m.RouteProfile(ctxT(t), "SELECT COUNT(*) FROM wordpress.wp_comments", "postgres", []dbconn.Profile{other, only})
	if !dec.Decisive || dec.Selected != "pg-wp" {
		t.Fatalf("expected pg-wp, got decisive=%v selected=%s reason=%s", dec.Decisive, dec.Selected, dec.Reason)
	}
}

func TestRouterExcludesWrongDialect(t *testing.T) {
	m := routeManager(t)
	pg := dbconn.Profile{
		ID: "pg-x", Type: "postgres", ConnectString: "127.0.0.1:55432/jamypg_meta",
		Username: "jamypg_ro", PasswordRef: "plain:jamypg_ro_pw",
		Routing: dbconn.Routing{Schemas: []string{"sakila"}},
	}
	my := dbconn.Profile{
		ID: "my-x", Type: "mysql", ConnectString: "127.0.0.1:53306/sakila",
		Username: "jamypg_ro", PasswordRef: "plain:jamypg_ro_pw",
		Routing: dbconn.Routing{Schemas: []string{"sakila"}},
	}
	dec := m.RouteProfile(ctxT(t), "SELECT COUNT(*) FROM sakila.film", "postgres", []dbconn.Profile{pg, my})
	if !dec.Decisive || dec.Selected != "pg-x" {
		t.Fatalf("dialect filter failed: decisive=%v selected=%s", dec.Decisive, dec.Selected)
	}
	if len(dec.Excluded) != 1 || dec.Excluded[0].ProfileID != "my-x" {
		t.Fatalf("expected my-x excluded by dialect, got %+v", dec.Excluded)
	}
}
