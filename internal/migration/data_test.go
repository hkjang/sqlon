package migration

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func write(t *testing.T, root, rel, value string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), mode); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareBacksUpAndMigratesProfilesCatalogAndAudit(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "metadb")
	target := filepath.Join(root, "sqlon")
	write(t, legacy, "db_profiles.json", "profiles", 0o600)
	write(t, legacy, "meta_physical_models.json", "models", 0o640)
	write(t, legacy, "audit/audit-20260717.jsonl", "legacy-audit\n", 0o600)

	now := time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC)
	result, err := Prepare(target, legacy, now)
	if err != nil || !result.Migrated {
		t.Fatalf("prepare failed: %+v err=%v", result, err)
	}
	for _, rel := range []string{"db_profiles.json", "meta_physical_models.json", "audit/audit-20260717.jsonl", ManifestFile} {
		if _, err := os.Stat(filepath.Join(target, rel)); err != nil {
			t.Fatalf("target %s: %v", rel, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(result.BackupDir, "legacy", "audit/audit-20260717.jsonl")); err != nil || string(b) != "legacy-audit\n" {
		t.Fatalf("audit backup=%q err=%v", b, err)
	}
	if info, err := os.Stat(filepath.Join(target, "db_profiles.json")); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("profile permissions=%v err=%v", info.Mode().Perm(), err)
	}
	again, err := Prepare(target, legacy, now.Add(time.Second))
	if err != nil || again.Migrated {
		t.Fatalf("idempotent run=%+v err=%v", again, err)
	}
}

func TestPreparePreservesExistingSQLONFilesAndFillsMissingLegacyFiles(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "metadb")
	target := filepath.Join(root, "sqlon")
	write(t, legacy, "glossary.json", "legacy", 0o640)
	write(t, legacy, "db_profiles.json", "profiles", 0o600)
	write(t, target, "glossary.json", "sqlon", 0o640)
	write(t, target, "audit/current.jsonl", "current", 0o600)
	result, err := Prepare(target, legacy, time.Date(2026, 7, 17, 2, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "glossary.json")); string(got) != "sqlon" {
		t.Fatalf("existing file overwritten: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "db_profiles.json")); string(got) != "profiles" {
		t.Fatalf("missing profile not migrated: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(result.BackupDir, "sqlon", "audit/current.jsonl")); string(got) != "current" {
		t.Fatalf("target audit not backed up: %q", got)
	}
}

func TestPrepareRejectsSymlinksBeforeActivatingTarget(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "metadb")
	target := filepath.Join(root, "sqlon")
	write(t, legacy, "safe.json", "{}", 0o600)
	if err := os.Symlink(filepath.Join(legacy, "safe.json"), filepath.Join(legacy, "link.json")); err != nil {
		t.Skip(err)
	}
	if _, err := Prepare(target, legacy, time.Now()); err == nil {
		t.Fatal("symlink migration accepted")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target changed after failed migration: %v", err)
	}
}
