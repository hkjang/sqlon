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
		Help: "예: https://host:6767/auth/sso/callback"},
	{Key: SetCacheTTL, Label: "쿼리 결과 캐시 TTL(초)", Group: "성능",
		Help: "동일 (프로파일, SQL, max_rows) 결과를 재사용하는 시간. 0=캐시 비활성. 기본 60."},
	// Mail keys follow the in-house mail standard verbatim (mail.enabled, ...)
	// so operators learn one set of names across services. Defaults match the
	// common relay: port 25, no auth, no TLS. Read on every send (no restart).
	{Key: "mail.enabled", Label: "메일 알림 사용", Group: SettingGroupMail,
		Help: "true/false. 기본 false — 켜기 전에는 아무것도 보내지 않습니다."},
	{Key: "mail.smtp_host", Label: "SMTP 릴레이 호스트", Group: SettingGroupMail,
		Help: "사내 릴레이 주소(예: smtp.corp.local, postra). 켜져 있어도 비어 있으면 보내지 않습니다."},
	{Key: "mail.smtp_port", Label: "SMTP 포트", Group: SettingGroupMail,
		Help: "기본 25(사내 릴레이). 587=STARTTLS, 465=암시적 TLS."},
	{Key: "mail.security", Label: "전송 보안", Group: SettingGroupMail,
		Help: "auto·none·starttls·tls. 기본 auto — 서버가 STARTTLS를 알리면 쓰고, 아니면 평문."},
	{Key: "mail.skip_tls_verify", Label: "TLS 인증서 검증 생략", Group: SettingGroupMail,
		Help: "true/false. 사내 사설 인증서일 때만 true."},
	{Key: "mail.username", Label: "SMTP 사용자 이름", Group: SettingGroupMail,
		Help: "선택. 비우면 인증 없이 보냅니다(사내 릴레이 기본)."},
	{Key: "mail.password", Label: "SMTP 비밀번호", Secret: true, Group: SettingGroupMail,
		Help: "선택. 저장 뒤에는 '설정됨'만 표시되며 API로 되읽을 수 없습니다."},
	{Key: "mail.from_address", Label: "보내는 주소", Group: SettingGroupMail,
		Help: "비우면 sqlon@<SMTP 호스트>."},
	{Key: "mail.from_name", Label: "보내는 이름", Group: SettingGroupMail, Help: "기본 sqlon."},
	{Key: "mail.base_url", Label: "메일 링크 기준 URL", Group: SettingGroupMail,
		Help: "메일 속 '바로 열기' 링크가 가리킬 이 서버의 주소(예: https://sqlon.corp). 비우면 링크를 넣지 않습니다."},
	{Key: "mail.timeout_seconds", Label: "SMTP 제한 시간(초)", Group: SettingGroupMail, Help: "기본 10."},
	{Key: "mail.notify_change_review", Label: "알림: 변경 승인 요청·실행 가능", Group: SettingGroupMail,
		Help: "true/false(기본 true). 변경 계획이 승인 대기(review_required)에 들어가거나 승인이 모두 모여 실행 가능(approved)해지면 dba/admin에게."},
	{Key: "mail.notify_change_failed", Label: "알림: 변경 실행 실패", Group: SettingGroupMail,
		Help: "true/false(기본 true). 변경 실행·롤백이 실패로 멈추면 dba/admin에게."},
	{Key: "mail.notify_query_finished", Label: "알림: 장기 비동기 쿼리 완료", Group: SettingGroupMail,
		Help: "true/false(기본 true). 1분 넘게 걸린 비동기 쿼리가 끝나면(성공·실패) 제출한 사용자에게."},
	{Key: "mail.notify_scheduler", Label: "알림: 예약 동기화 실패·회복", Group: SettingGroupMail,
		Help: "true/false(기본 true). 예약 메타데이터 동기화가 실패로 바뀌거나 회복되면 admin에게(틱마다 아님)."},
}

// SettingGroupMail is the admin-screen group that holds the mail.* keys; the
// UI shows the test-send panel under it.
const SettingGroupMail = "메일 알림 (SMTP)"

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
