// Package app owns the SQLON process lifecycle. Product commands are thin
// wrappers so the primary and deprecated binary cannot drift in flags,
// security checks, startup behavior, or service wiring.
package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"sqlon/internal/catalog"
	"sqlon/internal/mcp"
	"sqlon/internal/meta"
	"sqlon/internal/migration"
)

type Runtime struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
}

func DefaultRuntime() Runtime {
	return Runtime{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv}
}

type config struct {
	dataDir, transport, addr, endpoint, allowOrigins   string
	publicMCP, stateless, ssePost                      bool
	adminToken, feedbackTenant, metaDSN                string
	bootstrapAdmin                                     string
	oidcIssuer, oidcClientID, oidcSecret, oidcRedirect string
	syncSource, digestWebhook                          string
	syncInterval                                       time.Duration
	observeInterval                                    time.Duration
	observeRetentionDays                               int
	syncApply, dbaDigest                               bool
	dbaDigestProfile                                   string
	omURL, omToken, omScope                            string
	omSync                                             bool
	autoMigrate                                        bool
}

func (rt Runtime) Run(ctx context.Context, args []string) error {
	if rt.Stdin == nil {
		rt.Stdin = os.Stdin
	}
	if rt.Stdout == nil {
		rt.Stdout = os.Stdout
	}
	if rt.Stderr == nil {
		rt.Stderr = os.Stderr
	}
	if rt.Getenv == nil {
		rt.Getenv = os.Getenv
	}
	logger := log.New(rt.Stderr, "", log.LstdFlags)
	cfg, err := rt.parse(args)
	if err != nil {
		return err
	}
	if err := ValidateHTTPExposure(cfg.transport, cfg.addr, cfg.metaDSN, cfg.adminToken, cfg.publicMCP); err != nil {
		return err
	}
	if cfg.autoMigrate {
		legacyDir := filepath.Join(filepath.Dir(cfg.dataDir), "metadb")
		result, err := migration.Prepare(cfg.dataDir, legacyDir, time.Now())
		if err != nil {
			return fmt.Errorf("migrate legacy SQLON data: %w", err)
		}
		if result.Migrated {
			logger.Printf("SQLON data migrated to %s after backup at %s (%d files)", result.Target, result.BackupDir, result.Files)
		}
	}

	cat, err := catalog.Load(cfg.dataDir)
	if err != nil {
		return fmt.Errorf("load SQLON catalog: %w", err)
	}
	logger.Printf("SQLON catalog loaded: %d tables, %d relations, %d examples, %d metrics, dialect=%s", len(cat.Tables), len(cat.Relations), len(cat.Samples), len(cat.Metrics), cat.Dialect)
	for _, issue := range cat.Issues {
		if issue.Level == "error" {
			logger.Printf("catalog ERROR [%s] %s %s %s", issue.Source, issue.Message, issue.Table, issue.Column)
		}
	}

	var metaSvc *meta.Service
	if cfg.metaDSN != "" {
		store, err := meta.OpenPG(ctx, cfg.metaDSN)
		if err != nil {
			return fmt.Errorf("meta db: %w", err)
		}
		metaSvc = meta.NewService(store)
		defer metaSvc.Store.Close()
		user, _, created, err := metaSvc.Bootstrap(ctx, cfg.bootstrapAdmin)
		if err != nil {
			return fmt.Errorf("bootstrap admin: %w", err)
		}
		if created {
			logger.Printf("bootstrap admin created: username=%q (credential value is never logged)", user)
		}
	}

	switch strings.ToLower(strings.TrimSpace(cfg.transport)) {
	case "stdio":
		logger.Printf("SQLON SQL Lab compatibility MCP listening on stdio")
		return mcp.ServeStdio(ctx, cat, rt.Stdin, rt.Stdout, mcp.StdioOptions{Logf: logger.Printf})
	case "http", "streamable-http":
	default:
		return fmt.Errorf("unsupported transport %q: use http or stdio", cfg.transport)
	}

	srv := mcp.NewServer(cat, mcp.Options{Endpoint: cfg.endpoint, AllowedOrigins: splitCSV(cfg.allowOrigins), Stateful: !cfg.stateless, SSEPost: cfg.ssePost, AdminToken: cfg.adminToken, FeedbackTenantID: cfg.feedbackTenant, OpenMetadataURL: cfg.omURL, OpenMetadataToken: cfg.omToken, AlertWebhookURL: cfg.digestWebhook})
	if metaSvc != nil {
		var oidc *mcp.OIDCProvider
		if cfg.oidcIssuer != "" && cfg.oidcClientID != "" && cfg.oidcSecret != "" && cfg.oidcRedirect != "" {
			oidc = &mcp.OIDCProvider{Issuer: cfg.oidcIssuer, ClientID: cfg.oidcClientID, ClientSecret: cfg.oidcSecret, RedirectURL: cfg.oidcRedirect}
		}
		srv.EnableMeta(metaSvc, oidc)
		if err := srv.ApplySettings(ctx); err != nil {
			return fmt.Errorf("apply stored settings: %w", err)
		}
		if err := srv.InitDatasetStore(ctx); err != nil {
			return fmt.Errorf("init dataset store: %w", err)
		}
	} else if cfg.adminToken == "" {
		logger.Printf("WARNING: admin API auth disabled; configure SQLON_ADMIN_TOKEN or -meta-db")
	}
	if cfg.syncInterval > 0 {
		srv.StartScheduler(ctx, mcp.SchedulerConfig{Source: cfg.syncSource, Interval: cfg.syncInterval, WebhookURL: cfg.digestWebhook, OpenMetadata: cfg.omSync, OpenMetadataScope: cfg.omScope, ApplySync: cfg.syncApply, DBADigest: cfg.dbaDigest, DBADigestProfile: cfg.dbaDigestProfile})
	}
	srv.StartObservationCollector(ctx, cfg.observeInterval, cfg.observeRetentionDays)
	logger.Printf("SQLON AI Database Operations Platform listening on http://%s", cfg.addr)
	return mcp.ServeServer(cfg.addr, srv)
}

