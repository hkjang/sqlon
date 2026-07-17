package catalog

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	FeedbackSchemaVersion = 2

	FeedbackTrustUntrusted = "untrusted"
	FeedbackTrustTrusted   = "trusted"

	FeedbackReviewPending  = "pending"
	FeedbackReviewApproved = "approved"
	FeedbackReviewRejected = "rejected"
)

// FeedbackRecord is the persisted shape of record_feedback. Successful,
// adopted SQL feeds few-shot reuse and search-score boosting only after an
// operator approves it. Provenance and trust fields are server-owned: callers
// must never be able to self-assert that their feedback is trusted.
type FeedbackRecord struct {
	SchemaVersion int      `json:"schema_version,omitempty"`
	ID            string   `json:"id,omitempty"`
	RecordedAt    string   `json:"recorded_at,omitempty"`
	Question      string   `json:"question"`
	Analysis      any      `json:"analysis,omitempty"`
	Tables        []string `json:"tables,omitempty"`
	Columns       []string `json:"columns,omitempty"`
	GeneratedSQL  string   `json:"generated_sql,omitempty"`
	Errors        any      `json:"validation_errors,omitempty"`
	FinalSQL      string   `json:"final_sql,omitempty"`
	Executed      *bool    `json:"executed,omitempty"`
	Adopted       *bool    `json:"adopted,omitempty"`
	Outcome       string   `json:"outcome"` // success | failure | corrected | rejected
	DurationMS    float64  `json:"duration_ms,omitempty"`
	ResultRows    *int64   `json:"result_rows,omitempty"`
	FailureCause  string   `json:"failure_cause,omitempty"`
	Notes         string   `json:"notes,omitempty"`

	// Server-owned provenance and review state. DatasetID prevents a feedback
	// file copied from another catalog from silently influencing this one.
	Source       string `json:"source,omitempty"`
	DatasetID    string `json:"dataset_id,omitempty"`
	TenantID     string `json:"tenant_id,omitempty"`
	ActorID      string `json:"actor_id,omitempty"`
	ActorRole    string `json:"actor_role,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	Fingerprint  string `json:"fingerprint,omitempty"`
	TrustStatus  string `json:"trust_status,omitempty"`
	ReviewStatus string `json:"review_status,omitempty"`
	ReviewedAt   string `json:"reviewed_at,omitempty"`
	ReviewedBy   string `json:"reviewed_by,omitempty"`
	ReviewNotes  string `json:"review_notes,omitempty"`
}

var feedbackTableRE = regexp.MustCompile(`(?i)\b(?:FROM|JOIN)\s+([A-Za-z_][\w$#]*\s*\.\s*[A-Za-z_][\w$#]*)`)

// FeedbackDatasetID is the stable catalog-local scope attached to feedback.
// A server may host one catalog at a time; the directory name is therefore
// the compatibility-safe dataset identity used by existing deployments.
func (c *Catalog) FeedbackDatasetID() string {
	id := strings.TrimSpace(filepath.Base(filepath.Clean(c.DataDir)))
	if id == "" || id == "." || id == string(filepath.Separator) {
		return "catalog"
	}
	return id
}

// SetFeedbackTenant scopes feedback consumption to one server/workspace
// tenant. NewServer calls this with a server-owned value; direct catalog users
// may leave it empty when they do not implement tenancy.
func (c *Catalog) SetFeedbackTenant(tenantID string) {
	c.FeedbackTenantID = strings.TrimSpace(tenantID)
	c.loadFeedback(c.DataDir)
}

// FeedbackEligible is deliberately fail-closed. Legacy records and new
// pending records remain audit evidence, but cannot enter prompts, retrieval
// priors, or learned rules until an operator approves them.
func (c *Catalog) FeedbackEligible(rec FeedbackRecord) bool {
	if rec.SchemaVersion < FeedbackSchemaVersion || rec.DatasetID != c.FeedbackDatasetID() {
		return false
	}
	if c.FeedbackTenantID != "" && rec.TenantID != c.FeedbackTenantID {
		return false
	}
	return rec.Fingerprint != "" &&
		strings.EqualFold(rec.TrustStatus, FeedbackTrustTrusted) &&
		strings.EqualFold(rec.ReviewStatus, FeedbackReviewApproved)
}

// loadFeedback aggregates per-table success counts from feedback/*.jsonl so
// search_schema can boost tables that historically produced adopted SQL.
func (c *Catalog) loadFeedback(dataDir string) {
	c.FeedbackUsage = map[string]int{}
	dir := filepath.Join(dataDir, "feedback")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for sc.Scan() {
			var rec FeedbackRecord
			if json.Unmarshal(sc.Bytes(), &rec) != nil {
				continue
			}
			if !c.FeedbackEligible(rec) || seen[rec.Fingerprint] {
				continue
			}
			seen[rec.Fingerprint] = true
			outcome := strings.ToLower(rec.Outcome)
			if outcome != "success" && outcome != "corrected" {
				continue
			}
			tables := rec.Tables
			if len(tables) == 0 {
				sql := nonEmpty(rec.FinalSQL, rec.GeneratedSQL)
				for _, m := range feedbackTableRE.FindAllStringSubmatch(sql, -1) {
					tables = append(tables, strings.ReplaceAll(m[1], " ", ""))
				}
			}
			seen := map[string]bool{}
			for _, tn := range tables {
				if t, ok := c.ResolveTable(tn); ok && !seen[t.FQN] {
					seen[t.FQN] = true
					c.FeedbackUsage[t.FQN]++
				}
			}
		}
		f.Close()
	}
}

// SuccessfulFeedbackExamples returns adopted question→SQL pairs matching the
// question tokens, for few-shot reuse alongside sql_datasets examples.
func (c *Catalog) SuccessfulFeedbackExamples(question string, topK int) []map[string]any {
	if topK <= 0 {
		topK = 3
	}
	dir := filepath.Join(c.DataDir, "feedback")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	tokens := c.expandTokens(tokenize(question))
	type scored struct {
		rec   FeedbackRecord
		score float64
	}
	var hits []scored
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for sc.Scan() {
			var rec FeedbackRecord
			if json.Unmarshal(sc.Bytes(), &rec) != nil {
				continue
			}
			if !c.FeedbackEligible(rec) || seen[rec.Fingerprint] {
				continue
			}
			seen[rec.Fingerprint] = true
			outcome := strings.ToLower(rec.Outcome)
			if (outcome != "success" && outcome != "corrected") || nonEmpty(rec.FinalSQL, rec.GeneratedSQL) == "" {
				continue
			}
			text := strings.ToLower(rec.Question)
			score := 0.0
			for _, tok := range tokens {
				if strings.Contains(text, strings.ToLower(tok)) {
					score++
				}
			}
			if score > 0 {
				hits = append(hits, scored{rec, score})
			}
		}
		f.Close()
	}
	if len(hits) == 0 {
		return nil
	}
	// simple selection sort of top K to avoid importing sort here twice
	out := []map[string]any{}
	for len(out) < topK && len(hits) > 0 {
		best := 0
		for i := range hits {
			if hits[i].score > hits[best].score {
				best = i
			}
		}
		r := hits[best].rec
		out = append(out, map[string]any{
			"question": r.Question,
			"sql":      nonEmpty(r.FinalSQL, r.GeneratedSQL),
			"outcome":  r.Outcome,
			"source":   "feedback",
			"score":    hits[best].score,
		})
		hits = append(hits[:best], hits[best+1:]...)
	}
	return out
}

// PendingFeedback returns the bounded, in-scope operator review queue. It is
// intentionally a catalog API rather than a generic dataset reader so callers
// cannot use it to inspect another dataset/tenant's feedback file.
func (c *Catalog) PendingFeedback(limit int) ([]FeedbackRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	dir := filepath.Join(c.DataDir, "feedback")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []FeedbackRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]FeedbackRecord, 0, limit)
	for i := len(entries) - 1; i >= 0 && len(out) < limit; i-- {
		e := entries[i]
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var fileRecords []FeedbackRecord
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for sc.Scan() {
			var rec FeedbackRecord
			if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.SchemaVersion < FeedbackSchemaVersion || rec.Fingerprint == "" {
				continue
			}
			if rec.DatasetID != c.FeedbackDatasetID() ||
				(c.FeedbackTenantID != "" && rec.TenantID != c.FeedbackTenantID) ||
				!strings.EqualFold(rec.ReviewStatus, FeedbackReviewPending) {
				continue
			}
			fileRecords = append(fileRecords, rec)
		}
		_ = f.Close()
		for j := len(fileRecords) - 1; j >= 0 && len(out) < limit; j-- {
			out = append(out, fileRecords[j])
		}
	}
	return out, nil
}

