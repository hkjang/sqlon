// Package dbconn implements the read-only query connector for PostgreSQL,
// MySQL, and MariaDB: profile store, SQL guard, connection-pool manager,
// executor, circuit breaker, audit, and metrics. Both drivers (pgx and
// go-sql-driver/mysql) are pure Go — no CGO, no client libraries.
package dbconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const ProfilesFile = "db_profiles.json"

// Profile is one target-database connection definition.
// Passwords are never stored in plain text in the profile itself unless the
// operator explicitly uses the discouraged "plain:" scheme (AC-012).
type Profile struct {
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"`
	Type          string   `json:"type,omitempty"`   // postgres | mysql | mariadb | oracle (default postgres)
	Driver        string   `json:"driver,omitempty"` // derived from type
	ServiceName   string   `json:"service_name,omitempty"`
	Environment   string   `json:"environment,omitempty"` // production | staging | development | dr
	Criticality   string   `json:"criticality,omitempty"` // critical | high | medium | low
	Role          string   `json:"role,omitempty"`        // primary | standby | replica | cdb | pdb
	OwnerTeam     string   `json:"owner_team,omitempty"`
	Location      string   `json:"location,omitempty"`
	Maintenance   string   `json:"maintenance_window,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	ConnectString string   `json:"connect_string"` // host:port/dbname, postgres://..., mysql://..., or user:pass@tcp(host:port)/db
	Username      string   `json:"username"`
	PasswordRef   string   `json:"password_ref"` // env:NAME | file:PATH | plain:VALUE
	Pool          Pool     `json:"pool,omitempty"`
	Policy        Policy   `json:"policy,omitempty"`
	Routing       Routing  `json:"routing,omitempty"`
	// Oracle contains connection topology settings. It is ignored by other
	// engines and keeps Oracle-specific conditionals out of service code.
	Oracle        *OracleConfig `json:"oracle,omitempty"`
	LicensePolicy LicensePolicy `json:"license_policy,omitempty"`
	// DBA is the opt-in privileged-credentials block enabling DBA operations
	// (user/role/database/settings/session management) on this profile via a
	// separate write-capable pool. Nil/disabled → DBA tools refuse the profile.
	DBA *DBAConfig `json:"dba,omitempty"`
}

type OracleConfig struct {
	ServiceName    string `json:"service_name,omitempty"`
	ConnectionRole string `json:"connection_role,omitempty"`
	CDBScope       string `json:"cdb_scope,omitempty"`
	RACEnabled     bool   `json:"rac_enabled,omitempty"`
	WalletDir      string `json:"wallet_dir,omitempty"`
	ClientLibDir   string `json:"client_lib_dir,omitempty"`
}

// LicensePolicy is operator-declared; CONTROL_MANAGEMENT_PACK_ACCESS is never
// treated as proof of contractual entitlement.
type LicensePolicy struct {
	DiagnosticsPack string `json:"diagnostics_pack,omitempty"`
	TuningPack      string `json:"tuning_pack,omitempty"`
	Source          string `json:"source,omitempty"`
}

// Routing carries the operator's profile-selection metadata used by
// RouteProfile when many profiles are registered.
type Routing struct {
	// Schemas this profile is declared to serve; a routing match on declared
	// schemas works even when the DB is temporarily unreachable.
	Schemas []string `json:"schemas,omitempty"`
	// Tags are free-form labels (env:prod, replica, team-a...) surfaced in
	// candidate output for human/LLM disambiguation.
	Tags []string `json:"tags,omitempty"`
	// Priority breaks ties between otherwise-equal candidates: 1 is the
	// highest preference, 100 (default) the lowest.
	Priority int `json:"priority,omitempty"`
	// Default marks the fallback profile preferred when no capability signal
	// separates candidates.
	Default bool `json:"default,omitempty"`
	// Discover controls live information_schema inventory probing for
	// capability matching (default true). Disable for expensive/slow targets;
	// routing then relies on declared Schemas only.
	Discover *bool `json:"discover,omitempty"`
}

func (r Routing) discoverEnabled() bool { return r.Discover == nil || *r.Discover }

type Pool struct {
	MaxOpenConns           int `json:"max_open_conns,omitempty"`             // default 10
	MaxIdleConns           int `json:"max_idle_conns,omitempty"`             // default 2
	ConnMaxLifetimeSeconds int `json:"conn_max_lifetime_seconds,omitempty"`  // default 1800
	ConnMaxIdleTimeSeconds int `json:"conn_max_idle_time_seconds,omitempty"` // default 600
}

type Policy struct {
	ReadOnly                  *bool    `json:"readonly,omitempty"`                        // always enforced true
	QueryTimeoutSeconds       int      `json:"query_timeout_seconds,omitempty"`           // default 30
	ConnectTestTimeoutSeconds int      `json:"connection_test_timeout_seconds,omitempty"` // default 5
	DefaultMaxRows            int      `json:"default_max_rows,omitempty"`                // default 100
	MaxRows                   int      `json:"max_rows,omitempty"`                        // default 1000
	MaxResponseBytes          int64    `json:"max_response_bytes,omitempty"`              // default 10MiB
	DeniedKeywords            []string `json:"denied_keywords,omitempty"`                 // extends built-in list

	// PlanGate: before running a query, run EXPLAIN and refuse to execute
	// when the estimated plan risk is at or above PlanGateRisk unless the
	// caller explicitly approves (ExecOptions.ApprovePlan). Protects the
	// target DB from accidental full scans / cartesian joins / huge sorts.
	// PlanGate defaults ON; PlanGateRisk defaults to "high".
	PlanGate     *bool  `json:"plan_gate,omitempty"`
	PlanGateRisk string `json:"plan_gate_risk,omitempty"` // low | medium | high (default high)

	// Absolute cost ceilings (cost guard): when > 0, a query whose EXPLAIN
	// estimates exceed these caps is refused BEFORE execution and CANNOT be
	// bypassed with ApprovePlan — a hard circuit breaker against runaway
	// queries on the operational DB (distinct from the reviewable risk gate).
	// 0 = disabled. Changing the cap requires an admin policy edit.
	MaxPlanCost int64 `json:"max_plan_cost,omitempty"` // EXPLAIN estimated total cost
	MaxPlanRows int64 `json:"max_plan_rows,omitempty"` // EXPLAIN estimated max cardinality
}

func (p *Profile) withDefaults() Profile {
	out := *p
	if out.Type == "" {
		out.Type = "postgres"
	}
	out.Type = strings.ToLower(out.Type)
	out.Environment = strings.ToLower(strings.TrimSpace(out.Environment))
	if out.Environment == "" {
		out.Environment = "unspecified"
	}
	out.Criticality = strings.ToLower(strings.TrimSpace(out.Criticality))
	if out.Criticality == "" {
		out.Criticality = "medium"
	}
	out.Role = strings.ToLower(strings.TrimSpace(out.Role))
	if out.Role == "" {
		out.Role = "unspecified"
	}
	if d, err := DialectFor(out.Type); err == nil {
		out.Type = d.Name()
		out.Driver = d.DriverName()
	}
	if out.Type == "oracle" {
		if out.Oracle == nil {
			out.Oracle = &OracleConfig{}
		}
		if out.LicensePolicy.DiagnosticsPack == "" {
			out.LicensePolicy.DiagnosticsPack = "disabled"
		}
		if out.LicensePolicy.TuningPack == "" {
			out.LicensePolicy.TuningPack = "disabled"
		}
		if out.LicensePolicy.Source == "" {
			out.LicensePolicy.Source = "operator_declared"
		}
		if out.Oracle.ConnectionRole == "" {
			out.Oracle.ConnectionRole = "normal"
		}
		if out.Oracle.CDBScope == "" {
			out.Oracle.CDBScope = "pdb"
		}
	}
	if out.Pool.MaxOpenConns <= 0 {
		out.Pool.MaxOpenConns = 10
	}
	if out.Pool.MaxIdleConns <= 0 {
		out.Pool.MaxIdleConns = 2
	}
	if out.Pool.ConnMaxLifetimeSeconds <= 0 {
		out.Pool.ConnMaxLifetimeSeconds = 1800
	}
	if out.Pool.ConnMaxIdleTimeSeconds <= 0 {
		out.Pool.ConnMaxIdleTimeSeconds = 600
	}
	if out.Policy.QueryTimeoutSeconds <= 0 {
		out.Policy.QueryTimeoutSeconds = 30
	}
	if out.Policy.ConnectTestTimeoutSeconds <= 0 {
		out.Policy.ConnectTestTimeoutSeconds = 5
	}
	if out.Policy.DefaultMaxRows <= 0 {
		out.Policy.DefaultMaxRows = 100
	}
	if out.Policy.MaxRows <= 0 {
		out.Policy.MaxRows = 1000
	}
	if out.Policy.MaxResponseBytes <= 0 {
		out.Policy.MaxResponseBytes = 10 << 20
	}
	if out.Policy.PlanGate == nil {
		enabled := true
		out.Policy.PlanGate = &enabled
	}
	switch strings.ToLower(out.Policy.PlanGateRisk) {
	case "low", "medium", "high":
		out.Policy.PlanGateRisk = strings.ToLower(out.Policy.PlanGateRisk)
	default:
		out.Policy.PlanGateRisk = "high"
	}
	return out
}

// planGateEnabled reports whether the plan-approval gate is active for this
// profile (default true).
func (p Policy) planGateEnabled() bool { return p.PlanGate == nil || *p.PlanGate }

// riskRank maps a risk label to an ordinal for threshold comparison.
func riskRank(risk string) int {
	switch risk {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

var profileIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Validate checks a profile definition before it is saved (admin CRUD).
func (p *Profile) Validate() error {
	if !profileIDRE.MatchString(p.ID) {
		return errors.New("id must match [A-Za-z0-9._-]{1,64}")
	}
	if strings.TrimSpace(p.ConnectString) == "" {
		return errors.New("connect_string is required (host:port/dbname, postgres://..., mysql://..., or a driver DSN)")
	}
	if strings.TrimSpace(p.Username) == "" {
		return errors.New("username is required (use a SELECT-only account)")
	}
	if strings.TrimSpace(p.PasswordRef) == "" {
		return errors.New("password_ref is required (env:NAME, file:PATH, or plain:VALUE)")
	}
	if p.Environment != "" {
		switch strings.ToLower(strings.TrimSpace(p.Environment)) {
		case "production", "staging", "development", "dr", "test", "unspecified":
		default:
			return errors.New("environment must be production, staging, development, dr, test, or unspecified")
		}
	}
	if p.Criticality != "" {
		switch strings.ToLower(strings.TrimSpace(p.Criticality)) {
		case "critical", "high", "medium", "low":
		default:
			return errors.New("criticality must be critical, high, medium, or low")
		}
	}
	passwordScheme, err := parsePasswordRef(p.PasswordRef)
	if err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(p.Environment), "production") && passwordScheme == "plain" {
		return errors.New("production profiles must use env: or file: secret references; plain: is forbidden")
	}
	d, err := DialectFor(p.Type)
	if err != nil {
		return err
	}
	if strings.EqualFold(p.Type, "oracle") {
		if strings.EqualFold(strings.TrimSpace(p.Username), "sys") {
			return errors.New("SYS is not allowed for an Oracle read-only monitoring profile")
		}
		if p.Oracle != nil && p.Oracle.ConnectionRole != "" && !strings.EqualFold(p.Oracle.ConnectionRole, "normal") {
			return errors.New("oracle.connection_role must be normal for the read-only profile")
		}
		if p.Oracle != nil && p.Oracle.CDBScope != "" && !strings.EqualFold(p.Oracle.CDBScope, "pdb") && !strings.EqualFold(p.Oracle.CDBScope, "root") {
			return errors.New("oracle.cdb_scope must be pdb or root")
		}
		for _, v := range []string{p.LicensePolicy.DiagnosticsPack, p.LicensePolicy.TuningPack} {
			if v != "" && !strings.EqualFold(v, "enabled") && !strings.EqualFold(v, "disabled") {
				return errors.New("oracle license pack policy must be enabled or disabled")
			}
		}
		if strings.EqualFold(p.LicensePolicy.TuningPack, "enabled") && !strings.EqualFold(p.LicensePolicy.DiagnosticsPack, "enabled") {
			return errors.New("oracle tuning_pack requires diagnostics_pack to be enabled")
		}
		if p.LicensePolicy.Source != "" && !strings.EqualFold(p.LicensePolicy.Source, "operator_declared") {
			return errors.New("oracle license_policy.source must be operator_declared")
		}
	}
	if p.Driver != "" && !strings.EqualFold(p.Driver, d.DriverName()) {
		return fmt.Errorf("driver %q does not match type %s (expected %s or empty)", p.Driver, d.Name(), d.DriverName())
	}
	if p.Policy.ReadOnly != nil && !*p.Policy.ReadOnly {
		return errors.New("readonly=false is not allowed; this connector is read-only by design")
	}
	if p.Policy.MaxRows > 0 && p.Policy.DefaultMaxRows > p.Policy.MaxRows {
		return errors.New("default_max_rows must not exceed max_rows")
	}
	// DBA block: when present and enabled, require privileged credentials with
	// a valid password_ref scheme (same rules as the query account).
	if p.DBA != nil && (p.DBA.Enabled || p.DBA.Username != "" || p.DBA.PasswordRef != "") {
		if strings.TrimSpace(p.DBA.Username) == "" {
			return errors.New("dba.username is required when dba is configured")
		}
		if strings.TrimSpace(p.DBA.PasswordRef) == "" {
			return errors.New("dba.password_ref is required when dba is configured (env:NAME, file:PATH, or plain:VALUE)")
		}
		dbaScheme, err := parsePasswordRef(p.DBA.PasswordRef)
		if err != nil {
			return fmt.Errorf("dba.%w", err)
		}
		if strings.EqualFold(strings.TrimSpace(p.Environment), "production") && dbaScheme == "plain" {
			return errors.New("production DBA credentials must use env: or file: secret references; plain: is forbidden")
		}
	}
	return nil
}

// parsePasswordRef splits "scheme:value" and validates the scheme.
func parsePasswordRef(ref string) (scheme string, err error) {
	i := strings.Index(ref, ":")
	if i <= 0 {
		return "", errors.New("password_ref must be scheme-prefixed: env:NAME, file:PATH, or plain:VALUE")
	}
	scheme = strings.ToLower(ref[:i])
	switch scheme {
	case "env", "file", "plain":
		if strings.TrimSpace(ref[i+1:]) == "" {
			return "", errors.New("password_ref value is empty")
		}
		return scheme, nil
	default:
		return "", fmt.Errorf("unsupported password_ref scheme %q (use env:, file:, or plain:)", scheme)
	}
}

// ResolvePassword materializes the secret at use time only (never persisted).
func ResolvePassword(ref string) (string, error) {
	scheme, err := parsePasswordRef(ref)
	if err != nil {
		return "", err
	}
	value := ref[strings.Index(ref, ":")+1:]
	switch scheme {
	case "env":
		v := os.Getenv(value)
		if v == "" {
			return "", fmt.Errorf("environment variable %s is not set", value)
		}
		return v, nil
	case "file":
		b, err := os.ReadFile(value)
		if err != nil {
			return "", fmt.Errorf("password file: %w", err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	default: // plain
		return value, nil
	}
}

// MaskedRef hides the sensitive part of a password_ref for API responses.
func MaskedRef(ref string) string {
	scheme, err := parsePasswordRef(ref)
	if err != nil {
		return "(invalid)"
	}
	if scheme == "plain" {
		return "plain:****"
	}
	return ref // env:NAME / file:PATH are pointers, not secrets
}

// ---- profile store abstraction ----

// ProfileStore supplies connection profiles to the Manager. The default is
// the file-backed store (db_profiles.json); when a Postgres meta DB is
// configured the server swaps in a store backed by per-user profile records.
type ProfileStore interface {
	GetProfileByID(ctx context.Context, id string) (Profile, error)
	ListAllProfiles(ctx context.Context) ([]Profile, error)
}

// ApplyDefaults exposes default-filling for external stores.
func ApplyDefaults(p Profile) Profile { return p.withDefaults() }

// FileProfileStore reads db_profiles.json (standalone mode).
type FileProfileStore struct{ DataDir string }

func (f FileProfileStore) GetProfileByID(_ context.Context, id string) (Profile, error) {
	return GetProfile(f.DataDir, id)
}

func (f FileProfileStore) ListAllProfiles(_ context.Context) ([]Profile, error) {
	return LoadProfiles(f.DataDir)
}

// ---- profile store (db_profiles.json) ----

func profilesPath(dataDir string) string { return filepath.Join(dataDir, ProfilesFile) }

// LoadProfiles reads all profiles with defaults applied.
func LoadProfiles(dataDir string) ([]Profile, error) {
	b, err := os.ReadFile(profilesPath(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return []Profile{}, nil
		}
		return nil, err
	}
	var raw []Profile
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", ProfilesFile, err)
	}
	out := make([]Profile, 0, len(raw))
	for _, p := range raw {
		out = append(out, p.withDefaults())
	}
	return out, nil
}

// GetProfile returns one profile by id.
func GetProfile(dataDir, id string) (Profile, error) {
	profiles, err := LoadProfiles(dataDir)
	if err != nil {
		return Profile{}, err
	}
	for _, p := range profiles {
		if p.ID == id {
			return p, nil
		}
	}
	return Profile{}, fmt.Errorf("db profile not found: %s", id)
}

// SaveProfiles persists the full profile list (caller handles backups).
func SaveProfiles(dataDir string, profiles []Profile) error {
	b, err := json.MarshalIndent(profiles, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(profilesPath(dataDir), append(b, '\n'), 0o600)
}

// UpsertProfile validates and inserts/replaces a profile, returning the new
// full list. create=true fails on duplicate id; create=false requires it.
func UpsertProfile(dataDir string, p Profile, create bool) ([]Profile, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	profiles, err := LoadProfiles(dataDir)
	if err != nil {
		return nil, err
	}
	idx := -1
	for i := range profiles {
		if profiles[i].ID == p.ID {
			idx = i
			break
		}
	}
	if create && idx >= 0 {
		return nil, fmt.Errorf("db profile already exists: %s", p.ID)
	}
	if !create && idx < 0 {
		return nil, fmt.Errorf("db profile not found: %s", p.ID)
	}
	np := p.withDefaults()
	if idx >= 0 {
		profiles[idx] = np
	} else {
		profiles = append(profiles, np)
	}
	return profiles, nil
}

// RemoveProfile deletes a profile by id, returning the new full list.
func RemoveProfile(dataDir, id string) ([]Profile, error) {
	profiles, err := LoadProfiles(dataDir)
	if err != nil {
		return nil, err
	}
	out := profiles[:0]
	found := false
	for _, p := range profiles {
		if p.ID == id {
			found = true
			continue
		}
		out = append(out, p)
	}
	if !found {
		return nil, fmt.Errorf("db profile not found: %s", id)
	}
	return out, nil
}

// Masked returns an API-safe copy (password_ref masked when plain).
func (p Profile) Masked() map[string]any {
	m := map[string]any{
		"id":                 p.ID,
		"name":               p.Name,
		"type":               p.Type,
		"driver":             p.Driver,
		"service_name":       p.ServiceName,
		"environment":        p.Environment,
		"criticality":        p.Criticality,
		"role":               p.Role,
		"owner_team":         p.OwnerTeam,
		"location":           p.Location,
		"maintenance_window": p.Maintenance,
		"tags":               p.Tags,
		"connect_string":     p.ConnectString,
		"username":           p.Username,
		"password_ref":       MaskedRef(p.PasswordRef),
		"pool":               p.Pool,
		"policy":             p.Policy,
	}
	if p.Oracle != nil {
		m["oracle"] = p.Oracle
		m["license_policy"] = p.LicensePolicy
	}
	if p.DBA != nil {
		// expose DBA config for the console, but never the raw secret
		m["dba"] = map[string]any{
			"enabled":        p.DBA.Enabled,
			"username":       p.DBA.Username,
			"password_ref":   MaskedRef(p.DBA.PasswordRef),
			"connect_string": p.DBA.ConnectString,
		}
	}
	return m
}

func durationSeconds(s int) time.Duration { return time.Duration(s) * time.Second }
