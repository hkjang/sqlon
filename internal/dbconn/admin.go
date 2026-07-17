package dbconn

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Privileged DBA execution path.
//
// The normal connector is read-only by construction: every session sets
// default_transaction_read_only / transaction_read_only and the SQL guard
// rejects anything but SELECT/WITH. DBA operations (CREATE USER, GRANT,
// CREATE DATABASE, ALTER SYSTEM, terminate session, VACUUM, …) fundamentally
// require a WRITE-capable, privileged session, so they run through a SEPARATE
// pool built from the profile's opt-in DBA credentials — never the read-only
// query account. A profile with no DBA block cannot be used for DBA actions.
//
// Safety posture: this path is gated in the MCP/REST layer to the dba/admin
// role, every statement is audited, and it is disabled unless the operator
// explicitly configures DBAConfig on the profile.

// DBAConfig is the opt-in privileged-credentials block on a Profile. When
// absent or Enabled=false, DBA tools refuse to act on the profile.
type DBAConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// Username/PasswordRef of a privileged account (e.g. a postgres superuser
	// or a role with CREATEROLE/CREATEDB). PasswordRef uses the same
	// env:/file:/plain: scheme as the query account.
	Username    string `json:"username,omitempty"`
	PasswordRef string `json:"password_ref,omitempty"`
	// ConnectString optionally overrides the target for DBA connections — e.g.
	// point at the "postgres" maintenance database so CREATE/DROP DATABASE do
	// not run against the connected database. Defaults to the profile's own
	// connect string.
	ConnectString string `json:"connect_string,omitempty"`
}

// adminEnabled reports whether the profile opted into DBA operations.
func (p *Profile) adminEnabled() bool {
	return p.DBA != nil && p.DBA.Enabled && strings.TrimSpace(p.DBA.Username) != ""
}

type adminPool struct {
	db  *sql.DB
	sig string
}

var (
	adminMu    sync.Mutex
	adminPools = map[string]*adminPool{}
)

func adminSignature(p Profile) string {
	var b strings.Builder
	b.WriteString(p.Type)
	b.WriteString("|")
	b.WriteString(p.ConnectString)
	if p.DBA != nil {
		fmt.Fprintf(&b, "|%v|%s|%s|%s", p.DBA.Enabled, p.DBA.Username, p.DBA.PasswordRef, p.DBA.ConnectString)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// adminDB returns (building if needed) the privileged pool for a profile.
func (m *Manager) adminDB(p Profile) (*sql.DB, error) {
	if !p.adminEnabled() {
		return nil, fmt.Errorf("profile %q has no DBA credentials configured (set dba.enabled + dba.username/password_ref)", p.ID)
	}
	sig := adminSignature(p)
	adminMu.Lock()
	defer adminMu.Unlock()
	if cur, ok := adminPools[p.ID]; ok {
		if cur.sig == sig {
			return cur.db, nil
		}
		_ = cur.db.Close()
		delete(adminPools, p.ID)
	}
	password, err := ResolvePassword(p.DBA.PasswordRef)
	if err != nil {
		return nil, fmt.Errorf("resolve DBA password: %w", err)
	}
	d, err := DialectFor(p.Type)
	if err != nil {
		return nil, err
	}
	dsn, err := buildAdminDSN(d, p, password)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(d.DriverName(), dsn)
	if err != nil {
		return nil, err
	}
	// keep the privileged pool small — DBA work is low-volume
	db.SetMaxOpenConns(3)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(10 * time.Minute)
	adminPools[p.ID] = &adminPool{db: db, sig: sig}
	return db, nil
}

// buildAdminDSN reuses the dialect DSN builder with the DBA credentials, then
// strips the read-only session flags so the privileged session can write.
func buildAdminDSN(d Dialect, p Profile, password string) (string, error) {
	ap := p
	ap.Username = p.DBA.Username
	ap.PasswordRef = p.DBA.PasswordRef
	if cs := strings.TrimSpace(p.DBA.ConnectString); cs != "" {
		ap.ConnectString = cs
	}
	dsn, err := d.BuildDSN(ap, password)
	if err != nil {
		return "", err
	}
	switch d.Name() {
	case "postgres":
		u, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}
		q := u.Query()
		q.Del("default_transaction_read_only")
		u.RawQuery = q.Encode()
		return u.String(), nil
	case "oracle":
		// Oracle has no DSN read-only flag. The Oracle admin executor is linked
		// only in sqlon-oracle builds and uses a separately approved plan.
		return dsn, nil
	default: // mysql / mariadb
		cfg, err := mysql.ParseDSN(dsn)
		if err != nil {
			return "", err
		}
		delete(cfg.Params, "tx_read_only")
		delete(cfg.Params, "transaction_read_only")
		return cfg.FormatDSN(), nil
	}
}

// AdminResult is the outcome of a privileged statement.
type AdminResult struct {
	ProfileID    string `json:"profile_id"`
	Dialect      string `json:"dialect"`
	Statement    string `json:"statement"`
	RowsAffected int64  `json:"rows_affected"`
	ElapsedMs    int64  `json:"elapsed_ms"`
}

// AdminExec runs ONE privileged statement (DDL/DCL/maintenance) on the
// profile's DBA pool. It is the write counterpart to SystemQuery: no read-only
// guard, no plan gate. The caller (MCP/REST) is responsible for authorization
// and audit. Multiple statements are rejected here — callers pass one at a
// time so each is individually audited and errors are unambiguous.
func (m *Manager) AdminExec(ctx context.Context, profileID, statement string, timeoutSeconds int) (*AdminResult, error) {
	p, err := m.store.GetProfileByID(ctx, profileID)
	if err != nil {
		return nil, err
	}
	if err := EnforceOracleLicense(p, statement); err != nil {
		return nil, err
	}
	d, err := DialectFor(p.Type)
	if err != nil {
		return nil, err
	}
	db, err := m.adminDB(p)
	if err != nil {
		return nil, err
	}
	if timeoutSeconds <= 0 {
		timeoutSeconds = 60
	}
	ectx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	start := time.Now()
	res, err := db.ExecContext(ectx, statement)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return nil, fmt.Errorf("dba exec failed: %s", sanitizeDBError(err))
	}
	affected, _ := res.RowsAffected()
	return &AdminResult{
		ProfileID: p.ID, Dialect: d.Name(), Statement: statement,
		RowsAffected: affected, ElapsedMs: elapsed,
	}, nil
}

// AdminQuery runs a privileged read (system catalogs the read-only account may
// not see, e.g. pg_authid) on the DBA pool. Same generic-map shape as
// SystemQuery, same row ceiling.
func (m *Manager) AdminQuery(ctx context.Context, profileID, query string, args ...any) ([]map[string]any, error) {
	p, err := m.store.GetProfileByID(ctx, profileID)
	if err != nil {
		return nil, err
	}
	db, err := m.adminDB(p)
	if err != nil {
		return nil, err
	}
	qctx, cancel := context.WithTimeout(ctx, durationSeconds(p.Policy.QueryTimeoutSeconds))
	defer cancel()
	rows, err := db.QueryContext(qctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("dba query failed: %s", sanitizeDBError(err))
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, 256)
	for rows.Next() {
		if len(out) >= SystemQueryMaxRows {
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = normalizeValue(vals[i])
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// AdminAvailable reports whether a profile can run DBA operations.
func (m *Manager) AdminAvailable(ctx context.Context, profileID string) (bool, error) {
	p, err := m.store.GetProfileByID(ctx, profileID)
	if err != nil {
		return false, err
	}
	return p.adminEnabled(), nil
}
