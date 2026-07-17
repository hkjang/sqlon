package catalog

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Candidate approval workflow (Phase 9, FR-META-018/019/020). Reviewable
// candidates from the enrichment (Phase 5) and model-candidate (Phase 6)
// engines are surfaced here as a single review queue. An operator (or LLM
// client) approves or rejects each; decisions are persisted with reviewer +
// timestamp + notes. Approved items are compiled into a paste-ready
// overrides.json fragment — the catalog is still never mutated automatically,
// preserving the "physical facts auto, business meaning only on approval"
// principle end to end.

// reviewCandidate is a normalized candidate from any generator, with a stable
// content-addressed ID so a decision survives regeneration.
type reviewCandidate struct {
	ID         string   `json:"id"`
	Source     string   `json:"source"` // semantic | model
	Kind       string   `json:"kind"`   // logical_name|semantic_type|description|code_dict|metric|relation
	Table      string   `json:"table"`
	Column     string   `json:"column,omitempty"`
	Suggested  any      `json:"suggested"`
	Confidence float64  `json:"confidence"`
	Evidence   []string `json:"evidence,omitempty"`
}

// ReviewRecord is a persisted approve/reject decision plus a snapshot of what
// was decided (so history is meaningful even after the catalog changes).
type ReviewRecord struct {
	ID         string   `json:"id"`
	Source     string   `json:"source"`
	Kind       string   `json:"kind"`
	Table      string   `json:"table"`
	Column     string   `json:"column,omitempty"`
	Suggested  any      `json:"suggested"`
	Confidence float64  `json:"confidence"`
	Evidence   []string `json:"evidence,omitempty"`
	Status     string   `json:"status"` // approved | rejected
	Reviewer   string   `json:"reviewer,omitempty"`
	Notes      string   `json:"notes,omitempty"`
	DecidedAt  string   `json:"decided_at"`
	AppliedAt  string   `json:"applied_at,omitempty"` // set once written to dataset files
}

func (c *Catalog) reviewsPath() string {
	return filepath.Join(c.DataDir, "reviews", "decisions.json")
}

func (c *Catalog) loadReviewRecords() (map[string]ReviewRecord, error) {
	out := map[string]ReviewRecord{}
	path := c.reviewsPath()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}
	var recs []ReviewRecord
	if err := json.Unmarshal(b, &recs); err != nil {
		return out, err
	}
	for _, r := range recs {
		out[r.ID] = r
	}
	return out, nil
}

