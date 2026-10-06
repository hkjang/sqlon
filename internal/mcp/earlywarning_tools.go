package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/earlywarning"
)

// The early-warning MCP surface is designed for an agent to run the whole
// loop, not just read a list: see what is wrong and what to do first
// (get_early_warnings → next_actions), understand one alert and its cause
// chain (explain_early_warning), size the fix (plan_capacity), act —
// acknowledge, silence planned work, propose an approval-gated fix — and
// confirm (run_early_warning_check). Administrators also manage the system
// itself: channels, thresholds, per-database routing.
//
// Every tool here touches database-profile state, so in standalone HTTP mode
// they all require the master token (earlyWarningToolNames in
// authorizeDBProfileTool); in meta mode each call is scoped to the caller's
// permitted profiles.

var earlyWarningToolNames = map[string]bool{
	"get_early_warnings": true, "explain_early_warning": true, "plan_capacity": true,
	"acknowledge_early_warning": true, "manage_early_warning_silences": true, "report_host_disk": true,
	"configure_early_warning": true, "configure_profile_alerting": true, "run_early_warning_check": true,
	"test_alert_channel": true, "propose_early_warning_fix": true,
}

// propose_early_warning_fix writes change plans, so it sits behind the same
// dba/admin gate as the rest of the change workflow.
func init() { dbaTools["propose_early_warning_fix"] = true }

