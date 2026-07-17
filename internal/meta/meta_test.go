package meta

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func newSvc(t *testing.T) (*Service, context.Context) {
	t.Helper()
	return NewService(NewMemStore()), t.Context()
}

func TestBootstrapAndLogin(t *testing.T) {
	s, ctx := newSvc(t)
	user, gen, created, err := s.Bootstrap(ctx, "")
	if err != nil || !created || user != "admin" || len(gen) < 12 {
		t.Fatalf("bootstrap: %v %q %q %v", err, user, gen, created)
	}
	// 두 번째 부트스트랩은 no-op
	if _, _, created, _ := s.Bootstrap(ctx, "x:yyyyyyyy"); created {
		t.Fatal("bootstrap must be idempotent")
	}
	// 로그인 성공 → 세션 인증
	u, token, err := s.Login(ctx, "admin", gen, "127.0.0.1", "test")
	if err != nil || !u.IsAdmin() || token == "" {
		t.Fatalf("login: %v", err)
	}
	got, err := s.Authenticate(ctx, token)
	if err != nil || got.ID != u.ID {
		t.Fatalf("authenticate: %v", err)
	}
	// 잘못된 비밀번호
	if _, _, err := s.Login(ctx, "admin", "wrong-password", "", ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong password must be unauthorized: %v", err)
	}
	// 로그아웃 → 세션 폐기
	if err := s.Logout(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, token); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked session must fail: %v", err)
	}
}

func TestBootstrapWithSpec(t *testing.T) {
	s, ctx := newSvc(t)
	user, gen, created, err := s.Bootstrap(ctx, "boss:supersecret1")
	if err != nil || !created || user != "boss" || gen != "" {
		t.Fatalf("bootstrap spec: %v %q %q", err, user, gen)
	}
	if _, _, err := s.Login(ctx, "boss", "supersecret1", "", ""); err != nil {
		t.Fatalf("login with spec password: %v", err)
	}
	if _, _, _, err := NewService(NewMemStore()).Bootstrap(t.Context(), "bad-spec"); err == nil {
		t.Fatal("malformed spec must error")
	}
}

func TestInactiveUserBlocked(t *testing.T) {
	s, ctx := newSvc(t)
	u, err := s.CreateLocalUser(ctx, "worker", "password1", RoleUser, "", "")
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := s.Login(ctx, "worker", "password1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	u.IsActive = false
	if err := s.Store.UpdateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, token); err == nil {
		t.Fatal("deactivated user's session must fail")
	}
	if _, _, err := s.Login(ctx, "worker", "password1", "", ""); !errors.Is(err, ErrInactive) {
		t.Fatalf("deactivated login must be ErrInactive: %v", err)
	}
}

func TestSSOUpsert(t *testing.T) {
	s, ctx := newSvc(t)
	// 첫 SSO 사용자는 admin (부트스트랩)
	u1, err := s.UpsertSSOUser(ctx, "sub-1", "kc-user", "KC User", "u@x.com")
	if err != nil || u1.Role != RoleAdmin || u1.Provider != ProviderKeycloak {
		t.Fatalf("first sso user should be admin: %+v %v", u1, err)
	}
	// 재로그인은 같은 계정
	again, err := s.UpsertSSOUser(ctx, "sub-1", "kc-user", "", "")
	if err != nil || again.ID != u1.ID {
		t.Fatalf("sso re-login must reuse account: %v", err)
	}
	// 두 번째 subject는 user + username 충돌 시 유니크 처리
	u2, err := s.UpsertSSOUser(ctx, "sub-2", "kc-user", "", "")
	if err != nil || u2.Role != RoleUser || u2.Username == u1.Username {
		t.Fatalf("second sso user: %+v %v", u2, err)
	}
	// SSO 계정은 로컬 로그인 불가
	if _, _, err := s.Login(ctx, u1.Username, "whatever12", "", ""); err == nil {
		t.Fatal("sso account must not local-login")
	}
}

