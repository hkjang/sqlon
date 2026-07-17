package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// PGStore is the production Store on PostgreSQL (pure-Go pgx driver, no CGO).
type PGStore struct {
	db *sql.DB
}

// migrations run in order inside one transaction; all statements are
// idempotent so re-running on startup is safe.
var migrations = []string{
	`CREATE TABLE IF NOT EXISTS jamypg_users (
		id TEXT PRIMARY KEY,
		username TEXT NOT NULL,
		display_name TEXT NOT NULL DEFAULT '',
		email TEXT NOT NULL DEFAULT '',
		password_hash TEXT,
		role TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('admin','user')),
		provider TEXT NOT NULL DEFAULT 'local' CHECK (provider IN ('local','keycloak')),
		provider_subject TEXT,
		is_active BOOLEAN NOT NULL DEFAULT TRUE,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		last_login_at TIMESTAMPTZ
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS jamypg_users_username_key ON jamypg_users (lower(username))`,
	`CREATE UNIQUE INDEX IF NOT EXISTS jamypg_users_provider_subject_key
		ON jamypg_users (provider, provider_subject) WHERE provider_subject IS NOT NULL AND provider_subject <> ''`,
	`CREATE TABLE IF NOT EXISTS jamypg_sessions (
		token_hash TEXT PRIMARY KEY,
		user_id TEXT NOT NULL REFERENCES jamypg_users(id) ON DELETE CASCADE,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		expires_at TIMESTAMPTZ NOT NULL,
		ip TEXT NOT NULL DEFAULT '',
		user_agent TEXT NOT NULL DEFAULT '',
		revoked_at TIMESTAMPTZ
	)`,
	`CREATE INDEX IF NOT EXISTS jamypg_sessions_user_idx ON jamypg_sessions (user_id)`,
	`CREATE TABLE IF NOT EXISTS jamypg_mcp_keys (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL REFERENCES jamypg_users(id) ON DELETE CASCADE,
		name TEXT NOT NULL DEFAULT '',
		key_hash TEXT NOT NULL UNIQUE,
		key_prefix TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		expires_at TIMESTAMPTZ,
		last_used_at TIMESTAMPTZ,
		revoked_at TIMESTAMPTZ,
		rotated_from TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS jamypg_mcp_keys_user_idx ON jamypg_mcp_keys (user_id)`,
	`CREATE TABLE IF NOT EXISTS jamypg_db_profiles (
		id TEXT PRIMARY KEY,
		owner_id TEXT NOT NULL REFERENCES jamypg_users(id),
		definition JSONB NOT NULL,
		visibility TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private','shared')),
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE IF NOT EXISTS jamypg_profile_grants (
		profile_id TEXT NOT NULL REFERENCES jamypg_db_profiles(id) ON DELETE CASCADE,
		user_id TEXT NOT NULL REFERENCES jamypg_users(id) ON DELETE CASCADE,
		permission TEXT NOT NULL CHECK (permission IN ('use','manage')),
		granted_by TEXT NOT NULL DEFAULT '',
		granted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (profile_id, user_id)
	)`,
	`CREATE TABLE IF NOT EXISTS jamypg_settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL DEFAULT '',
		updated_by TEXT NOT NULL DEFAULT '',
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE IF NOT EXISTS jamypg_datasets (
		name TEXT PRIMARY KEY,
		content BYTEA NOT NULL,
		updated_by TEXT NOT NULL DEFAULT '',
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE IF NOT EXISTS jamypg_mcp_activity (
		id TEXT PRIMARY KEY,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		user_id TEXT NOT NULL DEFAULT '',
		username TEXT NOT NULL DEFAULT '',
		session_id TEXT NOT NULL DEFAULT '',
		tool TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL DEFAULT '',
		prompt TEXT NOT NULL DEFAULT '',
		sql_text TEXT NOT NULL DEFAULT '',
		profile TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT '',
		row_count INT NOT NULL DEFAULT 0,
		elapsed_ms BIGINT NOT NULL DEFAULT 0,
		params JSONB
	)`,
	`CREATE INDEX IF NOT EXISTS idx_mcp_activity_user ON jamypg_mcp_activity (user_id, created_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_mcp_activity_time ON jamypg_mcp_activity (created_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_mcp_activity_session ON jamypg_mcp_activity (session_id, created_at DESC)`,
}

