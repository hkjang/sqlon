package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/engine"
)

type Service struct {
	Queryer   SystemQueryer
	Providers *Registry
	Engines   *engine.Registry
	Now       func() time.Time
}

func New(queryer SystemQueryer) *Service {
	return &Service{Queryer: queryer, Providers: NewRegistry(), Engines: engine.NewDefaultRegistry(), Now: time.Now}
}

func (s *Service) Sessions(ctx context.Context, raw dbconn.Profile) Response[SessionData] {
	p := dbconn.ApplyDefaults(raw)
	now := s.now()
	data := SessionData{ProfileID: p.ID, Engine: p.Type, Sessions: []Session{}}
	response := Response[SessionData]{Status: "ok", Data: data, Evidence: []Evidence{}, Warnings: []string{},
		Limitations: []string{"SQL 본문과 bind 값은 민감정보 노출을 막기 위해 세션 목록에 포함하지 않습니다."}, CollectedAt: now, TraceID: observationTraceID()}
	provider, reason := s.provider(p.Type, "sessions")
	if provider == nil {
		response.Status = "unsupported"
		response.Limitations = append(response.Limitations, reason)
		response.Evidence = append(response.Evidence, Evidence{Code: "SESSIONS_UNSUPPORTED", Severity: "warning", Summary: reason, CollectedAt: now})
		return response
	}
	items, err := provider.Sessions(ctx, s.Queryer, p)
	response.CollectedAt = s.now()
	if err != nil {
		return sessionError(response, err)
	}
	response.Data.Sessions = items
	response.Data.Total = len(items)
	if len(items) >= SnapshotRowLimit {
		response.Status = "warning"
		response.Limitations = append(response.Limitations, "세션 결과가 안전 상한 10,000행에 도달하여 일부 세션이 생략되었을 수 있습니다.")
		response.Evidence = append(response.Evidence, Evidence{Code: "SESSION_SNAPSHOT_TRUNCATED", Severity: "warning", Summary: "세션 스냅숏이 안전 상한에 도달함", Attributes: map[string]any{"row_limit": SnapshotRowLimit}, CollectedAt: response.CollectedAt})
	}
	for _, item := range items {
		state := strings.ToLower(item.State)
		if state == "active" || state == "query" {
			response.Data.Active++
		}
		if item.WaitEvent != "" && !strings.Contains(strings.ToLower(item.WaitClass), "idle") && !strings.Contains(strings.ToLower(item.WaitEvent), "client") {
			response.Data.Waiting++
		}
		if item.DurationSeconds >= 300 {
			response.Data.LongRunning++
		}
	}
	severity := "info"
	if response.Data.LongRunning > 0 || response.Data.Waiting > 0 {
		response.Status = "warning"
		severity = "warning"
	}
	response.Evidence = append(response.Evidence, Evidence{Code: "SESSION_SNAPSHOT_COLLECTED", Severity: severity,
		Summary: "세션 스냅숏 수집 완료", Attributes: map[string]any{"total": response.Data.Total, "active": response.Data.Active,
			"waiting": response.Data.Waiting, "long_running": response.Data.LongRunning, "long_running_threshold_seconds": 300}, CollectedAt: response.CollectedAt})
	return response
}

func (s *Service) Locks(ctx context.Context, raw dbconn.Profile) Response[LockData] {
	p := dbconn.ApplyDefaults(raw)
	now := s.now()
	data := LockData{ProfileID: p.ID, Engine: p.Type, Edges: []LockEdge{}, Roots: []LockRoot{}}
	response := Response[LockData]{Status: "ok", Data: data, Evidence: []Evidence{}, Warnings: []string{}, Limitations: []string{}, CollectedAt: now, TraceID: observationTraceID()}
	provider, reason := s.provider(p.Type, "locks")
	if provider == nil {
		response.Status = "unsupported"
		response.Limitations = append(response.Limitations, reason)
		response.Evidence = append(response.Evidence, Evidence{Code: "LOCKS_UNSUPPORTED", Severity: "warning", Summary: reason, CollectedAt: now})
		return response
	}
	edges, err := provider.Locks(ctx, s.Queryer, p)
	response.CollectedAt = s.now()
	if err != nil {
		return lockError(response, err)
	}
	response.Data.Edges = edges
	if len(edges) >= SnapshotRowLimit {
		response.Status = "warning"
		response.Limitations = append(response.Limitations, "잠금 결과가 안전 상한 10,000행에 도달하여 블로킹 그래프가 불완전할 수 있습니다.")
		response.Evidence = append(response.Evidence, Evidence{Code: "LOCK_SNAPSHOT_TRUNCATED", Severity: "warning", Summary: "잠금 스냅숏이 안전 상한에 도달함", Attributes: map[string]any{"row_limit": SnapshotRowLimit}, CollectedAt: response.CollectedAt})
	}
	response.Data.BlockedSessions = distinctBlocked(edges)
	response.Data.Roots = lockRoots(edges)
	severity := "info"
	code := "NO_LOCK_CONTENTION"
	summary := "현재 블로킹 관계 없음"
	if len(edges) > 0 {
		response.Status = "critical"
		severity, code, summary = "critical", "LOCK_CONTENTION_DETECTED", "블로킹 세션 관계 감지"
	}
	response.Evidence = append(response.Evidence, Evidence{Code: code, Severity: severity, Summary: summary,
		Attributes: map[string]any{"edges": len(edges), "blocked_sessions": response.Data.BlockedSessions, "root_blockers": len(response.Data.Roots)}, CollectedAt: response.CollectedAt})
	return response
}

