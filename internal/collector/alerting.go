package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Alert struct {
	ID         string    `json:"id"`
	ProfileID  string    `json:"profile_id"`
	MetricName string    `json:"metric_name"`
	Severity   string    `json:"severity"` // info | warning | critical
	Message    string    `json:"message"`
	Value      float64   `json:"value"`
	Threshold  float64   `json:"threshold"`
	Timestamp  time.Time `json:"timestamp"`
}

var (
	alertsMu   sync.RWMutex
	alertsLog  = []Alert{}
	alertIDSeq = 0
)

const maxAlertsLogSize = 100

// Alert dedup: the periodic collector re-evaluates every minute, so without
// suppression a standing condition (a full disk, a lagging replica) would raise
// the same alert every cycle. claimAlert lets a given (profile|key) fire at most
// once per cooldown window.
var (
	dedupMu       sync.Mutex
	alertLastSeen = map[string]time.Time{}
)

// AlertCooldown is how long the same alert key is suppressed after firing.
const AlertCooldown = 10 * time.Minute

// Capacity-saturation thresholds (percent of allocated/max).
const (
	capacityWarnPercent     = 80.0
	capacityCriticalPercent = 90.0
)

// claimAlert reports whether an alert with this key may fire now, recording the
// time when it may. Returns false while within the cooldown window.
func claimAlert(key string, now time.Time) bool {
	dedupMu.Lock()
	defer dedupMu.Unlock()
	if last, ok := alertLastSeen[key]; ok && now.Sub(last) < AlertCooldown {
		return false
	}
	alertLastSeen[key] = now
	return true
}

// ClearAlertDedup resets the dedup state (test isolation).
func ClearAlertDedup() {
	dedupMu.Lock()
	defer dedupMu.Unlock()
	alertLastSeen = map[string]time.Time{}
}

func AddAlert(alert Alert) {
	alertsMu.Lock()
	defer alertsMu.Unlock()
	alertIDSeq++
	alert.ID = fmt.Sprintf("alt-%d", alertIDSeq)
	if alert.Timestamp.IsZero() {
		alert.Timestamp = time.Now()
	}

	// Prepend to maintain reverse chronological order
	alertsLog = append([]Alert{alert}, alertsLog...)
	if len(alertsLog) > maxAlertsLogSize {
		alertsLog = alertsLog[:maxAlertsLogSize]
	}
}

func GetAlerts() []Alert {
	alertsMu.RLock()
	defer alertsMu.RUnlock()
	// Return a copy to avoid race conditions
	copied := make([]Alert, len(alertsLog))
	copy(copied, alertsLog)
	return copied
}

func ClearAlerts() {
	alertsMu.Lock()
	defer alertsMu.Unlock()
	alertsLog = []Alert{}
}

// RunAlertingEngine compares the current snapshot with the previous snapshot
// to detect query regressions and schema drifts, raising alerts as needed.
func RunAlertingEngine(ctx context.Context, current *Snapshot, previous *Snapshot, webhookURL string) {
	if current == nil {
		return
	}
	now := current.CollectedAt
	if now.IsZero() {
		now = time.Now()
	}

	var raisedAlerts []Alert
	// raise applies dedup: an alert with the same key is suppressed for the
	// cooldown window so a standing condition does not re-fire every cycle.
	raise := func(a Alert, key string) {
		if a.Timestamp.IsZero() {
			a.Timestamp = now
		}
		if !claimAlert(current.ProfileID+"|"+key, now) {
			return
		}
		AddAlert(a)
		raisedAlerts = append(raisedAlerts, a)
	}

	// 1. Query performance regression (vs previous snapshot)
	if previous != nil && len(current.TopSQL) > 0 && len(previous.TopSQL) > 0 {
		prevMap := make(map[string]SQLStat)
		for _, p := range previous.TopSQL {
			prevMap[p.Fingerprint] = p
		}
		for _, curr := range current.TopSQL {
			prev, exists := prevMap[curr.Fingerprint]
			if !exists {
				continue
			}
			if curr.Calls > 0 && prev.Calls > 0 {
				currMean := curr.ElapsedMS / curr.Calls
				prevMean := prev.ElapsedMS / prev.Calls
				if currMean > prevMean*2 && currMean > 100 {
					raise(Alert{
						ProfileID:  current.ProfileID,
						MetricName: "query_latency_regression",
						Severity:   "warning",
						Message:    fmt.Sprintf("Query latency regression: mean execution time spiked from %.1fms to %.1fms (fingerprint: %s)", prevMean, currMean, truncateFingerprint(curr.Fingerprint)),
						Value:      currMean,
						Threshold:  prevMean * 2,
					}, "query_latency_regression:"+curr.Fingerprint)
				}
			}
		}
	}

	// 2. Schema / catalog drift warnings
	for _, warning := range current.Warnings {
		if strings.Contains(strings.ToLower(warning), "drift") || strings.Contains(strings.ToLower(warning), "diverge") {
			raise(Alert{
				ProfileID:  current.ProfileID,
				MetricName: "schema_drift",
				Severity:   "warning",
				Message:    fmt.Sprintf("Database schema drift warning detected: %s", warning),
			}, "schema_drift:"+truncateFingerprint(warning))
		}
	}

	// 3. Capacity saturation threshold (disk/tablespace filling up)
	for _, cap := range current.Capacity {
		pct := cap.UsagePercent
		if pct <= 0 && cap.MaxBytes > 0 {
			pct = cap.UsedBytes / cap.MaxBytes * 100
		}
		severity := ""
		switch {
		case pct >= capacityCriticalPercent:
			severity = "critical"
		case pct >= capacityWarnPercent:
			severity = "warning"
		}
		if severity == "" {
			continue
		}
		object := cap.Scope + ":" + cap.Name
		raise(Alert{
			ProfileID:  current.ProfileID,
			MetricName: "capacity_saturation",
			Severity:   severity,
			Message:    fmt.Sprintf("Capacity saturation on %s: %.1f%% used", object, pct),
			Value:      pct,
			Threshold:  capacityWarnPercent,
		}, "capacity_saturation:"+object)
	}

	// 4. Evidence bridge: any warning/critical evidence the collector recorded
	// (e.g. replication lag, backup failure, wraparound risk surfaced through a
	// snapshot) becomes an alert. This is the extensible path for new signals.
	for _, e := range current.Evidence {
		if e.Severity != "warning" && e.Severity != "critical" {
			continue
		}
		raise(Alert{
			ProfileID:  current.ProfileID,
			MetricName: strings.ToLower(e.Code),
			Severity:   e.Severity,
			Message:    e.Summary,
		}, "evidence:"+e.Code)
	}

	// 5. Dispatch to webhook if configured
	if webhookURL != "" && len(raisedAlerts) > 0 {
		go sendWebhookAlerts(webhookURL, raisedAlerts)
	}
}

func truncateFingerprint(fp string) string {
	if len(fp) > 60 {
		return fp[:57] + "..."
	}
	return fp
}

func sendWebhookAlerts(url string, alerts []Alert) {
	payload, err := json.Marshal(map[string]any{
		"source": "sqlon_alerting_engine",
		"ts":     time.Now().UTC().Format(time.RFC3339),
		"alerts": alerts,
	})
	if err != nil {
		log.Printf("alerting engine: failed to marshal webhook payload: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("alerting engine: failed to send webhook alert: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("alerting engine: webhook returned status %s", resp.Status)
	}
}
