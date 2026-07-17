package change

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Store persists change plans so approvals and execution history survive a
// process restart. Approval and transition policy stay in the Service; a
// store only provides durability.
type Store interface {
	SavePlan(Plan) error
	SaveIdempotency(map[string]string) error
	// Load returns every recoverable plan and the idempotency map. A non-nil
	// error reports entries that could not be decoded; the returned slices are
	// still valid so a caller can start with the recoverable subset.
	Load() ([]Plan, map[string]string, error)
}

const idempotencyFile = "_idempotency.json"

// FileStore keeps one JSON document per plan under dir plus an idempotency
// map, written atomically (temp file + rename). Plans are never deleted:
// cancelled and rolled-back plans remain as the durable change record.
type FileStore struct{ dir string }

func NewFileStore(dir string) *FileStore { return &FileStore{dir: dir} }

// planPath derives a filesystem-safe name from a client-supplied plan id.
// The id is sanitized (so it cannot traverse out of dir) and suffixed with a
// hash of the original id (so distinct ids never collide after sanitizing).
func (f *FileStore) planPath(id string) string {
	sum := sha256.Sum256([]byte(id))
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, id)
	if len(safe) > 64 {
		safe = safe[:64]
	}
	return filepath.Join(f.dir, fmt.Sprintf("%s-%s.json", safe, hex.EncodeToString(sum[:4])))
}

func (f *FileStore) SavePlan(p Plan) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(f.planPath(p.ID), data)
}

func (f *FileStore) SaveIdempotency(keys map[string]string) error {
	data, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(f.dir, idempotencyFile), data)
}

func (f *FileStore) Load() ([]Plan, map[string]string, error) {
	entries, err := os.ReadDir(f.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, map[string]string{}, nil
	}
	if err != nil {
		return nil, map[string]string{}, err
	}
	var plans []Plan
	var loadErrs []error
	idempotency := map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(f.dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			loadErrs = append(loadErrs, fmt.Errorf("read %s: %w", entry.Name(), err))
			continue
		}
		if entry.Name() == idempotencyFile {
			if err := json.Unmarshal(data, &idempotency); err != nil {
				loadErrs = append(loadErrs, fmt.Errorf("decode %s: %w", entry.Name(), err))
			}
			continue
		}
		var p Plan
		if err := json.Unmarshal(data, &p); err != nil || p.ID == "" {
			if err == nil {
				err = errors.New("missing plan id")
			}
			loadErrs = append(loadErrs, fmt.Errorf("decode %s: %w", entry.Name(), err))
			continue
		}
		plans = append(plans, p)
	}
	return plans, idempotency, errors.Join(loadErrs...)
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
