package mcp

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"sqlon/internal/meta"
)

// MCP over SSO — Keycloak access tokens as a second credential for /mcp.
//
// The MCP authorization specification (2025-06-18 and later) is OAuth 2.1:
// this server is a *resource server*. It publishes where its authorization
// server is (RFC 9728, /.well-known/oauth-protected-resource), answers an
// unauthenticated /mcp call with a 401 that points there, and checks that a
// presented access token was signed by Keycloak *for this deployment*. Login,
// consent, token issuance and client registration all stay in Keycloak —
// nothing here mints or stores a token.
//
// The personal key (ssk_) stays exactly as it is. A token is a second door
// into the same room: it authenticates an *existing, active* sqlon account
// (the one the person created by signing in to the web console with SSO),
// is accepted on /mcp only — never on REST, the console or the admin API —
// and is held to a ceiling the administrator sets (mcp.oauth.scopes) on top
// of the account's role. It never creates an account, never revives a
// deactivated one, and never reads roles out of the token.

// mcpOAuthConfig is the effective MCP SSO configuration, recomputed by
// ApplySettings from the stored settings (over flag/env boot defaults).
type mcpOAuthConfig struct {
	Enabled   bool     // the administrator's switch (mcp.oauth.enabled)
	Issuer    string   // shared with the web sign-in (oidc_issuer), trailing slash trimmed
	Resource  string   // RFC 8707 identifier: mcp.oauth.resource, else redirect-URL origin + endpoint
	Audiences []string // mcp.oauth.audience — accepted aud/azp values besides Resource
	Scopes    []string // mcp.oauth.scopes — ceiling for SSO subjects (default mcp:read)
	// Reason says why the feature is inactive although Enabled is true, so
	// the console and the log can tell the operator what is missing.
	Reason string
}

// Active reports whether tokens are actually accepted: switched on and every
// prerequisite (issuer, resource identifier) present.
func (c mcpOAuthConfig) Active() bool { return c.Enabled && c.Reason == "" }

// MetadataURL is the RFC 9728 document location derived from the resource
// identifier: origin + /.well-known/oauth-protected-resource + resource path.
func (c mcpOAuthConfig) MetadataURL() string {
	u, err := url.Parse(c.Resource)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource" + strings.TrimSuffix(u.Path, "/")
}

// view is the secret-free shape the console and /auth/me show.
func (c mcpOAuthConfig) view() map[string]any {
	return map[string]any{
		"enabled":      c.Enabled,
		"active":       c.Active(),
		"reason":       c.Reason,
		"issuer":       c.Issuer,
		"resource":     c.Resource,
		"metadata_url": c.MetadataURL(),
		"audience":     c.Audiences,
		"scopes":       c.Scopes,
	}
}

// buildMCPOAuthConfig derives the effective configuration from merged
// settings. The resource identifier deliberately never comes from the request
// Host header — anybody can set that header, and the identifier is what a
// token's audience is compared against.
func buildMCPOAuthConfig(eff map[string]string, endpoint string) mcpOAuthConfig {
	c := mcpOAuthConfig{
		Enabled:   strings.EqualFold(strings.TrimSpace(eff[meta.SetMCPOAuthEnabled]), "true"),
		Issuer:    strings.TrimRight(strings.TrimSpace(eff[meta.SetOIDCIssuer]), "/"),
		Resource:  strings.TrimSpace(eff[meta.SetMCPOAuthResource]),
		Audiences: strings.Fields(eff[meta.SetMCPOAuthAudience]),
		Scopes:    strings.Fields(eff[meta.SetMCPOAuthScopes]),
	}
	if len(c.Scopes) == 0 {
		c.Scopes = strings.Fields(meta.DefaultMCPOAuthScopes)
	}
	if c.Resource == "" {
		// The public origin this deployment already knows about is the one
		// Keycloak sends browsers back to; the MCP endpoint lives next to it.
		if red, err := url.Parse(strings.TrimSpace(eff[meta.SetOIDCRedirect])); err == nil && red.Scheme != "" && red.Host != "" {
			c.Resource = red.Scheme + "://" + red.Host + endpoint
		}
	}
	if c.Enabled {
		switch {
		case c.Issuer == "":
			c.Reason = "OIDC Issuer URL(oidc_issuer)이 비어 있습니다 — Keycloak SSO 를 먼저 구성하세요"
		case c.Resource == "":
			c.Reason = "리소스 식별자를 만들 수 없습니다 — mcp.oauth.resource 또는 OIDC Redirect URL 을 적으세요"
		}
	}
	return c
}