func TestMCPKeyLifecycle(t *testing.T) {
	s, ctx := newSvc(t)
	u, _ := s.CreateLocalUser(ctx, "keyuser", "password1", RoleUser, "", "")

	raw, k, err := s.CreateMCPKey(ctx, u.ID, "ci-key", time.Hour)
	if err != nil || len(raw) < 20 || k.KeyPrefix != raw[:12] {
		t.Fatalf("create key: %v", err)
	}
	if !strings.HasPrefix(raw, "ssk_") {
		t.Fatalf("new SQLON key prefix: %q", raw)
	}
	// 인증 + last_used 기록
	gotU, gotK, err := s.AuthenticateKey(ctx, raw)
	if err != nil || gotU.ID != u.ID || gotK.ID != k.ID {
		t.Fatalf("authenticate key: %v", err)
	}
	if k2, _ := s.Store.GetKeyByID(ctx, k.ID); k2.LastUsedAt == nil {
		t.Fatal("last_used_at must be recorded")
	}
	// 회전: 구 키 폐기 + 신 키 발급 + rotated_from 연결
	newRaw, nk, err := s.RotateMCPKey(ctx, k.ID)
	if err != nil || nk.RotatedFrom != k.ID || newRaw == raw {
		t.Fatalf("rotate: %v %+v", err, nk)
	}
	if _, _, err := s.AuthenticateKey(ctx, raw); !errors.Is(err, ErrRevoked) {
		t.Fatalf("old key must be revoked after rotation: %v", err)
	}
	if _, _, err := s.AuthenticateKey(ctx, newRaw); err != nil {
		t.Fatalf("new key must work: %v", err)
	}
	// 폐기
	if err := s.Store.RevokeKey(ctx, nk.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AuthenticateKey(ctx, newRaw); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked key must fail: %v", err)
	}
	// 만료
	expRaw, expK, _ := s.createKey(ctx, u.ID, "short", time.Millisecond, "")
	time.Sleep(5 * time.Millisecond)
	if _, _, err := s.AuthenticateKey(ctx, expRaw); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired key must fail: %v (status=%s)", err, expK.Status(time.Now()))
	}
	// 접두사 없는 문자열 즉시 거부
	if _, _, err := s.AuthenticateKey(ctx, "not-a-key"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("unknown key prefix must be rejected")
	}
	// 한 릴리스 동안 저장된 JAMYPG 키를 계속 인증한다.
	legacyRaw := "jsk_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	legacy := &MCPKey{
		ID: NewID(), UserID: u.ID, Name: "legacy", KeyHash: hashToken(legacyRaw),
		KeyPrefix: legacyRaw[:12], CreatedAt: time.Now(),
	}
	if err := s.Store.CreateKey(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if got, _, err := s.AuthenticateKey(ctx, legacyRaw); err != nil || got.ID != u.ID {
		t.Fatalf("legacy JAMYPG key must remain valid: user=%v err=%v", got, err)
	}
}

func TestProfilePermissions(t *testing.T) {
	s, ctx := newSvc(t)
	admin, _ := s.CreateLocalUser(ctx, "root", "password1", RoleAdmin, "", "")
	owner, _ := s.CreateLocalUser(ctx, "owner", "password1", RoleUser, "", "")
	other, _ := s.CreateLocalUser(ctx, "other", "password1", RoleUser, "", "")

	rec := &ProfileRecord{ID: "p1", OwnerID: owner.ID, Definition: []byte(`{"id":"p1"}`), Visibility: VisibilityPrivate}
	if err := s.Store.UpsertProfile(ctx, rec, true); err != nil {
		t.Fatal(err)
	}
	grants, _ := s.Store.ListGrants(ctx, "p1")

	if !CanUseProfile(admin, *rec, grants) || !CanManageProfile(admin, *rec, grants) {
		t.Fatal("admin must have full access")
	}
	if !CanUseProfile(owner, *rec, grants) || !CanManageProfile(owner, *rec, grants) {
		t.Fatal("owner must have full access")
	}
	if CanUseProfile(other, *rec, grants) || CanManageProfile(other, *rec, grants) {
		t.Fatal("other must have no access to private profile")
	}
	// use grant → 사용만 가능
	_ = s.Store.SetGrant(ctx, Grant{ProfileID: "p1", UserID: other.ID, Permission: PermUse, GrantedBy: owner.ID})
	grants, _ = s.Store.ListGrants(ctx, "p1")
	if !CanUseProfile(other, *rec, grants) || CanManageProfile(other, *rec, grants) {
		t.Fatal("use grant must allow use only")
	}
	// manage grant → 관리 가능
	_ = s.Store.SetGrant(ctx, Grant{ProfileID: "p1", UserID: other.ID, Permission: PermManage, GrantedBy: owner.ID})
	grants, _ = s.Store.ListGrants(ctx, "p1")
	if !CanManageProfile(other, *rec, grants) {
		t.Fatal("manage grant must allow manage")
	}
	// grant 회수
	_ = s.Store.RemoveGrant(ctx, "p1", other.ID)
	grants, _ = s.Store.ListGrants(ctx, "p1")
	if CanUseProfile(other, *rec, grants) {
		t.Fatal("removed grant must revoke access")
	}
	// shared 가시성 → 모든 사용자 use 가능(manage는 불가)
	rec.Visibility = VisibilityShared
	if !CanUseProfile(other, *rec, grants) || CanManageProfile(other, *rec, grants) {
		t.Fatal("shared visibility must allow use only")
	}
}