// ReviewFeedback moves exactly one in-scope record across the trust boundary.
// The caller must enforce administrator authorization and serialize this with
// appends. approve is idempotent; reject can also revoke a prior approval.
func (c *Catalog) ReviewFeedback(id, decision, reviewer, notes string, now time.Time) (FeedbackRecord, error) {
	id = strings.TrimSpace(id)
	reviewer = strings.TrimSpace(reviewer)
	if id == "" || reviewer == "" {
		return FeedbackRecord{}, errors.New("feedback id and reviewer are required")
	}
	if len(id) > 160 || len(reviewer) > 256 || len(notes) > 8192 {
		return FeedbackRecord{}, errors.New("feedback review field exceeds size limit")
	}
	switch strings.ToLower(strings.TrimSpace(decision)) {
	case "approve", FeedbackReviewApproved:
		decision = FeedbackReviewApproved
	case "reject", FeedbackReviewRejected:
		decision = FeedbackReviewRejected
	default:
		return FeedbackRecord{}, errors.New("decision must be approve or reject")
	}

	dir := filepath.Join(c.DataDir, "feedback")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return FeedbackRecord{}, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
		changed := false
		var reviewed FeedbackRecord
		for i, line := range lines {
			var raw map[string]any
			if json.Unmarshal([]byte(line), &raw) != nil || fmt.Sprint(raw["id"]) != id {
				continue
			}
			var rec FeedbackRecord
			if rb, err := json.Marshal(raw); err != nil || json.Unmarshal(rb, &rec) != nil {
				return FeedbackRecord{}, errors.New("feedback record is malformed")
			}
			if rec.SchemaVersion < FeedbackSchemaVersion || rec.DatasetID != c.FeedbackDatasetID() ||
				(c.FeedbackTenantID != "" && rec.TenantID != c.FeedbackTenantID) {
				return FeedbackRecord{}, errors.New("feedback record is outside the active dataset/tenant scope")
			}
			if rec.Fingerprint == "" {
				return FeedbackRecord{}, errors.New("feedback record has no server-issued fingerprint and cannot be approved")
			}
			if decision == FeedbackReviewApproved {
				raw["trust_status"] = FeedbackTrustTrusted
			} else {
				raw["trust_status"] = FeedbackTrustUntrusted
			}
			raw["review_status"] = decision
			raw["reviewed_at"] = now.UTC().Format(time.RFC3339Nano)
			raw["reviewed_by"] = reviewer
			if strings.TrimSpace(notes) == "" {
				delete(raw, "review_notes")
			} else {
				raw["review_notes"] = strings.TrimSpace(notes)
			}
			updated, err := json.Marshal(raw)
			if err != nil {
				return FeedbackRecord{}, err
			}
			lines[i] = string(updated)
			if err := json.Unmarshal(updated, &reviewed); err != nil {
				return FeedbackRecord{}, err
			}
			changed = true
			break
		}
		if !changed {
			continue
		}
		if err := replaceFeedbackFile(path, []byte(strings.Join(lines, "\n")+"\n")); err != nil {
			return FeedbackRecord{}, err
		}
		return reviewed, nil
	}
	return FeedbackRecord{}, fmt.Errorf("feedback %q not found", id)
}

func replaceFeedbackFile(path string, content []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".feedback-review-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
