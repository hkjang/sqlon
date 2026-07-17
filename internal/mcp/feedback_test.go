package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"sqlon/internal/catalog"
)

func feedbackToolCall(t *testing.T, s *Server, ctx context.Context, name string, args any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": json.RawMessage(raw)})
	res, err := s.callTool(ctx, params)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("%s returned %T", name, res)
	}
	return m
}

func readOnlyFeedbackRecord(t *testing.T, dataDir string) catalog.FeedbackRecord {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dataDir, "feedback"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("feedback files: entries=%d err=%v", len(entries), err)
	}
	f, err := os.Open(filepath.Join(dataDir, "feedback", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		t.Fatal("missing feedback record")
	}
	var rec catalog.FeedbackRecord
	if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if sc.Scan() {
		t.Fatal("expected exactly one feedback record")
	}
	return rec
}

func TestFeedbackIsPendingScopedAndAdminReviewed(t *testing.T) {
	s, _, _, _ := newAuthServer(t)
	dataDir := s.cat().DataDir
	alice, _ := s.Meta.Store.GetUserByUsername(t.Context(), "alice")
	admin, _ := s.Meta.Store.GetUserByUsername(t.Context(), "admin")
	ctx := withSession(withUser(t.Context(), alice), "feedback-session-1")

	queued := feedbackToolCall(t, s, ctx, "record_feedback", map[string]any{
		"question":      "테스트 고객별 이용금액",
		"tables":        []string{"TS.TBL1"},
		"final_sql":     "SELECT CUST_NO, USE_AMT FROM TS.TBL1",
		"outcome":       "success",
		"trust_status":  "trusted",
		"review_status": "approved",
		"dataset_id":    "attacker-dataset",
		"tenant_id":     "attacker-tenant",
		"actor_id":      "attacker",
		"source":        "clarification",
	})
	if queued["status"] != "queued_for_review" {
		t.Fatalf("record status = %v", queued)
	}
	rec := readOnlyFeedbackRecord(t, dataDir)
	if rec.SchemaVersion != catalog.FeedbackSchemaVersion || rec.DatasetID != s.cat().FeedbackDatasetID() || rec.TenantID != "default" {
		t.Fatalf("server scope was not enforced: %+v", rec)
	}
	if rec.ActorID != alice.ID || rec.ActorRole != "user" || rec.SessionID != "feedback-session-1" || rec.Source != "client" {
		t.Fatalf("server provenance was not enforced: %+v", rec)
	}
	if rec.TrustStatus != catalog.FeedbackTrustUntrusted || rec.ReviewStatus != catalog.FeedbackReviewPending || rec.Fingerprint == "" {
		t.Fatalf("new feedback crossed trust boundary: %+v", rec)
	}
	if got := s.cat().SuccessfulFeedbackExamples("고객 이용금액", 3); len(got) != 0 {
		t.Fatalf("pending feedback entered prompt examples: %+v", got)
	}

	// A regular user cannot approve even their own record.
	forbidden := feedbackToolCall(t, s, withUser(t.Context(), alice), "review_feedback", map[string]any{
		"feedback_id": rec.ID, "decision": "approve",
	})
	if forbidden["status"] != "forbidden" {
		t.Fatalf("non-admin review = %+v", forbidden)
	}

	queue := feedbackToolCall(t, s, withUser(t.Context(), admin), "review_feedback", map[string]any{})
	if queue["count"] != 1 {
		t.Fatalf("review queue = %+v", queue)
	}
	approved := feedbackToolCall(t, s, withUser(t.Context(), admin), "review_feedback", map[string]any{
		"feedback_id": rec.ID, "decision": "approve", "notes": "verified against catalog",
	})
	if approved["status"] != catalog.FeedbackReviewApproved || approved["trust_status"] != catalog.FeedbackTrustTrusted {
		t.Fatalf("approve result = %+v", approved)
	}
	if s.cat().FeedbackTenantID != "default" {
		t.Fatalf("catalog reload lost feedback tenant: %q", s.cat().FeedbackTenantID)
	}
	if got := s.cat().SuccessfulFeedbackExamples("고객 이용금액", 3); len(got) != 1 {
		t.Fatalf("approved feedback was not reusable: %+v", got)
	}
	if got := s.cat().FeedbackUsage["TS.TBL1"]; got != 1 {
		t.Fatalf("approved feedback usage prior = %d, want 1", got)
	}

	// Rejection revokes trust and hot-reloads the prior.
	rejected := feedbackToolCall(t, s, withUser(t.Context(), admin), "review_feedback", map[string]any{
		"feedback_id": rec.ID, "decision": "reject",
	})
	if rejected["status"] != catalog.FeedbackReviewRejected || rejected["trust_status"] != catalog.FeedbackTrustUntrusted {
		t.Fatalf("reject result = %+v", rejected)
	}
	if got := s.cat().SuccessfulFeedbackExamples("고객 이용금액", 3); len(got) != 0 {
		t.Fatalf("revoked feedback remained reusable: %+v", got)
	}
}

