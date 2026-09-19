package meta

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
)

// Settings are runtime-tunable server options stored in the meta DB so admins
// can change them from the console without editing flags/env or restarting.
// Only options that can be applied live belong here; boot-only values (listen
// address, meta DSN, transport) stay as flags and are shown read-only.

// Known setting keys.
const (
	SetAdminToken   = "admin_token"    // secret
	SetOIDCIssuer   = "oidc_issuer"    //
	SetOIDCClientID = "oidc_client_id" //
	SetOIDCSecret   = "oidc_client_secret"
	SetOIDCRedirect = "oidc_redirect_url"
	SetAllowOrigins = "allow_origins"     // comma-separated
	SetCacheTTL     = "cache_ttl_seconds" // result cache lifetime; 0 disables

	// MCP SSO (OAuth 2.1 resource server). Key names follow the cross-service
	// MCP-OAUTH standard verbatim so operators learn them once; the Keycloak
	// issuer is shared with the web sign-in (SetOIDCIssuer) rather than
	// duplicated here.
	SetMCPOAuthEnabled  = "mcp.oauth.enabled"  // "true" opens /mcp to Keycloak access tokens
	SetMCPOAuthResource = "mcp.oauth.resource" // RFC 8707 resource identifier, e.g. https://sqlon.example.com/mcp
	SetMCPOAuthAudience = "mcp.oauth.audience" // space-separated aud/azp values accepted besides the resource
	SetMCPOAuthScopes   = "mcp.oauth.scopes"   // space-separated ceiling for SSO subjects (mcp:read mcp:admin mcp:dba)
)

// MCPOAuthScopeVocabulary is the closed set of scope words mcp.oauth.scopes
// accepts. mcp:read covers every tool that is not admin- or DBA-gated (plus
// resources and prompts); mcp:admin and mcp:dba lift the ceiling for the
// adminOnlyTools / dbaTools tiers — the caller's role is still required.
var MCPOAuthScopeVocabulary = []string{"mcp:read", "mcp:admin", "mcp:dba"}

// DefaultMCPOAuthScopes is the ceiling applied when mcp.oauth.scopes is unset.
const DefaultMCPOAuthScopes = "mcp:read"

// SettingDef describes a manageable setting for the admin UI.
type SettingDef struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Secret bool   `json:"secret"`
	Group  string `json:"group"`
	Help   string `json:"help"`
	// Type hints the console widget: "" (text) or "bool" (checkbox storing
	// "true"/"false").
	Type string `json:"type,omitempty"`
	// Validate, when set, rejects a value at save time (ApplySetting) so a
	// typo cannot silently disable a feature at the next ApplySettings.
	Validate func(value string) error `json:"-"`
}

// SettingDefs is the catalog of editable settings (order = display order).
var SettingDefs = []SettingDef{
	{Key: SetAdminToken, Label: "마스터 관리 토큰", Secret: true, Group: "보안",
		Help: "비상용 break-glass 토큰. X-Admin-Token 헤더로 admin 권한 획득. 비우면 비활성."},
	{Key: SetAllowOrigins, Label: "허용 Origin(쉼표 구분)", Group: "보안",
		Help: "브라우저 교차 출처 허용 목록. localhost는 항상 허용됨."},
	{Key: SetOIDCIssuer, Label: "OIDC Issuer URL", Group: "Keycloak SSO",
		Help: "예: https://kc.example.com/realms/myrealm"},
	{Key: SetOIDCClientID, Label: "OIDC Client ID", Group: "Keycloak SSO", Help: ""},
	{Key: SetOIDCSecret, Label: "OIDC Client Secret", Secret: true, Group: "Keycloak SSO", Help: ""},
	{Key: SetOIDCRedirect, Label: "OIDC Redirect URL", Group: "Keycloak SSO",
		Help: "예: https://host:6767/auth/sso/callback"},
	{Key: SetMCPOAuthEnabled, Label: "MCP SSO(OAuth) 켜기", Group: "MCP SSO (OAuth)", Type: "bool",
		Help:     "Keycloak 액세스 토큰으로 /mcp 에 들어오게 합니다(개인 키는 그대로 유효). OIDC Issuer 가 있어야 실제로 켜집니다. 기본 꺼짐.",
		Validate: validateBoolSetting},
	{Key: SetMCPOAuthResource, Label: "리소스 식별자(resource)", Group: "MCP SSO (OAuth)",
		Help:     "클라이언트가 실제 접속하는 공개 MCP 주소. 예: https://sqlon.example.com/mcp. 비우면 OIDC Redirect URL 의 오리진 + MCP 경로로 만듭니다.",
		Validate: validateMCPOAuthResource},
	{Key: SetMCPOAuthAudience, Label: "허용 대상(aud/azp, 공백 구분)", Group: "MCP SSO (OAuth)",
		Help:     "리소스 식별자 외에 받아들일 토큰 대상. 보통 Keycloak 의 MCP 클라이언트 ID(azp). 예: claude-mcp cursor-mcp",
		Validate: validateMCPOAuthAudience},
	{Key: SetMCPOAuthScopes, Label: "SSO 토큰에 주는 범위(공백 구분)", Group: "MCP SSO (OAuth)",
		Help:     "mcp:read(기본) mcp:admin mcp:dba 중에서. 역할과의 교집합이 천장입니다 — 역할이 없는 권한을 열지 않습니다.",
		Validate: validateMCPOAuthScopes},
	{Key: SetCacheTTL, Label: "쿼리 결과 캐시 TTL(초)", Group: "성능",
		Help: "동일 (프로파일, SQL, max_rows) 결과를 재사용하는 시간. 0=캐시 비활성. 기본 60."},
}

