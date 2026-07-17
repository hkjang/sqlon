package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"sqlon/internal/catalog"
)

const (
	feedbackMaxRecordBytes = 512 << 10
	feedbackDefaultLimit   = 30
	feedbackDefaultWindow  = time.Minute
	feedbackMaxRateActors  = 10_000
)

type feedbackRateBucket struct {
	windowStart time.Time
	count       int
}

// feedbackRateLimiter is a bounded, in-memory fixed-window limiter. Feedback
// is an advisory signal, so dropping excess records is preferable to allowing
// an unauthenticated/stateless client to fill disk or flood the review queue.
type feedbackRateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	now     func() time.Time
	buckets map[string]feedbackRateBucket
}

func newFeedbackRateLimiter(limit int, window time.Duration) *feedbackRateLimiter {
	if limit <= 0 {
		limit = feedbackDefaultLimit
	}
	if window <= 0 {
		window = feedbackDefaultWindow
	}
	return &feedbackRateLimiter{limit: limit, window: window, now: time.Now, buckets: map[string]feedbackRateBucket{}}
}

func (l *feedbackRateLimiter) allow(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.buckets[key]; !exists && len(l.buckets) >= feedbackMaxRateActors {
		for k, old := range l.buckets {
			if now.Sub(old.windowStart) >= 2*l.window || now.Before(old.windowStart) {
				delete(l.buckets, k)
			}
		}
		if len(l.buckets) >= feedbackMaxRateActors {
			return false, l.window
		}
	}
	b := l.buckets[key]
	if b.windowStart.IsZero() || now.Sub(b.windowStart) >= l.window || now.Before(b.windowStart) {
		b = feedbackRateBucket{windowStart: now}
	}
	if b.count >= l.limit {
		retry := l.window - now.Sub(b.windowStart)
		if retry < time.Second {
			retry = time.Second
		}
		return false, retry
	}
	b.count++
	l.buckets[key] = b
	// Avoid retaining unbounded actor IDs on a long-running public endpoint.
	if len(l.buckets) >= feedbackMaxRateActors {
		for k, old := range l.buckets {
			if now.Sub(old.windowStart) >= 2*l.window {
				delete(l.buckets, k)
			}
		}
	}
	return true, 0
}

func (s *Server) feedbackActor(ctx context.Context) (id, role, rateKey string) {
	if u := userFrom(ctx); u != nil {
		return u.ID, u.Role, "user:" + u.ID
	}
	// Standalone clients can choose arbitrary Mcp-Session-Id values (the MCP
	// transport is deliberately lenient), so a session id is provenance only,
	// never a rate-limit identity. Otherwise rotating the header bypasses the
	// limiter. HTTP master-token and anonymous callers get distinct shared
	// buckets; stdio/no-token local callers share the local bucket.
	if admin, ok := ctx.Value(ctxKeyHTTPAdmin{}).(bool); ok {
		if admin {
			return "master-token", "admin", "standalone:master"
		}
		return "anonymous", "anonymous", "standalone:anonymous"
	}
	return "local", "local", "standalone:local"
}

func (s *Server) feedbackReviewer(ctx context.Context) string {
	if u := userFrom(ctx); u != nil {
		return u.ID
	}
	if v, ok := ctx.Value(ctxKeyHTTPAdmin{}).(bool); ok && v {
		return "master-token"
	}
	return "local-admin"
}

