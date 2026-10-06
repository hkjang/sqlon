package earlywarning

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// The heartbeat is a dead man's switch for SQLON itself: if SQLON stops,
// every warning stops with it and nobody notices. After each cycle that
// both completed and could deliver notifications, SQLON pings an external
// monitor (healthchecks.io, Uptime Kuma push monitor, Better Stack …); the
// monitor alerts when the pings stop. A cycle whose default channel keeps
// failing does not ping — "evaluating but unable to tell anyone" is as bad
// as down.

type HeartbeatStatus struct {
	Configured  bool       `json:"configured"`
	Target      string     `json:"target,omitempty"`
	LastPingAt  *time.Time `json:"last_ping_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	LastSkipped string     `json:"last_skipped,omitempty"`
	Pings       int64      `json:"pings"`
}

var heartbeatClient = &http.Client{Timeout: 10 * time.Second}

func (e *Engine) heartbeat(ctx context.Context, report CycleReport) {
	e.mu.Lock()
	url := e.HeartbeatURL
	skip := ""
	switch {
	case url == "":
		e.st.Heartbeat = HeartbeatStatus{}
		e.mu.Unlock()
		return
	case report.Error != "":
		skip = "평가 실패: " + report.Error
	case e.Notifier != nil && e.st.Delivery.ConsecutiveFailures > 0:
		skip = "기본 알림 채널 전달 실패 중: " + e.st.Delivery.LastError
	}
	e.st.Heartbeat.Configured, e.st.Heartbeat.Target = true, MaskURL(url)
	if skip != "" {
		e.st.Heartbeat.LastSkipped = skip
		e.mu.Unlock()
		e.logf("early-warning: heartbeat withheld (%s)", skip)
		return
	}
	e.mu.Unlock()

	err := ping(ctx, url)
	e.mu.Lock()
	defer e.mu.Unlock()
	at := e.now()
	if err != nil {
		e.st.Heartbeat.LastError = err.Error()
		e.logf("early-warning: heartbeat failed: %v", err)
		return
	}
	e.st.Heartbeat.LastPingAt, e.st.Heartbeat.LastError, e.st.Heartbeat.LastSkipped = &at, "", ""
	e.st.Heartbeat.Pings++
}

func ping(ctx context.Context, url string) error {
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("heartbeat: invalid url %s", MaskURL(url))
	}
	req.Header.Set("User-Agent", "sqlon-early-warning")
	resp, err := heartbeatClient.Do(req)
	if err != nil {
		return fmt.Errorf("heartbeat: %w", redactURL(err))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("heartbeat returned %s", resp.Status)
	}
	return nil
}
