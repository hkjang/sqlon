//go:build integration

package integration

import (
	"testing"

	"sqlon/internal/metasync"
)

// Live profiling against the three seeded engines. jamypg_users has an
// `email` column (sensitive by name) and a `role` code-like column, so the
// test can assert both privacy protection and top-value distribution.

func TestProfileStandardAllEngines(t *testing.T) {
	svc, _ := metasyncService(t)
	for _, id := range profiles {
		res, err := svc.Profile(ctxT(t), metasync.ProfileRequest{
			SourceID: id,
			Tables:   []string{"public.jamypg_users"},
			Mode:     metasync.ProfileStandard,
		})
		if err != nil {
			t.Fatalf("%s: profile: %v", id, err)
		}
		if res.ScannedTables != 1 || len(res.Columns) == 0 {
			t.Fatalf("%s: scanned=%d cols=%d", id, res.ScannedTables, len(res.Columns))
		}
		byName := map[string]metasync.ColumnProfile{}
		for _, c := range res.Columns {
			byName[c.Column] = c
		}

		// role: non-sensitive, low cardinality → distinct + min/max + top values
		role, ok := byName["role"]
		if !ok {
			t.Fatalf("%s: role column missing", id)
		}
		if role.Sensitive {
			t.Fatalf("%s: role must not be sensitive", id)
		}
		if role.Distinct != 2 { // admin, user
			t.Fatalf("%s: role distinct = %d, want 2", id, role.Distinct)
		}
		if len(role.TopValues) == 0 {
			t.Fatalf("%s: role should have top values", id)
		}
		// the seed has 8 users + 2 admins → 'user' is the top value with count 8
		if role.TopValues[0].Value != "user" || role.TopValues[0].Count != 8 {
			t.Fatalf("%s: role top value = %+v, want user/8", id, role.TopValues[0])
		}

		// email: sensitive → NO raw min/max, NO top values, but null ratio +
		// distinct + length/pattern are present
		email, ok := byName["email"]
		if !ok {
			t.Fatalf("%s: email column missing", id)
		}
		if !email.Sensitive {
			t.Fatalf("%s: email must be classified sensitive", id)
		}
		if email.Min != "" || email.Max != "" || len(email.TopValues) > 0 {
			t.Fatalf("%s: sensitive email leaked values (min=%q max=%q top=%d)", id, email.Min, email.Max, len(email.TopValues))
		}
		if email.Distinct != 10 { // 10 distinct emails seeded
			t.Fatalf("%s: email distinct = %d, want 10", id, email.Distinct)
		}
		if email.FormatPattern == "" || email.LengthMax == 0 {
			t.Fatalf("%s: email should retain length/pattern shape, got pattern=%q lenmax=%d", id, email.FormatPattern, email.LengthMax)
		}
		t.Logf("%s: role top=%v | email pattern=%q len=%d..%d distinct=%d",
			id, role.TopValues[0], email.FormatPattern, email.LengthMin, email.LengthMax, email.Distinct)
	}
}

func TestProfileFastMode(t *testing.T) {
	svc, _ := metasyncService(t)
	res, err := svc.Profile(ctxT(t), metasync.ProfileRequest{
		SourceID: "pg-meta", Tables: []string{"public.jamypg_users"}, Mode: metasync.ProfileFast,
	})
	if err != nil {
		t.Fatal(err)
	}
	// fast mode: null ratio present, but no distinct/min/max/top values
	for _, c := range res.Columns {
		if c.Distinct != 0 {
			t.Fatalf("fast mode must not compute distinct, got %d for %s", c.Distinct, c.Column)
		}
		if c.Min != "" || c.Max != "" || len(c.TopValues) > 0 {
			t.Fatalf("fast mode must not compute values for %s", c.Column)
		}
		if c.Sampled == 0 {
			t.Fatalf("fast mode should still sample rows for %s", c.Column)
		}
	}
}

func TestProfilePIIOverride(t *testing.T) {
	svc, _ := metasyncService(t)
	// force-classify the non-PII-named `role` column as sensitive
	res, err := svc.Profile(ctxT(t), metasync.ProfileRequest{
		SourceID: "pg-meta", Tables: []string{"public.jamypg_users"},
		Mode: metasync.ProfileStandard, PIIColumns: []string{"*.role"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Columns {
		if c.Column == "role" {
			if !c.Sensitive {
				t.Fatal("role must be sensitive when listed in pii_columns")
			}
			if len(c.TopValues) > 0 || c.Min != "" {
				t.Fatal("sensitive role must not expose values")
			}
		}
	}
}

func TestProfilePersistedAndListed(t *testing.T) {
	svc, _ := metasyncService(t)
	res, err := svc.Profile(ctxT(t), metasync.ProfileRequest{
		SourceID: "pg-meta", Tables: []string{"public.jamypg_users"}, Mode: metasync.ProfileFast,
	})
	if err != nil {
		t.Fatal(err)
	}
	list, err := svc.Profiles("pg-meta")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range list {
		if r.ProfileID == res.ProfileID {
			found = true
		}
	}
	if !found {
		t.Fatalf("profile %s not found in stored list", res.ProfileID)
	}
}