// mcpOAuthConfigSnapshot returns the current effective configuration.
func (s *Server) mcpOAuthConfigSnapshot() mcpOAuthConfig {
	s.settingsMu.RLock()
	defer s.settingsMu.RUnlock()
	return s.mcpOAuth
}

// ---- HTTP: metadata and the 401 challenge ----

// registerMCPOAuth serves the RFC 9728 document at both well-known paths.
// Public by design — it says where to sign in, not who is signed in — and
// bare JSON with CORS open, because the reader is an OAuth client library
// (sometimes running in a browser) that knows nothing about product envelopes.
func (s *Server) registerMCPOAuth(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.handleProtectedResourceMetadata)
	if p := strings.TrimSuffix(s.Options.Endpoint, "/"); p != "" && p != "/.well-known/oauth-protected-resource" {
		mux.HandleFunc("GET /.well-known/oauth-protected-resource"+p, s.handleProtectedResourceMetadata)
	}
}

func (s *Server) handleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	c := s.mcpOAuthConfigSnapshot()
	if !s.authEnabled() || !c.Active() {
		// A document that exists while tokens are refused sends clients into a
		// sign-in loop, so an inactive feature is simply absent.
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "mcp_oauth_disabled",
			"error_description": "이 서버의 MCP 는 SSO 토큰을 받지 않습니다. 개인 MCP 키(ssk_)를 사용하세요."})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resource":                 c.Resource,
		"authorization_servers":    []string{c.Issuer},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         c.Scopes,
		"resource_name":            "sqlon MCP",
	})
}

// writeMCPUnauthorized answers a refused /mcp call. With MCP SSO active the
// WWW-Authenticate header carries resource_metadata so an OAuth-capable
// client can find Keycloak on its own; a presented-but-rejected credential
// additionally gets error="invalid_token". This header shape is attached on
// the MCP path only — REST 401s keep their own message so browsers and other
// clients are not sent chasing OAuth metadata.
func (s *Server) writeMCPUnauthorized(w http.ResponseWriter, r *http.Request, err error) {
	c := s.mcpOAuthConfigSnapshot()
	challenge := `Bearer realm="sqlon"`
	body := "authentication required: pass an MCP key via Authorization: Bearer ssk_... or X-MCP-Key (manage keys at /admin/keys)"
	if c.Active() {
		challenge += `, resource_metadata="` + c.MetadataURL() + `"`
		if bearerToken(r) != "" || r.Header.Get("X-MCP-Key") != "" {
			challenge += `, error="invalid_token"`
		}
		body += " — 또는 SSO: 클라이언트에 " + c.Resource + " 만 주면 Keycloak 로그인으로 연결됩니다"
	}
	var refusal *oauthRefusal
	if errors.As(err, &refusal) {
		body = refusal.message
	}
	w.Header().Set("WWW-Authenticate", challenge)
	http.Error(w, body, http.StatusUnauthorized)
}

// ---- authentication: key or token, told apart in one header ----

// oauthPrincipal rides the request context for a subject that came in with
// an SSO token. Its presence (not the length of Scopes) is what marks the
// caller as external-OAuth: Scopes is never empty for a live principal
// because an empty ceiling is refused before one is created.
type oauthPrincipal struct {
	Scopes []string
}

type ctxKeyOAuth struct{}

func withOAuth(ctx context.Context, p *oauthPrincipal) context.Context {
	return context.WithValue(ctx, ctxKeyOAuth{}, p)
}

// oauthFrom returns the OAuth principal, or nil for key/session/master-token
// callers and for stdio.
func oauthFrom(ctx context.Context) *oauthPrincipal {
	p, _ := ctx.Value(ctxKeyOAuth{}).(*oauthPrincipal)
	return p
}