func validateBoolSetting(v string) error {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "true", "false":
		return nil
	}
	return errors.New("true 또는 false 여야 합니다")
}

// validateMCPOAuthResource accepts an absolute http(s) URL with a path and no
// credentials, query or fragment — the shape RFC 8707 / RFC 9728 expect and
// the one a Keycloak Audience mapper must reproduce byte-for-byte.
func validateMCPOAuthResource(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(v, " \t\r\n\"\\") {
		return errors.New("리소스 식별자는 인증정보·쿼리·프래그먼트 없는 절대 http(s) URL 이어야 합니다 (예: https://sqlon.example.com/mcp)")
	}
	return nil
}

func validateMCPOAuthAudience(v string) error {
	for _, a := range strings.Fields(v) {
		if len(a) > 256 || strings.ContainsAny(a, "\"\\") {
			return errors.New("허용 대상 항목이 너무 길거나 따옴표·역슬래시를 포함합니다: " + a)
		}
	}
	return nil
}

// validateMCPOAuthScopes only admits the closed vocabulary; a stray word
// would otherwise be silently dropped and the operator would wonder why the
// ceiling never lifted.
func validateMCPOAuthScopes(v string) error {
	for _, sc := range strings.Fields(v) {
		if !slices.Contains(MCPOAuthScopeVocabulary, sc) {
			return errors.New("알 수 없는 범위 " + sc + " — 허용: " + strings.Join(MCPOAuthScopeVocabulary, " "))
		}
	}
	return nil
}

func isKnownSetting(key string) bool {
	for _, d := range SettingDefs {
		if d.Key == key {
			return true
		}
	}
	return false
}

func settingDef(key string) (SettingDef, bool) {
	for _, d := range SettingDefs {
		if d.Key == key {
			return d, true
		}
	}
	return SettingDef{}, false
}

func isSecretSetting(key string) bool {
	for _, d := range SettingDefs {
		if d.Key == key {
			return d.Secret
		}
	}
	return false
}

// SettingsStore persists key→value settings.
type SettingsStore interface {
	GetSettings(ctx context.Context) (map[string]string, error)
	SetSetting(ctx context.Context, key, value, updatedBy string) error
	DeleteSetting(ctx context.Context, key string) error
}

// MaskSettingValue hides secret values for API display while signalling
// whether a value is set.
func MaskSettingValue(key, value string) string {
	if value == "" {
		return ""
	}
	if isSecretSetting(key) {
		return "••••••(set)"
	}
	return value
}

// SortedSettingKeys returns known keys in display order.
func SortedSettingKeys() []string {
	keys := make([]string, 0, len(SettingDefs))
	for _, d := range SettingDefs {
		keys = append(keys, d.Key)
	}
	return keys
}

// ---- MemStore settings ----

func (m *MemStore) GetSettings(_ context.Context) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for k, v := range m.settings {
		out[k] = v
	}
	return out, nil
}

func (m *MemStore) SetSetting(_ context.Context, key, value, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.settings == nil {
		m.settings = map[string]string{}
	}
	m.settings[key] = value
	return nil
}

func (m *MemStore) DeleteSetting(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.settings, key)
	return nil
}

// ---- service helpers ----

// EffectiveSettings merges stored settings over the provided bootstrap
// defaults (from flags/env). Stored non-empty values win.
func (s *Service) EffectiveSettings(ctx context.Context, defaults map[string]string) (map[string]string, error) {
	stored, err := s.Store.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range defaults {
		out[k] = v
	}
	for k, v := range stored {
		if strings.TrimSpace(v) != "" {
			out[k] = v
		}
	}
	return out, nil
}

// SettingsView returns masked settings plus definitions for the admin UI.
func (s *Service) SettingsView(ctx context.Context) ([]map[string]any, error) {
	stored, err := s.Store.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, d := range SettingDefs {
		v := stored[d.Key]
		out = append(out, map[string]any{
			"key": d.Key, "label": d.Label, "secret": d.Secret, "group": d.Group, "type": d.Type,
			"help": d.Help, "value": MaskSettingValue(d.Key, v), "is_set": v != "",
		})
	}
	return out, nil
}

// ValidateSetting checks that key is a known setting and that value passes
// its Validate hook, without touching the store. Callers saving several keys
// at once run this over the whole batch first so a bad value rejects the
// request as a unit instead of leaving part of it saved.
func ValidateSetting(key, value string) error {
	d, ok := settingDef(key)
	if !ok {
		return ErrNotFound
	}
	if d.Validate != nil {
		return d.Validate(value)
	}
	return nil
}

// ApplySetting validates and stores a single setting.
func (s *Service) ApplySetting(ctx context.Context, key, value, updatedBy string) error {
	if err := ValidateSetting(key, value); err != nil {
		return err
	}
	return s.Store.SetSetting(ctx, key, value, updatedBy)
}
