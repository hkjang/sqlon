package change

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStoredService(t *testing.T, dir string) *Service {
	t.Helper()
	s, err := NewServiceWithStore(NewFileStore(dir))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return s
}

func TestFileStorePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s := newStoredService(t, dir)
	p := validPlan()
	if _, err := s.Create(p, "request-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(p.ID); err != nil {
		t.Fatal(err)
	}
	approved, err := s.Approve(p.ID, "dba1")
	if err != nil {
		t.Fatal(err)
	}

	restarted := newStoredService(t, dir)
	got, ok := restarted.Get(p.ID)
	if !ok {
		t.Fatalf("plan %q lost across restart", p.ID)
	}
	if got.State != Approved || len(got.Approvals) != 1 || got.Approvals[0].ID != approved.Approvals[0].ID {
		t.Fatalf("restored plan diverged: state=%s approvals=%+v", got.State, got.Approvals)
	}
	// Idempotency keys must survive too: the same key returns the stored plan
	// instead of a duplicate-id error.
	same, err := restarted.Create(p, "request-1")
	if err != nil || same.ID != p.ID {
		t.Fatalf("idempotency key lost across restart: plan=%+v err=%v", same, err)
	}
}

func TestFileStorePersistsExecutionOutcome(t *testing.T) {
	dir := t.TempDir()
	s := newStoredService(t, dir)
	p := validPlan()
	if _, err := s.Create(p, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(p.ID, "dba1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(context.Background(), p.ID, testRunner{fail: true}); err == nil {
		t.Fatal("failing runner reported success")
	}

	restarted := newStoredService(t, dir)
	got, ok := restarted.Get(p.ID)
	if !ok || got.State != RollbackRequired {
		t.Fatalf("execution failure not durable: ok=%v state=%s", ok, got.State)
	}
}

func TestFileStorePlanIDCannotEscapeDir(t *testing.T) {
	dir := t.TempDir()
	s := newStoredService(t, dir)
	p := validPlan()
	p.ID = "../../escape/../../outside"
	if _, err := s.Create(p, ""); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one plan file inside the store dir, got %d", len(entries))
	}
	if strings.Contains(entries[0].Name(), "..") || strings.ContainsAny(entries[0].Name(), `/\`) {
		t.Fatalf("unsafe plan filename %q", entries[0].Name())
	}
	restarted := newStoredService(t, dir)
	if _, ok := restarted.Get(p.ID); !ok {
		t.Fatal("sanitized plan not restored")
	}
}

func TestFileStoreLoadReportsCorruptFilesButKeepsRest(t *testing.T) {
	dir := t.TempDir()
	s := newStoredService(t, dir)
	p := validPlan()
	if _, err := s.Create(p, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "corrupt-deadbeef.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewServiceWithStore(NewFileStore(dir))
	if err == nil {
		t.Fatal("corrupt plan file was silently ignored")
	}
	if _, ok := restarted.Get(p.ID); !ok {
		t.Fatal("valid plan discarded because a sibling file was corrupt")
	}
}

type failingStore struct{ fail bool }

func (f *failingStore) SavePlan(Plan) error {
	if f.fail {
		return errors.New("disk full")
	}
	return nil
}
func (f *failingStore) SaveIdempotency(map[string]string) error { return nil }
func (f *failingStore) Load() ([]Plan, map[string]string, error) {
	return nil, map[string]string{}, nil
}

func TestPersistFailureLeavesPureTransitionsRetryable(t *testing.T) {
	store := &failingStore{fail: true}
	s, err := NewServiceWithStore(store)
	if err != nil {
		t.Fatal(err)
	}
	p := validPlan()
	if _, err := s.Create(p, ""); err == nil {
		t.Fatal("create succeeded despite persist failure")
	}
	if _, ok := s.Get(p.ID); ok {
		t.Fatal("plan committed to memory despite persist failure")
	}
	store.fail = false
	if _, err := s.Create(p, ""); err != nil {
		t.Fatalf("retry after persist recovery failed: %v", err)
	}
}
