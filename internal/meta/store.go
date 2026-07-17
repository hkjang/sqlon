package meta

import (
	"context"
	"time"
)

// Store abstracts the metadata persistence so the auth service and HTTP
// handlers are testable with MemStore and production runs on PGStore.
type Store interface {
	// users
	CreateUser(ctx context.Context, u *User) error
	GetUserByID(ctx context.Context, id string) (*User, error)
	GetUserByUsername(ctx context.Context, username string) (*User, error)
	GetUserByProviderSubject(ctx context.Context, provider, subject string) (*User, error)
	ListUsers(ctx context.Context) ([]*User, error)
	UpdateUser(ctx context.Context, u *User) error
	CountUsers(ctx context.Context) (int, error)
	TouchLogin(ctx context.Context, userID string, at time.Time) error

	// sessions
	CreateSession(ctx context.Context, s *Session) error
	GetSession(ctx context.Context, tokenHash string) (*Session, error)
	ExtendSession(ctx context.Context, tokenHash string, until time.Time) error
	RevokeSession(ctx context.Context, tokenHash string) error
	RevokeUserSessions(ctx context.Context, userID string) error

	// mcp keys
	CreateKey(ctx context.Context, k *MCPKey) error
	GetKeyByHash(ctx context.Context, keyHash string) (*MCPKey, error)
	GetKeyByID(ctx context.Context, id string) (*MCPKey, error)
	ListKeys(ctx context.Context, userID string) ([]*MCPKey, error) // ""=all users
	RevokeKey(ctx context.Context, id string) error
	TouchKey(ctx context.Context, id string, at time.Time) error

	// db profiles + grants
	ListProfiles(ctx context.Context) ([]*ProfileRecord, error)
	GetProfile(ctx context.Context, id string) (*ProfileRecord, error)
	UpsertProfile(ctx context.Context, rec *ProfileRecord, create bool) error
	DeleteProfile(ctx context.Context, id string) error
	ListGrants(ctx context.Context, profileID string) ([]Grant, error)
	ListGrantsForUser(ctx context.Context, userID string) ([]Grant, error)
	SetGrant(ctx context.Context, g Grant) error
	RemoveGrant(ctx context.Context, profileID, userID string) error

	// settings (runtime-tunable options)
	GetSettings(ctx context.Context) (map[string]string, error)
	SetSetting(ctx context.Context, key, value, updatedBy string) error
	DeleteSetting(ctx context.Context, key string) error

	// datasets (PG as source of truth for catalog JSON)
	GetDataset(ctx context.Context, name string) (*DatasetRow, error)
	ListDatasets(ctx context.Context) ([]*DatasetRow, error)
	PutDataset(ctx context.Context, name string, content []byte, updatedBy string) error
	DeleteDataset(ctx context.Context, name string) error

	// MCP activity history (personalized query/execution log)
	RecordActivity(ctx context.Context, a *MCPActivity) error
	ListActivity(ctx context.Context, f ActivityFilter) ([]*MCPActivity, error)
	LastPromptForSession(ctx context.Context, sessionID, userID string) (string, error)

	Close()
}

// DatasetRow is a catalog dataset JSON stored in the meta DB.
type DatasetRow struct {
	Name      string    `json:"name"`
	Content   []byte    `json:"-"`
	UpdatedBy string    `json:"updated_by,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}
