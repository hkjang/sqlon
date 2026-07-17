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

	var raisedAlerts []Alert

	// 1. Check Query Performance Regression
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

				// Trigger alert if mean latency has spiked by > 2x and is at least 100ms
				if currMean > prevMean*2 && currMean > 100 {
					alert := Alert{
						ProfileID:  current.ProfileID,
						MetricName: "query_latency_regression",
						Severity:   "warning",
						Message:    fmt.Sprintf("Query latency regression: mean execution time spiked from %.1fms to %.1fms (fingerprint: %s)", prevMean, currMean, truncateFingerprint(curr.Fingerprint)),
						Value:      currMean,
						Threshold:  prevMean * 2,
						Timestamp:  current.CollectedAt,
					}
					AddAlert(alert)
					raisedAlerts = append(raisedAlerts, alert)
				}
			}
		}
	}

	// 2. Check Schema / Catalog warnings (potential schema drift)
	for _, warning := range current.Warnings {
		if strings.Contains(strings.ToLower(warning), "drift") || strings.Contains(strings.ToLower(warning), "diverge") {
			alert := Alert{
				ProfileID:  current.ProfileID,
				MetricName: "schema_drift",
				Severity:   "warning",
				Message:    fmt.Sprintf("Database schema drift warning detected: %s", warning),
				Timestamp:  current.CollectedAt,
			}
			AddAlert(alert)
			raisedAlerts = append(raisedAlerts, alert)
		}
	}

	// 3. Dispatch to Webhook if configured
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