// oauthScopeAllows is the single gate that turns the administrator's scope
// ceiling into a yes/no for an MCP capability. Non-OAuth callers (keys,
// sessions, master token, stdio) are not subject to it and always pass.
func oauthScopeAllows(ctx context.Context, scope string) bool {
	p := oauthFrom(ctx)
	if p == nil {
		return true
	}
	return slices.Contains(p.Scopes, scope)
}

// oauthScopeDenied renders the tool-level refusal for a missing scope.
func oauthScopeDenied(name, scope string) map[string]any {
	return map[string]any{
		"status": "forbidden",
		"error":  "tool '" + name + "' requires SSO scope " + scope,
		"notice": "SSO 토큰으로 연결한 주체의 범위에 " + scope + " 가 없습니다. 관리자가 서버 설정 mcp.oauth.scopes 에 " + scope + " 를 더해야 합니다(계정 역할도 함께 필요).",
	}
}

// oauthRefusal is a refusal that carries two texts: the cause, for the
// server log, and the message, for the client.
type oauthRefusal struct {
	cause   error
	message string
}

func (e *oauthRefusal) Error() string { return e.cause.Error() }
func (e *oauthRefusal) Unwrap() error { return e.cause }

// oauthRefuser is the only way a refusal is made: every path logs its
// concrete cause (signature, issuer, expiry, audience, account…) together
// with the request id and, once the claims have been read, the subject — so
// the short client text can be matched to one log line. The token itself
// never reaches the log.
type oauthRefuser struct {
	requestID string
	sub       string
}

func (f oauthRefuser) refuse(cause error, message string) error {
	log.Printf("sqlon: mcp oauth: refused: request_id=%s sub=%q cause=%v", f.requestID, f.sub, cause)
	return &oauthRefusal{cause: fmt.Errorf("%w: %v", meta.ErrUnauthorized, cause), message: message}
}

// ---- request correlation ----

type ctxKeyRequestID struct{}

// ensureRequestID gives the request a correlation id: the caller's
// X-Request-Id when it is a sane header value, a fresh one otherwise. The id
// is echoed in the response so client and server logs can be matched. Only
// the MCP path uses it for now.
func ensureRequestID(w http.ResponseWriter, r *http.Request) *http.Request {
	id := strings.TrimSpace(r.Header.Get("X-Request-Id"))
	if len(id) > 128 || strings.ContainsFunc(id, func(c rune) bool { return c < 0x21 || c > 0x7e }) {
		id = ""
	}
	if id == "" {
		id = newRequestID()
	}
	w.Header().Set("X-Request-Id", id)
	return r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID{}, id))
}

func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req_" + time.Now().Format("20060102150405.000000000")
	}
	return "req_" + base64.RawURLEncoding.EncodeToString(b[:])
}

// requestIDFrom returns the request's correlation id; for contexts that did
// not pass through ensureRequestID (stdio, tests calling oauthUser directly)
// it mints one so a log line never goes out without an id.
func requestIDFrom(ctx context.Context) string {
	if id, _ := ctx.Value(ctxKeyRequestID{}).(string); id != "" {
		return id
	}
	return newRequestID()
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}

// looksLikeJWT is the cheap shape test that separates "not a key" from "not
// a token of any kind we accept": three non-empty dot-separated segments.
func looksLikeJWT(token string) bool {
	parts := strings.Split(token, ".")
	return len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != ""
}

// authenticateMCP resolves the caller of /mcp. Keys, sessions and the master
// token go through authenticate exactly as before; only when that fails and
// the bearer is JWT-shaped (and not a key) is the SSO path tried. This is the
// one place OAuth tokens are accepted.
func (s *Server) authenticateMCP(r *http.Request) (*meta.User, *oauthPrincipal, error) {
	u, err := s.authenticate(r)
	if err == nil {
		return u, nil, nil
	}
	tok := bearerToken(r)
	if tok == "" || meta.IsMCPKeyFormat(tok) || !looksLikeJWT(tok) {
		return nil, nil, err
	}
	return s.oauthUser(r.Context(), tok)
}

