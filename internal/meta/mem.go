package meta

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemStore is an in-memory Store used by unit tests (and nothing else — the
// server refuses to run auth without a durable store).
type MemStore struct {
	mu       sync.Mutex
	users    map[string]*User
	sessions map[string]*Session
	keys     map[string]*MCPKey
	profiles map[string]*ProfileRecord
	grants   map[string]map[string]Grant // profileID -> userID -> grant
	settings map[string]string
	datasets map[string]*DatasetRow
	activity []*MCPActivity // newest last
}

func NewMemStore() *MemStore {
	return &MemStore{
		users:    map[string]*User{},
		sessions: map[string]*Session{},
		keys:     map[string]*MCPKey{},
		profiles: map[string]*ProfileRecord{},
		grants:   map[string]map[string]Grant{},
		settings: map[string]string{},
		datasets: map[string]*DatasetRow{},
	}
}

func (m *MemStore) Close() {}

func cloneUser(u *User) *User { c := *u; return &c }

func (m *MemStore) CreateUser(_ context.Context, u *User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.users {
		if strings.EqualFold(e.Username, u.Username) {
			return ErrDuplicate
		}
		if u.ProviderSubject != "" && e.Provider == u.Provider && e.ProviderSubject == u.ProviderSubject {
			return ErrDuplicate
		}
	}
	m.users[u.ID] = cloneUser(u)
	return nil
}

func (m *MemStore) GetUserByID(_ context.Context, id string) (*User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.users[id]; ok {
		return cloneUser(u), nil
	}
	return nil, ErrNotFound
}

func (m *MemStore) GetUserByUsername(_ context.Context, username string) (*User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if strings.EqualFold(u.Username, username) {
			return cloneUser(u), nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemStore) GetUserByProviderSubject(_ context.Context, provider, subject string) (*User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if u.Provider == provider && u.ProviderSubject == subject {
			return cloneUser(u), nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemStore) ListUsers(_ context.Context) ([]*User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*User, 0, len(m.users))
	for _, u := range m.users {
		out = append(out, cloneUser(u))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out, nil
}

func (m *MemStore) UpdateUser(_ context.Context, u *User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev, ok := m.users[u.ID]
	if !ok {
		return ErrNotFound
	}
	// Mirror PGStore's NULLIF($password,'') semantics: an empty PasswordHash
	// means "leave the stored hash unchanged".
	if u.PasswordHash == "" {
		u.PasswordHash = prev.PasswordHash
	}
	u.UpdatedAt = time.Now()
	m.users[u.ID] = cloneUser(u)
	return nil
}

func (m *MemStore) CountUsers(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.users), nil
}

func (m *MemStore) TouchLogin(_ context.Context, userID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.users[userID]; ok {
		u.LastLoginAt = &at
		return nil
	}
	return ErrNotFound
}

// ---- sessions ----

func (m *MemStore) CreateSession(_ context.Context, s *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := *s
	m.sessions[s.TokenHash] = &c
	return nil
}

func (m *MemStore) GetSession(_ context.Context, tokenHash string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[tokenHash]; ok {
		c := *s
		return &c, nil
	}
	return nil, ErrNotFound
}

func (m *MemStore) ExtendSession(_ context.Context, tokenHash string, until time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[tokenHash]; ok {
		s.ExpiresAt = until
		return nil
	}
	return ErrNotFound
}

func (m *MemStore) RevokeSession(_ context.Context, tokenHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[tokenHash]; ok {
		now := time.Now()
		s.RevokedAt = &now
		return nil
	}
	return ErrNotFound
}

func (m *MemStore) RevokeUserSessions(_ context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for _, s := range m.sessions {
		if s.UserID == userID && s.RevokedAt == nil {
			s.RevokedAt = &now
		}
	}
	return nil
}

// ---- keys ----

func (m *MemStore) CreateKey(_ context.Context, k *MCPKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := *k
	m.keys[k.ID] = &c
	return nil
}

func (m *MemStore) GetKeyByHash(_ context.Context, keyHash string) (*MCPKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range m.keys {
		if k.KeyHash == keyHash {
			c := *k
			return &c, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemStore) GetKeyByID(_ context.Context, id string) (*MCPKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if k, ok := m.keys[id]; ok {
		c := *k
		return &c, nil
	}
	return nil, ErrNotFound
}

func (m *MemStore) ListKeys(_ context.Context, userID string) ([]*MCPKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []*MCPKey{}
	for _, k := range m.keys {
		if userID == "" || k.UserID == userID {
			c := *k
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *MemStore) RevokeKey(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if k, ok := m.keys[id]; ok {
		now := time.Now()
		k.RevokedAt = &now
		return nil
	}
	return ErrNotFound
}

func (m *MemStore) TouchKey(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if k, ok := m.keys[id]; ok {
		k.LastUsedAt = &at
		return nil
	}
	return ErrNotFound
}

// ---- profiles + grants ----

func (m *MemStore) ListProfiles(_ context.Context) ([]*ProfileRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []*ProfileRecord{}
	for _, p := range m.profiles {
		c := *p
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) GetProfile(_ context.Context, id string) (*ProfileRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.profiles[id]; ok {
		c := *p
		return &c, nil
	}
	return nil, ErrNotFound
}

func (m *MemStore) UpsertProfile(_ context.Context, rec *ProfileRecord, create bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, exists := m.profiles[rec.ID]
	if create && exists {
		return ErrDuplicate
	}
	if !create && !exists {
		return ErrNotFound
	}
	c := *rec
	c.UpdatedAt = time.Now()
	if !exists {
		c.CreatedAt = c.UpdatedAt
	}
	m.profiles[rec.ID] = &c
	return nil
}

func (m *MemStore) DeleteProfile(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.profiles[id]; !ok {
		return ErrNotFound
	}
	delete(m.profiles, id)
	delete(m.grants, id)
	return nil
}

func (m *MemStore) ListGrants(_ context.Context, profileID string) ([]Grant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Grant{}
	for _, g := range m.grants[profileID] {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out, nil
}

func (m *MemStore) ListGrantsForUser(_ context.Context, userID string) ([]Grant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Grant{}
	for _, byUser := range m.grants {
		if g, ok := byUser[userID]; ok {
			out = append(out, g)
		}
	}
	return out, nil
}

func (m *MemStore) SetGrant(_ context.Context, g Grant) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.profiles[g.ProfileID]; !ok {
		return ErrNotFound
	}
	if m.grants[g.ProfileID] == nil {
		m.grants[g.ProfileID] = map[string]Grant{}
	}
	g.GrantedAt = time.Now()
	m.grants[g.ProfileID][g.UserID] = g
	return nil
}

func (m *MemStore) RemoveGrant(_ context.Context, profileID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if byUser, ok := m.grants[profileID]; ok {
		if _, ok := byUser[userID]; ok {
			delete(byUser, userID)
			return nil
		}
	}
	return ErrNotFound
}

// ---- datasets ----

func (m *MemStore) GetDataset(_ context.Context, name string) (*DatasetRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.datasets[name]; ok {
		c := *d
		c.Content = append([]byte(nil), d.Content...)
		return &c, nil
	}
	return nil, ErrNotFound
}

func (m *MemStore) ListDatasets(_ context.Context) ([]*DatasetRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []*DatasetRow{}
	for _, d := range m.datasets {
		c := *d
		c.Content = append([]byte(nil), d.Content...)
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *MemStore) PutDataset(_ context.Context, name string, content []byte, updatedBy string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.datasets[name] = &DatasetRow{Name: name, Content: append([]byte(nil), content...), UpdatedBy: updatedBy, UpdatedAt: time.Now()}
	return nil
}

func (m *MemStore) DeleteDataset(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.datasets[name]; !ok {
		return ErrNotFound
	}
	delete(m.datasets, name)
	return nil
}

// ---- MCP activity ----

func (m *MemStore) RecordActivity(_ context.Context, a *MCPActivity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := *a
	m.activity = append(m.activity, &c)
	return nil
}

func (m *MemStore) ListActivity(_ context.Context, f ActivityFilter) ([]*MCPActivity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	out := []*MCPActivity{}
	for i := len(m.activity) - 1; i >= 0 && len(out) < limit; i-- {
		if f.UserID != "" && m.activity[i].UserID != f.UserID {
			continue
		}
		c := *m.activity[i]
		out = append(out, &c)
	}
	return out, nil
}

func (m *MemStore) LastPromptForSession(_ context.Context, sessionID, userID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.activity) - 1; i >= 0; i-- {
		a := m.activity[i]
		if a.Kind != ActivityPrompt || a.Prompt == "" {
			continue
		}
		if sessionID != "" {
			if a.SessionID == sessionID {
				return a.Prompt, nil
			}
		} else if a.UserID == userID {
			return a.Prompt, nil
		}
	}
	return "", nil
}