func (s *Service) provider(engineName, capability string) (Provider, string) {
	adapter, ok := s.Engines.Get(engineName)
	if !ok {
		return nil, "등록되지 않은 데이터베이스 엔진입니다."
	}
	supported := adapter.Capabilities.Sessions
	if capability == "locks" {
		supported = adapter.Capabilities.LockTree
	}
	if !supported {
		return nil, capability + " 기능을 엔진 Capability가 지원하지 않습니다."
	}
	provider, ok := s.Providers.Get(engineName)
	if !ok {
		return nil, capability + " Provider가 이 배포판에 등록되지 않았습니다."
	}
	return provider, ""
}

func sessionError(response Response[SessionData], err error) Response[SessionData] {
	status, code, hint := classifyCollectionError(err)
	response.Status = status
	response.Warnings = append(response.Warnings, hint)
	response.Evidence = append(response.Evidence, Evidence{Code: code, Severity: "warning", Summary: hint, CollectedAt: response.CollectedAt})
	return response
}

func lockError(response Response[LockData], err error) Response[LockData] {
	status, code, hint := classifyCollectionError(err)
	response.Status = status
	response.Warnings = append(response.Warnings, hint)
	response.Evidence = append(response.Evidence, Evidence{Code: code, Severity: "warning", Summary: hint, CollectedAt: response.CollectedAt})
	return response
}

func classifyCollectionError(err error) (string, string, string) {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "license policy") || strings.Contains(message, "oracle license") {
		return "policy_blocked", "LICENSE_POLICY_BLOCKED", "라이선스 정책이 이 시스템 뷰 수집을 차단했습니다."
	}
	permissionTokens := []string{"permission denied", "access denied", "insufficient privilege", "ora-00942", "ora-01031", "command denied"}
	for _, token := range permissionTokens {
		if strings.Contains(message, token) {
			return "permission_denied", "COLLECTION_PERMISSION_DENIED", "시스템 뷰 조회 권한이 부족합니다. 모니터링 계정의 최소 조회 권한을 확인하세요."
		}
	}
	if strings.Contains(message, "driver") && (strings.Contains(message, "not included") || strings.Contains(message, "not compiled")) {
		return "unsupported_edition", "DRIVER_UNAVAILABLE", "현재 SQLON 배포판에 대상 엔진 드라이버가 없습니다."
	}
	return "error", "COLLECTION_FAILED", "세션·잠금 시스템 뷰 수집에 실패했습니다: " + err.Error()
}

func distinctBlocked(edges []LockEdge) int {
	seen := map[string]struct{}{}
	for _, edge := range edges {
		if edge.BlockedKey != "" {
			seen[edge.BlockedKey] = struct{}{}
		}
	}
	return len(seen)
}

func lockRoots(edges []LockEdge) []LockRoot {
	children := map[string][]string{}
	blocked := map[string]struct{}{}
	users := map[string]string{}
	for _, edge := range edges {
		if edge.BlockerKey == "" || edge.BlockedKey == "" {
			continue
		}
		children[edge.BlockerKey] = append(children[edge.BlockerKey], edge.BlockedKey)
		blocked[edge.BlockedKey] = struct{}{}
		if edge.BlockerUser != "" {
			users[edge.BlockerKey] = edge.BlockerUser
		}
	}
	roots := make([]LockRoot, 0)
	for key := range children {
		if _, isBlocked := blocked[key]; isBlocked {
			continue
		}
		roots = append(roots, LockRoot{SessionKey: key, User: users[key], AffectedSessions: descendantCount(key, children)})
	}
	// Defensive fallback for a malformed/cyclic graph: preserve evidence rather
	// than falsely reporting that no root blocker exists.
	if len(roots) == 0 && len(children) > 0 {
		for key := range children {
			roots = append(roots, LockRoot{SessionKey: key, User: users[key], AffectedSessions: descendantCount(key, children)})
		}
	}
	sort.Slice(roots, func(i, j int) bool {
		if roots[i].AffectedSessions != roots[j].AffectedSessions {
			return roots[i].AffectedSessions > roots[j].AffectedSessions
		}
		return roots[i].SessionKey < roots[j].SessionKey
	})
	return roots
}

func descendantCount(root string, children map[string][]string) int {
	seen := map[string]struct{}{root: {}}
	var visit func(string)
	visit = func(key string) {
		for _, child := range children[key] {
			if _, ok := seen[child]; ok {
				continue
			}
			seen[child] = struct{}{}
			visit(child)
		}
	}
	visit(root)
	return len(seen) - 1
}

func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

func observationTraceID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("observe-%d", time.Now().UnixNano())
	}
	return "observe-" + hex.EncodeToString(b[:])
}