// OpenPG connects, verifies, and migrates the meta database.
func OpenPG(ctx context.Context, dsn string) (*PGStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open meta db: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("meta db unreachable: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	for i, stmt := range migrations {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			return nil, fmt.Errorf("meta migration %d failed: %w", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &PGStore{db: db}, nil
}

func (p *PGStore) Close() { _ = p.db.Close() }

func isPGDuplicate(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate key")
}

// ---- users ----

const userCols = `id, username, display_name, email, COALESCE(password_hash,''), role, provider,
	COALESCE(provider_subject,''), is_active, created_at, updated_at, last_login_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var last sql.NullTime
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Email, &u.PasswordHash,
		&u.Role, &u.Provider, &u.ProviderSubject, &u.IsActive, &u.CreatedAt, &u.UpdatedAt, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if last.Valid {
		u.LastLoginAt = &last.Time
	}
	return &u, nil
}

func (p *PGStore) CreateUser(ctx context.Context, u *User) error {
	_, err := p.db.ExecContext(ctx, `INSERT INTO jamypg_users
		(id, username, display_name, email, password_hash, role, provider, provider_subject, is_active)
		VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,$7,NULLIF($8,''),$9)`,
		u.ID, u.Username, u.DisplayName, u.Email, u.PasswordHash, u.Role, u.Provider, u.ProviderSubject, u.IsActive)
	if isPGDuplicate(err) {
		return ErrDuplicate
	}
	return err
}

func (p *PGStore) GetUserByID(ctx context.Context, id string) (*User, error) {
	return scanUser(p.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM jamypg_users WHERE id=$1`, id))
}

func (p *PGStore) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	return scanUser(p.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM jamypg_users WHERE lower(username)=lower($1)`, username))
}

func (p *PGStore) GetUserByProviderSubject(ctx context.Context, provider, subject string) (*User, error) {
	return scanUser(p.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM jamypg_users WHERE provider=$1 AND provider_subject=$2`, provider, subject))
}

func (p *PGStore) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT `+userCols+` FROM jamypg_users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (p *PGStore) UpdateUser(ctx context.Context, u *User) error {
	// An empty PasswordHash means "leave the stored password unchanged" —
	// COALESCE(NULLIF(...)) keeps the existing hash instead of nulling it.
	res, err := p.db.ExecContext(ctx, `UPDATE jamypg_users SET
		display_name=$2, email=$3, password_hash=COALESCE(NULLIF($4,''), password_hash), role=$5, is_active=$6, updated_at=now()
		WHERE id=$1`,
		u.ID, u.DisplayName, u.Email, u.PasswordHash, u.Role, u.IsActive)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *PGStore) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jamypg_users`).Scan(&n)
	return n, err
}

func (p *PGStore) TouchLogin(ctx context.Context, userID string, at time.Time) error {
	_, err := p.db.ExecContext(ctx, `UPDATE jamypg_users SET last_login_at=$2 WHERE id=$1`, userID, at)
	return err
}

// ---- sessions ----

func (p *PGStore) CreateSession(ctx context.Context, s *Session) error {
	_, err := p.db.ExecContext(ctx, `INSERT INTO jamypg_sessions
		(token_hash, user_id, created_at, expires_at, ip, user_agent)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		s.TokenHash, s.UserID, s.CreatedAt, s.ExpiresAt, s.IP, s.UserAgent)
	return err
}

