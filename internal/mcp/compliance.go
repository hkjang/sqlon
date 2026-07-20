package mcp

import (
	"context"
	"strings"
	"time"

	"sqlon/internal/dbconn"
)

// Compliance posture maps SQLON's read-only observations onto the control
// frameworks a Korean credit bureau is audited against (ISMS-P, PCI-DSS, and
// 개인정보보호법/PIPA). It evaluates nothing new — it re-expresses the security
// posture, configuration drift, backup, secret handling, and audit signals as
// pass/fail against named controls, so a DBA can produce an audit-ready snapshot
// instead of hand-collecting evidence. It is advisory: "pass" means the
// observable signal is clean, not a certification.

type complianceControl struct {
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	Frameworks  map[string]string `json:"frameworks"`
	Status      string            `json:"status"` // pass | fail | manual | unknown
	Severity    string            `json:"severity,omitempty"`
	Evidence    []string          `json:"evidence,omitempty"`
	Remediation string            `json:"remediation,omitempty"`
}

// complianceInputs is the extracted signal set the pure evaluator reasons over.
type complianceInputs struct {
	SecurityStatus       string // ok|warning|critical|permission_denied|unsupported|...
	LoginSuperusers      int    // critical superuser findings
	ExpiredAccounts      int
	WildcardHosts        int
	DangerousPrivileges  int
	ConfigBaselineSet    bool
	ConfigDrifted        int
	ConfigStatus         string
	ArchivingEnabled     bool
	BackupStatusKnown    bool
	PlaintextSecret      bool
	AuditChainActive     bool
	TLSObserved          string // "on"|"off"|"unknown"
}

func ctrlPass(id, title string, fw map[string]string, evidence ...string) complianceControl {
	return complianceControl{ID: id, Title: title, Frameworks: fw, Status: "pass", Evidence: evidence}
}
func ctrlFail(id, title string, fw map[string]string, severity, remediation string, evidence ...string) complianceControl {
	return complianceControl{ID: id, Title: title, Frameworks: fw, Status: "fail", Severity: severity, Remediation: remediation, Evidence: evidence}
}
func ctrlManual(id, title string, fw map[string]string, note string) complianceControl {
	return complianceControl{ID: id, Title: title, Frameworks: fw, Status: "manual", Evidence: []string{note}}
}