func earlyWarningToolDefs() []map[string]any {
	settings := map[string]any{"type": "object", "description": "변경할 항목만: webhook_ref(env:NAME|file:PATH|plain:URL), console_url, min_notify_severity(info|warning|critical), renotify_interval(6h|off), digest_at(HH:MM|off), maintenance_interval(5m), schema_interval(15m|off), host_disk_stale_after(15m), heartbeat_ref(외부 감시 핑 URL — SQLON 자신이 멈추면 감시 서비스가 알림), escalation_ref(당직 호출 채널), escalate_after(30m|0=즉시|off — critical 이 이 시간 동안 ack 안 되면 당직 호출), chat_actions(mattermost|off — 알림에 확인·2시간 무음·수정안 버튼), action_url(채팅 서버가 버튼 콜백을 보낼 SQLON 주소, 비우면 console_url 의 origin)"}
	return []map[string]any{
		tool("explain_early_warning", "예방 경보 하나를 깊게 봅니다: 같은 DB에서 함께 발생 중인 경보와 원인 사슬(예: WAL 아카이브 실패 → pg_wal 과다 → 고갈 예측), 같은 경보의 과거 발생 이력, 저장공간 경보면 예측(6시간·7일 추세)과 최근 시계열, 무음·흔들림 여부, 그리고 다음에 쓸 도구(next_actions)와 자동 수정안 존재 여부. 원인 경보부터 고치는 것이 전략입니다.", objectSchema(map[string]any{
			"alert_id": str("get_early_warnings 의 firing[].id"),
		}, []string{"alert_id"})),
		tool("plan_capacity", "용량 계획(what-if): DB의 각 저장 자산(DB 점유량·디스크 볼륨)에 대해 현재 추세로 target_days 일을 버티려면 필요한 크기, 부족분, 경고 임계 도달·가득 차는 날짜, 추세 1배·2배·3배 시나리오를 계산합니다. 증설 여부와 시점을 결정할 때 씁니다. 읽기 전용.", objectSchema(map[string]any{
			"profile":     str("DB 프로파일 ID"),
			"target_days": integer("버텨야 할 기간(일, 기본 90)"),
		}, []string{"profile"})),
		tool("acknowledge_early_warning", "경보를 확인(ack)합니다. 지속 경보는 해소되거나 더 심각해질 때까지 재알림을 멈추고, 스키마 변경 같은 이벤트는 종료됩니다. 감사 로그에 남습니다. 원인을 고치지 않은 채 ack 만 하면 위험이 그대로이므로 note 에 조치 계획을 적으세요.", objectSchema(map[string]any{
			"alert_id": str("경보 ID"),
			"note":     str("확인 메모 (조치 계획·티켓 번호)"),
		}, []string{"alert_id"})),
		tool("manage_early_warning_silences", "무음(silence) 관리: 계획 작업(볼륨 증설·마이그레이션·페일오버 훈련) 동안 DB·규칙 단위로 알림만 멈춥니다. 경보는 계속 표시되고, 무음이 끝날 때 여전히 유효한 것은 그때 전송됩니다. action=list|create|end. create 는 duration(최대 168h)과 reason 필수, rule 은 capacity_* 같은 접두어 가능. profile 없이 만드는 전체 무음은 관리자만.", objectSchema(map[string]any{
			"action":     str("list | create | end"),
			"profile":    str("대상 DB 프로파일 ID (create; 비우면 전체 — 관리자)"),
			"rule":       str("규칙 (비우면 전체, 예: capacity_*, maint_wal_archive)"),
			"duration":   str("기간 (예: 30m, 2h, 24h)"),
			"reason":     str("사유 (예: CHG-1042 볼륨 증설)"),
			"silence_id": str("end 할 무음 ID"),
		}, []string{"action"})),
		tool("report_host_disk", "DB 서버의 디스크 사용량(df)을 보고합니다. SQL로는 보이지 않는 DB 밖 파일(덤프 백업·외부 로그)과 볼륨의 실제 크기로 고갈을 예측하게 됩니다. 보통은 DB 서버의 sqlon-disk-report.sh 가 1분마다 보냅니다 — 이 도구는 셸이 있는 에이전트나 수동 보고용입니다.", objectSchema(map[string]any{
			"profile":  str("DB 프로파일 ID"),
			"profiles": arrayOf("string", "같은 서버의 여러 DB에 같은 보고를 붙일 때"),
			"host":     str("호스트 이름"),
			"volumes":  arrayOfObjects("[{mount, filesystem, total_bytes, used_bytes, avail_bytes}] — df -Pk 값×1024"),
		}, []string{"volumes"})),
		tool("configure_early_warning", "관리자: 예방 경보 서버 설정을 재시작 없이 조회·변경·초기화합니다 — 기본 알림 채널(webhook_ref, 비밀은 가려짐), 콘솔 링크, 최소 알림 위험도, 재알림 간격, 일일 리포트 시각, 예방 점검·스키마 감시 주기, 디스크 보고 중단 판정 시간, SQLON 생존 신호(heartbeat_ref), 당직 호출(escalation_ref·escalate_after), Mattermost 버튼(chat_actions·action_url). action=get|set|reset. 변경은 감사 로그에 남고 settings.json 에 저장됩니다.", objectSchema(map[string]any{
			"action":   str("get | set | reset"),
			"settings": settings,
			"fields":   arrayOf("string", "reset 할 항목 이름 (all = 전체)"),
		}, []string{"action"})),
		tool("configure_profile_alerting", "관리자: DB 하나의 예방 경보 설정을 바꿉니다 — capacity(storage_limit 볼륨 크기 예: 500GiB, warn/critical_percent, warn/critical_days)와 alerting(webhook_ref 팀 채널 env:/file:/plain:, escalation_ref 이 DB의 당직 호출 채널, min_severity). 지정한 묶음만 바꾸고 clear 로 묶음을 지웁니다. 프로파일의 다른 설정은 그대로입니다.", objectSchema(map[string]any{
			"profile":  str("DB 프로파일 ID"),
			"capacity": map[string]any{"type": "object", "description": "{storage_limit, warn_percent, critical_percent, warn_days, critical_days}"},
			"alerting": map[string]any{"type": "object", "description": "{webhook_ref, escalation_ref, min_severity}"},
			"clear":    arrayOf("string", "capacity | alerting"),
		}, []string{"profile"})),
		tool("run_early_warning_check", "관리자: 모든 DB를 지금 수집하고 예방 점검·스키마 점검까지 주기와 무관하게 실행해 경보를 갱신합니다. 이번에 새로 생긴 경보와 해소된 경보를 돌려줍니다 — 조치 후 효과를 확인할 때 씁니다. 알림도 평소처럼 나갑니다.", objectSchema(map[string]any{}, nil)),
		tool("test_alert_channel", "관리자: 알림 채널로 테스트 메시지를 보냅니다. profile 을 주면 그 DB의 전용 채널(alerting.webhook_ref)을, 없으면 기본 채널을 시험합니다. channel=escalation 이면 당직 호출 채널을 시험합니다.", objectSchema(map[string]any{
			"profile": str("DB별 채널을 시험할 프로파일 ID (선택)"),
			"channel": str("team(기본) | escalation"),
		}, nil)),
		tool("propose_early_warning_fix", "DBA: 경보를 고치는 변경계획 초안(draft)을 만듭니다 — 버려진 복제 슬롯 제거, VACUUM 을 막는 세션 종료·prepared 트랜잭션 롤백, 블로트 VACUUM, max_slot_wal_keep_size 상한 설정, autovacuum 켜기, 모니터링 계정에 pg_monitor 부여. 실행하지 않으며 submit_change → approve_change → execute_approved_change 승인 게이트를 거쳐야 합니다. 검증 단계는 조치가 실제로 적용되지 않으면 실패합니다. 자동 수정이 없는 경보(아카이브 대상 장애·용량)는 무엇을 해야 하는지 안내합니다.", objectSchema(map[string]any{
			"alert_id": str("경보 ID"),
			"value_mb": integer("max_slot_wal_keep_size 상한(MB) — 생략 시 볼륨의 20%"),
		}, []string{"alert_id"})),
	}
}

