package mcp

import "testing"

func controlByID(cs []complianceControl, id string) (complianceControl, bool) {
	for _, c := range cs {
		if c.ID == id {
			return c, true
		}
	}
	return complianceControl{}, false
}

func TestComplianceCleanProfilePasses(t *testing.T) {
	in := complianceInputs{
		SecurityStatus: "ok", ConfigBaselineSet: true, ConfigStatus: "ok",
		BackupStatusKnown: true, ArchivingEnabled: true, AuditChainActive: true, TLSObserved: "unknown",
	}
	cs := evaluateCompliance(in)
	for _, id := range []string{"least-privilege", "account-lifecycle", "secret-handling", "config-integrity", "backup-recovery", "audit-logging"} {
		c, ok := controlByID(cs, id)
		if !ok {
			t.Fatalf("control %s missing", id)
		}
		if c.Status == "fail" {
			t.Fatalf("control %s should not fail on a clean profile: %+v", id, c)
		}
	}
	// every control must carry framework mappings for the audit trail
	for _, c := range cs {
		if len(c.Frameworks) == 0 {
			t.Fatalf("control %s has no framework mapping", c.ID)
		}
	}
}

func TestComplianceFailsOnPlaintextSecret(t *testing.T) {
	cs := evaluateCompliance(complianceInputs{SecurityStatus: "ok", PlaintextSecret: true, BackupStatusKnown: true, ArchivingEnabled: true})
	c, _ := controlByID(cs, "secret-handling")
	if c.Status != "fail" || c.Severity != "critical" {
		t.Fatalf("plaintext secret must be a critical fail: %+v", c)
	}
}

func TestComplianceFailsOnSuperuserAndDrift(t *testing.T) {
	cs := evaluateCompliance(complianceInputs{
		SecurityStatus: "critical", LoginSuperusers: 2,
		ConfigBaselineSet: true, ConfigDrifted: 3, ConfigStatus: "warning",
		BackupStatusKnown: true, ArchivingEnabled: false,
	})
	if c, _ := controlByID(cs, "least-privilege"); c.Status != "fail" {
		t.Fatalf("login superusers must fail least-privilege: %+v", c)
	}
	if c, _ := controlByID(cs, "config-integrity"); c.Status != "fail" {
		t.Fatalf("config drift must fail config-integrity: %+v", c)
	}
	if c, _ := controlByID(cs, "backup-recovery"); c.Status != "fail" {
		t.Fatalf("disabled archiving must fail backup-recovery: %+v", c)
	}
}

func TestComplianceUnavailableSecurityIsManualNotFail(t *testing.T) {
	cs := evaluateCompliance(complianceInputs{SecurityStatus: "permission_denied", BackupStatusKnown: false})
	if c, _ := controlByID(cs, "least-privilege"); c.Status != "manual" {
		t.Fatalf("unreadable security must be manual, not fail: %+v", c)
	}
	if c, _ := controlByID(cs, "backup-recovery"); c.Status != "manual" {
		t.Fatalf("unreadable backup must be manual: %+v", c)
	}
}
