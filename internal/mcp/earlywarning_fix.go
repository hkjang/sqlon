package mcp

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"sqlon/internal/change"
	"sqlon/internal/dbconn"
	"sqlon/internal/earlywarning"
)

// Early-warning fixes turn an alert into a draft ChangePlan: the same
// approval-gated path every other change takes (submit → approve → execute,
// audited, with compensation). Nothing here touches the database except
// read-only lookups needed to write a correct plan.
//
// The change executor treats a verification as passed when it runs without
// error, so these verifications raise an error when the fix did not take
// effect: CAST('verification failed: …' AS integer) fails with that text.

type fixProposal struct {
	Available bool         `json:"available"`
	Kind      string       `json:"kind,omitempty"`
	Plan      *change.Plan `json:"plan,omitempty"`
	Guidance  string       `json:"guidance"`
	NextTools []string     `json:"next_tools,omitempty"`
}

// assertSQL fails with message unless cond (a boolean SQL expression) holds.
func assertSQL(cond, message string) string {
	return "SELECT CAST(CASE WHEN " + cond + " THEN '1' ELSE " + quoteLiteral("verification failed: "+message) + " END AS integer)"
}

var (
	pidObject      = regexp.MustCompile(`^pid (\d+)$`)
	preparedObject = regexp.MustCompile(`^prepared (.+)$`)
	// quote_ident(schema).quote_ident(table) as the maintenance check emits it
	qualifiedIdent = regexp.MustCompile(`^("([^"]|"")+"|[a-z_][a-z0-9_$]*)\.("([^"]|"")+"|[a-z_][a-z0-9_$]*)$`)
)

const fixRecheck = "조치 후 run_early_warning_check 로 다시 평가해 경보가 해소됐는지 확인하세요."

func guidanceFor(a earlywarning.Alert) fixProposal {
	p := fixProposal{Guidance: a.Recommendation}
	switch {
	case a.Rule == earlywarning.RuleCapacityForecast || a.Rule == earlywarning.RuleCapacityUsage || a.Rule == earlywarning.RuleGrowthSurge:
		p.Guidance = "저장공간은 SQL 한 줄로 고칠 수 없습니다. explain_early_warning 의 원인 사슬(WAL·슬롯·아카이브·테이블)을 먼저 보고, 원인 경보에 propose_early_warning_fix 를 쓰세요. 증설이 답이면 plan_capacity 로 필요한 크기와 시점을 계산하세요."
		p.NextTools = []string{"explain_early_warning", "plan_capacity"}
	case a.Rule == "maint_wal_archive":
		p.Guidance = "archive_command 실패는 아카이브 대상(경로 권한·용량·네트워크·백업 도구) 쪽 문제라 DB 변경으로 고치지 않습니다. 서버 로그의 archive_command 오류를 확인해 대상을 복구하면 쌓인 WAL은 자동으로 아카이브·정리됩니다."
	case a.Rule == earlywarning.RuleSchemaChange:
		p.Guidance = "이미 일어난 스키마 변경입니다. 의도된 배포면 acknowledge_early_warning 으로 확인하고, 아니면 변경 주체를 추적하세요."
		p.NextTools = []string{"acknowledge_early_warning"}
	case a.Rule == earlywarning.RuleCollectionDown:
		p.Guidance = "DB에 접속할 수 없습니다. 디스크가 가득 차 멈췄는지 먼저 확인하세요(같은 DB의 디스크 경보, DB 서버의 df)."
	}
	if p.Guidance == "" {
		p.Guidance = "이 경보에는 자동 수정안이 없습니다. 경보의 권고를 따르세요."
	}
	return p
}

