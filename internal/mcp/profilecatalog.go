package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sqlon/internal/catalog"
	"sqlon/internal/metasync"
)

// Per-profile catalog workspaces. Beyond the single global catalog (-data),
// each registered DB profile can own an independent set of metadata JSON files
// under <data>/profiles/<profileID>/. This lets operators view and manage
// catalog metadata per connected database, and build a profile's catalog
// straight from its live schema. The global catalog stays the NL2SQL default;
// these workspaces are managed/inspected via the tools below and can be
// promoted into the active catalog by pointing -data at one.

// profileCatalogDir returns the workspace directory for a profile id, kept
// under the main dataset dir so it works in standalone mode.
func (s *Server) profileCatalogDir(profileID string) string {
	return filepath.Join(s.opDir(), "profiles", sanitizeProfileID(profileID))
}

// ---- workspace catalog cache (per-request profile catalogs) ----

// wsCacheEntry caches a compiled workspace catalog together with a fingerprint
// of its JSON files, so edits (put_profile_dataset, build, OM import) are
// picked up automatically without an explicit reload call.
type wsCacheEntry struct {
	cat *catalog.Catalog
	fp  string
}

// workspaceFingerprint hashes the workspace's JSON file names+sizes+mtimes.
func workspaceFingerprint(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if info, err := e.Info(); err == nil {
			fmt.Fprintf(&b, "%s|%d|%d;", e.Name(), info.Size(), info.ModTime().UnixNano())
		}
	}
	return b.String()
}

// workspaceCatalog returns the compiled catalog for a profile's workspace,
// loading and caching it (fingerprint-invalidated). ok=false when the profile
// has no workspace or it fails to compile.
func (s *Server) workspaceCatalog(profileID string) (*catalog.Catalog, bool) {
	dir := s.profileCatalogDir(profileID)
	if _, err := os.Stat(filepath.Join(dir, "meta_physical_models.json")); err != nil {
		return nil, false
	}
	fp := workspaceFingerprint(dir)

	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	if s.wsCache == nil {
		s.wsCache = map[string]wsCacheEntry{}
	}
	if e, ok := s.wsCache[profileID]; ok && e.fp == fp && fp != "" {
		return e.cat, true
	}
	cat, err := catalog.Load(dir)
	if err != nil {
		return nil, false
	}
	s.wsCache[profileID] = wsCacheEntry{cat: cat, fp: fp}
	return cat, true
}

// catalogFor picks the catalog a request should use: the profile's workspace
// when one exists for the given profile id, else the active global catalog.
// The second return names the source for response transparency.
func (s *Server) catalogFor(profileID string) (*catalog.Catalog, string) {
	if strings.TrimSpace(profileID) != "" {
		if ws, ok := s.workspaceCatalog(profileID); ok {
			return ws, "profile-workspace:" + profileID
		}
	}
	return s.cat(), "active"
}