// oauthUser turns a bearer access token into an existing sqlon account, or
// says exactly why it will not.
func (s *Server) oauthUser(ctx context.Context, token string) (*meta.User, *oauthPrincipal, error) {
	c := s.mcpOAuthConfigSnapshot()
	ref := oauthRefuser{requestID: requestIDFrom(ctx)}
	if !c.Enabled {
		return nil, nil, ref.refuse(errors.New("sso tokens disabled (mcp.oauth.enabled=false)"),
			"이 서버는 SSO 액세스 토큰을 받지 않습니다. 개인 MCP 키(ssk_)를 쓰거나, 관리자가 서버 설정에서 MCP SSO(OAuth)를 켜야 합니다.")
	}
	if !c.Active() {
		return nil, nil, ref.refuse(errors.New("sso tokens enabled but inactive: "+c.Reason),
			"MCP SSO(OAuth)가 켜져 있지만 구성이 불완전합니다("+c.Reason+"). 관리자에게 알리세요.")
	}
	if len(token) > 32<<10 {
		return nil, nil, ref.refuse(errors.New("token longer than 32KiB"), "SSO 액세스 토큰이 비정상적으로 큽니다.")
	}
	hdr, claims, err := verifyJWT(token, func(kid, alg string) (crypto.PublicKey, error) {
		return s.jwksKeys.key(ctx, c.Issuer, kid)
	})
	if err != nil {
		return nil, nil, ref.refuse(fmt.Errorf("token rejected: %w", err),
			"SSO 액세스 토큰이 유효하지 않습니다(서명·발급자 키). 클라이언트에서 다시 로그인하세요.")
	}
	// From here on the subject is known and goes into every refusal log line.
	ref.sub = claimString(claims, "sub")
	now := time.Now()
	const leeway = 30 * time.Second
	if iss := claimString(claims, "iss"); strings.TrimRight(iss, "/") != c.Issuer {
		return nil, nil, ref.refuse(fmt.Errorf("issuer %q is not %q", iss, c.Issuer),
			"SSO 액세스 토큰의 발급자(iss)가 이 서버의 OIDC Issuer 와 다릅니다. 같은 Keycloak realm 으로 로그인하세요.")
	}
	exp, ok := claimTime(claims, "exp")
	if !ok || !now.Before(exp.Add(leeway)) {
		return nil, nil, ref.refuse(fmt.Errorf("token expired at %v (exp present=%v)", exp, ok),
			"SSO 액세스 토큰이 만료되었습니다. 클라이언트에서 다시 로그인하세요.")
	}
	if nbf, ok := claimTime(claims, "nbf"); ok && now.Add(leeway).Before(nbf) {
		return nil, nil, ref.refuse(fmt.Errorf("token not valid before %v", nbf),
			"SSO 액세스 토큰이 아직 유효하지 않습니다(nbf). 서버와 Keycloak 의 시계를 확인하세요.")
	}
	// An ID token is proof of login, not an API credential; Keycloak stamps
	// the payload typ ("Bearer" for access tokens, "ID"/"Refresh"/… otherwise).
	if typ := claimString(claims, "typ"); typ != "" && !strings.EqualFold(typ, "Bearer") {
		return nil, nil, ref.refuse(fmt.Errorf("token typ %q is not Bearer", typ),
			"SSO 토큰이 액세스 토큰이 아닙니다(typ="+typ+"). ID 토큰이 아니라 액세스 토큰을 보내세요.")
	}
	if typ, _ := hdr["typ"].(string); strings.EqualFold(typ, "ID") {
		return nil, nil, ref.refuse(errors.New("header typ is ID"), "SSO ID 토큰은 MCP 자격이 아닙니다. 액세스 토큰을 보내세요.")
	}
	// A sender-constrained token (DPoP / mTLS, RFC 7800 cnf) needs a proof we
	// cannot verify; accepting it as a plain bearer would defeat the binding.
	if _, has := claims["cnf"]; has {
		return nil, nil, ref.refuse(errors.New("token carries cnf (sender-constrained)"),
			"소지자 증명(cnf)이 묶인 SSO 토큰은 이 서버가 검증할 수 없어 받지 않습니다. 일반 Bearer 액세스 토큰을 쓰세요.")
	}
	sub := ref.sub
	if sub == "" {
		return nil, nil, ref.refuse(errors.New("sub missing"), "SSO 액세스 토큰에 사용자 식별자(sub)가 없습니다.")
	}
	// Whom the token was minted for. A real Keycloak 26 access token carries
	// aud=["account"] and the client id in azp, so "aud names our resource"
	// (Audience mapper, the formal path) and "aud or azp is on the
	// administrator's list" (no mapper, the plain path) are both accepted;
	// anything else is a token for a different application in the realm.
	aud := claimStrings(claims, "aud")
	azp := claimString(claims, "azp")
	accepted := append([]string{c.Resource}, c.Audiences...)
	bound := append(slices.Clone(aud), azp)
	if !slices.ContainsFunc(bound, func(v string) bool { return v != "" && slices.Contains(accepted, v) }) {
		return nil, nil, ref.refuse(fmt.Errorf("audience %v / azp %q not accepted (accepted %v)", aud, azp, accepted),
			fmt.Sprintf("SSO 토큰이 이 서버를 위해 발급된 것이 아닙니다(토큰의 aud=%v, azp=%q). 관리자가 서버 설정 mcp.oauth.audience 에 %q 를 더하거나, Keycloak 클라이언트의 Audience 매퍼에 %q 를 넣어야 합니다.",
				aud, azp, azp, c.Resource))
	}
	// Scopes: the administrator's ceiling, narrowed by the token only when the
	// token speaks our vocabulary (mcp:*). A token that carries none of it —
	// the normal case, "openid profile email" — gets the ceiling as is. An
	// empty intersection is a refusal, never an "unlimited" empty list.
	scopes := slices.Clone(c.Scopes)
	if tokScopes := strings.Fields(claimString(claims, "scope")); slices.ContainsFunc(tokScopes, func(v string) bool { return strings.HasPrefix(v, "mcp:") }) {
		scopes = slices.DeleteFunc(scopes, func(v string) bool { return !slices.Contains(tokScopes, v) })
		if len(scopes) == 0 {
			return nil, nil, ref.refuse(fmt.Errorf("token scopes %v share nothing with ceiling %v", tokScopes, c.Scopes),
				fmt.Sprintf("SSO 토큰의 범위(%s)와 서버가 허용한 범위(%s)에 공통 항목이 없습니다. 클라이언트가 요청하는 scope 를 줄이거나 관리자가 mcp.oauth.scopes 를 넓혀야 합니다.",
					strings.Join(tokScopes, " "), strings.Join(c.Scopes, " ")))
		}
	}
	// The same lookup the web sign-in uses, without the provisioning half.
	u, err := s.Meta.Store.GetUserByProviderSubject(ctx, meta.ProviderKeycloak, sub)
	if err != nil {
		return nil, nil, ref.refuse(fmt.Errorf("no account for sso subject %q: %v", sub, err),
			"이 SSO 계정은 sqlon 에 등록되어 있지 않습니다. 먼저 웹 콘솔에 Keycloak(SSO)으로 한 번 로그인한 뒤 다시 연결하세요.")
	}
	if !u.IsActive {
		return nil, nil, ref.refuse(fmt.Errorf("account %q (sub %q) is deactivated", u.Username, sub),
			"이 sqlon 계정은 비활성 상태입니다. 관리자에게 활성화를 요청하세요.")
	}
	return u, &oauthPrincipal{Scopes: scopes}, nil
}