// evaluateCompliance is the pure control evaluator (unit-tested without a DB).
func evaluateCompliance(in complianceInputs) []complianceControl {
	var c []complianceControl

	// 1. Least privilege — no non-default login superusers.
	fwPriv := map[string]string{"ISMS-P": "2.5.2 최소권한", "PCI-DSS": "7.2", "PIPA": "제29조(안전조치)"}
	if in.SecurityStatus == "permission_denied" || in.SecurityStatus == "unsupported" {
		c = append(c, ctrlManual("least-privilege", "최소 권한 · 과다 권한 계정", fwPriv, "권한 진단을 수집할 수 없어 수동 확인이 필요합니다."))
	} else if in.LoginSuperusers > 0 || in.DangerousPrivileges > 0 {
		c = append(c, ctrlFail("least-privilege", "최소 권한 · 과다 권한 계정", fwPriv, "critical",
			"과다 권한을 최소 권한 원칙에 맞게 회수하세요(변경계획).",
			itoaEvidence("로그인 가능 SUPERUSER/DBA", in.LoginSuperusers), itoaEvidence("위험 시스템 권한", in.DangerousPrivileges)))
	} else {
		c = append(c, ctrlPass("least-privilege", "최소 권한 · 과다 권한 계정", fwPriv, "과다 권한 계정이 발견되지 않았습니다."))
	}

	// 2. Account lifecycle — no expired-active / wildcard-host accounts.
	fwAcct := map[string]string{"ISMS-P": "2.5.1 사용자 계정 관리", "PCI-DSS": "8.1", "PIPA": "제29조"}
	if in.SecurityStatus == "permission_denied" || in.SecurityStatus == "unsupported" {
		c = append(c, ctrlManual("account-lifecycle", "계정 생명주기 관리", fwAcct, "계정 상태를 수집할 수 없어 수동 확인이 필요합니다."))
	} else if in.ExpiredAccounts > 0 || in.WildcardHosts > 0 {
		c = append(c, ctrlFail("account-lifecycle", "계정 생명주기 관리", fwAcct, "warning",
			"만료 계정 잠금·정리, 와일드카드 호스트 계정 제한을 수행하세요.",
			itoaEvidence("만료/미정리 계정", in.ExpiredAccounts), itoaEvidence("와일드카드 호스트 계정", in.WildcardHosts)))
	} else {
		c = append(c, ctrlPass("account-lifecycle", "계정 생명주기 관리", fwAcct, "만료·와일드카드 계정이 없습니다."))
	}

	// 3. Secret handling — no plaintext credential references.
	fwSecret := map[string]string{"ISMS-P": "2.7.1 암호정책", "PCI-DSS": "8.3.1", "PIPA": "제29조"}
	if in.PlaintextSecret {
		c = append(c, ctrlFail("secret-handling", "자격증명 보호 (평문 금지)", fwSecret, "critical",
			"password_ref를 env:/file: 시크릿 참조로 전환하세요. 운영 프로파일은 plain: 금지입니다.",
			"프로파일에 plain: 평문 자격증명 참조가 있습니다."))
	} else {
		c = append(c, ctrlPass("secret-handling", "자격증명 보호 (평문 금지)", fwSecret, "평문 자격증명 참조가 없습니다."))
	}

	// 4. Configuration integrity — no drift from declared baseline.
	fwConf := map[string]string{"ISMS-P": "2.9.1 변경관리", "PCI-DSS": "2.2", "PIPA": "제29조"}
	switch {
	case !in.ConfigBaselineSet:
		c = append(c, ctrlManual("config-integrity", "구성 무결성 (베이스라인)", fwConf, "config_baseline 미선언 — 기대 파라미터를 선언하면 드리프트를 자동 감지합니다."))
	case in.ConfigDrifted > 0:
		c = append(c, ctrlFail("config-integrity", "구성 무결성 (베이스라인)", fwConf, "warning",
			"드리프트된 파라미터를 베이스라인으로 원복하세요(변경계획).",
			itoaEvidence("베이스라인과 다른 파라미터", in.ConfigDrifted)))
	default:
		c = append(c, ctrlPass("config-integrity", "구성 무결성 (베이스라인)", fwConf, "서버 파라미터가 선언된 베이스라인과 일치합니다."))
	}

	// 5. Backup / recoverability — continuous archiving enabled.
	fwBackup := map[string]string{"ISMS-P": "2.9.7 백업 및 복구", "PCI-DSS": "12.10", "PIPA": "제29조"}
	switch {
	case !in.BackupStatusKnown:
		c = append(c, ctrlManual("backup-recovery", "백업 · 복구 가능성", fwBackup, "백업 상태를 수집할 수 없어 수동 확인이 필요합니다."))
	case in.ArchivingEnabled:
		c = append(c, ctrlPass("backup-recovery", "백업 · 복구 가능성", fwBackup, "지속 아카이빙(PITR 기반)이 활성화되어 있습니다."))
	default:
		c = append(c, ctrlFail("backup-recovery", "백업 · 복구 가능성", fwBackup, "warning",
			"지속 아카이빙(WAL/binlog/ARCHIVELOG)을 활성화해 시점 복구를 확보하세요.",
			"지속 아카이빙이 비활성화되어 있습니다."))
	}

	// 6. Audit logging — tamper-evident audit chain active.
	fwAudit := map[string]string{"ISMS-P": "2.11.3 로그 관리", "PCI-DSS": "10.2", "PIPA": "제29조"}
	if in.AuditChainActive {
		c = append(c, ctrlPass("audit-logging", "감사 로깅 (변조 방지)", fwAudit, "관리 작업이 해시 체인 감사 로그로 기록됩니다(/api/audit/verify)."))
	} else {
		c = append(c, ctrlManual("audit-logging", "감사 로깅 (변조 방지)", fwAudit, "감사 로그 활성 여부를 확인하세요."))
	}

	// 7. Transport encryption (TLS) — usually not observable read-only.
	fwTLS := map[string]string{"ISMS-P": "2.7.2 전송구간 암호화", "PCI-DSS": "4.1", "PIPA": "제29조"}
	switch in.TLSObserved {
	case "on":
		c = append(c, ctrlPass("transport-tls", "전송구간 암호화(TLS)", fwTLS, "TLS 연결이 확인되었습니다."))
	case "off":
		c = append(c, ctrlFail("transport-tls", "전송구간 암호화(TLS)", fwTLS, "warning", "DB 연결에 TLS를 강제하세요.", "TLS 미적용 연결이 관찰되었습니다."))
	default:
		c = append(c, ctrlManual("transport-tls", "전송구간 암호화(TLS)", fwTLS, "연결 TLS 적용 여부는 이 진단으로 확정할 수 없어 수동 확인이 필요합니다."))
	}

	return c
}