// ensureWorkspaceScaffold creates the minimal required dataset files so
// catalog.Load succeeds on a fresh workspace (physical + logical models are
// required; an empty logical model is valid — tables simply have no logical
// names until managed).
func ensureWorkspaceScaffold(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range []string{"meta_physical_models.json", "meta_logical_models.json"} {
		p := filepath.Join(dir, f)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			if err := os.WriteFile(p, []byte("[]\n"), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

func sanitizeProfileID(id string) string {
	id = strings.TrimSpace(id)
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		return "_"
	}
	return out
}

// listProfileCatalogs reports, for every usable DB profile, whether it has a
// catalog workspace and its size.
func (s *Server) listProfileCatalogs(ctx context.Context) map[string]any {
	profs, err := s.usableProfiles(ctx)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	activeDir, _ := filepath.Abs(s.cat().DataDir)
	out := make([]map[string]any, 0, len(profs))
	for _, p := range profs {
		dir := s.profileCatalogDir(p.ID)
		absDir, _ := filepath.Abs(dir)
		entry := map[string]any{"profile": p.ID, "name": p.Name, "type": p.Type, "workspace": false, "active": absDir == activeDir}
		if fi, err := os.Stat(filepath.Join(dir, "meta_physical_models.json")); err == nil {
			entry["workspace"] = true
			entry["built_at"] = fi.ModTime().UTC().Format(time.RFC3339)
			if cat, err := catalog.Load(dir); err == nil {
				sum := cat.Summary()
				entry["tables"] = sum.TableCount
				entry["relations"] = sum.RelationCount
				q := cat.QualityReport()
				entry["quality_score"] = q.OverallScore
				entry["quality_grade"] = q.OverallGrade
			} else {
				entry["load_error"] = err.Error()
			}
		}
		out = append(out, entry)
	}
	return map[string]any{
		"profiles": out,
		"count":    len(out),
		"note":     "각 프로파일은 <data>/profiles/<profile>/ 에 독립 메타데이터 JSON을 가집니다. build_profile_catalog로 라이브 DB에서 구축하고, get_profile_catalog/get_profile_dataset로 조회, put_profile_dataset로 관리하세요.",
	}
}

// getProfileCatalog returns a profile workspace's catalog summary, dataset
// inventory, and health.
func (s *Server) getProfileCatalog(profileID string) map[string]any {
	dir := s.profileCatalogDir(profileID)
	if _, err := os.Stat(dir); err != nil {
		return map[string]any{"profile": profileID, "workspace": false,
			"note": "이 프로파일에는 카탈로그 워크스페이스가 없습니다. build_profile_catalog로 라이브 DB에서 생성하세요."}
	}
	cat, err := catalog.Load(dir)
	if err != nil {
		return map[string]any{"profile": profileID, "workspace": true, "error": "load failed: " + err.Error()}
	}
	q := cat.QualityReport()
	gate := cat.QualityGate()
	blocking := 0
	for _, v := range gate.Violations {
		if v.Severity == "block" {
			blocking++
		}
	}
	return map[string]any{
		"profile":   profileID,
		"workspace": true,
		"dir":       dir,
		"summary":   cat.Summary(),
		"datasets":  cat.DatasetStatus(),
		"health":    cat.Health(),
		"quality": map[string]any{
			"overall_score": q.OverallScore,
			"overall_grade": q.OverallGrade,
			"gate_pass":     gate.Pass,
			"blocking":      blocking,
		},
	}
}

// buildProfileCatalog collects a profile DB's live physical model and writes it
// into the profile's workspace (meta_physical_models.json + relations). ADMIN.
func (s *Server) buildProfileCatalog(ctx context.Context, profileID string, schemas []string, prune bool) map[string]any {
	snap, err := s.metasyncService().Collect(ctx, metasync.CollectRequest{SourceID: profileID, Schemas: schemas})
	if err != nil {
		return map[string]any{"error": "collect failed: " + err.Error()}
	}
	dir := s.profileCatalogDir(profileID)
	if err := ensureWorkspaceScaffold(dir); err != nil {
		return map[string]any{"error": err.Error()}
	}
	cols, rels := snapshotToPhysical(snap)
	// a workspace-scoped catalog: apply merges physical facts into the
	// workspace files (preserving any existing descriptions there).
	pc := &catalog.Catalog{DataDir: dir, Tables: map[string]*catalog.Table{}}
	res := pc.ApplyPhysicalSnapshot(cols, rels, prune, profileID, time.Now())
	res["profile"] = profileID
	res["dir"] = dir
	res["dialect"] = snap.Dialect
	if errMsg, _ := res["error"].(string); errMsg != "" {
		return res
	}
	// report the resulting workspace catalog summary
	if cat, err := catalog.Load(dir); err == nil {
		res["summary"] = cat.Summary()
	}
	res["note"] = "프로파일 워크스페이스에 물리 모델을 구축했습니다. get_profile_catalog로 조회, put_profile_dataset로 논리명·용어집 등 업무 메타데이터를 추가 관리하세요. 이 워크스페이스를 활성 카탈로그로 쓰려면 서버를 -data " + dir + " 로 기동하세요."
	return res
}

// buildAllProfileCatalogs builds/refreshes catalog workspaces for many profiles
// in one call (all usable profiles when the list is empty). Each profile is
// permission-checked; failures are reported per-profile without aborting the
// batch. ADMIN.
func (s *Server) buildAllProfileCatalogs(ctx context.Context, profiles []string, prune bool) map[string]any {
	targets := profiles
	if len(targets) == 0 {
		profs, err := s.usableProfiles(ctx)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		for _, p := range profs {
			targets = append(targets, p.ID)
		}
	}
	results := make([]map[string]any, 0, len(targets))
	built, failed := 0, 0
	for _, pid := range targets {
		r := map[string]any{"profile": pid}
		if err := s.canUseProfileID(ctx, userFrom(ctx), pid); err != nil {
			r["status"] = "forbidden"
			r["error"] = err.Error()
			failed++
			results = append(results, r)
			continue
		}
		res := s.buildProfileCatalog(ctx, pid, nil, prune)
		if em, _ := res["error"].(string); em != "" {
			r["status"] = "failed"
			r["error"] = em
			failed++
		} else {
			r["status"] = "built"
			r["columns_added"] = res["columns_added"]
			r["columns_updated"] = res["columns_updated"]
			if sum, ok := res["summary"].(catalog.CatalogSummary); ok {
				r["tables"] = sum.TableCount
			}
			built++
		}
		results = append(results, r)
	}
	return map[string]any{
		"requested": len(targets),
		"built":     built,
		"failed":    failed,
		"results":   results,
		"note":      "각 프로파일 워크스페이스를 라이브 DB로 구축했습니다. get_profile_catalog로 개별 조회하세요.",
	}
}

// omImportToProfile imports OpenMetadata's curated business metadata into a
// specific profile's catalog workspace (not the global catalog) — so each
// database's descriptions/PII/glossary land in that database's own workspace.
// apply=false previews; apply=true merges into the workspace (gaps only,
// existing values preserved). ADMIN.
func (s *Server) omImportToProfile(ctx context.Context, profileID, scope string, apply bool) map[string]any {
	imp, fetched, warnings, err := s.omBuildImport(ctx, scope, 0, true)
	if err != nil {
		return map[string]any{"error": "openmetadata fetch failed: " + err.Error()}
	}
	dir := s.profileCatalogDir(profileID)
	if err := ensureWorkspaceScaffold(dir); err != nil {
		return map[string]any{"error": err.Error()}
	}
	pc, err := catalog.Load(dir)
	if err != nil {
		return map[string]any{"error": "workspace load failed: " + err.Error()}
	}
	res := pc.ImportExternalMetadata(imp, apply, time.Now())
	res["profile"] = profileID
	res["fetched_tables"] = fetched
	res["target"] = "profile-workspace"
	if len(warnings) > 0 {
		res["warnings"] = warnings
	}
	return res
}

// getProfileDataset returns one dataset JSON file's raw content from a
// profile's workspace.
func (s *Server) getProfileDataset(profileID, name string) map[string]any {
	dir := s.profileCatalogDir(profileID)
	info, body, err := catalog.DatasetContent(dir, name)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	return map[string]any{
		"profile": profileID, "dataset": info.Name, "file": info.File,
		"content": json.RawMessage(nonEmptyJSON(body)),
	}
}

// putProfileDataset validates and writes one dataset JSON file into a profile's
// workspace, backing up the previous file and rolling back on load failure.
// ADMIN.
func (s *Server) putProfileDataset(profileID, name string, content json.RawMessage) map[string]any {
	dir := s.profileCatalogDir(profileID)
	if err := ensureWorkspaceScaffold(dir); err != nil {
		return map[string]any{"error": err.Error()}
	}
	info, backup, err := catalog.ReplaceDataset(dir, name, content)
	if err != nil {
		return map[string]any{"applied": false, "error": err.Error()}
	}
	// validate by recompiling the workspace; roll back on failure
	if _, lerr := catalog.Load(dir); lerr != nil {
		_ = catalog.RestoreDatasetBackup(dir, info.File, backup)
		return map[string]any{"applied": false, "error": "workspace failed to compile, rolled back: " + lerr.Error(), "backup": backup}
	}
	return map[string]any{"applied": true, "profile": profileID, "dataset": info.Name, "file": info.File, "backup": backup}
}

func nonEmptyJSON(b []byte) string {
	if len(b) == 0 {
		return "null"
	}
	return string(b)
}

// activeCatalogInfo reports which catalog is currently serving NL2SQL: the
// default (boot -data) or a profile workspace. Operational side-channels
// (profiles, audit, ...) always stay at the fixed operational dir regardless.
func (s *Server) activeCatalogInfo() map[string]any {
	activeDir := s.cat().DataDir
	info := map[string]any{
		"active_dir":      activeDir,
		"operational_dir": s.opDir(),
		"is_default":      activeDir == s.opDir(),
		"can_activate":    !s.datasetsInDB(),
	}
	if s.datasetsInDB() {
		info["activation_note"] = "메타 DB 모드에서는 요청의 profile 값으로 워크스페이스가 자동 선택됩니다. 전역 활성 전환은 필요하지 않습니다."
	} else {
		info["activation_note"] = "단독 모드에서는 워크스페이스를 전역 활성 카탈로그로 임시 전환할 수 있습니다."
	}
	base := filepath.Join(s.opDir(), "profiles") + string(filepath.Separator)
	if strings.HasPrefix(activeDir, base) {
		info["active_profile"] = strings.TrimSuffix(strings.TrimPrefix(activeDir, base), string(filepath.Separator))
	}
	sum := s.cat().Summary()
	info["tables"] = sum.TableCount
	return info
}

// setActiveCatalog hot-swaps the NL2SQL catalog to a profile workspace (or back
// to the default when profile is empty) WITHOUT a restart. Standalone only —
// in meta-DB mode datasets are materialized from Postgres to the boot dir, so a
// swap would fight the reload path. Operational data (DB profiles via s.DB and
// s.opDir, audit, workspaces) is unaffected. ADMIN.
func (s *Server) setActiveCatalog(profileID string) map[string]any {
	if s.datasetsInDB() {
		return map[string]any{"error": "메타 DB 모드에서는 활성 카탈로그 전환을 지원하지 않습니다(데이터셋이 Postgres에서 관리됨). 워크스페이스를 -data로 기동하세요."}
	}
	s.dataMu.Lock()
	defer s.dataMu.Unlock()

	dir := s.opDir()
	label := "default"
	if p := strings.TrimSpace(profileID); p != "" && !strings.EqualFold(p, "default") {
		dir = s.profileCatalogDir(p)
		if _, err := os.Stat(filepath.Join(dir, "meta_physical_models.json")); err != nil {
			return map[string]any{"error": "프로파일 '" + p + "' 워크스페이스가 없습니다. build_profile_catalog로 먼저 생성하세요."}
		}
		label = p
	}
	cat, err := catalog.Load(dir)
	if err != nil {
		return map[string]any{"error": "카탈로그 로드 실패, 기존 카탈로그 유지: " + err.Error()}
	}
	s.setCatalog(cat)
	res := s.activeCatalogInfo()
	res["activated"] = label
	res["note"] = "활성 NL2SQL 카탈로그를 전환했습니다(무재기동). DB 프로파일·감사 로그·워크스페이스는 운영 디렉터리에 그대로 유지됩니다. 재기동 시 -data 값으로 되돌아갑니다."
	return res
}
