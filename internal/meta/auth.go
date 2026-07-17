package meta

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	SessionTTL          = 24 * time.Hour
	SessionCookie       = "sqlon_session"
	LegacySessionCookie = "jamypg_session"
	keyPrefixMagic      = "ssk_" // SQLON secret key
	legacyKeyPrefix     = "jsk_"
)

// IsMCPKeyFormat recognizes current SQLON keys and the one-release JAMYPG
// compatibility prefix. It checks only format; AuthenticateKey verifies the
// stored hash, owner, expiry and revocation state.
func IsMCPKeyFormat(raw string) bool {
	raw = strings.TrimSpace(raw)
	return strings.HasPrefix(raw, keyPrefixMagic) || strings.HasPrefix(raw, legacyKeyPrefix)
}

// Service is the authentication/authorization facade the HTTP layer uses.
type Service struct {
	Store Store
}

func NewService(store Store) *Service { return &Service{Store: store} }

func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(b[:])
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// ---- users ----

func HashPassword(plain string) (string, error) {
	if len(plain) < 8 {
		return "", errors.New("password must be at least 8 characters")
	}
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(b), err
}

// CreateLocalUser registers a username/password account.
func (s *Service) CreateLocalUser(ctx context.Context, username, password, role, displayName, email string) (*User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, errors.New("username is required")
	}
	if role != RoleAdmin && role != RoleUser && role != RoleDBA {
		role = RoleUser
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	u := &User{
		ID: NewID(), Username: username, DisplayName: displayName, Email: email,
		PasswordHash: hash, Role: role, Provider: ProviderLocal, IsActive: true,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.Store.CreateUser(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// UpsertSSOUser finds-or-creates the account for an OIDC subject. The first
// user ever created becomes admin (bootstrap); afterwards SSO users join as
// role=user and admins promote them in the console.
func (s *Service) UpsertSSOUser(ctx context.Context, subject, username, displayName, email string) (*User, error) {
	if u, err := s.Store.GetUserByProviderSubject(ctx, ProviderKeycloak, subject); err == nil {
		if !u.IsActive {
			return nil, ErrInactive
		}
		return u, nil
	}
	role := RoleUser
	if n, err := s.Store.CountUsers(ctx); err == nil && n == 0 {
		role = RoleAdmin
	}
	if username == "" {
		username = "kc-" + subject
	}
	u := &User{
		ID: NewID(), Username: username, DisplayName: displayName, Email: email,
		Role: role, Provider: ProviderKeycloak, ProviderSubject: subject, IsActive: true,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	err := s.Store.CreateUser(ctx, u)
	if errors.Is(err, ErrDuplicate) {
		// username collision with a local account — make it unique
		u.Username = username + "-" + u.ID[:6]
		err = s.Store.CreateUser(ctx, u)
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// Bootstrap ensures at least one admin exists when the meta DB is fresh.
// spec is "username:password"; empty spec generates a random password which
// is returned exactly once for the operator to record.
func (s *Service) Bootstrap(ctx context.Context, spec string) (username, generatedPassword string, created bool, err error) {
	n, err := s.Store.CountUsers(ctx)
	if err != nil || n > 0 {
		return "", "", false, err
	}
	username = "admin"
	password := ""
	if spec != "" {
		parts := strings.SplitN(spec, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", "", false, errors.New("bootstrap-admin must be username:password")
		}
		username, password = parts[0], parts[1]
	} else {
		var b [12]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", "", false, err
		}
		password = hex.EncodeToString(b[:])
		generatedPassword = password
	}
	if _, err := s.CreateLocalUser(ctx, username, password, RoleAdmin, "Administrator", ""); err != nil {
		return "", "", false, err
	}
	return username, generatedPassword, true, nil
}

// ---- login / sessions ----

// VerifyPassword checks a local account's password without creating a
// session (used for re-auth flows like password change).
func (s *Service) VerifyPassword(ctx context.Context, u *User, password string) error {
	if u == nil || u.Provider != ProviderLocal || u.PasswordHash == "" {
		return ErrUnauthorized
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return ErrUnauthorized
	}
	return nil
}

// Login verifies a local account and issues a session token (returned raw;
// only its hash is stored).
func (s *Service) Login(ctx context.Context, username, password, ip, userAgent string) (*User, string, error) {
	u, err := s.Store.GetUserByUsername(ctx, username)
	if err != nil {
		// burn comparable time so username probing is not measurably faster
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$7EqJtq98hPqEX7fNZaFWoOhi5B0G1Ck7t6mJ1oQ3o7ZBq0Wl0y0y."), []byte(password))
		return nil, "", ErrUnauthorized
	}
	if u.Provider != ProviderLocal || u.PasswordHash == "" {
		return nil, "", errors.New("this account signs in via SSO")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return nil, "", ErrUnauthorized
	}
	if !u.IsActive {
		return nil, "", ErrInactive
	}
	token, err := s.IssueSession(ctx, u.ID, ip, userAgent)
	if err != nil {
		return nil, "", err
	}
	_ = s.Store.TouchLogin(ctx, u.ID, time.Now())
	return u, token, nil
}

// IssueSession creates a session for an already-authenticated user (SSO).
func (s *Service) IssueSession(ctx context.Context, userID, ip, userAgent string) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b[:])
	sess := &Session{
		TokenHash: hashToken(token), UserID: userID,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(SessionTTL),
		IP: ip, UserAgent: userAgent,
	}
	if err := s.Store.CreateSession(ctx, sess); err != nil {
		return "", err
	}
	return token, nil
}

// Authenticate resolves a raw session token to its active user, sliding the
// expiry forward when less than half the TTL remains.
func (s *Service) Authenticate(ctx context.Context, token string) (*User, error) {
	if token == "" {
		return nil, ErrUnauthorized
	}
	sess, err := s.Store.GetSession(ctx, hashToken(token))
	if err != nil {
		return nil, ErrUnauthorized
	}
	now := time.Now()
	if sess.RevokedAt != nil {
		return nil, ErrRevoked
	}
	if now.After(sess.ExpiresAt) {
		return nil, ErrExpired
	}
	u, err := s.Store.GetUserByID(ctx, sess.UserID)
	if err != nil || !u.IsActive {
		return nil, ErrUnauthorized
	}
	if sess.ExpiresAt.Sub(now) < SessionTTL/2 {
		_ = s.Store.ExtendSession(ctx, sess.TokenHash, now.Add(SessionTTL))
	}
	return u, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.Store.RevokeSession(ctx, hashToken(token))
}

// ---- MCP key lifecycle ----

// CreateMCPKey issues a new API key. The raw key ("ssk_<64hex>") is returned
// exactly once; ttl<=0 means no expiry.
func (s *Service) CreateMCPKey(ctx context.Context, userID, name string, ttl time.Duration) (string, *MCPKey, error) {
	return s.createKey(ctx, userID, name, ttl, "")
}

func (s *Service) createKey(ctx context.Context, userID, name string, ttl time.Duration, rotatedFrom string) (string, *MCPKey, error) {
	if strings.TrimSpace(name) == "" {
		name = "default"
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, err
	}
	raw := keyPrefixMagic + hex.EncodeToString(b[:])
	k := &MCPKey{
		ID: NewID(), UserID: userID, Name: name,
		KeyHash: hashToken(raw), KeyPrefix: raw[:12],
		CreatedAt: time.Now(), RotatedFrom: rotatedFrom,
	}
	if ttl > 0 {
		exp := time.Now().Add(ttl)
		k.ExpiresAt = &exp
	}
	if err := s.Store.CreateKey(ctx, k); err != nil {
		return "", nil, err
	}
	return raw, k, nil
}

// AuthenticateKey resolves a raw MCP key to its active user, enforcing
// revocation and expiry, and records last use.
func (s *Service) AuthenticateKey(ctx context.Context, raw string) (*User, *MCPKey, error) {
	raw = strings.TrimSpace(raw)
	if !IsMCPKeyFormat(raw) {
		return nil, nil, ErrUnauthorized
	}
	k, err := s.Store.GetKeyByHash(ctx, hashToken(raw))
	if err != nil {
		return nil, nil, ErrUnauthorized
	}
	if subtle.ConstantTimeCompare([]byte(k.KeyHash), []byte(hashToken(raw))) != 1 {
		return nil, nil, ErrUnauthorized
	}
	now := time.Now()
	switch k.Status(now) {
	case "revoked":
		return nil, nil, ErrRevoked
	case "expired":
		return nil, nil, ErrExpired
	}
	u, err := s.Store.GetUserByID(ctx, k.UserID)
	if err != nil || !u.IsActive {
		return nil, nil, ErrUnauthorized
	}
	_ = s.Store.TouchKey(ctx, k.ID, now)
	return u, k, nil
}

// RotateMCPKey revokes the old key and issues a replacement with the same
// name/owner/TTL policy, returning the new raw key once.
func (s *Service) RotateMCPKey(ctx context.Context, keyID string) (string, *MCPKey, error) {
	old, err := s.Store.GetKeyByID(ctx, keyID)
	if err != nil {
		return "", nil, err
	}
	if err := s.Store.RevokeKey(ctx, old.ID); err != nil && !errors.Is(err, ErrNotFound) {
		return "", nil, err
	}
	ttl := time.Duration(0)
	if old.ExpiresAt != nil {
		if remaining := time.Until(*old.ExpiresAt); remaining > 0 {
			ttl = remaining
		} else {
			ttl = SessionTTL // 만료 임박/경과 키 회전 시 최소 유효기간 부여
		}
	}
	return s.createKey(ctx, old.UserID, old.Name, ttl, old.ID)
}
