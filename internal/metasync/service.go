package metasync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Service orchestrates collection → snapshot storage → change detection
// (FR-META-003/004/005) against a pluggable time source and file-backed
// snapshot store. Snapshots live under <dataDir>/metasync/snapshots/ as JSON,
// mirroring how audit/feedback/backups are already stored, so the feature
// works in standalone mode with no meta DB.
type Service struct {
	col     *Collector
	dir     string
	nowFn   func() time.Time
	newIDFn func() string
}

// NewService builds a Service. dataDir is the dataset directory.
func NewService(q SystemQuerier, dataDir string) *Service {
	return &Service{
		col:     NewCollector(q),
		dir:     filepath.Join(dataDir, "metasync", "snapshots"),
		nowFn:   time.Now,
		newIDFn: newSnapshotID,
	}
}

// SyncResult is returned by Sync.
type SyncResult struct {
	Snapshot   *RawSnapshot `json:"snapshot"`
	ChangeSet  *ChangeSet   `json:"change_set,omitempty"`
	BaselineID string       `json:"baseline_snapshot_id,omitempty"`
	Skipped    bool         `json:"skipped"` // structure unchanged since baseline
	Note       string       `json:"note,omitempty"`
}

// Sync collects the current physical model, and — unless incremental=false —
// short-circuits when the schema hash matches the latest stored snapshot
// (FR-META-005 adaptive/incremental). Otherwise it stores the new snapshot
// and returns the change set versus the previous one.
func (s *Service) Sync(ctx context.Context, req CollectRequest, incremental bool) (*SyncResult, error) {
	snap, err := s.col.Collect(ctx, req)
	if err != nil {
		return nil, err
	}
	snap.CollectedAt = s.nowFn()
	snap.SnapshotID = s.newIDFn()

	baseline, _ := s.latest(req.SourceID)
	res := &SyncResult{Snapshot: snap}
	if baseline != nil {
		res.BaselineID = baseline.SnapshotID
	}

	if incremental && baseline != nil && baseline.SchemaHash == snap.SchemaHash {
		// nothing structural changed — do not persist a redundant snapshot
		res.Skipped = true
		res.Note = "schema hash unchanged since baseline; no structural change (incremental skip)"
		res.ChangeSet = &ChangeSet{SourceID: req.SourceID, FromSnapshotID: baseline.SnapshotID,
			ToSnapshotID: baseline.SnapshotID, ComputedAt: snap.CollectedAt, Summary: map[ChangeKind]int{}}
		return res, nil
	}

	if err := s.store(snap); err != nil {
		return nil, err
	}
	res.ChangeSet = Diff(baseline, snap)
	return res, nil
}

// Snapshots lists stored snapshot metadata for a source, newest first.
func (s *Service) Snapshots(sourceID string) ([]RawSnapshot, error) {
	entries, err := os.ReadDir(s.sourceDir(sourceID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []RawSnapshot
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		snap, err := s.load(sourceID, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		// list view: drop the heavy table body
		snap.Tables = nil
		out = append(out, *snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CollectedAt.After(out[j].CollectedAt) })
	return out, nil
}

// DiffSnapshots computes the change set between two stored snapshot ids.
func (s *Service) DiffSnapshots(sourceID, fromID, toID string) (*ChangeSet, error) {
	from, err := s.load(sourceID, fromID)
	if err != nil {
		return nil, fmt.Errorf("baseline snapshot %s: %w", fromID, err)
	}
	to, err := s.load(sourceID, toID)
	if err != nil {
		return nil, fmt.Errorf("target snapshot %s: %w", toID, err)
	}
	return Diff(from, to), nil
}

// DiscoverSchemas proxies to the collector.
func (s *Service) DiscoverSchemas(ctx context.Context, sourceID string) ([]DatabaseAsset, error) {
	return s.col.DiscoverSchemas(ctx, sourceID)
}

// GetSnapshot loads a full stored snapshot.
func (s *Service) GetSnapshot(sourceID, id string) (*RawSnapshot, error) {
	return s.load(sourceID, id)
}

// LatestSnapshot returns the most recent full stored snapshot for a source, or
// nil if none exist.
func (s *Service) LatestSnapshot(sourceID string) (*RawSnapshot, error) {
	return s.latest(sourceID)
}

// Collect performs a one-shot physical collection WITHOUT storing a snapshot —
// for live schema inspection (describe_db_schema) rather than change tracking.
func (s *Service) Collect(ctx context.Context, req CollectRequest) (*RawSnapshot, error) {
	return s.col.Collect(ctx, req)
}

// ---- storage ----

func (s *Service) sourceDir(sourceID string) string {
	return filepath.Join(s.dir, sanitizeID(sourceID))
}

func (s *Service) store(snap *RawSnapshot) error {
	dir := s.sourceDir(snap.SourceID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(snap, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, sanitizeID(snap.SnapshotID)+".json"), append(b, '\n'), 0o644)
}

func (s *Service) load(sourceID, id string) (*RawSnapshot, error) {
	b, err := os.ReadFile(filepath.Join(s.sourceDir(sourceID), sanitizeID(id)+".json"))
	if err != nil {
		return nil, err
	}
	var snap RawSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

func (s *Service) latest(sourceID string) (*RawSnapshot, error) {
	list, err := s.Snapshots(sourceID)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return s.load(sourceID, list[0].SnapshotID)
}

func newSnapshotID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "snap-" + time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:4])
}

func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
