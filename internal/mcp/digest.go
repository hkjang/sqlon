package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// Metadata digest (improvement: 메타 다이제스트/웹훅). One compact snapshot of
// the catalog's operational health — quality score + release gate, the review
// queue backlog, golden-promotion candidates, and catalog size/warnings — for
// a daily ops glance or a webhook push. Read-only; composes existing views.

func (s *Server) MetadataDigest() map[string]any {
	cat := s.cat()

	q := cat.QualityReport()
	gate := cat.QualityGate()

	review := cat.ReviewCandidates(nil, nil, "")
	reviewSummary, _ := review["summary"].(map[string]int)

	goldenN := 0
	if g, ok := cat.SuggestGoldenFromFeedback(200)["count"].(int); ok {
		goldenN = g
	}

	errCount, warnCount := 0, 0
	for _, i := range cat.Issues {
		if i.Level == "error" {
			errCount++
		} else {
			warnCount++
		}
	}

	blocking := 0
	for _, v := range gate.Violations {
		if v.Severity == "block" {
			blocking++
		}
	}

	return map[string]any{
		"dataset": cat.FeedbackDatasetID(),
		"catalog": map[string]any{
			"tables":        len(cat.Tables),
			"relations":     len(cat.Relations),
			"load_errors":   errCount,
			"load_warnings": warnCount,
		},
		"quality": map[string]any{
			"overall_score": q.OverallScore,
			"overall_grade": q.OverallGrade,
			"gate_pass":     gate.Pass,
			"blocking":      blocking,
		},
		"reviews":           reviewSummary,
		"golden_candidates": goldenN,
		"headline":          digestHeadline(q.OverallScore, q.OverallGrade, gate.Pass, reviewSummary, goldenN),
		"note":              "읽기 전용 요약입니다. 상세는 /admin/quality, /admin/reviews 를 참고하세요.",
	}
}

func digestHeadline(score float64, grade string, gatePass bool, review map[string]int, golden int) string {
	gateTxt := "게이트 통과"
	if !gatePass {
		gateTxt = "게이트 차단"
	}
	pending := 0
	if review != nil {
		pending = review["pending"]
	}
	return fmt.Sprintf("품질 %.1f(%s) · %s · 검토 대기 %d · 골든 후보 %d", score, grade, gateTxt, pending, golden)
}

// postDigestWebhook sends the digest JSON to a configured URL. Best-effort:
// failures are logged, never fatal. Used by the scheduler when -digest-webhook
// is set.
func (s *Server) postDigestWebhook(ctx context.Context, url string, digest map[string]any) {
	if url == "" {
		return
	}
	body, err := json.Marshal(digest)
	if err != nil {
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Printf("digest webhook: build request failed: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("digest webhook: POST failed: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("digest webhook: non-2xx status %d", resp.StatusCode)
	}
}
