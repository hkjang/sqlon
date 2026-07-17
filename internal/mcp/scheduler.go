package mcp

import (
	"context"
	"log"
	"time"
)

// Metadata scheduler (improvement: 스케줄러). When -sync-interval and
// -sync-source are set, the server periodically runs an incremental metadata
// sync against the source and logs a one-line digest (change count + quality
// score + release-gate status). It never mutates business meaning — the sync
// stays incremental and deletions remain retire-candidates — so it is safe to
// leave running. Cron-free; lives in the server process.

// SchedulerConfig configures the background metadata loop.
type SchedulerConfig struct {
	Source            string        // db profile id to sync (required to sync)
	Interval          time.Duration // <=0 disables the whole scheduler
	WebhookURL        string        // optional: POST the digest here each tick
	OpenMetadata      bool          // optional: import from OpenMetadata each tick
	OpenMetadataScope string        // optional scope FQN for the import
	ApplySync         bool          // optional: auto-apply the sync snapshot to the catalog
	DBADigest         bool          // optional: also POST the DBA digest to the webhook each tick
	DBADigestProfile  string        // optional: scope the DBA digest to this profile (empty = all)
}

func (c SchedulerConfig) enabled() bool {
	return c.Source != "" || c.WebhookURL != "" || c.OpenMetadata
}

// StartScheduler launches the background loop unless interval<=0. The loop
// stops when ctx is canceled. The first run fires after one interval (not at
// boot) to avoid racing startup. Each tick runs, in order: DB sync (physical) →
// OpenMetadata import (business meaning, gaps only) → digest webhook (notify) —
// whichever are configured.
func (s *Server) StartScheduler(ctx context.Context, cfg SchedulerConfig) {
	if cfg.Interval <= 0 || !cfg.enabled() {
		return
	}
	if cfg.Interval < time.Minute {
		cfg.Interval = time.Minute // floor: never hammer the source DB
	}
	log.Printf("metadata scheduler: every %s (source=%q openmetadata=%v webhook=%v)",
		cfg.Interval, cfg.Source, cfg.OpenMetadata, cfg.WebhookURL != "")
	go func() {
		t := time.NewTicker(cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Printf("metadata scheduler stopped")
				return
			case <-t.C:
				if cfg.Source != "" {
					s.runScheduledSync(ctx, cfg.Source, cfg.ApplySync)
				}
				if cfg.OpenMetadata {
					s.runScheduledOMImport(ctx, cfg.OpenMetadataScope)
				}
				if cfg.WebhookURL != "" {
					s.postDigestWebhook(ctx, cfg.WebhookURL, s.MetadataDigest())
					if cfg.DBADigest {
						dg := s.mcpDBADigest(cfg.DBADigestProfile, 0, 0)
						log.Printf("scheduled dba digest: %v", dg["headline"])
						s.postDigestWebhook(ctx, cfg.WebhookURL, dg)
					}
				}
			}
		}
	}()
}

// runScheduledOMImport applies an incremental OpenMetadata import (gaps only)
// on a schedule and logs/audits the outcome. Skips cleanly when OpenMetadata is
// not configured.
func (s *Server) runScheduledOMImport(ctx context.Context, scope string) {
	if _, _, src := s.omConfig(); src == "unset" {
		return // not configured; nothing to do
	}
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	res := s.omImport(runCtx, scope, 0, true, true, false)
	if errMsg, _ := res["error"].(string); errMsg != "" {
		log.Printf("scheduled openmetadata import failed: %s", errMsg)
		s.appendAudit(map[string]any{
			"ts": time.Now().Format(time.RFC3339Nano), "tool": "scheduler:openmetadata_import",
			"detail": scope, "is_error": true, "error": errMsg,
		})
		return
	}
	written, _ := res["written"].(map[string]int)
	fetched, _ := res["fetched_tables"].(int)
	log.Printf("scheduled openmetadata import: fetched=%d written=%v", fetched, written)
	s.appendAudit(map[string]any{
		"ts": time.Now().Format(time.RFC3339Nano), "tool": "scheduler:openmetadata_import",
		"detail": scope, "fetched_tables": fetched, "written": written,
	})
}

func (s *Server) runScheduledSync(ctx context.Context, source string, applySync bool) {
	// bound each run so a stuck DB cannot wedge the ticker goroutine
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	res := s.mcpRunMetadataSync(runCtx, source, nil, true, false)
	if status, _ := res["status"].(string); status != "ok" {
		log.Printf("scheduled sync[%s] failed: %v", source, res["error"])
		s.appendAudit(map[string]any{
			"ts": time.Now().Format(time.RFC3339Nano), "tool": "scheduler:sync",
			"detail": source, "is_error": true, "error": res["error"],
		})
		return
	}
	changes := 0
	if n, ok := res["change_count"].(int); ok {
		changes = n
	}
	skipped, _ := res["skipped"].(bool)

	// auto-apply the physical model to the catalog (retire candidates kept).
	applied := ""
	if applySync && !skipped {
		ar := s.mcpApplyMetadataSync(source, false)
		if em, _ := ar["error"].(string); em != "" {
			applied = "apply-failed: " + em
		} else {
			applied = "applied"
		}
	}

	q := s.cat().QualityReport()
	gate := s.cat().QualityGate()
	log.Printf("scheduled sync[%s]: changes=%d skipped=%v apply=%q quality=%.1f(%s) gate=%v",
		source, changes, skipped, applied, q.OverallScore, q.OverallGrade, gate.Pass)
	s.appendAudit(map[string]any{
		"ts": time.Now().Format(time.RFC3339Nano), "tool": "scheduler:sync",
		"detail":        source,
		"change_count":  changes,
		"skipped":       skipped,
		"applied":       applied,
		"quality_score": q.OverallScore,
		"gate_pass":     gate.Pass,
	})
}