func (c *Catalog) saveReviewRecords(recs map[string]ReviewRecord) error {
	path := c.reviewsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	list := make([]ReviewRecord, 0, len(recs))
	for _, r := range recs {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// candidateID content-addresses a candidate so the same suggestion always maps
// to the same decision, but a CHANGED suggestion becomes a new pending item.
func candidateID(kind, table, column string, suggested any) string {
	h := fnv.New64a()
	payload, _ := json.Marshal(suggested)
	fmt.Fprintf(h, "%s|%s|%s|%s", kind, strings.ToUpper(table), strings.ToUpper(column), payload)
	return fmt.Sprintf("%s-%016x", kindShort(kind), h.Sum64())
}

func kindShort(kind string) string {
	switch kind {
	case "logical_name":
		return "ln"
	case "semantic_type":
		return "st"
	case "description":
		return "ds"
	case "code_dict":
		return "cd"
	case "metric":
		return "mt"
	case "relation":
		return "rl"
	default:
		return "xx"
	}
}

// collectCandidates gathers every current candidate from both engines into one
// normalized, ID'd list.
func (c *Catalog) collectCandidates(tables, kinds []string) []reviewCandidate {
	var out []reviewCandidate

	sem := c.SuggestSemanticMetadata(tables, kinds)
	if sugg, ok := sem["suggestions"].([]SemanticSuggestion); ok {
		for _, s := range sugg {
			out = append(out, reviewCandidate{
				ID:     candidateID(s.Kind, s.Table, s.Column, s.Suggested),
				Source: "semantic", Kind: s.Kind, Table: s.Table, Column: s.Column,
				Suggested: s.Suggested, Confidence: s.Confidence, Evidence: s.Evidence,
			})
		}
	}
	mod := c.SuggestModelCandidates(tables, kinds)
	if cands, ok := mod["candidates"].([]ModelCandidate); ok {
		for _, m := range cands {
			out = append(out, reviewCandidate{
				ID:     candidateID(m.Kind, m.Table, m.Column, m.Suggested),
				Source: "model", Kind: m.Kind, Table: m.Table, Column: m.Column,
				Suggested: m.Suggested, Confidence: m.Confidence, Evidence: m.Evidence,
			})
		}
	}
	// externally-staged candidates (e.g. OpenMetadata import) join the same
	// queue so they flow through the identical approve → apply pipeline.
	kindWanted := map[string]bool{}
	for _, k := range kinds {
		kindWanted[strings.ToLower(strings.TrimSpace(k))] = true
	}
	tableWanted := map[string]bool{}
	for _, t := range tables {
		if rt, ok := c.ResolveTable(t); ok {
			tableWanted[rt.FQN] = true
		}
	}
	for _, ec := range c.loadExternalCandidates() {
		if len(kindWanted) > 0 && !kindWanted[ec.Kind] {
			continue
		}
		if len(tableWanted) > 0 && !tableWanted[ec.Table] {
			continue
		}
		out = append(out, ec)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		return out[i].Table+out[i].Column < out[j].Table+out[j].Column
	})
	return out
}

// ReviewCandidates lists candidates joined with any stored decision. status
// filters to "pending" | "approved" | "rejected" (empty = all).
func (c *Catalog) ReviewCandidates(tables, kinds []string, status string) map[string]any {
	decisions, err := c.loadReviewRecords()
	if err != nil {
		return map[string]any{"error": "failed to load decisions: " + err.Error()}
	}
	cands := c.collectCandidates(tables, kinds)

	type item struct {
		reviewCandidate
		Status    string `json:"status"`
		Reviewer  string `json:"reviewer,omitempty"`
		Notes     string `json:"notes,omitempty"`
		DecidedAt string `json:"decided_at,omitempty"`
	}
	var items []item
	counts := map[string]int{"pending": 0, "approved": 0, "rejected": 0}
	live := map[string]bool{}
	for _, cand := range cands {
		live[cand.ID] = true
		st := "pending"
		var rec ReviewRecord
		if r, ok := decisions[cand.ID]; ok {
			st, rec = r.Status, r
		}
		counts[st]++
		if status != "" && status != st {
			continue
		}
		items = append(items, item{reviewCandidate: cand, Status: st,
			Reviewer: rec.Reviewer, Notes: rec.Notes, DecidedAt: rec.DecidedAt})
	}

	// stale decisions: recorded for candidates that no longer regenerate (the
	// underlying value was already applied, or the schema changed)
	stale := 0
	for id := range decisions {
		if !live[id] {
			stale++
		}
	}
	return map[string]any{
		"items":        items,
		"count":        len(items),
		"summary":      counts,
		"stale_count":  stale,
		"note":         "승인은 overrides/metrics/relations 스니펫으로만 반영되며 카탈로그를 자동 수정하지 않습니다. /api/reviews/apply 로 승인분 스니펫을 받으세요.",
		"how_to_apply": "결정: POST /api/reviews/decide {decisions:[{id,decision:approved|rejected,notes}], reviewer}. 승인 스니펫: GET /api/reviews/apply.",
	}
}

// DecideCandidate is one approve/reject instruction.
type DecideCandidate struct {
	ID       string `json:"id"`
	Decision string `json:"decision"` // approved | rejected
	Notes    string `json:"notes,omitempty"`
}

// DecideCandidates records approve/reject decisions. now is injected so the
// call is testable/deterministic.
func (c *Catalog) DecideCandidates(decisions []DecideCandidate, reviewer string, now time.Time) map[string]any {
	stored, err := c.loadReviewRecords()
	if err != nil {
		return map[string]any{"error": "failed to load decisions: " + err.Error()}
	}
	byID := map[string]reviewCandidate{}
	for _, cand := range c.collectCandidates(nil, nil) {
		byID[cand.ID] = cand
	}

	applied, unknown := 0, []string{}
	ts := now.UTC().Format(time.RFC3339)
	for _, d := range decisions {
		status := strings.ToLower(strings.TrimSpace(d.Decision))
		if status == "approve" {
			status = "approved"
		} else if status == "reject" {
			status = "rejected"
		}
		if status != "approved" && status != "rejected" {
			unknown = append(unknown, d.ID+" (bad decision)")
			continue
		}
		cand, ok := byID[d.ID]
		if !ok {
			// allow decisions on already-stored items too (e.g. re-reject)
			if existing, had := stored[d.ID]; had {
				existing.Status, existing.Notes, existing.Reviewer, existing.DecidedAt = status, d.Notes, reviewer, ts
				stored[d.ID] = existing
				applied++
				continue
			}
			unknown = append(unknown, d.ID)
			continue
		}
		stored[cand.ID] = ReviewRecord{
			ID: cand.ID, Source: cand.Source, Kind: cand.Kind, Table: cand.Table, Column: cand.Column,
			Suggested: cand.Suggested, Confidence: cand.Confidence, Evidence: cand.Evidence,
			Status: status, Reviewer: reviewer, Notes: d.Notes, DecidedAt: ts,
		}
		applied++
	}
	if err := c.saveReviewRecords(stored); err != nil {
		return map[string]any{"error": "failed to save decisions: " + err.Error()}
	}
	res := map[string]any{"applied": applied, "reviewer": reviewer}
	if len(unknown) > 0 {
		res["unknown_ids"] = unknown
		res["note"] = "일부 ID가 현재 후보/기존 결정과 일치하지 않아 무시되었습니다. 최신 목록을 다시 조회하세요."
	}
	return res
}

// ApprovedOverrides compiles all approved decisions into paste-ready fragments
// grouped by destination file.
func (c *Catalog) ApprovedOverrides() map[string]any {
	stored, err := c.loadReviewRecords()
	if err != nil {
		return map[string]any{"error": "failed to load decisions: " + err.Error()}
	}
	colByKey := map[string]map[string]any{}
	var colOrder []string
	var metrics, relations, codeDicts []any

	for _, r := range stored {
		if r.Status != "approved" {
			continue
		}
		switch r.Kind {
		case "logical_name", "semantic_type", "description":
			key := r.Table + "|" + r.Column
			entry := colByKey[key]
			if entry == nil {
				entry = map[string]any{"table": r.Table, "column": r.Column}
				colByKey[key] = entry
				colOrder = append(colOrder, key)
			}
			field := map[string]string{"logical_name": "logical_name", "semantic_type": "semantic_type", "description": "description"}[r.Kind]
			if s, ok := r.Suggested.(string); ok {
				entry[field] = s
			} else {
				entry[field] = r.Suggested
			}
		case "code_dict":
			codeDicts = append(codeDicts, map[string]any{"table": r.Table, "column": r.Column, "code_dict": r.Suggested})
		case "metric":
			metrics = append(metrics, r.Suggested)
		case "relation":
			relations = append(relations, r.Suggested)
		}
	}
	cols := make([]map[string]any, 0, len(colOrder))
	for _, k := range colOrder {
		cols = append(cols, colByKey[k])
	}
	return map[string]any{
		"overrides_columns": cols,
		"metrics":           metrics,
		"relations":         relations,
		"code_dicts":        codeDicts,
		"counts": map[string]int{
			"overrides_columns": len(cols), "metrics": len(metrics),
			"relations": len(relations), "code_dicts": len(codeDicts),
		},
		"note": "승인된 후보만 포함합니다. overrides.json(columns), metrics.json, relations.json에 반영 후 재기동/재적재하세요.",
	}
}

// externalCandidatesPath stores candidates staged from an external catalog
// (e.g. OpenMetadata import in review mode) so they join the review queue.
func (c *Catalog) externalCandidatesPath() string {
	return filepath.Join(c.DataDir, "reviews", "imported.json")
}

func (c *Catalog) loadExternalCandidates() []reviewCandidate {
	b, err := os.ReadFile(c.externalCandidatesPath())
	if err != nil {
		return nil
	}
	var out []reviewCandidate
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

// StageExternalImport turns an external catalog's column metadata into review
// candidates (logical_name / description GAPS only — the kinds apply_approved
// can safely merge) and appends them to the staging file, deduped by id. They
// then appear in the review queue for human approval. Returns counts.
func (c *Catalog) StageExternalImport(imp ExternalImport) map[string]any {
	existing := c.loadExternalCandidates()
	byID := map[string]bool{}
	for _, e := range existing {
		byID[e.ID] = true
	}

	staged := 0
	add := func(kind, fqn, column, suggested string) {
		id := candidateID(kind, fqn, column, suggested)
		if byID[id] {
			return
		}
		byID[id] = true
		existing = append(existing, reviewCandidate{
			ID: id, Source: imp.Source, Kind: kind, Table: fqn, Column: column,
			Suggested: suggested, Confidence: 0.9,
			Evidence: []string{"imported from " + imp.Source},
		})
		staged++
	}

	for _, cm := range imp.Columns {
		t, ok := c.ResolveTable(cm.Table)
		if !ok {
			continue
		}
		col := t.ColumnMap[cleanIdent(cm.Column)]
		if col == nil {
			continue
		}
		if cm.LogicalName != "" && col.LogicalName == "" {
			add("logical_name", t.FQN, col.Name, cm.LogicalName)
		}
		if cm.Description != "" && col.Description == "" {
			add("description", t.FQN, col.Name, cm.Description)
		}
	}

	if staged > 0 {
		path := c.externalCandidatesPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return map[string]any{"error": err.Error()}
		}
		b, _ := json.MarshalIndent(existing, "", "  ")
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, b, 0o644); err != nil {
			return map[string]any{"error": err.Error()}
		}
		if err := os.Rename(tmp, path); err != nil {
			return map[string]any{"error": err.Error()}
		}
	}
	return map[string]any{
		"staged":     staged,
		"total_open": len(existing),
		"note":       "리뷰 큐에 스테이징했습니다. /admin/reviews 또는 review_candidates로 검토·승인 후 apply_approved_candidates로 반영하세요. (논리명·설명 gap만; PII·충돌은 import apply/drift로 처리)",
	}
}
