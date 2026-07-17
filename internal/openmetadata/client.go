// Package openmetadata is a dependency-free REST client for OpenMetadata
// (https://open-metadata.org). It reads curated business metadata — table and
// column descriptions, display names, PII/tier tags, and glossary terms — and
// can push jamypg-generated metadata back. Auth is a bot JWT bearer token.
package openmetadata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to an OpenMetadata server's /api/v1 surface.
type Client struct {
	BaseURL string // e.g. http://localhost:8585/api  (with or without /api)
	Token   string // bot JWT (Authorization: Bearer ...)
	HTTP    *http.Client
}

// New builds a client. baseURL may or may not include the trailing /api; it is
// normalized so callers can pass either the console URL or the API URL.
func New(baseURL, token string) *Client {
	b := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if b != "" && !strings.HasSuffix(b, "/api") {
		b += "/api"
	}
	return &Client{BaseURL: b, Token: strings.TrimSpace(token), HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// ValidateBaseURL rejects malformed or unsafe-to-interpret targets before a
// config is persisted. OpenMetadata may be HTTP on an internal network, so
// both http and https are accepted, but credentials/userinfo are not.
func ValidateBaseURL(baseURL string) error {
	raw := strings.TrimSpace(baseURL)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("OpenMetadata URL must be an absolute http(s) URL, e.g. http://openmetadata:8585")
	}
	if u.User != nil {
		return fmt.Errorf("OpenMetadata URL must not contain credentials; use the bot token field")
	}
	if strings.Contains(strings.TrimRight(u.Path, "/"), "/api/") {
		return fmt.Errorf("OpenMetadata URL must end at the server root or /api, not an /api/v1 resource path")
	}
	return nil
}

// Configured reports whether a base URL is set.
func (c *Client) Configured() bool { return c != nil && c.BaseURL != "" }

func (c *Client) do(ctx context.Context, method, path string, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 400 {
			msg = msg[:400]
		}
		return fmt.Errorf("openmetadata %s %s: HTTP %d: %s", method, path, resp.StatusCode, msg)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Version returns the server version (also serves as a connectivity/auth test).
func (c *Client) Version(ctx context.Context) (map[string]any, error) {
	var v map[string]any
	if err := c.do(ctx, http.MethodGet, "/v1/system/version", "", nil, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// ListTables pages through tables in scope (a database/schema FQN filter, or ""
// for all), requesting the columns/tags/description fields. It follows the
// `paging.after` cursor up to maxTables.
func (c *Client) ListTables(ctx context.Context, scope string, maxTables int) ([]Table, error) {
	if maxTables <= 0 {
		maxTables = 500
	}
	const pageSize = 100
	var out []Table
	after := ""
	seenCursors := map[string]bool{}
	for len(out) < maxTables {
		q := url.Values{}
		q.Set("fields", "columns,tags,description")
		limit := pageSize
		if remaining := maxTables - len(out); remaining < limit {
			limit = remaining
		}
		q.Set("limit", fmt.Sprint(limit))
		if scope != "" {
			// scope may be a database ("svc.db") or schema ("svc.db.schema") FQN
			q.Set("database", scope)
		}
		if after != "" {
			q.Set("after", after)
		}
		var page tableList
		if err := c.do(ctx, http.MethodGet, "/v1/tables?"+q.Encode(), "", nil, &page); err != nil {
			// a database-scoped filter that doesn't exist should not hard-fail
			// the whole import; surface as empty when the first page 404s.
			if strings.Contains(err.Error(), "HTTP 404") && len(out) == 0 {
				return nil, err
			}
			return out, err
		}
		out = append(out, page.Data...)
		if page.Paging.After == "" || len(page.Data) == 0 {
			break
		}
		if seenCursors[page.Paging.After] {
			return out, fmt.Errorf("openmetadata table pagination cursor repeated: %q", page.Paging.After)
		}
		seenCursors[page.Paging.After] = true
		after = page.Paging.After
	}
	if len(out) > maxTables {
		out = out[:maxTables]
	}
	return out, nil
}

// ListGlossaryTerms pages through glossary terms.
func (c *Client) ListGlossaryTerms(ctx context.Context, maxTerms int) ([]GlossaryTerm, error) {
	if maxTerms <= 0 {
		maxTerms = 500
	}
	const pageSize = 100
	var out []GlossaryTerm
	after := ""
	seenCursors := map[string]bool{}
	for len(out) < maxTerms {
		q := url.Values{}
		q.Set("fields", "synonyms,description")
		limit := pageSize
		if remaining := maxTerms - len(out); remaining < limit {
			limit = remaining
		}
		q.Set("limit", fmt.Sprint(limit))
		if after != "" {
			q.Set("after", after)
		}
		var page glossaryList
		if err := c.do(ctx, http.MethodGet, "/v1/glossaryTerms?"+q.Encode(), "", nil, &page); err != nil {
			return out, err
		}
		out = append(out, page.Data...)
		if page.Paging.After == "" || len(page.Data) == 0 {
			break
		}
		if seenCursors[page.Paging.After] {
			return out, fmt.Errorf("openmetadata glossary pagination cursor repeated: %q", page.Paging.After)
		}
		seenCursors[page.Paging.After] = true
		after = page.Paging.After
	}
	if len(out) > maxTerms {
		out = out[:maxTerms]
	}
	return out, nil
}

// jsonPatchOp is one RFC-6902 JSON Patch operation.
type jsonPatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// PatchTableDescription sets a table's description via JSON Patch.
func (c *Client) PatchTableDescription(ctx context.Context, id, description string) error {
	return c.patch(ctx, "/v1/tables/"+id, []jsonPatchOp{{Op: "add", Path: "/description", Value: description}})
}

// PatchColumnDescription sets one column's description. colIndex is the 0-based
// position of the column in the table's columns array (OpenMetadata patches
// columns positionally).
func (c *Client) PatchColumnDescription(ctx context.Context, id string, colIndex int, description string) error {
	path := fmt.Sprintf("/columns/%d/description", colIndex)
	return c.patch(ctx, "/v1/tables/"+id, []jsonPatchOp{{Op: "add", Path: path, Value: description}})
}

func (c *Client) patch(ctx context.Context, path string, ops []jsonPatchOp) error {
	b, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPatch, path, "application/json-patch+json", strings.NewReader(string(b)), nil)
}

// AddTableLineage records a table→table lineage edge (fromEntity is upstream,
// toEntity downstream) via PUT /api/v1/lineage. Optional description is stored
// in lineageDetails. Idempotent on the OpenMetadata side (re-adding the same
// edge is a no-op).
func (c *Client) AddTableLineage(ctx context.Context, fromID, toID, description string) error {
	edge := map[string]any{
		"fromEntity": map[string]any{"id": fromID, "type": "table"},
		"toEntity":   map[string]any{"id": toID, "type": "table"},
	}
	if description != "" {
		edge["lineageDetails"] = map[string]any{"description": description}
	}
	body, err := json.Marshal(map[string]any{"edge": edge})
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, "/v1/lineage", "application/json", strings.NewReader(string(body)), nil)
}
