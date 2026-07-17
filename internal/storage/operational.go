// Package storage contains SQLON operational snapshot persistence. The local
// implementation is append-only JSONL so standalone deployments need no
// external database; multi-user deployments can replace the Store contract.
package storage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Record struct {
	Kind        string          `json:"kind"`
	ProfileID   string          `json:"profile_id"`
	Engine      string          `json:"engine"`
	CollectedAt time.Time       `json:"collected_at"`
	Data        json.RawMessage `json:"data"`
}

type Query struct {
	Kind, ProfileID string
	Since           time.Time
	Limit           int
}

type QueryResult struct {
	Records  []Record `json:"records"`
	Warnings []string `json:"warnings"`
}

type OperationalStore interface {
	Append(context.Context, Record) error
	Query(context.Context, Query) (QueryResult, error)
	Prune(context.Context, time.Time) (int, error)
}

type FileStore struct {
	dir string
	mu  sync.Mutex
}

func NewFileStore(dataDir string) *FileStore {
	return &FileStore{dir: filepath.Join(dataDir, "operations", "snapshots")}
}

func (s *FileStore) Append(ctx context.Context, record Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(record.Kind) == "" || strings.TrimSpace(record.ProfileID) == "" || record.CollectedAt.IsZero() || len(record.Data) == 0 || !json.Valid(record.Data) {
		return errors.New("operational record requires kind, profile_id, collected_at, and valid JSON data")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create operational store: %w", err)
	}
	path := filepath.Join(s.dir, record.CollectedAt.UTC().Format("20060102")+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open operational snapshot: %w", err)
	}
	enc := json.NewEncoder(f)
	err = enc.Encode(record)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("append operational snapshot: %w", err)
	}
	return nil
}

func (s *FileStore) Query(ctx context.Context, query Query) (QueryResult, error) {
	if query.Limit <= 0 || query.Limit > 10_000 {
		query.Limit = 1_000
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return QueryResult{Records: []Record{}, Warnings: []string{}}, nil
	}
	if err != nil {
		return QueryResult{}, fmt.Errorf("read operational store: %w", err)
	}
	result := QueryResult{Records: []Record{}, Warnings: []string{}}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return QueryResult{}, err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(s.dir, entry.Name())
		f, openErr := os.Open(path)
		if openErr != nil {
			result.Warnings = append(result.Warnings, "스냅숏 파일을 열지 못했습니다: "+entry.Name())
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64<<10), 4<<20)
		line := 0
		for scanner.Scan() {
			line++
			var record Record
			if json.Unmarshal(scanner.Bytes(), &record) != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("손상된 스냅숏 레코드: %s:%d", entry.Name(), line))
				continue
			}
			if query.Kind != "" && record.Kind != query.Kind || query.ProfileID != "" && record.ProfileID != query.ProfileID || !query.Since.IsZero() && record.CollectedAt.Before(query.Since) {
				continue
			}
			result.Records = append(result.Records, record)
		}
		if scanErr := scanner.Err(); scanErr != nil {
			result.Warnings = append(result.Warnings, "스냅숏 파일 읽기가 불완전합니다: "+entry.Name())
		}
		_ = f.Close()
	}
	sort.Slice(result.Records, func(i, j int) bool { return result.Records[i].CollectedAt.After(result.Records[j].CollectedAt) })
	if len(result.Records) > query.Limit {
		result.Records = result.Records[:query.Limit]
		result.Warnings = append(result.Warnings, "조회 결과가 요청 상한에 도달했습니다.")
	}
	return result, nil
}

func (s *FileStore) Prune(ctx context.Context, before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		day, parseErr := time.Parse("20060102", strings.TrimSuffix(entry.Name(), ".jsonl"))
		if parseErr != nil || !day.Before(before.UTC().Truncate(24*time.Hour)) {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, entry.Name())); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
