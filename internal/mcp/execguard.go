package mcp

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sqlon/internal/dbconn"
)

// ---- PII masking (defense-in-depth behind validate_sql's PII block) ----

const piiMask = "***MASKED***"

// maskPIIResult replaces values of result columns whose name is flagged PII in
// the catalog. Returns the masked column names (empty → nothing touched).
// Aliased SELECT expressions can dodge a name match — that path is already
// rejected by validate_sql; this guard catches direct column output that slips
// through (e.g. SELECT *, hand-written SQL over the REST console).
func (s *Server) maskPIIResult(res *dbconn.QueryResult) []string {
	if res == nil || len(res.Columns) == 0 {
		return nil
	}
	pii := s.cat().PIIColumnNames()
	if len(pii) == 0 {
		return nil
	}
	masked := []string{}
	for _, col := range res.Columns {
		if !pii[strings.ToUpper(col.Name)] {
			continue
		}
		masked = append(masked, col.Name)
		for _, row := range res.Rows {
			if _, ok := row[col.Name]; ok {
				row[col.Name] = piiMask
			}
		}
	}
	return masked
}

// ---- result cache (TTL, size-bounded) ----

const (
	defaultCacheTTLSeconds = 60
	cacheMaxEntries        = 100
	cacheMaxRows           = 5000 // don't hold giant results
)

type cacheEntry struct {
	result  *dbconn.QueryResult
	masked  []string
	expires time.Time
}

type resultCache struct {
	mu         sync.Mutex
	entries    map[string]cacheEntry
	ttlSeconds atomic.Int64 // 0 → caching disabled
}

func newResultCache() *resultCache {
	rc := &resultCache{entries: map[string]cacheEntry{}}
	rc.ttlSeconds.Store(defaultCacheTTLSeconds)
	return rc
}

// SetTTL adjusts the cache lifetime live (from /admin/settings). 0 disables
// caching and flushes existing entries.
func (rc *resultCache) SetTTL(seconds int) {
	if seconds < 0 {
		seconds = 0
	}
	rc.ttlSeconds.Store(int64(seconds))
	if seconds == 0 {
		rc.mu.Lock()
		rc.entries = map[string]cacheEntry{}
		rc.mu.Unlock()
	}
}

func (rc *resultCache) enabled() bool { return rc.ttlSeconds.Load() > 0 }

func cacheKey(profile, sql string, maxRows int) string {
	return profile + "\x00" + strings.TrimSpace(sql) + "\x00" + strconv.Itoa(maxRows)
}

func (rc *resultCache) get(key string) (*dbconn.QueryResult, []string, bool) {
	if !rc.enabled() {
		return nil, nil, false
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	e, ok := rc.entries[key]
	if !ok || time.Now().After(e.expires) {
		if ok {
			delete(rc.entries, key)
		}
		return nil, nil, false
	}
	return e.result, e.masked, true
}

func (rc *resultCache) put(key string, res *dbconn.QueryResult, masked []string) {
	ttl := rc.ttlSeconds.Load()
	if ttl <= 0 || res == nil || res.RowCount > cacheMaxRows {
		return
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.entries) >= cacheMaxEntries {
		// drop expired first; if still full, drop everything (simple + bounded)
		now := time.Now()
		for k, e := range rc.entries {
			if now.After(e.expires) {
				delete(rc.entries, k)
			}
		}
		if len(rc.entries) >= cacheMaxEntries {
			rc.entries = map[string]cacheEntry{}
		}
	}
	rc.entries[key] = cacheEntry{result: res, masked: masked, expires: time.Now().Add(time.Duration(ttl) * time.Second)}
}

// executeGuarded wraps DB execution with the shared result pipeline used by
// both the MCP tool and the REST endpoints: TTL cache → execute → PII mask.
// fresh=true bypasses the cache read (the result still refreshes it).
func (s *Server) executeGuarded(ctx context.Context, profile, sql string, opts dbconn.ExecOptions, fresh bool) (*dbconn.QueryResult, []string, bool, error) {
	key := cacheKey(profile, sql, opts.MaxRows)
	if !fresh && !opts.Preview {
		if res, masked, ok := s.queryCache.get(key); ok {
			return res, masked, true, nil
		}
	}
	res, err := s.DB.Execute(ctx, profile, sql, opts)
	if err != nil {
		return nil, nil, false, err
	}
	masked := s.maskPIIResult(res)
	if !opts.Preview {
		s.queryCache.put(key, res, masked)
	}
	return res, masked, false, nil
}
