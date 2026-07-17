package mcp

import (
	"os"
	"runtime"
	"testing"

	"sqlon/internal/catalog"
)

func newOMServer(t *testing.T) *Server {
	t.Helper()
	s := &Server{Options: Options{}}
	s.setCatalog(&catalog.Catalog{DataDir: t.TempDir(), Tables: map[string]*catalog.Table{}})
	return s
}

func TestOMConfigFileOverridesFlags(t *testing.T) {
	s := newOMServer(t)
	s.Options.OpenMetadataURL = "http://flag:8585"
	s.Options.OpenMetadataToken = "flagtok"

	// no file → flag wins (omConfig returns the raw url; client normalizes)
	if url, tok, src := s.omConfig(); url != "http://flag:8585" || tok != "flagtok" || src != "flag" {
		t.Fatalf("flag config wrong: %q %q %q", url, tok, src)
	}

	// save file → file wins
	if err := s.saveOMConfig("http://file:8585", "filetok"); err != nil {
		t.Fatal(err)
	}
	url, tok, src := s.omConfig()
	if url != "http://file:8585" || tok != "filetok" || src != "file" {
		t.Fatalf("file config should win: %q %q %q", url, tok, src)
	}
}

func TestOMConfigTokenPreservedOnBlank(t *testing.T) {
	s := newOMServer(t)
	if err := s.saveOMConfig("http://h:8585", "secret"); err != nil {
		t.Fatal(err)
	}
	// update url with blank token → keep existing token
	if err := s.saveOMConfig("http://h2:8585", ""); err != nil {
		t.Fatal(err)
	}
	url, tok, _ := s.omConfig()
	if url != "http://h2:8585" || tok != "secret" {
		t.Fatalf("token should be preserved on blank update: %q %q", url, tok)
	}
}

func TestOMConfigEmptyURLRemovesFile(t *testing.T) {
	s := newOMServer(t)
	if err := s.saveOMConfig("http://h:8585", "t"); err != nil {
		t.Fatal(err)
	}
	if err := s.saveOMConfig("", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.omConfigPath()); !os.IsNotExist(err) {
		t.Fatal("empty URL should remove the config file")
	}
	if _, _, src := s.omConfig(); src != "unset" {
		t.Fatalf("after removal source should be unset, got %q", src)
	}
}

func TestOMConfigFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping file permission check on Windows")
	}
	s := newOMServer(t)
	if err := s.saveOMConfig("http://h:8585", "t"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.omConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config file must be 0600, got %o", info.Mode().Perm())
	}
}

func TestOMFailureStage(t *testing.T) {
	tests := map[string]string{
		"openmetadata GET: HTTP 401":        "authentication",
		"dial tcp: connection refused":      "network",
		"lookup openmetadata: no such host": "dns",
		"context deadline exceeded":         "timeout",
		"HTTP 404":                          "api-path",
	}
	for msg, want := range tests {
		if got := omFailureStage(msg); got != want {
			t.Errorf("omFailureStage(%q)=%q want %q", msg, got, want)
		}
	}
}