func (p *PGStore) GetSession(ctx context.Context, tokenHash string) (*Session, error) {
	var s Session
	var revoked sql.NullTime
	err := p.db.QueryRowContext(ctx, `SELECT token_hash, user_id, created_at, expires_at, ip, user_agent, revoked_at
		FROM jamypg_sessions WHERE token_hash=$1`, tokenHash).
		Scan(&s.TokenHash, &s.UserID, &s.CreatedAt, &s.ExpiresAt, &s.IP, &s.UserAgent, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if revoked.Valid {
		s.RevokedAt = &revoked.Time
	}
	return &s, nil
}

func (p *PGStore) ExtendSession(ctx context.Context, tokenHash string, until time.Time) error {
	_, err := p.db.ExecContext(ctx, `UPDATE jamypg_sessions SET expires_at=$2 WHERE token_hash=$1`, tokenHash, until)
	return err
}

func (p *PGStore) RevokeSession(ctx context.Context, tokenHash string) error {
	_, err := p.db.ExecContext(ctx, `UPDATE jamypg_sessions SET revoked_at=now() WHERE token_hash=$1 AND revoked_at IS NULL`, tokenHash)
	return err
}

func (p *PGStore) RevokeUserSessions(ctx context.Context, userID string) error {
	_, err := p.db.ExecContext(ctx, `UPDATE jamypg_sessions SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, userID)
	return err
}

// ---- mcp keys ----

const keyCols = `id, user_id, name, key_hash, key_prefix, created_at, expires_at, last_used_at, revoked_at, rotated_from`

func scanKey(row interface{ Scan(...any) error }) (*MCPKey, error) {
	var k MCPKey
	var exp, used, rev sql.NullTime
	err := row.Scan(&k.ID, &k.UserID, &k.Name, &k.KeyHash, &k.KeyPrefix, &k.CreatedAt, &exp, &used, &rev, &k.RotatedFrom)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if exp.Valid {
		k.ExpiresAt = &exp.Time
	}
	if used.Valid {
		k.LastUsedAt = &used.Time
	}
	if rev.Valid {
		k.RevokedAt = &rev.Time
	}
	return &k, nil
}

func (p *PGStore) CreateKey(ctx context.Context, k *MCPKey) error {
	_, err := p.db.ExecContext(ctx, `INSERT INTO jamypg_mcp_keys
		(id, user_id, name, key_hash, key_prefix, created_at, expires_at, rotated_from)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		k.ID, k.UserID, k.Name, k.KeyHash, k.KeyPrefix, k.CreatedAt, k.ExpiresAt, k.RotatedFrom)
	if isPGDuplicate(err) {
		return ErrDuplicate
	}
	return err
}

func (p *PGStore) GetKeyByHash(ctx context.Context, keyHash string) (*MCPKey, error) {
	return scanKey(p.db.QueryRowContext(ctx, `SELECT `+keyCols+` FROM jamypg_mcp_keys WHERE key_hash=$1`, keyHash))
}

func (p *PGStore) GetKeyByID(ctx context.Context, id string) (*MCPKey, error) {
	return scanKey(p.db.QueryRowContext(ctx, `SELECT `+keyCols+` FROM jamypg_mcp_keys WHERE id=$1`, id))
}

func (p *PGStore) ListKeys(ctx context.Context, userID string) ([]*MCPKey, error) {
	q := `SELECT ` + keyCols + ` FROM jamypg_mcp_keys`
	args := []any{}
	if userID != "" {
		q += ` WHERE user_id=$1`
		args = append(args, userID)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := p.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MCPKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (p *PGStore) RevokeKey(ctx context.Context, id string) error {
	res, err := p.db.ExecContext(ctx, `UPDATE jamypg_mcp_keys SET revoked_at=now() WHERE id=$1 AND revoked_at IS NULL`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *PGStore) TouchKey(ctx context.Context, id string, at time.Time) error {
	_, err := p.db.ExecContext(ctx, `UPDATE jamypg_mcp_keys SET last_used_at=$2 WHERE id=$1`, id, at)
	return err
}

// ---- profiles + grants ----

func (p *PGStore) ListProfiles(ctx context.Context) ([]*ProfileRecord, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, owner_id, definition, visibility, created_at, updated_at FROM jamypg_db_profiles ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ProfileRecord
	for rows.Next() {
		var r ProfileRecord
		if err := rows.Scan(&r.ID, &r.OwnerID, &r.Definition, &r.Visibility, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

func (p *PGStore) GetProfile(ctx context.Context, id string) (*ProfileRecord, error) {
	var r ProfileRecord
	err := p.db.QueryRowContext(ctx,
		`SELECT id, owner_id, definition, visibility, created_at, updated_at FROM jamypg_db_profiles WHERE id=$1`, id).
		Scan(&r.ID, &r.OwnerID, &r.Definition, &r.Visibility, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (p *PGStore) UpsertProfile(ctx context.Context, rec *ProfileRecord, create bool) error {
	if create {
		_, err := p.db.ExecContext(ctx, `INSERT INTO jamypg_db_profiles (id, owner_id, definition, visibility)
			VALUES ($1,$2,$3,$4)`, rec.ID, rec.OwnerID, rec.Definition, rec.Visibility)
		if isPGDuplicate(err) {
			return ErrDuplicate
		}
		return err
	}
	res, err := p.db.ExecContext(ctx, `UPDATE jamypg_db_profiles
		SET definition=$2, visibility=$3, updated_at=now() WHERE id=$1`,
		rec.ID, rec.Definition, rec.Visibility)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *PGStore) DeleteProfile(ctx context.Context, id string) error {
	res, err := p.db.ExecContext(ctx, `DELETE FROM jamypg_db_profiles WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *PGStore) ListGrants(ctx context.Context, profileID string) ([]Grant, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT profile_id, user_id, permission, granted_by, granted_at
		FROM jamypg_profile_grants WHERE profile_id=$1 ORDER BY user_id`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGrants(rows)
}

func (p *PGStore) ListGrantsForUser(ctx context.Context, userID string) ([]Grant, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT profile_id, user_id, permission, granted_by, granted_at
		FROM jamypg_profile_grants WHERE user_id=$1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGrants(rows)
}

func scanGrants(rows *sql.Rows) ([]Grant, error) {
	out := []Grant{}
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.ProfileID, &g.UserID, &g.Permission, &g.GrantedBy, &g.GrantedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (p *PGStore) SetGrant(ctx context.Context, g Grant) error {
	_, err := p.db.ExecContext(ctx, `INSERT INTO jamypg_profile_grants (profile_id, user_id, permission, granted_by)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (profile_id, user_id) DO UPDATE SET permission=EXCLUDED.permission, granted_by=EXCLUDED.granted_by, granted_at=now()`,
		g.ProfileID, g.UserID, g.Permission, g.GrantedBy)
	if err != nil && strings.Contains(err.Error(), "violates foreign key") {
		return ErrNotFound
	}
	return err
}

func (p *PGStore) RemoveGrant(ctx context.Context, profileID, userID string) error {
	res, err := p.db.ExecContext(ctx, `DELETE FROM jamypg_profile_grants WHERE profile_id=$1 AND user_id=$2`, profileID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- settings ----

func (p *PGStore) GetSettings(ctx context.Context) (map[string]string, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT key, value FROM jamypg_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (p *PGStore) SetSetting(ctx context.Context, key, value, updatedBy string) error {
	_, err := p.db.ExecContext(ctx, `INSERT INTO jamypg_settings (key, value, updated_by)
		VALUES ($1,$2,$3)
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_by=EXCLUDED.updated_by, updated_at=now()`,
		key, value, updatedBy)
	return err
}

func (p *PGStore) DeleteSetting(ctx context.Context, key string) error {
	_, err := p.db.ExecContext(ctx, `DELETE FROM jamypg_settings WHERE key=$1`, key)
	return err
}

// ---- datasets ----

func (p *PGStore) GetDataset(ctx context.Context, name string) (*DatasetRow, error) {
	var d DatasetRow
	err := p.db.QueryRowContext(ctx, `SELECT name, content, updated_by, updated_at FROM jamypg_datasets WHERE name=$1`, name).
		Scan(&d.Name, &d.Content, &d.UpdatedBy, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (p *PGStore) ListDatasets(ctx context.Context) ([]*DatasetRow, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT name, content, updated_by, updated_at FROM jamypg_datasets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DatasetRow
	for rows.Next() {
		var d DatasetRow
		if err := rows.Scan(&d.Name, &d.Content, &d.UpdatedBy, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

func (p *PGStore) PutDataset(ctx context.Context, name string, content []byte, updatedBy string) error {
	_, err := p.db.ExecContext(ctx, `INSERT INTO jamypg_datasets (name, content, updated_by)
		VALUES ($1,$2,$3)
		ON CONFLICT (name) DO UPDATE SET content=EXCLUDED.content, updated_by=EXCLUDED.updated_by, updated_at=now()`,
		name, content, updatedBy)
	return err
}

func (p *PGStore) DeleteDataset(ctx context.Context, name string) error {
	_, err := p.db.ExecContext(ctx, `DELETE FROM jamypg_datasets WHERE name=$1`, name)
	return err
}

func (p *PGStore) RecordActivity(ctx context.Context, a *MCPActivity) error {
	var params any
	if len(a.Params) > 0 {
		params = []byte(a.Params)
	}
	_, err := p.db.ExecContext(ctx, `INSERT INTO jamypg_mcp_activity
		(id, user_id, username, session_id, tool, kind, prompt, sql_text, profile, status, row_count, elapsed_ms, params)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		a.ID, a.UserID, a.Username, a.SessionID, a.Tool, a.Kind, a.Prompt, a.SQL, a.Profile, a.Status, a.RowCount, a.ElapsedMs, params)
	return err
}

func (p *PGStore) ListActivity(ctx context.Context, f ActivityFilter) ([]*MCPActivity, error) {
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := `SELECT id, created_at, user_id, username, session_id, tool, kind, prompt, sql_text, profile, status, row_count, elapsed_ms, params
		FROM jamypg_mcp_activity`
	args := []any{}
	if f.UserID != "" {
		q += ` WHERE user_id=$1`
		args = append(args, f.UserID)
	}
	q += fmt.Sprintf(` ORDER BY created_at DESC LIMIT %d`, limit)
	rows, err := p.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*MCPActivity{}
	for rows.Next() {
		a := &MCPActivity{}
		var params []byte
		if err := rows.Scan(&a.ID, &a.CreatedAt, &a.UserID, &a.Username, &a.SessionID, &a.Tool, &a.Kind,
			&a.Prompt, &a.SQL, &a.Profile, &a.Status, &a.RowCount, &a.ElapsedMs, &params); err != nil {
			return nil, err
		}
		if len(params) > 0 {
			a.Params = params
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (p *PGStore) LastPromptForSession(ctx context.Context, sessionID, userID string) (string, error) {
	var prompt string
	err := p.db.QueryRowContext(ctx, `SELECT prompt FROM jamypg_mcp_activity
		WHERE kind=$1 AND prompt<>'' AND (($2<>'' AND session_id=$2) OR ($2='' AND user_id=$3))
		ORDER BY created_at DESC LIMIT 1`, ActivityPrompt, sessionID, userID).Scan(&prompt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return prompt, err
}
