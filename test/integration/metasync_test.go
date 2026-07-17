//go:build integration

package integration

import (
	"path/filepath"
	"testing"

	"sqlon/internal/dbconn"
	"sqlon/internal/metasync"
)

// Live metadata collection against the three seeded engines. The test env
// carries the sakila/northwind/wordpress schemas plus the jamypg_* meta
// tables, so the collector should return a non-trivial physical model with
// PK/FK constraints and be able to detect changes across snapshots.

func metasyncService(t *testing.T) (*metasync.Service, *dbconn.Manager) {
	t.Helper()
	m := dbconn.NewManager(filepath.Join(repoRoot(t), "data", "metadb"))
	t.Cleanup(m.Close)
	// snapshots go to a temp dir so the test never pollutes the repo
	svc := metasync.NewService(m, t.TempDir())
	return svc, m
}

func TestMetadataCollectAllEngines(t *testing.T) {
	svc, _ := metasyncService(t)
	for _, id := range profiles { // pg-meta, mysql-meta, mariadb-meta
		res, err := svc.Sync(ctxT(t), metasync.CollectRequest{
			SourceID: id,
		}, false)
		if err != nil {
			t.Fatalf("%s: sync: %v", id, err)
		}
		snap := res.Snapshot
		if snap.ObjectCount.Tables == 0 {
			t.Fatalf("%s: expected tables, got %+v", id, snap.ObjectCount)
		}
		// the jamypg_users table must be present with a primary key
		var users *metasync.TableAsset
		for i := range snap.Tables {
			if snap.Tables[i].Name == "jamypg_users" {
				users = &snap.Tables[i]
			}
		}
		if users == nil {
			t.Fatalf("%s: jamypg_users not collected", id)
		}
		if len(users.Columns) < 5 {
			t.Fatalf("%s: jamypg_users columns: %d", id, len(users.Columns))
		}
		hasPK := false
		for _, c := range users.Columns {
			if c.IsPrimaryKey {
				hasPK = true
			}
		}
		if !hasPK {
			t.Fatalf("%s: jamypg_users has no PK column flagged (constraints: %+v)", id, users.Constraints)
		}
		if snap.SchemaHash == "" {
			t.Fatalf("%s: empty schema hash", id)
		}
		t.Logf("%s: %d tables, %d columns, %d constraints, %d indexes",
			id, snap.ObjectCount.Tables, snap.ObjectCount.Columns, snap.ObjectCount.Constraints, snap.ObjectCount.Indexes)
	}
}

func TestMetadataIncrementalSkip(t *testing.T) {
	svc, _ := metasyncService(t)
	id := "pg-meta"
	// first sync stores a snapshot
	first, err := svc.Sync(ctxT(t), metasync.CollectRequest{SourceID: id}, true)
	if err != nil {
		t.Fatal(err)
	}
	if first.Skipped {
		t.Fatal("first sync must not be skipped")
	}
	// second incremental sync with no schema change must skip (FR-META-005)
	second, err := svc.Sync(ctxT(t), metasync.CollectRequest{SourceID: id}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Skipped {
		t.Fatalf("unchanged schema must be skipped, got change_count=%d", len(second.ChangeSet.Changes))
	}
	// snapshots list should hold exactly one stored snapshot
	list, err := svc.Snapshots(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 stored snapshot after incremental skip, got %d", len(list))
	}
}

func TestMetadataDiscoverSchemas(t *testing.T) {
	svc, _ := metasyncService(t)
	schemas, err := svc.DiscoverSchemas(ctxT(t), "pg-meta")
	if err != nil {
		t.Fatal(err)
	}
	// public + sakila + northwind + wordpress at minimum
	names := map[string]bool{}
	for _, s := range schemas {
		names[s.Schema] = true
	}
	for _, want := range []string{"public", "sakila", "northwind", "wordpress"} {
		if !names[want] {
			t.Fatalf("expected schema %q in discovery, got %v", want, names)
		}
	}
}

func TestMetadataScopedCollectionAndDiff(t *testing.T) {
	svc, _ := metasyncService(t)
	id := "pg-meta"
	// collect only the sakila schema, non-incremental so both snapshots persist
	a, err := svc.Sync(ctxT(t), metasync.CollectRequest{SourceID: id, Schemas: []string{"sakila"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if a.Snapshot.ObjectCount.Tables < 8 {
		t.Fatalf("sakila should have ~9 tables, got %d", a.Snapshot.ObjectCount.Tables)
	}
	for _, tbl := range a.Snapshot.Tables {
		if tbl.Schema != "sakila" {
			t.Fatalf("scoped collection leaked schema %q", tbl.Schema)
		}
	}
	// re-collect same scope; diff of identical snapshots must be empty
	b, err := svc.Sync(ctxT(t), metasync.CollectRequest{SourceID: id, Schemas: []string{"sakila"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := svc.DiffSnapshots(id, a.Snapshot.SnapshotID, b.Snapshot.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Changes) != 0 {
		t.Fatalf("identical re-collection must diff to zero changes, got %+v", cs.Changes)
	}
}
