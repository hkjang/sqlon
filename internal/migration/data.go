// Package migration performs the one-release JAMYPG-to-SQLON data layout
// migration. It never overwrites a SQLON file and creates a complete backup
// before changing the target directory.
package migration

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const ManifestFile = ".sqlon-migration.json"

type Result struct {
	Migrated  bool   `json:"migrated"`
	Source    string `json:"source"`
	Target    string `json:"target"`
	BackupDir string `json:"backup_dir,omitempty"`
	Files     int    `json:"files"`
}

type manifest struct {
	Version    int       `json:"version"`
	Source     string    `json:"source"`
	MigratedAt time.Time `json:"migrated_at"`
	Files      int       `json:"files"`
}

// Prepare migrates legacyDir into targetDir. Existing SQLON files win; legacy
// files are copied only when the relative path is absent from the target.
func Prepare(targetDir, legacyDir string, now time.Time) (Result, error) {
	targetDir = filepath.Clean(targetDir)
	legacyDir = filepath.Clean(legacyDir)
	result := Result{Source: legacyDir, Target: targetDir}
	if samePath(targetDir, legacyDir) {
		return result, nil
	}
	legacyInfo, err := os.Stat(legacyDir)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("inspect legacy data: %w", err)
	}
	if !legacyInfo.IsDir() {
		return result, fmt.Errorf("legacy data path is not a directory: %s", legacyDir)
	}
	needed, err := migrationNeeded(targetDir, legacyDir)
	if err != nil || !needed {
		return result, err
	}

	parent := filepath.Dir(targetDir)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return result, err
	}
	stamp := now.UTC().Format("20060102T150405.000000000Z")
	backup := filepath.Join(parent, "backups", "sqlon-pre-migration-"+stamp)
	if err := os.MkdirAll(backup, 0o700); err != nil {
		return result, fmt.Errorf("create migration backup: %w", err)
	}
	if _, err := copyTree(legacyDir, filepath.Join(backup, "legacy"), false); err != nil {
		return result, fmt.Errorf("backup legacy data: %w", err)
	}
	if info, err := os.Stat(targetDir); err == nil && info.IsDir() {
		if _, err := copyTree(targetDir, filepath.Join(backup, "sqlon"), false); err != nil {
			return result, fmt.Errorf("backup SQLON data: %w", err)
		}
	}

	tmp, err := os.MkdirTemp(parent, ".sqlon-migration-")
	if err != nil {
		return result, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(tmp)
		}
	}()
	if info, err := os.Stat(targetDir); err == nil && info.IsDir() {
		if _, err := copyTree(targetDir, tmp, false); err != nil {
			return result, err
		}
	}
	files, err := copyTree(legacyDir, tmp, true)
	if err != nil {
		return result, fmt.Errorf("stage legacy data: %w", err)
	}
	b, _ := json.MarshalIndent(manifest{Version: 1, Source: legacyDir, MigratedAt: now.UTC(), Files: files}, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, ManifestFile), append(b, '\n'), 0o600); err != nil {
		return result, err
	}

	if _, err := os.Stat(targetDir); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(tmp, targetDir); err != nil {
			return result, fmt.Errorf("activate SQLON data: %w", err)
		}
	} else {
		rollback := targetDir + ".pre-migration-" + stamp
		if err := os.Rename(targetDir, rollback); err != nil {
			return result, fmt.Errorf("stage current SQLON data: %w", err)
		}
		if err := os.Rename(tmp, targetDir); err != nil {
			_ = os.Rename(rollback, targetDir)
			return result, fmt.Errorf("activate SQLON data: %w", err)
		}
		_ = os.RemoveAll(rollback) // complete backups above remain recoverable
	}
	committed = true
	result.Migrated, result.BackupDir, result.Files = true, backup, files
	return result, nil
}

func migrationNeeded(target, legacy string) (bool, error) {
	if info, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		return true, nil
	} else if err != nil {
		return false, err
	} else if !info.IsDir() {
		return false, fmt.Errorf("SQLON data path is not a directory: %s", target)
	}
	needed := false
	err := filepath.WalkDir(legacy, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(legacy, path)
		if err != nil {
			return err
		}
		if rel == ManifestFile {
			return nil
		}
		if _, err := os.Lstat(filepath.Join(target, rel)); errors.Is(err, os.ErrNotExist) {
			needed = true
			return fs.SkipAll
		} else if err != nil {
			return err
		}
		return nil
	})
	return needed, err
}

func copyTree(src, dst string, missingOnly bool) (int, error) {
	files := 0
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o750)
		}
		to := filepath.Join(dst, rel)
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symbolic links are not migrated: %s", rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(to, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported data file type: %s", rel)
		}
		if missingOnly {
			if _, err := os.Lstat(to); err == nil {
				return nil
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil {
			return err
		}
		if err := copyFile(path, to, info.Mode().Perm()); err != nil {
			return err
		}
		files++
		return nil
	})
	return files, err
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func samePath(a, b string) bool {
	aa, _ := filepath.Abs(a)
	bb, _ := filepath.Abs(b)
	return strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb))
}