func itoaEvidence(label string, n int) string {
	return label + ": " + itoa(n)
}
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// compliancePosture gathers the live signals for a profile and returns the
// control-mapped report. Best-effort: an unavailable signal downgrades the
// affected control to "manual" rather than failing the whole report.
func (s *Server) compliancePosture(ctx context.Context, profile dbconn.Profile) map[string]any {
	now := time.Now().UTC()
	in := complianceInputs{TLSObserved: "unknown", AuditChainActive: true}

	sec := s.Observability.Security(ctx, profile)
	in.SecurityStatus = sec.Status
	for _, f := range sec.Data.Findings {
		switch f.Kind {
		case "superuser", "dba_role":
			if f.Severity == "critical" {
				in.LoginSuperusers++
			}
		case "dangerous_privilege":
			in.DangerousPrivileges++
		case "expired_password":
			in.ExpiredAccounts++
		case "wildcard_host":
			in.WildcardHosts++
		}
	}

	if len(profile.ConfigBaseline) > 0 {
		in.ConfigBaselineSet = true
		drift := s.Observability.ConfigDrift(ctx, profile)
		in.ConfigStatus = drift.Status
		in.ConfigDrifted = drift.Data.Drifted
	}

	backup := s.Observability.Backup(ctx, profile)
	if backup.Status != "unsupported" && backup.Status != "permission_denied" && backup.Status != "error" {
		in.BackupStatusKnown = true
		in.ArchivingEnabled = strings.EqualFold(backup.Data.Archiving, "enabled")
	}

	if strings.HasPrefix(strings.ToLower(profile.PasswordRef), "plain:") {
		in.PlaintextSecret = true
	}
	if profile.DBA != nil && strings.HasPrefix(strings.ToLower(profile.DBA.PasswordRef), "plain:") {
		in.PlaintextSecret = true
	}

	controls := evaluateCompliance(in)
	pass, fail, manual := 0, 0, 0
	for _, ctl := range controls {
		switch ctl.Status {
		case "pass":
			pass++
		case "fail":
			fail++
		default:
			manual++
		}
	}
	scored := pass + fail
	score := 100
	if scored > 0 {
		score = pass * 100 / scored
	}
	status := "compliant"
	if fail > 0 {
		status = "gaps"
	}
	return map[string]any{
		"status":       status,
		"profile_id":   profile.ID,
		"engine":       profile.Type,
		"score":        score,
		"controls":     controls,
		"summary":      map[string]any{"total": len(controls), "pass": pass, "fail": fail, "manual": manual},
		"frameworks":   []string{"ISMS-P", "PCI-DSS", "PIPA(개인정보보호법)"},
		"collected_at": now,
		"notice":       "읽기 전용 진단을 통제 항목에 매핑한 참고 리포트입니다. 인증/심사를 대체하지 않으며, 조치는 변경 관리로 수행하세요.",
	}
}