func (rt Runtime) parse(args []string) (config, error) {
	var c config
	fs := flag.NewFlagSet("sqlon", flag.ContinueOnError)
	fs.SetOutput(rt.Stderr)
	defaultDir := rt.env("SQLON_DATA_DIR", "JAMYPG_DATA_DIR")
	c.autoMigrate = defaultDir == ""
	if defaultDir == "" {
		defaultDir = filepath.Join("data", "sqlon")
	}
	observeInterval := time.Minute
	if raw := rt.env("SQLON_OBSERVE_INTERVAL", "JAMYPG_OBSERVE_INTERVAL"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return c, fmt.Errorf("invalid SQLON_OBSERVE_INTERVAL: %w", err)
		}
		observeInterval = parsed
	}
	observeRetention := 30
	if raw := rt.env("SQLON_OBSERVE_RETENTION_DAYS", "JAMYPG_OBSERVE_RETENTION_DAYS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return c, fmt.Errorf("invalid SQLON_OBSERVE_RETENTION_DAYS %q", raw)
		}
		observeRetention = parsed
	}
	fs.StringVar(&c.dataDir, "data", defaultDir, "SQLON operation data directory")
	fs.StringVar(&c.transport, "transport", "http", "MCP transport: http or stdio")
	fs.StringVar(&c.addr, "addr", "127.0.0.1:9797", "HTTP listen address")
	fs.StringVar(&c.endpoint, "endpoint", "/mcp", "MCP endpoint path")
	fs.StringVar(&c.allowOrigins, "allow-origin", "", "Comma-separated allowed Origin values")
	fs.BoolVar(&c.publicMCP, "public-mcp", false, "Explicitly allow standalone non-loopback HTTP")
	fs.BoolVar(&c.stateless, "stateless", false, "Disable MCP session management")
	fs.BoolVar(&c.ssePost, "sse-post", false, "Return POST as text/event-stream")
	fs.StringVar(&c.adminToken, "admin-token", rt.env("SQLON_ADMIN_TOKEN", "JAMYPG_ADMIN_TOKEN"), "Master token")
	fs.StringVar(&c.feedbackTenant, "feedback-tenant", rt.env("SQLON_FEEDBACK_TENANT", "JAMYPG_FEEDBACK_TENANT"), "Feedback tenant")
	fs.StringVar(&c.metaDSN, "meta-db", rt.env("SQLON_META_DB", "JAMYPG_META_DB"), "Postgres DSN for SQLON meta DB")
	fs.StringVar(&c.bootstrapAdmin, "bootstrap-admin", rt.env("SQLON_BOOTSTRAP_ADMIN", "JAMYPG_BOOTSTRAP_ADMIN"), "Initial admin username:password")
	fs.StringVar(&c.oidcIssuer, "oidc-issuer", rt.env("SQLON_OIDC_ISSUER", "JAMYPG_OIDC_ISSUER"), "OIDC issuer")
	fs.StringVar(&c.oidcClientID, "oidc-client-id", rt.env("SQLON_OIDC_CLIENT_ID", "JAMYPG_OIDC_CLIENT_ID"), "OIDC client id")
	fs.StringVar(&c.oidcSecret, "oidc-client-secret", rt.env("SQLON_OIDC_CLIENT_SECRET", "JAMYPG_OIDC_CLIENT_SECRET"), "OIDC client secret")
	fs.StringVar(&c.oidcRedirect, "oidc-redirect-url", rt.env("SQLON_OIDC_REDIRECT_URL", "JAMYPG_OIDC_REDIRECT_URL"), "OIDC redirect URL")
	fs.StringVar(&c.syncSource, "sync-source", rt.env("SQLON_SYNC_SOURCE", "JAMYPG_SYNC_SOURCE"), "Metadata sync profile")
	fs.DurationVar(&c.syncInterval, "sync-interval", 0, "Metadata sync interval")
	fs.DurationVar(&c.observeInterval, "observe-interval", observeInterval, "Workload/capacity collection interval (0 disables)")
	fs.IntVar(&c.observeRetentionDays, "observe-retention-days", observeRetention, "Local operational snapshot retention days")
	fs.BoolVar(&c.syncApply, "sync-apply", false, "Apply collected physical metadata")
	fs.StringVar(&c.digestWebhook, "digest-webhook", rt.env("SQLON_DIGEST_WEBHOOK", "JAMYPG_DIGEST_WEBHOOK"), "Digest webhook")
	fs.BoolVar(&c.dbaDigest, "dba-digest", false, "Include DBA digest")
	fs.StringVar(&c.dbaDigestProfile, "dba-digest-profile", rt.env("SQLON_DBA_DIGEST_PROFILE", "JAMYPG_DBA_DIGEST_PROFILE"), "DBA digest profile")
	fs.StringVar(&c.omURL, "openmetadata-url", rt.env("SQLON_OPENMETADATA_URL", "JAMYPG_OPENMETADATA_URL"), "OpenMetadata URL")
	fs.StringVar(&c.omToken, "openmetadata-token", rt.env("SQLON_OPENMETADATA_TOKEN", "JAMYPG_OPENMETADATA_TOKEN"), "OpenMetadata token")
	fs.BoolVar(&c.omSync, "openmetadata-sync", false, "Sync OpenMetadata")
	fs.StringVar(&c.omScope, "openmetadata-scope", rt.env("SQLON_OPENMETADATA_SCOPE", "JAMYPG_OPENMETADATA_SCOPE"), "OpenMetadata scope")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "data" {
			c.autoMigrate = false
		}
	})
	return c, nil
}