func claimString(claims map[string]any, name string) string {
	v, _ := claims[name].(string)
	return v
}

// claimStrings reads a claim that may be a string or an array of strings
// (aud is defined that way).
func claimStrings(claims map[string]any, name string) []string {
	switch v := claims[name].(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func claimTime(claims map[string]any, name string) (time.Time, bool) {
	switch v := claims[name].(type) {
	case float64:
		return time.Unix(int64(v), 0), true
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return time.Unix(n, 0), true
		}
	}
	return time.Time{}, false
}

// ---- JWT verification against Keycloak's JWKS ----

// jwtAlgs is the closed list of accepted signature algorithms: asymmetric
// only. HS* would let anybody who can read the JWKS forge a token, and
// "none" is not a signature.
var jwtAlgs = map[string]crypto.Hash{
	"RS256": crypto.SHA256, "RS384": crypto.SHA384, "RS512": crypto.SHA512,
	"PS256": crypto.SHA256, "PS384": crypto.SHA384, "PS512": crypto.SHA512,
	"ES256": crypto.SHA256, "ES384": crypto.SHA384, "ES512": crypto.SHA512,
}

// verifyJWT checks the compact serialization's signature with the key the
// callback returns for the header's kid, and returns header and claims.
// Everything about *what the claims say* is the caller's job.
func verifyJWT(token string, keyFor func(kid, alg string) (crypto.PublicKey, error)) (map[string]any, map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, nil, errors.New("not a compact jws")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, nil, errors.New("header is not base64url")
	}
	var hdr map[string]any
	if err := json.Unmarshal(hb, &hdr); err != nil {
		return nil, nil, errors.New("header is not json")
	}
	alg, _ := hdr["alg"].(string)
	hash, ok := jwtAlgs[alg]
	if !ok {
		return nil, nil, fmt.Errorf("alg %q not accepted", alg)
	}
	kid, _ := hdr["kid"].(string)
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, nil, errors.New("signature is not base64url")
	}
	pub, err := keyFor(kid, alg)
	if err != nil {
		return nil, nil, err
	}
	signed := []byte(parts[0] + "." + parts[1])
	var digest []byte
	switch hash {
	case crypto.SHA256:
		d := sha256.Sum256(signed)
		digest = d[:]
	case crypto.SHA384:
		d := sha512.Sum384(signed)
		digest = d[:]
	default:
		d := sha512.Sum512(signed)
		digest = d[:]
	}
	switch alg[:2] {
	case "RS":
		k, ok := pub.(*rsa.PublicKey)
		if !ok {
			return nil, nil, errors.New("key is not rsa")
		}
		if err := rsa.VerifyPKCS1v15(k, hash, digest, sig); err != nil {
			return nil, nil, errors.New("signature mismatch")
		}
	case "PS":
		k, ok := pub.(*rsa.PublicKey)
		if !ok {
			return nil, nil, errors.New("key is not rsa")
		}
		if err := rsa.VerifyPSS(k, hash, digest, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}); err != nil {
			return nil, nil, errors.New("signature mismatch")
		}
	case "ES":
		k, ok := pub.(*ecdsa.PublicKey)
		if !ok {
			return nil, nil, errors.New("key is not ec")
		}
		n := (k.Curve.Params().BitSize + 7) / 8
		if len(sig) != 2*n {
			return nil, nil, errors.New("signature length mismatch")
		}
		rr := new(big.Int).SetBytes(sig[:n])
		ss := new(big.Int).SetBytes(sig[n:])
		if !ecdsa.Verify(k, digest, rr, ss) {
			return nil, nil, errors.New("signature mismatch")
		}
	}
	cb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, nil, errors.New("payload is not base64url")
	}
	var claims map[string]any
	if err := json.Unmarshal(cb, &claims); err != nil {
		return nil, nil, errors.New("payload is not json")
	}
	return hdr, claims, nil
}

