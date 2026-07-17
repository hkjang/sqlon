package meta

import (
	"context"
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
)

// SettingDef describes a manageable setting for the admin UI.
type SettingDef struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Secret bool   `json:"secret"`
	Group  string `json:"group"`
	Help   string `json:"help"`
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
		Help: "예: https://host:9797/auth/sso/callback"},
	{Key: SetCacheTTL, Label: "쿼리 결과 캐시 TTL(초)", Group: "성능",
		Help: "동일 (프로파일, SQL, max_rows) 결과를 재사용하는 시간. 0=캐시 비활성. 기본 60."},
}

func isKnownSetting(key string) bool {
	for _, d := range SettingDefs {
		if d.Key == key {
			return true
		}
	}
	return false
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
			"key": d.Key, "label": d.Label, "secret": d.Secret, "group": d.Group,
			"help": d.Help, "value": MaskSettingValue(d.Key, v), "is_set": v != "",
		})
	}
	return out, nil
}

// ApplySetting validates and stores a single setting.
func (s *Service) ApplySetting(ctx context.Context, key, value, updatedBy string) error {
	if !isKnownSetting(key) {
		return ErrNotFound
	}
	return s.Store.SetSetting(ctx, key, value, updatedBy)
}