func toolActorName(ctx context.Context) string {
	if u := userFrom(ctx); u != nil {
		return u.Username
	}
	return "mcp-admin"
}

func ewError(status, msg string) map[string]any {
	return map[string]any{"status": status, "error": msg}
}

// nextAction is a concrete, machine-actionable suggestion.
type nextAction struct {
	Priority  int            `json:"priority"`
	AlertID   string         `json:"alert_id,omitempty"`
	ProfileID string         `json:"profile_id,omitempty"`
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
	Why       string         `json:"why"`
}

var fixableRules = map[string]bool{"maint_replication_slot": true, "maint_vacuum_blocker": true, "maint_bloat": true, "maint_config_risk": true, earlywarning.RuleMonitorPrivilege: true}

// nextActions ranks what to do: causes before symptoms, critical before
// warning, then a fix or a plan per alert.
func nextActions(board earlywarning.Board) []nextAction {
	roots, symptoms := map[string]bool{}, map[string]string{}
	for _, inc := range board.Incidents {
		for _, id := range inc.RootIDs {
			roots[id] = true
		}
		for _, id := range inc.AlertIDs {
			if !roots[id] {
				symptoms[id] = inc.Summary
			}
		}
	}
	var out []nextAction
	for _, a := range board.Firing {
		if a.Severity == earlywarning.SevInfo && a.Rule != earlywarning.RuleMonitorPrivilege {
			continue
		}
		prio := map[string]int{earlywarning.SevCritical: 10, earlywarning.SevWarning: 20, earlywarning.SevInfo: 40}[a.Severity]
		switch {
		case roots[a.ID]:
			prio -= 5
		case symptoms[a.ID] != "":
			prio += 3
		}
		args := map[string]any{"alert_id": a.ID}
		switch {
		case a.AckedAt != nil || a.SilencedUntil != nil:
			continue
		case symptoms[a.ID] != "":
			out = append(out, nextAction{prio, a.ID, a.ProfileID, "explain_early_warning", args, "증상 경보입니다 — " + symptoms[a.ID] + ". 원인 경보를 먼저 처리하세요."})
		case fixableRules[a.Rule]:
			out = append(out, nextAction{prio, a.ID, a.ProfileID, "propose_early_warning_fix", args, a.Title + " — 승인 게이트를 거치는 수정 변경계획 초안을 만들 수 있습니다."})
		case a.Rule == earlywarning.RuleCapacityForecast || a.Rule == earlywarning.RuleCapacityUsage:
			out = append(out, nextAction{prio, a.ID, a.ProfileID, "plan_capacity", map[string]any{"profile": a.ProfileID, "target_days": 90}, a.Title + " — 원인 경보가 없다면 증설 크기와 시점을 계산하세요."})
		case a.Rule == earlywarning.RuleSchemaChange:
			out = append(out, nextAction{prio, a.ID, a.ProfileID, "acknowledge_early_warning", args, a.Title + " — 의도된 배포인지 확인한 뒤 ack 하세요."})
		default:
			out = append(out, nextAction{prio, a.ID, a.ProfileID, "explain_early_warning", args, a.Title})
		}
	}
	for _, p := range board.Profiles {
		limited := false
		for _, f := range p.Forecasts {
			limited = limited || f.LimitBytes > 0
		}
		if !limited && len(p.Forecasts) > 0 {
			out = append(out, nextAction{50, "", p.ProfileID, "configure_profile_alerting", map[string]any{"profile": p.ProfileID, "capacity": map[string]any{"storage_limit": "<볼륨 크기, 예: 500GiB>"}}, p.ProfileID + " 은 용량 한도가 없어 고갈 시점을 계산하지 못합니다."})
		}
	}
	if !board.Delivery.Configured && len(board.Channels) == 0 {
		out = append(out, nextAction{60, "", "", "configure_early_warning", map[string]any{"action": "set", "settings": map[string]any{"webhook_ref": "env:SQLON_ALERT_WEBHOOK"}}, "알림 채널이 없어 경보가 콘솔에만 표시됩니다."})
	}
	if out == nil {
		out = []nextAction{} // agents iterate this; never null
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	if len(out) > 15 {
		out = out[:15]
	}
	return out
}

// earlyWarningTool dispatches the early-warning tools other than
// get_early_warnings. Admin/DBA gates are applied by callTool.
func (s *Server) earlyWarningTool(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	if s.EarlyWarning == nil {
		return s.earlyWarningDisabled(), nil
	}
	profiles, err := s.usableProfiles(ctx)
	if err != nil {
		return nil, err
	}
	return s.earlyWarningToolScoped(ctx, profiles, name, raw)
}

// earlyWarningToolScoped runs a tool against an already permission-filtered
// profile list (shared by MCP and REST).
func (s *Server) earlyWarningToolScoped(ctx context.Context, profiles []dbconn.Profile, name string, raw json.RawMessage) (any, error) {
	if s.EarlyWarning == nil {
		return s.earlyWarningDisabled(), nil
	}
	permitted := map[string]bool{}
	for _, p := range profiles {
		permitted[p.ID] = true
	}
	alertFor := func(id string) (earlywarning.Alert, map[string]any) {
		a, ok := s.EarlyWarning.Alert(strings.TrimSpace(id))
		if !ok || !permitted[a.ProfileID] {
			return a, ewError("not_found", "alert not found or not permitted")
		}
		return a, nil
	}
	audit := func(action, profile string, fields map[string]any) {
		entry := map[string]any{"ts": time.Now().Format(time.RFC3339Nano), "tool": "mcp:" + action, "detail": profile, "actor": toolActorName(ctx)}
		for k, v := range fields {
			entry[k] = v
		}
		s.appendAudit(entry)
	}

	switch name {
	case "explain_early_warning":
		var a struct {
			AlertID string `json:"alert_id"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		alert, denied := alertFor(a.AlertID)
		if denied != nil {
			return denied, nil
		}
		detail, _ := s.EarlyWarning.Explain(alert.ID)
		p, _ := allowedProfile(profiles, alert.ProfileID)
		board := s.EarlyWarning.Board([]dbconn.Profile{p})
		actions := []nextAction{}
		for _, act := range nextActions(board) {
			if act.AlertID == alert.ID || act.AlertID == "" {
				actions = append(actions, act)
			}
		}
		if detail.Incident != nil {
			for _, rid := range detail.Incident.RootIDs {
				if rid != alert.ID && fixableRules[ruleOf(board, rid)] {
					actions = append(actions, nextAction{1, rid, alert.ProfileID, "propose_early_warning_fix", map[string]any{"alert_id": rid}, "원인 경보를 고치면 이 경보도 함께 해소될 가능성이 큽니다."})
				}
			}
		}
		return map[string]any{"status": "ok", "detail": detail, "fix_available": fixableRules[alert.Rule], "next_actions": actions}, nil

	case "plan_capacity":
		var a struct {
			Profile    string  `json:"profile"`
			TargetDays float64 `json:"target_days"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		p, ok := allowedProfile(profiles, a.Profile)
		if !ok {
			return ewError("not_found", "db profile not found or not permitted"), nil
		}
		return map[string]any{"status": "ok", "plan": s.EarlyWarning.PlanCapacity(dbconn.ApplyDefaults(p), a.TargetDays)}, nil

	case "acknowledge_early_warning":
		var a struct {
			AlertID string `json:"alert_id"`
			Note    string `json:"note"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		alert, denied := alertFor(a.AlertID)
		if denied != nil {
			return denied, nil
		}
		acked, err := s.EarlyWarning.Ack(alert.ID, toolActorName(ctx), a.Note)
		if err != nil {
			return ewError("conflict", err.Error()), nil
		}
		audit("early_warning_ack", acked.ProfileID, map[string]any{"alert": acked.ID, "rule": acked.Rule, "note": a.Note})
		return map[string]any{"status": "ok", "alert": acked}, nil

	case "manage_early_warning_silences":
		var a struct {
			Action, Profile, Rule, Duration, Reason string
			SilenceID                               string `json:"silence_id"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		switch strings.ToLower(a.Action) {
		case "list":
			return map[string]any{"status": "ok", "silences": s.EarlyWarning.Board(profiles).Silences}, nil
		case "create":
			if a.Profile == "" && !s.toolActorIsAdmin(ctx) {
				return ewError("forbidden", "silencing every database requires admin privileges; give a profile"), nil
			}
			if a.Profile != "" && !permitted[a.Profile] {
				return ewError("not_found", "db profile not found or not permitted"), nil
			}
			d, err := time.ParseDuration(strings.TrimSpace(a.Duration))
			if err != nil {
				return ewError("invalid", "duration must be like 30m, 2h, 24h"), nil
			}
			silence, err := s.EarlyWarning.AddSilence(earlywarning.Silence{ProfileID: a.Profile, Rule: a.Rule, Reason: a.Reason, CreatedBy: toolActorName(ctx)}, d)
			if err != nil {
				return ewError("invalid", err.Error()), nil
			}
			audit("early_warning_silence", a.Profile, map[string]any{"silence": silence.ID, "rule": a.Rule, "until": silence.EndsAt, "reason": a.Reason})
			return map[string]any{"status": "ok", "silence": silence}, nil
		case "end":
			current, found := s.EarlyWarning.Silence(a.SilenceID)
			if !found || (current.ProfileID != "" && !permitted[current.ProfileID]) {
				return ewError("not_found", "silence not found or already ended"), nil
			}
			if current.ProfileID == "" && !s.toolActorIsAdmin(ctx) {
				return ewError("forbidden", "ending a silence on every database requires admin privileges"), nil
			}
			ended, err := s.EarlyWarning.EndSilence(a.SilenceID)
			if err != nil {
				return ewError("not_found", err.Error()), nil
			}
			audit("early_warning_silence_end", ended.ProfileID, map[string]any{"silence": ended.ID})
			return map[string]any{"status": "ok", "silence": ended, "notice": "보류된 알림은 다음 평가 주기에 전송됩니다."}, nil
		}
		return ewError("invalid", "action must be list, create, or end"), nil

	case "report_host_disk":
		var a struct {
			Profile  string                    `json:"profile"`
			Profiles []string                  `json:"profiles"`
			Host     string                    `json:"host"`
			Volumes  []earlywarning.DiskVolume `json:"volumes"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		ids := append([]string{}, a.Profiles...)
		if a.Profile != "" {
			ids = append(ids, a.Profile)
		}
		if len(ids) == 0 {
			return ewError("invalid", "profile is required"), nil
		}
		for _, id := range ids {
			if !permitted[id] {
				return ewError("not_found", "db profile not found or not permitted: "+id), nil
			}
		}
		for _, id := range ids {
			if err := s.EarlyWarning.ReportDisk(ctx, id, earlywarning.HostDiskReport{Host: a.Host, Volumes: a.Volumes}); err != nil {
				return ewError("invalid", err.Error()), nil
			}
		}
		return map[string]any{"status": "ok", "profiles": ids, "volumes": len(a.Volumes), "notice": "다음 평가 주기에 디스크 경보가 갱신됩니다 (즉시: run_early_warning_check)."}, nil

	case "configure_early_warning":
		var a struct {
			Action   string                `json:"action"`
			Settings earlywarning.Settings `json:"settings"`
			Fields   []string              `json:"fields"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		switch strings.ToLower(a.Action) {
		case "get":
			return map[string]any{"status": "ok", "settings": s.EarlyWarning.Settings()}, nil
		case "set", "reset":
			if strings.EqualFold(a.Action, "set") {
				a.Fields = nil
			} else {
				a.Settings = earlywarning.Settings{}
				if len(a.Fields) == 0 {
					return ewError("invalid", "reset needs fields (or [\"all\"])"), nil
				}
			}
			view, err := s.EarlyWarning.UpdateSettings(a.Settings, a.Fields, toolActorName(ctx))
			if err != nil {
				return ewError("invalid", err.Error()), nil
			}
			audit("early_warning_settings", "", map[string]any{"action": a.Action, "fields": a.Fields, "changed": settingsChanged(a.Settings)})
			return map[string]any{"status": "ok", "settings": view}, nil
		}
		return ewError("invalid", "action must be get, set, or reset"), nil

	case "configure_profile_alerting":
		var a struct {
			Profile  string                 `json:"profile"`
			Capacity *dbconn.CapacityConfig `json:"capacity"`
			Alerting *dbconn.AlertingConfig `json:"alerting"`
			Clear    []string               `json:"clear"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		updated, err := s.updateProfileEarlyWarning(ctx, a.Profile, a.Capacity, a.Alerting, a.Clear)
		if err != nil {
			return ewError("invalid", err.Error()), nil
		}
		audit("early_warning_profile_config", a.Profile, map[string]any{"capacity": a.Capacity != nil, "alerting": a.Alerting != nil, "clear": a.Clear})
		masked := updated.Masked()
		return map[string]any{"status": "ok", "profile": a.Profile, "capacity": masked["capacity"], "alerting": masked["alerting"], "notice": "다음 평가 주기부터 적용됩니다 (즉시: run_early_warning_check)."}, nil

	case "run_early_warning_check":
		before := map[string]bool{}
		for _, al := range s.EarlyWarning.Board(profiles).Firing {
			before[al.ID] = true
		}
		runCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
		defer cancel()
		batch := s.Collector.CollectAll(runCtx, nil, true)
		report := s.evaluateEarlyWarning(runCtx, batch, true)
		board := s.EarlyWarning.Board(profiles)
		opened := []earlywarning.Alert{}
		after := map[string]bool{}
		for _, al := range board.Firing {
			after[al.ID] = true
			if !before[al.ID] {
				opened = append(opened, al)
			}
		}
		cleared := []string{}
		for id := range before {
			if !after[id] {
				cleared = append(cleared, id)
			}
		}
		audit("early_warning_evaluate", "", map[string]any{"firing": report.Firing, "opened": len(opened), "resolved": len(cleared)})
		return map[string]any{"status": "ok", "report": report, "collected": batch.Succeeded, "collection_failed": batch.Failed,
			"opened": opened, "resolved_ids": cleared, "headline": board.Headline, "next_actions": nextActions(board)}, nil

	case "test_alert_channel":
		var a struct {
			Profile string `json:"profile"`
			Channel string `json:"channel"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		var prof *dbconn.Profile
		if a.Profile != "" {
			p, ok := allowedProfile(profiles, a.Profile)
			if !ok {
				return ewError("not_found", "db profile not found or not permitted"), nil
			}
			prof = &p
		}
		var notifier earlywarning.Notifier
		scope, title := "기본 채널", "#### 🛡️ SQLON 예방 경보 — 알림 경로 테스트\n이 메시지가 보이면 %s의 예방 경보가 이 채널로 전달됩니다."
		switch strings.ToLower(a.Channel) {
		case "", "team":
			notifier = s.EarlyWarning.DefaultNotifier()
			if prof != nil && s.EarlyWarning.Route != nil {
				custom, err := s.EarlyWarning.Route(*prof)
				if err != nil {
					return ewError("invalid", err.Error()), nil
				}
				if custom != nil {
					notifier, scope = custom, prof.ID+" 전용 채널"
				}
			}
		case "escalation":
			n, err := s.EarlyWarning.EscalationNotifier(prof)
			if err != nil {
				return ewError("invalid", err.Error()), nil
			}
			notifier, scope, title = n, "당직 호출 채널", "#### 📟 SQLON 당직 호출 — 경로 테스트\n이 메시지가 보이면 확인되지 않은 critical 경보가 %s로 호출됩니다."
			if prof != nil {
				scope = prof.ID + "의 당직 호출 채널"
			}
		default:
			return ewError("invalid", "channel must be team or escalation"), nil
		}
		if notifier == nil {
			if strings.EqualFold(a.Channel, "escalation") {
				return ewError("not_configured", "no escalation channel; set one with configure_early_warning (escalation_ref)"), nil
			}
			return ewError("not_configured", "no notification channel; set one with configure_early_warning (webhook_ref)"), nil
		}
		tctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		err := notifier.NotifyText(tctx, "test", fmt.Sprintf(title, scope))
		audit("early_warning_test_notification", a.Profile, map[string]any{"delivered": err == nil, "target": notifier.Target(), "channel": a.Channel})
		if err != nil {
			return map[string]any{"status": "error", "delivered": false, "target": notifier.Target(), "error": err.Error()}, nil
		}
		return map[string]any{"status": "ok", "delivered": true, "target": notifier.Target()}, nil

	case "propose_early_warning_fix":
		var a struct {
			AlertID string `json:"alert_id"`
		}
		var args map[string]any
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &args)
		alert, denied := alertFor(a.AlertID)
		if denied != nil {
			return denied, nil
		}
		if alert.State != earlywarning.StateFiring {
			return ewError("conflict", "alert is already resolved"), nil
		}
		p, _ := allowedProfile(profiles, alert.ProfileID)
		proposal, err := s.proposeFix(ctx, alert, dbconn.ApplyDefaults(p), args)
		if err != nil {
			return ewError("error", err.Error()), nil
		}
		if proposal.Available && proposal.Plan != nil {
			created, reused, err := s.saveFixDraft(alert.ID, *proposal.Plan)
			if err != nil {
				return ewError("error", err.Error()), nil
			}
			proposal.Plan = &created
			if reused {
				proposal.Guidance = "이 경보의 변경계획 " + created.ID + " (" + string(created.State) + ")이 이미 진행 중입니다 — 새로 만들지 않았습니다. " + proposal.Guidance
				return map[string]any{"status": "ok", "alert_id": alert.ID, "proposal": proposal, "existing": true}, nil
			}
			audit("early_warning_fix_proposed", alert.ProfileID, map[string]any{"alert": alert.ID, "plan": created.ID, "kind": proposal.Kind})
		}
		return map[string]any{"status": "ok", "alert_id": alert.ID, "proposal": proposal}, nil
	}
	return nil, fmt.Errorf("unknown early-warning tool %q", name)
}

func ruleOf(board earlywarning.Board, id string) string {
	for _, a := range board.Firing {
		if a.ID == id {
			return a.Rule
		}
	}
	return ""
}

// settingsChanged names the fields a patch sets (never their values: the
// webhook reference may be a secret).
func settingsChanged(s earlywarning.Settings) []string {
	var out []string
	b, _ := json.Marshal(s)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// updateProfileEarlyWarning changes only a profile's capacity/alerting
// blocks, in whichever store holds profiles.
func (s *Server) updateProfileEarlyWarning(ctx context.Context, id string, capacity *dbconn.CapacityConfig, alerting *dbconn.AlertingConfig, clear []string) (dbconn.Profile, error) {
	apply := func(p *dbconn.Profile) error {
		existing := *p
		for _, c := range clear {
			switch c {
			case "capacity":
				p.Capacity = nil
			case "alerting":
				p.Alerting = nil
			default:
				return fmt.Errorf("clear accepts capacity or alerting, not %q", c)
			}
		}
		if capacity != nil {
			p.Capacity = capacity
		}
		if alerting != nil {
			p.Alerting = alerting
			if err := dbconn.PreserveMaskedSecrets(p, &existing); err != nil {
				return err
			}
		}
		return p.Validate()
	}
	if !s.authEnabled() {
		profiles, err := dbconn.LoadProfiles(s.opDir())
		if err != nil {
			return dbconn.Profile{}, err
		}
		for i := range profiles {
			if profiles[i].ID != id {
				continue
			}
			if err := apply(&profiles[i]); err != nil {
				return dbconn.Profile{}, err
			}
			if err := s.saveProfiles(profiles); err != nil {
				return dbconn.Profile{}, err
			}
			s.DB.Invalidate(id)
			return profiles[i], nil
		}
		return dbconn.Profile{}, fmt.Errorf("db profile not found: %s", id)
	}
	rec, err := s.Meta.Store.GetProfile(ctx, id)
	if err != nil {
		return dbconn.Profile{}, err
	}
	var p dbconn.Profile
	if err := json.Unmarshal(rec.Definition, &p); err != nil {
		return dbconn.Profile{}, err
	}
	if err := apply(&p); err != nil {
		return dbconn.Profile{}, err
	}
	if rec.Definition, err = json.Marshal(p); err != nil {
		return dbconn.Profile{}, err
	}
	if err := s.Meta.Store.UpsertProfile(ctx, rec, false); err != nil {
		return dbconn.Profile{}, err
	}
	s.DB.Invalidate(id)
	return p, nil
}

// earlyWarningTriagePrompt is the MCP prompt that teaches an agent the loop.
const earlyWarningTriagePrompt = `SQLON 예방 경보 트리아지 — 장애가 나기 전에 막는 순서입니다.

1. get_early_warnings 를 호출해 headline, summary, incidents, next_actions 를 봅니다.
   next_actions 는 원인 경보 → 긴급 → 경고 순으로 정렬된 구체적 도구 호출입니다.
2. incidents 에 묶인 경보는 하나의 문제입니다. 증상(고갈 예측·사용률)보다 원인
   (WAL 아카이브 실패, 버려진 복제 슬롯, VACUUM 차단 트랜잭션, 테이블 급증)을 먼저 다룹니다.
   explain_early_warning(alert_id)로 원인 사슬·과거 이력·추세를 확인하세요.
3. 원인에 자동 수정이 있으면 propose_early_warning_fix(alert_id)로 변경계획 초안을 만들고,
   사람에게 내용(위험도·사전 조건·보상)을 보여준 뒤 submit_change 를 권합니다.
   직접 approve/execute 하지 말고 승인자의 결정을 받으세요.
4. 원인을 지금 고칠 수 없고 저장공간이 문제면 plan_capacity(profile, target_days)로
   증설 크기와 늦어도 언제까지인지 계산해 보고합니다.
5. 계획된 작업 중이면 manage_early_warning_silences(create)로 해당 DB·규칙만 기간을 정해
   무음 처리합니다(사유 필수). 확인만 할 경보는 acknowledge_early_warning 에 조치 계획을 적습니다.
6. 조치 후 run_early_warning_check 로 다시 평가해 resolved_ids 로 효과를 확인합니다.
7. next_actions 에 configure_profile_alerting(용량 한도 없음)이나 configure_early_warning
   (알림 채널 없음)이 있으면 감시 공백이므로 관리자에게 알립니다.

보고는 한국어로: 지금 위험한 것(언제 가득 차는지 포함), 원인, 제안한 조치와 승인 필요 여부, 남은 감시 공백.`
