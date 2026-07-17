package openmetadata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewNormalizesURL(t *testing.T) {
	cases := map[string]string{
		"http://h:8585":      "http://h:8585/api",
		"http://h:8585/":     "http://h:8585/api",
		"http://h:8585/api":  "http://h:8585/api",
		"http://h:8585/api/": "http://h:8585/api",
	}
	for in, want := range cases {
		if got := New(in, "").BaseURL; got != want {
			t.Errorf("New(%q).BaseURL=%q want %q", in, got, want)
		}
	}
}

func TestValidateBaseURL(t *testing.T) {
	for _, good := range []string{"http://openmetadata:8585", "https://meta.example.com/api"} {
		if err := ValidateBaseURL(good); err != nil {
			t.Errorf("ValidateBaseURL(%q): %v", good, err)
		}
	}
	for _, bad := range []string{"openmetadata:8585", "ftp://host", "http://user:pass@host", "http://host/api/v1/tables"} {
		if err := ValidateBaseURL(bad); err == nil {
			t.Errorf("ValidateBaseURL(%q) should fail", bad)
		}
	}
}

func TestSchemaTable(t *testing.T) {
	cases := map[string]string{
		"svc.db.public.customer":      "public.customer",
		`svc.db."my.schema".customer`: "my.schema.customer",
		"public.customer":             "public.customer",
		"customer":                    "customer",
		`"svc"."db"."public"."tbl"`:   "public.tbl",
	}
	for in, want := range cases {
		if got := SchemaTable(in); got != want {
			t.Errorf("SchemaTable(%q)=%q want %q", in, got, want)
		}
	}
}

func TestColumnIsPII(t *testing.T) {
	pii := Column{Tags: []TagLabel{{TagFQN: "PII.Sensitive"}}}
	if !pii.IsPII() {
		t.Fatal("PII.Sensitive should be PII")
	}
	non := Column{Tags: []TagLabel{{TagFQN: "PII.NonSensitive"}, {TagFQN: "Tier.Tier1"}}}
	if non.IsPII() {
		t.Fatal("NonSensitive/Tier must not be PII")
	}
}

func TestListTablesPaginates(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		calls++
		after := r.URL.Query().Get("after")
		w.Header().Set("Content-Type", "application/json")
		if after == "" {
			_ = json.NewEncoder(w).Encode(tableList{
				Data:   []Table{{Name: "a", FullyQualifiedName: "s.d.p.a"}},
				Paging: paging{Total: 2, After: "cursor2"},
			})
		} else {
			_ = json.NewEncoder(w).Encode(tableList{
				Data:   []Table{{Name: "b", FullyQualifiedName: "s.d.p.b"}},
				Paging: paging{Total: 2},
			})
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	tables, err := c.ListTables(context.Background(), "svc.db", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != 2 || tables[0].Name != "a" || tables[1].Name != "b" {
		t.Fatalf("pagination wrong: %+v", tables)
	}
	if calls != 2 {
		t.Fatalf("expected 2 page calls, got %d", calls)
	}
}

func TestListTablesStopsOnRepeatedCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(tableList{Data: []Table{{Name: "a"}}, Paging: paging{After: "same"}})
	}))
	defer srv.Close()
	tables, err := New(srv.URL, "").ListTables(context.Background(), "", 500)
	if err == nil || !strings.Contains(err.Error(), "cursor repeated") || len(tables) != 2 {
		t.Fatalf("repeated cursor should stop safely with partial data, tables=%d err=%v", len(tables), err)
	}
}

func TestPatchColumnDescriptionSendsJSONPatch(t *testing.T) {
	var gotBody, gotCT, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	if err := c.PatchColumnDescription(context.Background(), "id123", 2, "설명"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPatch {
		t.Fatalf("expected PATCH, got %s", gotMethod)
	}
	if gotCT != "application/json-patch+json" {
		t.Fatalf("expected json-patch content type, got %q", gotCT)
	}
	if !strings.Contains(gotBody, `/columns/2/description`) || !strings.Contains(gotBody, `"op":"add"`) {
		t.Fatalf("json patch body wrong: %s", gotBody)
	}
}

func TestVersionErrorSurfacesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"denied"}`))
	}))
	defer srv.Close()
	_, err := New(srv.URL, "bad").Version(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("expected HTTP 403 error, got %v", err)
	}
}

func TestAddTableLineageSendsEdge(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	if err := c.AddTableLineage(context.Background(), "from-id", "to-id", "rel a→b"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPut || gotPath != "/api/v1/lineage" {
		t.Fatalf("expected PUT /api/v1/lineage, got %s %s", gotMethod, gotPath)
	}
	for _, want := range []string{`"fromEntity"`, `"from-id"`, `"toEntity"`, `"to-id"`, `"type":"table"`, `"lineageDetails"`} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("lineage body missing %q: %s", want, gotBody)
		}
	}
}