func TestFeedbackRateLimitIsPerActorAndDoesNotWriteExcess(t *testing.T) {
	s, _, _, _ := newAuthServer(t)
	dataDir := s.cat().DataDir
	alice, _ := s.Meta.Store.GetUserByUsername(t.Context(), "alice")
	admin, _ := s.Meta.Store.GetUserByUsername(t.Context(), "admin")
	limiter := newFeedbackRateLimiter(2, time.Minute)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	limiter.now = func() time.Time { return now }
	s.feedbackLimiter = limiter

	call := func(ctx context.Context, question string) map[string]any {
		return feedbackToolCall(t, s, ctx, "record_feedback", map[string]any{
			"question": question, "outcome": "failure",
		})
	}
	ctx := withSession(withUser(t.Context(), alice), "rate-a")
	if got := call(ctx, "q1")["status"]; got != "queued_for_review" {
		t.Fatalf("first = %v", got)
	}
	if got := call(ctx, "q2")["status"]; got != "queued_for_review" {
		t.Fatalf("second = %v", got)
	}
	if got := call(ctx, "q3")["status"]; got != "rate_limited" {
		t.Fatalf("third = %v", got)
	}
	// Rotating session ids does not bypass the authenticated actor bucket.
	if got := call(withSession(withUser(t.Context(), alice), "rate-b"), "q4")["status"]; got != "rate_limited" {
		t.Fatalf("rotated session = %v", got)
	}
	// A genuinely different authenticated actor has a separate bucket.
	if got := call(withUser(t.Context(), admin), "q5")["status"]; got != "queued_for_review" {
		t.Fatalf("other actor = %v", got)
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, "feedback"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("feedback files: %v %v", entries, err)
	}
	f, err := os.Open(filepath.Join(dataDir, "feedback", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lines := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines++
	}
	if lines != 3 {
		t.Fatalf("persisted records = %d, want 3 (rate-limited calls must not write)", lines)
	}

	now = now.Add(time.Minute)
	if got := call(ctx, "q6")["status"]; got != "queued_for_review" {
		t.Fatalf("new window = %v", got)
	}
}

func TestFeedbackRejectsInvalidOutcome(t *testing.T) {
	s, _ := newFixtureServer(t)
	params, _ := json.Marshal(map[string]any{
		"name":      "record_feedback",
		"arguments": map[string]any{"question": "q", "outcome": "auto-approved"},
	})
	if _, err := s.callTool(t.Context(), params); err == nil {
		t.Fatal("invalid outcome must be rejected")
	}
}

func TestStandaloneSessionRotationCannotBypassFeedbackLimit(t *testing.T) {
	s, _ := newFixtureServer(t)
	s.feedbackLimiter = newFeedbackRateLimiter(1, time.Minute)
	first, err := s.recordFeedback(withSession(t.Context(), "attacker-chosen-a"), map[string]any{
		"question": "q1", "outcome": "failure",
	}, "client")
	if err != nil || first["status"] != "queued_for_review" {
		t.Fatalf("first: result=%v err=%v", first, err)
	}
	second, err := s.recordFeedback(withSession(t.Context(), "attacker-chosen-b"), map[string]any{
		"question": "q2", "outcome": "failure",
	}, "client")
	if err != nil || second["status"] != "rate_limited" {
		t.Fatalf("rotated session bypassed limiter: result=%v err=%v", second, err)
	}
}

func TestFeedbackAppendAndReviewAreSerialized(t *testing.T) {
	s, dataDir := newFixtureServer(t)
	first, err := s.recordFeedback(withSession(t.Context(), "concurrent-seed"), map[string]any{
		"question": "seed", "outcome": "success", "final_sql": "SELECT CUST_NO FROM TS.TBL1",
	}, "client")
	if err != nil {
		t.Fatal(err)
	}
	id := first["feedback_id"].(string)

	var wg sync.WaitGroup
	errCh := make(chan error, 9)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.recordFeedback(withSession(context.Background(), "writer"), map[string]any{
				"question": "concurrent-" + string(rune('a'+i)), "outcome": "failure",
			}, "client")
			if err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := s.reviewFeedback(context.Background(), id, "approve", "concurrency test", 0)
		if err != nil {
			errCh <- err
		}
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, "feedback"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("feedback files: entries=%d err=%v", len(entries), err)
	}
	f, err := os.Open(filepath.Join(dataDir, "feedback", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	count, approved := 0, false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		count++
		var rec catalog.FeedbackRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("corrupt concurrent JSONL line: %v", err)
		}
		if rec.ID == id {
			approved = rec.ReviewStatus == catalog.FeedbackReviewApproved && rec.TrustStatus == catalog.FeedbackTrustTrusted
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 9 || !approved {
		t.Fatalf("count=%d approved=%v, want 9/true", count, approved)
	}
}