func (s *Server) recordFeedback(ctx context.Context, input map[string]any, source string) (map[string]any, error) {
	actorID, actorRole, rateKey := s.feedbackActor(ctx)
	if ok, retry := s.feedbackLimiter.allow(rateKey); !ok {
		return map[string]any{
			"status":              "rate_limited",
			"retry_after_seconds": int(retry.Round(time.Second) / time.Second),
			"review_status":       catalog.FeedbackReviewPending,
			"trust_status":        catalog.FeedbackTrustUntrusted,
		}, nil
	}

	data := make(map[string]any, len(input)+16)
	for k, v := range input {
		data[k] = v
	}
	if err := validateFeedbackInput(data); err != nil {
		return nil, err
	}

	// These fields define the trust boundary and are always server-owned,
	// regardless of what a client supplied in the arguments map.
	for _, key := range []string{
		"schema_version", "id", "recorded_at", "source", "dataset_id", "tenant_id",
		"actor_id", "actor_role", "session_id", "fingerprint", "trust_status",
		"review_status", "reviewed_at", "reviewed_by", "review_notes",
	} {
		delete(data, key)
	}
	data["schema_version"] = catalog.FeedbackSchemaVersion
	data["source"] = source
	data["dataset_id"] = s.cat().FeedbackDatasetID()
	data["tenant_id"] = s.Options.FeedbackTenantID
	data["actor_id"] = actorID
	data["actor_role"] = actorRole
	if sid := sessionFrom(ctx); sid != "" {
		data["session_id"] = sid
	}
	data["trust_status"] = catalog.FeedbackTrustUntrusted
	data["review_status"] = catalog.FeedbackReviewPending
	data["fingerprint"] = feedbackFingerprint(data)
	data["id"] = newFeedbackID()
	data["recorded_at"] = time.Now().UTC().Format(time.RFC3339Nano)

	b, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	if len(b) > feedbackMaxRecordBytes {
		return nil, fmt.Errorf("feedback record is too large: %d bytes (max %d)", len(b), feedbackMaxRecordBytes)
	}

	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	dir := filepath.Join(s.cat().DataDir, "feedback")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "feedback-"+time.Now().Format("20060102")+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return map[string]any{
		"feedback_id":   data["id"],
		"path":          path, // retained for backwards compatibility
		"status":        "queued_for_review",
		"review_status": catalog.FeedbackReviewPending,
		"trust_status":  catalog.FeedbackTrustUntrusted,
		"dataset_id":    data["dataset_id"],
	}, nil
}

func validateFeedbackInput(data map[string]any) error {
	question, ok := data["question"].(string)
	if !ok || strings.TrimSpace(question) == "" {
		return errors.New("feedback question is required")
	}
	if len(question) > 8192 {
		return errors.New("feedback question exceeds 8192 bytes")
	}
	outcome, ok := data["outcome"].(string)
	if !ok {
		return errors.New("feedback outcome is required")
	}
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case "success", "failure", "corrected", "rejected":
		data["outcome"] = strings.ToLower(strings.TrimSpace(outcome))
	default:
		return errors.New("feedback outcome must be success, failure, corrected, or rejected")
	}
	for _, key := range []string{"generated_sql", "final_sql"} {
		if v, ok := data[key].(string); ok && len(v) > 128<<10 {
			return fmt.Errorf("feedback %s exceeds 128 KiB", key)
		}
	}
	for _, key := range []string{"notes", "failure_cause"} {
		if v, ok := data[key].(string); ok && len(v) > 8192 {
			return fmt.Errorf("feedback %s exceeds 8192 bytes", key)
		}
	}
	return nil
}

func feedbackFingerprint(data map[string]any) string {
	// Only semantic feedback fields participate. Provenance, timestamps, and
	// caller-supplied unknown fields cannot be varied to bypass de-duplication.
	keys := []string{
		"question", "tables", "columns", "generated_sql", "validation_errors",
		"final_sql", "executed", "adopted", "outcome", "failure_cause",
	}
	canonical := make(map[string]any, len(keys))
	for _, key := range keys {
		if v, ok := data[key]; ok {
			canonical[key] = v
		}
	}
	b, _ := json.Marshal(canonical)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func newFeedbackID() string {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return time.Now().UTC().Format("20060102T150405.000000000Z")
	}
	return time.Now().UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(suffix[:])
}

func (s *Server) reviewFeedback(ctx context.Context, feedbackID, decision, notes string, limit int) (map[string]any, error) {
	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	if strings.TrimSpace(feedbackID) == "" {
		queue, err := s.cat().PendingFeedback(limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"status":     "review_queue",
			"pending":    queue,
			"count":      len(queue),
			"dataset_id": s.cat().FeedbackDatasetID(),
			"tenant_id":  s.Options.FeedbackTenantID,
		}, nil
	}
	rec, err := s.cat().ReviewFeedback(feedbackID, decision, s.feedbackReviewer(ctx), notes, time.Now())
	if err != nil {
		return nil, err
	}
	// Recompile so approval/revocation changes the in-memory usage prior now;
	// SuccessfulFeedbackExamples and LearnFromFeedback also re-check the file.
	cat, err := catalog.Load(s.cat().DataDir)
	if err != nil {
		return nil, fmt.Errorf("feedback reviewed but catalog reload failed: %w", err)
	}
	s.setCatalog(cat)
	return map[string]any{
		"status":         rec.ReviewStatus,
		"feedback_id":    rec.ID,
		"trust_status":   rec.TrustStatus,
		"reviewed_at":    rec.ReviewedAt,
		"reviewed_by":    rec.ReviewedBy,
		"dataset_id":     rec.DatasetID,
		"catalog_reload": true,
	}, nil
}