// jwksCache holds the issuer's signing keys. Discovery and the key set are
// network round trips to Keycloak; doing them per request would put
// Keycloak's latency in front of every MCP call. Keys are refreshed on a
// timer, and once more when a token names an unknown kid (rotation) — but
// that unknown-kid refresh is rate limited so a stream of forged tokens
// cannot turn this server into a JWKS hammer.
//
// The lock guards the fields only; the network round trip itself runs
// outside it, so a request whose kid is already cached is never queued
// behind a slow Keycloak. Concurrent callers that all need a fetch share the
// one in flight (inflight), each waiting under its own ctx. A failed fetch
// is remembered exactly like a successful one (nextRefetch): while Keycloak
// is down the keys already held keep serving past their TTL, and retries are
// spaced by jwksRefetchEvery instead of happening on every token.
type jwksCache struct {
	mu          sync.Mutex
	hc          *http.Client
	issuer      string
	keys        map[string]crypto.PublicKey
	fetchedAt   time.Time
	nextRefetch time.Time
	inflight    chan struct{} // closed when the running fetch has recorded its result; nil when none runs
}

const (
	jwksTTL          = time.Hour
	jwksRefetchEvery = 10 * time.Second
)

func newJWKSCache() *jwksCache {
	return &jwksCache{hc: &http.Client{Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// key returns the public key for kid, fetching or refreshing the set as
// described above.
func (c *jwksCache) key(ctx context.Context, issuer, kid string) (crypto.PublicKey, error) {
	for {
		c.mu.Lock()
		now := time.Now()
		held := c.issuer == issuer && c.keys != nil
		if held {
			if k, ok := c.keys[kid]; ok && now.Sub(c.fetchedAt) <= jwksTTL {
				c.mu.Unlock()
				return k, nil
			}
		}
		if wait := c.inflight; wait != nil {
			// Somebody is already talking to Keycloak; use their answer.
			c.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if now.Before(c.nextRefetch) {
			// Inside the throttle window (after a refetch, successful or not):
			// a key we still hold — even past its TTL — beats a refusal.
			if held {
				if k, ok := c.keys[kid]; ok {
					c.mu.Unlock()
					return k, nil
				}
			}
			c.mu.Unlock()
			return nil, fmt.Errorf("unknown kid %q (refetch throttled)", kid)
		}
		if c.issuer != issuer {
			// A new issuer starts from nothing; the throttle below then
			// applies to it, not to whatever was configured before.
			c.issuer, c.keys, c.fetchedAt = issuer, nil, time.Time{}
			held = false
		}
		done := make(chan struct{})
		c.inflight = done
		c.mu.Unlock()

		keys, err := fetchJWKS(ctx, c.hc, issuer)

		c.mu.Lock()
		c.inflight = nil
		c.nextRefetch = time.Now().Add(jwksRefetchEvery)
		if err == nil {
			c.keys, c.fetchedAt = keys, time.Now()
		} else if held {
			keys = c.keys // Keycloak is unreachable: the keys we have stay in service
		}
		close(done)
		c.mu.Unlock()
		if k, ok := keys[kid]; ok {
			return k, nil
		}
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("unknown kid %q", kid)
	}
}

// fetchJWKS reads discovery for jwks_uri (which must live on the issuer's
// own scheme+host — a key set on some other host is not this issuer's) and
// parses the RSA/EC signing keys.
func fetchJWKS(ctx context.Context, hc *http.Client, issuer string) (map[string]crypto.PublicKey, error) {
	get := func(u string, out any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		res, err := hc.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("%s returned %d", u, res.StatusCode)
		}
		return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out)
	}
	var disc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := get(issuer+"/.well-known/openid-configuration", &disc); err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	if strings.TrimRight(disc.Issuer, "/") != issuer {
		return nil, fmt.Errorf("discovery issuer %q does not match %q", disc.Issuer, issuer)
	}
	ju, err := url.Parse(disc.JWKSURI)
	iu, _ := url.Parse(issuer)
	if err != nil || iu == nil || ju.Scheme != iu.Scheme || ju.Host != iu.Host {
		return nil, fmt.Errorf("jwks_uri %q is not on the issuer host", disc.JWKSURI)
	}
	var set struct {
		Keys []struct {
			Kty, Kid, Use, Crv, N, E, X, Y string
		} `json:"keys"`
	}
	if err := get(disc.JWKSURI, &set); err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	keys := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		switch k.Kty {
		case "RSA":
			n, err1 := base64.RawURLEncoding.DecodeString(k.N)
			e, err2 := base64.RawURLEncoding.DecodeString(k.E)
			if err1 != nil || err2 != nil || len(n) == 0 || len(e) == 0 {
				continue
			}
			keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		case "EC":
			var curve elliptic.Curve
			switch k.Crv {
			case "P-256":
				curve = elliptic.P256()
			case "P-384":
				curve = elliptic.P384()
			case "P-521":
				curve = elliptic.P521()
			default:
				continue
			}
			x, err1 := base64.RawURLEncoding.DecodeString(k.X)
			y, err2 := base64.RawURLEncoding.DecodeString(k.Y)
			if err1 != nil || err2 != nil {
				continue
			}
			pub := &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
			if !curve.IsOnCurve(pub.X, pub.Y) {
				continue
			}
			keys[k.Kid] = pub
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("jwks has no usable signing keys")
	}
	return keys, nil
}