// proposeFix builds a draft plan for the alert when a safe, well-defined
// fix exists. args may carry an explicit value (max_slot_wal_keep_size).
func (s *Server) proposeFix(ctx context.Context, a earlywarning.Alert, p dbconn.Profile, args map[string]any) (fixProposal, error) {
	if !strings.EqualFold(p.Type, "postgres") {
		return guidanceFor(a), nil
	}
	plan := change.Plan{
		ID:        fmt.Sprintf("chg-ew-%s-%d", sanitizeID(strings.TrimPrefix(a.Rule, "maint_")), time.Now().UnixNano()),
		ProfileID: p.ID, Target: a.Object,
		Reason:   fmt.Sprintf("예방 경보 조치: %s (경보 %s)", a.Title, a.ID),
		PreState: map[string]any{"alert_id": a.ID, "rule": a.Rule, "severity": a.Severity, "detail": a.Detail, "value": a.Value, "observed_at": a.LastSeen},
	}
	kind := ""
	switch a.Rule {
	case "maint_replication_slot":
		if !strings.Contains(a.Detail, "비활성") && !strings.Contains(a.Detail, "무효화") {
			g := guidanceFor(a)
			g.Guidance = "연결된 소비자가 뒤처진 슬롯입니다. 슬롯을 지우면 그 소비자(스탠바이·CDC)의 복제가 끊깁니다 — 소비자 쪽 지연 원인을 먼저 해결하세요. 정말 버릴 소비자라면 소비자를 멈춘 뒤 다시 평가하면 비활성 슬롯으로 바뀌어 제거안을 만들 수 있습니다."
			return g, nil
		}
		slot := a.Object
		rows, err := s.DB.SystemQuery(ctx, p.ID, "SELECT slot_type, COALESCE(plugin, '') AS plugin, active FROM pg_catalog.pg_replication_slots WHERE slot_name = $1", slot)
		if err != nil {
			return fixProposal{}, err
		}
		if len(rows) == 0 {
			return fixProposal{Guidance: "슬롯 " + slot + " 이 이미 없습니다. run_early_warning_check 로 다시 평가하세요.", NextTools: []string{"run_early_warning_check"}}, nil
		}
		if v := fmt.Sprint(rows[0]["active"]); v == "true" || v == "t" {
			return fixProposal{Guidance: "슬롯 " + slot + " 이 지금은 연결되어 있습니다(active). 소비자가 돌아왔으면 제거하지 마세요."}, nil
		}
		recreate := "SELECT pg_catalog.pg_create_physical_replication_slot(" + quoteLiteral(slot) + ", true)"
		if fmt.Sprint(rows[0]["slot_type"]) == "logical" {
			recreate = "SELECT pg_catalog.pg_create_logical_replication_slot(" + quoteLiteral(slot) + ", " + quoteLiteral(fmt.Sprint(rows[0]["plugin"])) + ")"
		}
		kind = "drop_replication_slot"
		plan.Risk = change.High
		plan.Preconditions = []string{
			"이 슬롯의 소비자(스탠바이·CDC·논리 복제 구독자)가 더 이상 쓰이지 않음을 확인",
			"제거 후 붙잡혀 있던 WAL은 다음 체크포인트에 삭제되어 되돌릴 수 없음 — 보상 단계는 빈 슬롯만 다시 만들며 소비자는 재동기화해야 함",
		}
		plan.Steps = []change.Step{{Order: 1,
			Command:      "SELECT pg_catalog.pg_drop_replication_slot(" + quoteLiteral(slot) + ")",
			Verification: assertSQL("NOT EXISTS (SELECT 1 FROM pg_catalog.pg_replication_slots WHERE slot_name = "+quoteLiteral(slot)+")", "slot "+slot+" still exists"),
			Compensation: recreate}}
	case "maint_vacuum_blocker":
		if m := pidObject.FindStringSubmatch(a.Object); m != nil {
			pid := m[1]
			version := s.serverVersionNum(ctx, p.ID)
			kind = "terminate_session"
			plan.Risk = change.Medium
			plan.Preconditions = []string{"세션 소유 애플리케이션이 재연결을 견디는지 확인", "종료된 트랜잭션은 롤백되며 되돌릴 수 없음"}
			step := change.Step{Order: 1, Compensation: "SELECT 1 /* 세션 종료는 되돌릴 수 없습니다 — 애플리케이션 재연결을 확인하세요 */"}
			if version >= 140000 {
				// waits up to 5s for the backend to exit, so the check is not a race
				step.Command = "SELECT pg_catalog.pg_terminate_backend(" + pid + ", 5000)"
				step.Verification = assertSQL("NOT EXISTS (SELECT 1 FROM pg_catalog.pg_stat_activity WHERE pid = "+pid+")", "backend "+pid+" is still running")
			} else {
				step.Command = "SELECT pg_catalog.pg_terminate_backend(" + pid + ")"
				step.Verification = "SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE pid = " + pid
			}
			plan.Steps = []change.Step{step}
		} else if m := preparedObject.FindStringSubmatch(a.Object); m != nil {
			gid := m[1]
			kind = "rollback_prepared"
			plan.Risk = change.High
			plan.Preconditions = []string{"트랜잭션 관리자(애플리케이션)가 이 2단계 커밋을 포기했음을 확인 — 커밋해야 하는 트랜잭션이면 COMMIT PREPARED 로 계획을 직접 작성", "되돌릴 수 없음"}
			plan.Steps = []change.Step{{Order: 1,
				Command:      "ROLLBACK PREPARED " + quoteLiteral(gid),
				Verification: assertSQL("NOT EXISTS (SELECT 1 FROM pg_catalog.pg_prepared_xacts WHERE gid = "+quoteLiteral(gid)+")", "prepared transaction "+gid+" still exists"),
				Compensation: "SELECT 1 /* ROLLBACK PREPARED 는 되돌릴 수 없습니다 */"}}
		}
	case "maint_bloat":
		if !qualifiedIdent.MatchString(a.Object) {
			return guidanceFor(a), nil
		}
		kind = "vacuum"
		plan.Risk = change.Low
		plan.Preconditions = []string{"VACUUM 은 테이블을 잠그지 않지만 I/O 를 씁니다 — 부하가 낮은 시간에 실행", "블로트가 VACUUM 을 막는 장기 트랜잭션 때문이면 그 경보를 먼저 해결"}
		plan.Steps = []change.Step{{Order: 1,
			Command:      "VACUUM (ANALYZE) " + a.Object,
			Verification: "SELECT n_dead_tup FROM pg_catalog.pg_stat_user_tables WHERE quote_ident(schemaname) || '.' || quote_ident(relname) = " + quoteLiteral(a.Object),
			Compensation: "SELECT 1 /* VACUUM 은 되돌릴 필요가 없습니다 */"}}
	case "maint_config_risk":
		switch a.Object {
		case "max_slot_wal_keep_size":
			mb := s.slotKeepBudgetMB(p, args)
			if mb <= 0 {
				g := guidanceFor(a)
				g.Guidance = "슬롯이 붙잡을 수 있는 WAL 상한을 정하려면 볼륨 크기가 필요합니다. 프로파일에 저장공간 한도를 선언하거나 디스크 보고를 설치하거나, value_mb 인자로 값을 직접 주세요."
				g.NextTools = []string{"configure_profile_alerting", "report_host_disk"}
				return g, nil
			}
			kind = "limit_slot_wal"
			plan.Risk = change.Medium
			plan.Preconditions = []string{fmt.Sprintf("상한 %dMB 를 넘긴 슬롯은 무효화됩니다(그 소비자는 재동기화 필요) — 대신 디스크가 가득 차 DB 전체가 멈추는 일은 막습니다", mb)}
			plan.Steps = []change.Step{
				{Order: 1, Command: fmt.Sprintf("ALTER SYSTEM SET max_slot_wal_keep_size = '%dMB'", mb), Verification: "SHOW max_slot_wal_keep_size", Compensation: "ALTER SYSTEM RESET max_slot_wal_keep_size"},
				{Order: 2, Command: "SELECT pg_catalog.pg_reload_conf()", Verification: "SHOW max_slot_wal_keep_size", Compensation: "SELECT pg_catalog.pg_reload_conf()"},
			}
		case "autovacuum":
			kind = "enable_autovacuum"
			plan.Risk = change.Medium
			plan.Steps = []change.Step{
				{Order: 1, Command: "ALTER SYSTEM SET autovacuum = on", Verification: "SHOW autovacuum", Compensation: "ALTER SYSTEM RESET autovacuum"},
				{Order: 2, Command: "SELECT pg_catalog.pg_reload_conf()", Verification: "SHOW autovacuum", Compensation: "SELECT pg_catalog.pg_reload_conf()"},
			}
		}
	case earlywarning.RuleMonitorPrivilege:
		role, err := quoteIdent("postgres", p.Username)
		if err != nil {
			return fixProposal{}, err
		}
		kind = "grant_pg_monitor"
		plan.Risk = change.Low
		plan.Target = p.Username
		plan.Preconditions = []string{"pg_monitor 는 읽기 전용 모니터링 내장 역할입니다(디렉터리 크기·통계 조회)"}
		plan.Steps = []change.Step{{Order: 1,
			Command:      "GRANT pg_monitor TO " + role,
			Verification: assertSQL("pg_catalog.pg_has_role("+quoteLiteral(p.Username)+", 'pg_monitor', 'MEMBER')", p.Username+" is not a member of pg_monitor"),
			Compensation: "REVOKE pg_monitor FROM " + role}}
	}
	if kind == "" {
		return guidanceFor(a), nil
	}
	for _, st := range plan.Steps {
		if impact := change.PredictImpact("postgres", st.Command); impact.LockLevel != "" && plan.ExpectedLock == "" {
			plan.ExpectedLock, plan.Impact = impact.LockLevel, impact
		}
	}
	return fixProposal{Available: true, Kind: kind, Plan: &plan,
		Guidance:  "초안(draft) 변경계획입니다. submit_change → approve_change → execute_approved_change 승인 게이트를 거쳐야 실행됩니다. " + fixRecheck,
		NextTools: []string{"submit_change", "approve_change", "execute_approved_change", "run_early_warning_check"}}, nil
}

func (s *Server) serverVersionNum(ctx context.Context, profileID string) int {
	rows, err := s.DB.SystemQuery(ctx, profileID, "SELECT current_setting('server_version_num') AS v")
	if err != nil || len(rows) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(fmt.Sprint(rows[0]["v"]))
	return n
}

// slotKeepBudgetMB is the WAL a slot may pin: an explicit value_mb, else 20%
// of the volume (host report) or the declared storage limit.
func (s *Server) slotKeepBudgetMB(p dbconn.Profile, args map[string]any) int {
	if v, ok := args["value_mb"]; ok {
		if f, err := strconv.ParseFloat(fmt.Sprint(v), 64); err == nil && f >= 64 {
			return int(f)
		}
	}
	limit := 0.0
	for _, f := range s.EarlyWarning.Board([]dbconn.Profile{p}).Profiles[0].Forecasts {
		if f.LimitBytes > 0 && (f.Scope == "volume" || f.Primary) && f.LimitBytes > limit {
			limit = f.LimitBytes
		}
	}
	if limit <= 0 {
		return 0
	}
	return int(math.Max(1024, math.Floor(limit*0.2/(1<<20))))
}
