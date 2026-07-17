package storage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileStoreAppendQueryFilterAndPermissions(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	base := time.Date(2026, 7, 17, 1, 0, 0, 0, time.UTC)
	for i, profile := range []string{"a", "b", "a"} {
		data, _ := json.Marshal(map[string]any{"sequence": i})
		if err := store.Append(context.Background(), Record{Kind: "workload", ProfileID: profile, Engine: "postgres", CollectedAt: base.Add(time.Duration(i) * time.Minute), Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.Query(context.Background(), Query{Kind: "workload", ProfileID: "a", Since: base.Add(30 * time.Second), Limit: 10})
	if err != nil || len(got.Records) != 1 || !strings.Contains(string(got.Records[0].Data), `"sequence":2`) {
		t.Fatalf("query mismatch: %+v err=%v", got, err)
	}
	path := filepath.Join(dir, "operations", "snapshots", "20260717.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("snapshot file is accessible to group/other: %o", info.Mode().Perm())
	}
}

func TestFileStoreReportsCorruptionAndPrunesOldDays(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	old := time.Date(2026, 6, 1, 1, 0, 0, 0, time.UTC)
	data := json.RawMessage(`{"ok":true}`)
	if err := store.Append(context.Background(), Record{Kind: "capacity", ProfileID: "p", CollectedAt: old, Data: data}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "operations", "snapshots", "20260601.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("not-json\n")
	_ = f.Close()
	got, err := store.Query(context.Background(), Query{Limit: 10})
	if err != nil || len(got.Records) != 1 || len(got.Warnings) == 0 {
		t.Fatalf("corruption was silent: %+v err=%v", got, err)
	}
	removed, err := store.Prune(context.Background(), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || removed != 1 {
		t.Fatalf("prune: removed=%d err=%v", removed, err)
	}
}

func TestFileStoreRejectsInvalidRecord(t *testing.T) {
	err := NewFileStore(t.TempDir()).Append(context.Background(), Record{Kind: "workload", ProfileID: "p", CollectedAt: time.Now(), Data: json.RawMessage(`{`)})
	if err == nil {
		t.Fatal("invalid JSON record accepted")
	}
}
