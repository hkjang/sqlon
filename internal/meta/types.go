// Package meta implements the Postgres-backed metadata store: users,
// sessions, MCP API keys, per-user DB profiles with grants, and the
// authentication service on top of them. The whole subsystem is optional —
// when no meta DB is configured the server runs in standalone (file-based,
// auth-disabled) mode exactly as before.
package meta

import (
	"encoding/json"
	"errors"
	"time"
)

const (
	RoleAdmin = "admin"
	RoleDBA   = "dba"
	RoleUser  = "user"

	ProviderLocal    = "local"
	ProviderKeycloak = "keycloak"

	VisibilityPrivate = "private"
	VisibilityShared  = "shared"

	PermUse    = "use"
	PermManage = "manage"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrDuplicate    = errors.New("already exists")
	ErrUnauthorized = errors.New("invalid credentials")
	ErrInactive     = errors.New("account is deactivated")
	ErrExpired      = errors.New("expired")
	ErrRevoked      = errors.New("revoked")
)

type User struct {
	ID              string     `json:"id"`
	Username        string     `json:"username"`
	DisplayName     string     `json:"display_name,omitempty"`
	Email           string     `json:"email,omitempty"`
	PasswordHash    string     `json:"-"`
	Role            string     `json:"role"`
	Provider        string     `json:"provider"`
	ProviderSubject string     `json:"-"`
	IsActive        bool       `json:"is_active"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	LastLoginAt     *time.Time `json:"last_login_at,omitempty"`
}

func (u *User) IsAdmin() bool { return u != nil && u.Role == RoleAdmin }

// IsDBA reports whether the user may perform privileged DBA operations. Admins
// are a superset — they implicitly hold the DBA capability.
func (u *User) IsDBA() bool { return u != nil && (u.Role == RoleDBA || u.Role == RoleAdmin) }

type Session struct {
	TokenHash string     `json:"-"`
	UserID    string     `json:"user_id"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	IP        string     `json:"ip,omitempty"`
	UserAgent string     `json:"user_agent,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// MCPKey is an API key for MCP/REST access. Only the SHA-256 hash is stored;
// the raw key is shown exactly once at creation/rotation.
type MCPKey struct {
	ID          string     `json:"id"`
	UserID      string     `json:"user_id"`
	Name        string     `json:"name"`
	KeyHash     string     `json:"-"`
	KeyPrefix   string     `json:"key_prefix"` // display-only, e.g. "ssk_ab12cd34"
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	RotatedFrom string     `json:"rotated_from,omitempty"`
}

// Status derives the lifecycle state for display.
func (k *MCPKey) Status(now time.Time) string {
	switch {
	case k.RevokedAt != nil:
		return "revoked"
	case k.ExpiresAt != nil && now.After(*k.ExpiresAt):
		return "expired"
	default:
		return "active"
	}
}

// ProfileRecord is a DB connection profile owned by a user. Definition holds
// the dbconn.Profile JSON verbatim so the meta store stays decoupled from the
// connector package.
type ProfileRecord struct {
	ID         string    `json:"id"`
	OwnerID    string    `json:"owner_id"`
	Definition []byte    `json:"-"`
	Visibility string    `json:"visibility"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type Grant struct {
	ProfileID  string    `json:"profile_id"`
	UserID     string    `json:"user_id"`
	Permission string    `json:"permission"` // use | manage
	GrantedBy  string    `json:"granted_by"`
	GrantedAt  time.Time `json:"granted_at"`
}

// MCPActivity is one recorded MCP tool call by an authenticated user: the
// driving prompt, the SQL generated/executed, and whatever parameters the agent
// sent. It powers the personalized query-history page.
type MCPActivity struct {
	ID        string          `json:"id"`
	CreatedAt time.Time       `json:"created_at"`
	UserID    string          `json:"user_id"`
	Username  string          `json:"username"`
	SessionID string          `json:"session_id,omitempty"`
	Tool      string          `json:"tool"`
	Kind      string          `json:"kind"`             // prompt | generate | execute
	Prompt    string          `json:"prompt,omitempty"` // driving user question (correlated)
	SQL       string          `json:"sql,omitempty"`
	Profile   string          `json:"profile,omitempty"`
	Status    string          `json:"status,omitempty"`
	RowCount  int             `json:"row_count,omitempty"`
	ElapsedMs int64           `json:"elapsed_ms,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"` // agent _meta + tool arguments
}

const (
	ActivityPrompt   = "prompt"
	ActivityGenerate = "generate"
	ActivityExecute  = "execute"
)

// ActivityFilter narrows a history query. UserID="" means all users (admin).
type ActivityFilter struct {
	UserID string
	Limit  int
}

// ---- authorization helpers ----

// CanUseProfile: admin, owner, shared visibility, or any grant.
func CanUseProfile(u *User, rec ProfileRecord, grants []Grant) bool {
	if u == nil {
		return false
	}
	if u.IsAdmin() || rec.OwnerID == u.ID || rec.Visibility == VisibilityShared {
		return true
	}
	for _, g := range grants {
		if g.UserID == u.ID {
			return true
		}
	}
	return false
}

// CanManageProfile: admin, owner, or an explicit manage grant.
func CanManageProfile(u *User, rec ProfileRecord, grants []Grant) bool {
	if u == nil {
		return false
	}
	if u.IsAdmin() || rec.OwnerID == u.ID {
		return true
	}
	for _, g := range grants {
		if g.UserID == u.ID && g.Permission == PermManage {
			return true
		}
	}
	return false
}
