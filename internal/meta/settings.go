package meta

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"sqlon/internal/tracking"
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
	// Type picks the input control: "" (text), "bool", "select", "multiline".
	Type string `json:"type,omitempty"`
	// Options lists the accepted values for a "select" setting.
	Options []string `json:"options,omitempty"`
	// Default is what an unset value means to the server, so the console can
	// draw a blank "bool" as the state it really is (e.g. proxy = on).
	Default string `json:"default,omitempty"`
	// Validate rejects a value before it is stored; nil accepts anything.
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
	{Key: SetCacheTTL, Label: "쿼리 결과 캐시 TTL(초)", Group: "성능",
		Help: "동일 (프로파일, SQL, max_rows) 결과를 재사용하는 시간. 0=캐시 비활성. 기본 60."},
	// 방문 추적 — 기본 꺼짐. 켜면 화면에 스니펫이 요청별 nonce 를 달고 들어가고
	// 정책(CSP)에 필요한 출처가 더해진다. 세부 동작은 internal/tracking 참고.
	{Key: tracking.SetEnabled, Label: "방문 추적 사용", Group: "방문 추적", Type: "bool",
		Help: "켜야 스니펫이 붙습니다. 기본 꺼짐 — 끄면 화면과 정책이 원래대로 돌아갑니다."},
	{Key: tracking.SetProvider, Label: "제공자", Group: "방문 추적", Type: "select", Options: tracking.Providers,
		Help: "momento = 사내 자체 호스팅 수집기(데이터가 밖으로 나가지 않음). custom = 붙여넣은 스니펫."},
	{Key: tracking.SetMomentoURL, Label: "Momento 수집기 주소", Group: "방문 추적",
		Help: "예: https://momento.internal:8443", Validate: validateOptionalURL},
	{Key: tracking.SetMomentoSiteID, Label: "Momento 사이트 ID", Group: "방문 추적", Help: ""},
	{Key: tracking.SetMomentoProxy, Label: "Momento 같은 오리진 프록시", Group: "방문 추적", Type: "bool", Default: "true",
		Help: "기본 켜짐. 이 서버가 /momento/* 를 수집기로 넘겨 외부 출처가 정책에 등장하지 않습니다."},
	{Key: tracking.SetMeasurementID, Label: "GA4 / GTM 측정 ID", Group: "방문 추적", Help: "예: G-XXXXXXX 또는 GTM-XXXXXXX"},
	{Key: tracking.SetMatomoURL, Label: "Matomo 주소", Group: "방문 추적", Help: "예: https://matomo.example.com", Validate: validateOptionalURL},
	{Key: tracking.SetMatomoSiteID, Label: "Matomo 사이트 ID", Group: "방문 추적", Help: ""},
	{Key: tracking.SetCustomSnippet, Label: "사용자 정의 스니펫", Group: "방문 추적", Type: "multiline",
		Help: "<script> 태그를 그대로 붙여 넣습니다(최대 8KB). 요청마다 nonce 가 자동으로 붙고, 스니펫 안의 http(s) 출처는 정책에 자동으로 더해집니다.",
		Validate: func(v string) error {
			if len(v) > tracking.MaxSnippetBytes {
				return fmt.Errorf("추적 코드는 %d바이트를 넘을 수 없습니다", tracking.MaxSnippetBytes)
			}
			return nil
		}},
	{Key: tracking.SetAllowedHosts, Label: "추가 허용 출처(쉼표 구분)", Group: "방문 추적",
		Help: "스니펫에서 자동으로 못 읽은 출처. 아래 '차단된 출처' 목록에서 한 번에 더할 수 있습니다."},
	{Key: tracking.SetIncludeAdmin, Label: "관리 화면도 추적", Group: "방문 추적", Type: "bool",
		Help: "기본 아니오. /admin 아래 화면은 이 값이 켜졌을 때만 추적합니다."},
	{Key: tracking.SetPlacement, Label: "삽입 위치", Group: "방문 추적", Type: "select", Options: []string{"head", "body"},
		Help: "head(기본) 또는 body 끝."},
}

func validateOptionalURL(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("http(s):// 로 시작하는 주소여야 합니다")
	}
	return nil
}

func settingDef(key string) (SettingDef, bool) {
	for _, d := range SettingDefs {
		if d.Key == key {
			return d, true
		}
	}
	return SettingDef{}, false
}

func isKnownSetting(key string) bool {
	_, ok := settingDef(key)
	return ok
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
			"type": d.Type, "options": d.Options, "default": d.Default,
		})
	}
	return out, nil
}

// ErrInvalidSetting wraps a definition's validation failure so callers can
// tell "unknown key" from "bad value" and show the reason.
var ErrInvalidSetting = errors.New("invalid setting value")

// CheckSetting validates a value against its definition without storing it,
// so a multi-key update can be refused as a whole before anything is written.
// Returns ErrNotFound for an unknown key and ErrInvalidSetting for a bad value.
func (s *Service) CheckSetting(key, value string) error {
	d, ok := settingDef(key)
	if !ok {
		return ErrNotFound
	}
	if d.Validate != nil {
		if err := d.Validate(value); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrInvalidSetting, key, err)
		}
	}
	return nil
}

// ApplySetting validates and stores a single setting.
func (s *Service) ApplySetting(ctx context.Context, key, value, updatedBy string) error {
	if err := s.CheckSetting(key, value); err != nil {
		return err
	}
	return s.Store.SetSetting(ctx, key, value, updatedBy)
}