func (rt Runtime) env(primary, legacy string) string {
	if v := rt.Getenv(primary); v != "" {
		return v
	}
	return rt.Getenv(legacy)
}
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func ValidateHTTPExposure(transport, addr, metaDSN, adminToken string, publicMCP bool) error {
	switch strings.ToLower(strings.TrimSpace(transport)) {
	case "stdio":
		return nil
	case "http", "streamable-http":
	default:
		return fmt.Errorf("unsupported transport %q: use http or stdio", transport)
	}
	loopback, err := isLoopbackListenAddress(addr)
	if err != nil {
		return err
	}
	if loopback || strings.TrimSpace(metaDSN) != "" {
		return nil
	}
	if publicMCP && strings.TrimSpace(adminToken) != "" {
		return nil
	}
	if publicMCP {
		return fmt.Errorf("refusing public standalone HTTP on %q without an admin token: configure SQLON_ADMIN_TOKEN, or use -meta-db", addr)
	}
	return fmt.Errorf("refusing standalone HTTP on non-loopback address %q: use loopback, -meta-db, or -public-mcp plus -admin-token", addr)
}
func isLoopbackListenAddress(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return false, fmt.Errorf("invalid HTTP listen address %q: %w", addr, err)
	}
	if strings.EqualFold(host, "localhost") {
		return true, nil
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback(), nil
}
