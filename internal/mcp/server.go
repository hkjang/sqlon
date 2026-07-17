package mcp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sqlon/internal/catalog"
	"sqlon/internal/change"
	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
	"sqlon/internal/fleet"
	"sqlon/internal/meta"
	"sqlon/internal/observability"
	"sqlon/internal/storage"
)

const ProtocolVersion = "2025-06-18"

// Version is the SQLON server version, surfaced in serverInfo, /auth/me, and
// the web UI (sidebar footer). It is a var so release builds can inject the
// git tag via -ldflags "-X jamypg/internal/mcp.Version=<v>", keeping the
// reported version in lockstep with the release instead of drifting.
var Version = "0.58.0"

type Options struct {
	Endpoint         string
	AllowedOrigins   []string
	Stateful         bool
	SSEPost          bool
	AdminToken       string // when set, mutating /api/* endpoints require it
	FeedbackTenantID string // server-owned workspace/tenant scope for feedback

	// OpenMetadata integration (optional): base URL + bot JWT for importing
	// curated metadata and exporting jamypg-owned descriptions.
	OpenMetadataURL   string
	OpenMetadataToken string
}

type Server struct {
	catalogPtr   atomic.Pointer[catalog.Catalog]
	Options      Options
	DB           *dbconn.Manager
	Collector    *collector.Service
	Changes      *change.Service
	Meta         *meta.Service // nil = standalone mode (auth disabled)
	OIDC         *OIDCProvider // nil = SSO disabled
	mu           sync.Mutex
	dataMu       sync.Mutex        // serializes dataset mutations + catalog reloads
	settingsMu   sync.RWMutex      // guards Options.AdminToken/AllowedOrigins/OIDC live updates
	bootDefaults map[string]string // flag/env setting values captured at EnableMeta
	sessions     map[string]time.Time
	events       map[string]uint64
	// pendingClar tracks blocking clarification questions per MCP session:
	// prepare_sql_context sets them when it withholds the skeleton and clears
	// them once the (re-)call succeeds; run_sql_safely refuses to execute
	// while any remain, closing the "ignore the question and run anyway" hole.
	pendingClar     map[string][]string
	queryCache      *resultCache // TTL result cache for repeated identical queries
	asyncJobs       *asyncJobStore
	feedbackLimiter *feedbackRateLimiter
	auditMu         sync.Mutex             // serializes audit appends + hash chaining
	auditTips       map[string]auditTipRec // audit file path → last {seq, hash}
	metrics         *metricsRegistry       // in-memory Prometheus counters
	// dataDir is the fixed operational data directory captured at boot. Unlike
	// s.cat().DataDir (which follows the active catalog when it is hot-swapped
	// to a profile workspace), operational side-channels — DB profile registry,
	// audit log, metasync snapshots, OpenMetadata config, profile workspaces —
	// stay pinned here so a catalog switch never relocates them.
	dataDir string
	// wsCache holds compiled profile-workspace catalogs for per-request use
	// (prepare/validate/execute with a profile), fingerprint-invalidated.
	wsMu    sync.Mutex
	wsCache map[string]wsCacheEntry
}

// opDir returns the fixed operational data dir (falls back to the active
// catalog's dir when unset, e.g. stdio construction).
func (s *Server) opDir() string {
	if s.dataDir != "" {
		return s.dataDir
	}
	return s.cat().DataDir
}

// cat returns the current catalog; dataset tools swap it atomically so
// in-flight requests keep a consistent snapshot.
func (s *Server) cat() *catalog.Catalog {
	return s.catalogPtr.Load()
}

func (s *Server) setCatalog(c *catalog.Catalog) {
	if c != nil {
		c.SetFeedbackTenant(s.Options.FeedbackTenantID)
	}
	s.catalogPtr.Store(c)
}

type rpcMessage struct {
	JSONRPC string           `json:"jsonrpc,omitempty"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func NewServer(c *catalog.Catalog, opts Options) *Server {
	if opts.Endpoint == "" {
		opts.Endpoint = "/mcp"
	}
	if strings.TrimSpace(opts.FeedbackTenantID) == "" {
		opts.FeedbackTenantID = "default"
	}
	dbManager := dbconn.NewManager(c.DataDir)
	s := &Server{
		Options:         opts,
		dataDir:         c.DataDir,
		DB:              dbManager,
		Collector:       collector.New(dbManager, storage.NewFileStore(c.DataDir)),
		Changes:         change.NewService(),
		sessions:        map[string]time.Time{},
		events:          map[string]uint64{},
		pendingClar:     map[string][]string{},
		queryCache:      newResultCache(),
		asyncJobs:       newAsyncJobStore(),
		feedbackLimiter: newFeedbackRateLimiter(feedbackDefaultLimit, feedbackDefaultWindow),
		metrics:         newMetricsRegistry(),
	}
	s.setCatalog(c)
	return s
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc(s.Options.Endpoint, s.handleMCP)
	mux.HandleFunc("/healthz", s.handleHealth)
	s.registerAuth(mux)
	s.registerAuthAPI(mux)
	s.registerAdmin(mux)
	s.registerDBAPI(mux)
	s.registerDBAConsole(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"endpoint": s.Options.Endpoint,
		"catalog":  s.cat().Summary(),
	})
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if !s.validateOrigin(r) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodOptions {
		s.writeCORS(w, r)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.writeCORS(w, r)
	// Session id rides the context in every mode: activity correlation and
	// the clarification gate both key on it.
	if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
		r = r.WithContext(withSession(r.Context(), sid))
	}
	// With the meta DB active, MCP over HTTP requires an authenticated
	// identity (MCP key / session / master token); the resolved user rides
	// the context so tools can enforce per-profile permissions and audit.
	if s.authEnabled() {
		u, err := s.authenticate(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="jamypg-mcp"`)
			http.Error(w, "authentication required: pass an MCP key via Authorization: Bearer ssk_... or X-MCP-Key (manage keys at /admin/keys)", http.StatusUnauthorized)
			return
		}
		r = r.WithContext(withUser(r.Context(), u))
	} else if strings.TrimSpace(s.Options.AdminToken) != "" {
		// Standalone with a master token set: the same token that guards
		// mutating REST endpoints must also gate mutating/DB-executing MCP
		// tools over HTTP, so tools/call can't be an unauthenticated bypass.
		// Read-only tools stay open. stdio has no headers and is treated as
		// locally trusted (handleRequest runs without this flag).
		r = r.WithContext(withHTTPAdmin(r.Context(), s.checkMasterToken(r)))
	}
	switch r.Method {
	case http.MethodPost:
		s.handlePost(w, r)
	case http.MethodGet:
		s.handleGet(w, r)
	case http.MethodDelete:
		s.handleDelete(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	if err := validateProtocolHeader(r); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()
	var msg rpcMessage
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	if err := dec.Decode(&msg); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(nil, -32700, "parse error", err.Error()))
		return
	}
	if msg.JSONRPC != "" && msg.JSONRPC != "2.0" {
		writeJSON(w, http.StatusBadRequest, errorResponse(msg.ID, -32600, "invalid request", "jsonrpc must be 2.0"))
		return
	}
	// Session handling is lenient: a known Mcp-Session-Id refreshes its
	// timestamp, but requests without one (or with an unknown/expired one)
	// are still served. Several MCP clients (qwen-code, opencode, ...) do
	// not echo the session header back, and rejecting them with 400
	// "missing or unknown Mcp-Session-Id" breaks tools/list entirely.
	s.touchSession(r)
	if msg.Method == "" || msg.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	resp := s.handleRequest(r.Context(), msg)
	if msg.Method == "initialize" && s.Options.Stateful && resp.Error == nil {
		sessionID := s.newSession()
		w.Header().Set("Mcp-Session-Id", sessionID)
	}
	w.Header().Set("MCP-Protocol-Version", ProtocolVersion)
	if s.Options.SSEPost || wantsSSE(r) {
		s.writeSSEResponse(w, r, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	if err := validateProtocolHeader(r); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !accepts(r, "text/event-stream") && !accepts(r, "*/*") {
		http.Error(w, "Accept must include text/event-stream", http.StatusNotAcceptable)
		return
	}
	sessionID := "stateless"
	if s.Options.Stateful {
		if id, ok := s.sessionFromRequest(r); ok {
			sessionID = id
		}
		// no session header is tolerated: stream proceeds with a shared
		// stateless event-ID space for compatibility with lenient clients
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("MCP-Protocol-Version", ProtocolVersion)
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	s.writeSSEEvent(w, sessionID, map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/message",
		"params": map[string]any{
			"level":  "info",
			"logger": "sqlon",
			"data":   "stream opened",
		},
	})
	flusher.Flush()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if !s.Options.Stateful {
		http.Error(w, "sessions disabled", http.StatusMethodNotAllowed)
		return
	}
	id := r.Header.Get("Mcp-Session-Id")
	if id == "" {
		http.Error(w, "missing Mcp-Session-Id", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	delete(s.sessions, id)
	delete(s.events, id)
	delete(s.pendingClar, id)
	s.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleRequest(ctx context.Context, msg rpcMessage) rpcResponse {
	switch msg.Method {
	case "initialize":
		return resultResponse(msg.ID, s.initializeResult())
	case "ping":
		return resultResponse(msg.ID, map[string]any{})
	case "tools/list":
		return resultResponse(msg.ID, map[string]any{"tools": s.tools()})
	case "tools/call":
		start := time.Now()
		result, err := s.callTool(ctx, msg.Params)
		elapsed := time.Since(start)
		s.auditToolCall(msg.Params, elapsed, err)
		s.recordMCPActivity(ctx, msg.Params, result, elapsed)
		if err != nil {
			return resultResponse(msg.ID, toolError(err.Error()))
		}
		return resultResponse(msg.ID, toolResult(result))
	case "resources/list":
		return resultResponse(msg.ID, map[string]any{"resources": s.resources()})
	case "resources/templates/list":
		return resultResponse(msg.ID, map[string]any{"resourceTemplates": s.resourceTemplates()})
	case "resources/read":
		result, err := s.readResource(msg.Params)
		if err != nil {
			return errorResponse(msg.ID, -32602, "invalid params", err.Error())
		}
		return resultResponse(msg.ID, result)
	case "prompts/list":
		return resultResponse(msg.ID, map[string]any{"prompts": s.prompts()})
	case "prompts/get":
		result, err := s.getPrompt(msg.Params)
		if err != nil {
			return errorResponse(msg.ID, -32602, "invalid params", err.Error())
		}
		return resultResponse(msg.ID, result)
	default:
		return errorResponse(msg.ID, -32601, "method not found", msg.Method)
	}
}

func (s *Server) initializeResult() map[string]any {
	return map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities": map[string]any{
			"tools":     map[string]any{"listChanged": false},
			"resources": map[string]any{"subscribe": false, "listChanged": false},
			"prompts":   map[string]any{"listChanged": false},
			"logging":   map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "sqlon",
			"version": Version,
		},
		"instructions": "Workflow: 1) prepare_sql_context(question) runs the whole front half in one call (analyze → search → metric defs → schema context → join paths → SQL skeleton) and returns one bundle — START HERE. (Only fall back to the individual tools — analyze_question, search_schema, find_filter_columns, resolve_time, get_metric_definition, get_schema_context, get_join_paths, build_sql_skeleton — when you need to refine one part.) 1b) If the response has status=needs_clarification, DO NOT generate SQL: relay each clarifications[].question to the user verbatim (show options with the recommended one marked), then re-call prepare_sql_context(question, clarifications={id: answer-or-option-key}). Advisory items come with safe defaults — proceed but list them as assumptions in the final answer. 2) Fill only the skeleton's /* SLOT */ comments to complete one DB SELECT, using solely the bundle's tables/columns, dictionary metric expressions, and provided join conditions; never expose pii columns and always bound rows. 3) validate_sql passing metrics=metric_names and expected_outputs=expected_output_columns from the bundle (apply fix_hints, max 2 retries); for hard questions generate 2-3 candidates and pick rank_candidates best_sql. 4) explain_sql; if risk=high, regenerate with limit/period constraints instead of executing. 5) run_sql_safely(sql, profile) to execute read-only (discover profiles with list_db_profiles); it refuses with status=clarification_required while blocking clarifications remain unanswered in this session. 6) Return a structured JSON answer: {sql, used_tables, used_columns, applied_metrics, applied_join_paths, applied_filters, assumptions, cautions, validation_result, executable}. 7) record_feedback with the outcome.",
	}
}

func (s *Server) tools() []map[string]any {
	list := []map[string]any{
		tool("analyze_question", "Analyze a Korean/English NL2SQL question into intent, metrics, dimensions, filters, schema hints, and likely metadata hits. → Usually unnecessary on its own: prepare_sql_context already runs this. → next: search_schema.", objectSchema(map[string]any{"question": str("Natural language question")}, []string{"question"})),
		tool("retrieve_context", "GraphRAG-style retrieval: seed search (order-preserving) → 1-hop join-graph expansion → append up to 2 graph discoveries (joinable neighbors / value-evidence tables). Every candidate carries 7-signal provenance (semantic, lexical, proximity, joinability, value evidence, usage prior, freshness) plus join paths, business terms, metrics, and value evidence. Golden-set calibrated: table recall@5 = plain search (no regression) + discoveries. prepare_sql_context uses this internally; call directly to inspect WHY tables were chosen.", objectSchema(map[string]any{
			"question": str("Natural language question"),
			"top_k":    integer("Number of ranked tables to return (default 6)"),
		}, []string{"question"})),
		tool("suggest_join_relations", "Scan the golden set for expected-table pairs with NO join path (the Join Path Recall gaps) and propose candidate join keys from shared key-like columns, as paste-ready relations.json entries for operator review (/admin/editor?ds=relations).", objectSchema(map[string]any{
			"golden_path": str("Optional golden_queries.json path"),
		}, nil)),
		tool("search_schema", "Search catalog tables and columns using compiled metadata, business terms, code dictionaries, and naming hints. → next: get_schema_context / get_join_paths for the chosen tables.", objectSchema(map[string]any{
			"question":        str("Natural language question or search phrase"),
			"top_k":           integer("Number of table candidates to return"),
			"schemas":         arrayOf("string", "Optional schema names to restrict the search to (e.g. as returned by list_datasets/get_catalog_health)"),
			"include_columns": boolSchema("Return matched columns for each table"),
			"max_columns":     integer("Maximum matched columns per table"),
		}, []string{"question"})),
		tool("get_schema_context", "Build compact SQL-generation context for selected tables or the best tables for a question. → next: build_sql_skeleton, then validate_sql.", objectSchema(map[string]any{
			"question":              str("Optional question used to select relevant columns"),
			"tables":                arrayOf("string", "Schema-qualified table names"),
			"max_columns_per_table": integer("Maximum columns per table in the context"),
		}, nil)),
		tool("get_join_paths", "Find recommended join paths from the compiled relation graph. Take ON conditions only from this output; never invent joins. → next: build_sql_skeleton.", objectSchema(map[string]any{
			"from_table": str("Start table"),
			"to_tables":  arrayOf("string", "Target tables"),
			"tables":     arrayOf("string", "Ordered tables; paths are found between adjacent pairs"),
			"max_depth":  integer("Maximum relation hops"),
		}, nil)),
		tool("get_metric_definition", "Infer candidate metric expressions from metadata and naming conventions.", objectSchema(map[string]any{
			"metric_name": str("Metric or business term"),
			"top_k":       integer("Number of candidates"),
		}, []string{"metric_name"})),
		tool("get_column_stats", "Return metadata-backed column stats: type, null constraint, PK/FK, index, code dictionary, and sample usage count.", objectSchema(map[string]any{
			"table":  str("Table name"),
			"column": str("Column name"),
		}, []string{"table", "column"})),
		tool("search_examples", "Search golden SQL examples from sql_datasets.json.", objectSchema(map[string]any{
			"question": str("Natural language question"),
			"top_k":    integer("Number of examples"),
			"table":    str("Optional table filter"),
		}, []string{"question"})),
		tool("validate_sql", "Statically validate SQL against catalog tables, columns, join graph, PII policy, code-value dictionaries, dialect, and read-only rules. CTE/inline-view aware. Returns structured fix_hints; retry at most twice. Accepts expected_output_columns / metric_names aliases from prepare_sql_context. Also returns advisory `lint` anti-pattern findings (SELECT *, leading-wildcard LIKE, non-sargable predicates, …) that never block validity. → next: explain_sql, then run_sql_safely.", objectSchema(map[string]any{
			"sql":              str("SQL to validate"),
			"limit":            integer("Preview row limit for bounded_sql"),
			"metrics":          arrayOf("string", "Dictionary metric names this SQL claims to implement; checked against metric expressions"),
			"expected_outputs": arrayOf("string", "Business terms (dimensions/metrics from analyze_question expected_output_columns) the SELECT list must cover"),
			"profile":          str("Optional DB profile id: when that profile has a catalog workspace, validate against IT (the right catalog for that DB) instead of the active catalog"),
		}, []string{"sql"})),
		tool("explain_sql", "Risk estimate for a query. Always returns the static metadata-based analysis; when a db profile is given it additionally runs a real EXPLAIN (postgres/mysql/mariadb, JSON format) and analyzes the plan for full scans, cartesian joins, oversized row estimates, large sorts, and high cost. If live risk is high, regenerate with period/limit constraints instead of executing.", objectSchema(map[string]any{
			"sql":     str("SQL to explain"),
			"limit":   integer("Preview row limit"),
			"profile": str("DB profile id for a live EXPLAIN PLAN; omit for static-only"),
		}, []string{"sql"})),
		tool("list_db_profiles", "List the DB DB connection profiles the caller may use (id, name, masked connect target, pool/policy, driver availability). Call this to discover which profile id to pass to run_sql_safely / explain_sql / run_evaluation. In auth mode only profiles you own, were granted, or that are shared are returned; admins see all.", objectSchema(map[string]any{}, nil)),
		tool("list_database_instances", "권한 범위 안의 SQLON DB 플릿 인벤토리를 반환합니다. 대상 DB에 연결하지 않으며 환경, 업무서비스, 중요도, 엔진, 역할, 담당팀, 위치와 선언된 Capability를 제공합니다.", objectSchema(map[string]any{}, nil)),
		tool("get_fleet_health", "모든 사용 가능 DB 프로파일의 연결 상태를 독립적으로 병렬 수집하고 위험도 순으로 반환합니다. 각 결과에는 수집 시각, 구조화된 실패 원인, 근거, 영향 중요도와 기능 지원 상태가 포함됩니다.", objectSchema(map[string]any{}, nil)),
		tool("list_sessions", "대상 DB의 활성·비활성 세션과 장기 SQL·장기 트랜잭션을 분리해 조회합니다. Oracle 세션 키는 INST_ID:SID:SERIAL#이며 시스템·복제 세션 보호 여부, 근거와 수집 시각을 포함합니다. SQL 본문과 bind 값은 반환하지 않습니다.", objectSchema(map[string]any{
			"profile": str("조회할 DB 프로파일 ID"),
		}, []string{"profile"})),
		tool("get_lock_tree", "엔진 시스템 뷰에서 blocker→blocked 관계를 수집해 루트 블로커, 영향받는 세션 수, 대기시간과 근거를 반환합니다. 읽기 전용이며 세션을 취소하거나 종료하지 않습니다.", objectSchema(map[string]any{
			"profile": str("조회할 DB 프로파일 ID"),
		}, []string{"profile"})),
		tool("get_workload_summary", "대상 DB 시스템 카운터의 QPS/TPS, 연결, I/O, 대기 이벤트를 최신 저장 스냅숏에서 반환합니다. fresh=true면 고정된 읽기 전용 Provider 쿼리로 새 스냅숏을 수집합니다.", objectSchema(map[string]any{
			"profile": str("조회할 DB 프로파일 ID"), "fresh": boolSchema("새 스냅숏을 즉시 수집하고 저장"),
		}, []string{"profile"})),
		tool("get_top_sql", "대상 엔진의 누적 SQL 통계에서 원문 없이 fingerprint/SQL ID별 호출 수, elapsed, CPU, reads, rows와 Oracle plan hash를 반환합니다. 라이선스·권한 제한은 limitation으로 구분합니다.", objectSchema(map[string]any{
			"profile": str("조회할 DB 프로파일 ID"), "fresh": boolSchema("새 스냅숏을 즉시 수집하고 저장"),
		}, []string{"profile"})),
		tool("get_storage_status", "DB·테이블·tablespace 용량과 사용률, 이전 스냅숏 대비 일간 증가량을 반환합니다. 80/90퍼센트 임계치 근거를 포함합니다.", objectSchema(map[string]any{
			"profile": str("조회할 DB 프로파일 ID"), "fresh": boolSchema("새 스냅숏을 즉시 수집하고 저장"),
		}, []string{"profile"})),
		tool("route_db_profile", "Given a SQL statement, pick the DB profile that can actually serve it when many profiles are registered. Extracts the referenced tables via the dialect parser and scores each usable profile on live table inventory (does the DB really contain those tables), operator-declared routing.schemas, engine dialect, circuit-breaker health, and routing priority/default. Returns selected_profile with decisive=true when there is one clear winner, otherwise decisive=false with ranked candidates to choose from. run_sql_safely(profile=\"auto\") calls this internally.", objectSchema(map[string]any{
			"sql": str("SQL whose target profile should be resolved"),
		}, []string{"sql"})),
		tool("run_sql_safely", "Validate SQL and, when a db profile is given, execute it read-only against the target DB (postgres/mysql/mariadb) with query timeout, row limit (+truncated flag), PII value masking, a 60s identical-query result cache (cached:true), and audit logging. Before executing it runs a live EXPLAIN plan-approval gate: if the estimated plan risk meets the profile threshold it returns status=plan_approval_required with the plan instead of running — narrow the query, or (only after user approval) re-call with approve_plan=true. Zero rows / NULL-heavy output come back with result_diagnosis hints. Successful responses may include advisory `lint_warnings` (SQL anti-patterns) — surface them but they do not block execution. Without a profile it stays a dry-run guard returning bounded SQL. Catalog-invalid SQL is never executed.", objectSchema(map[string]any{
			"sql":             str("SQL to validate/execute"),
			"limit":           integer("Row limit (capped by the profile's max_rows)"),
			"profile":         str("DB profile id from db_profiles (see /admin/db); omit for dry-run"),
			"timeout_seconds": integer("Query timeout override (must be shorter than the profile default)"),
			"fresh":           boolSchema("true → bypass the 60s result cache and hit the DB"),
			"approve_plan":    boolSchema("true → bypass the execution-plan approval gate after a prior status=plan_approval_required response; set only with explicit user approval"),
		}, []string{"sql"})),
		tool("execute_with_repair", "Self-correcting execution: validate → run read-only → diagnose in ONE call, and on any recoverable failure return a `repair` kit with everything needed to fix the SQL in one more turn — the failure phase (validation|plan|execution|empty_result), the classified error_code (PG-…/MY-…) with a Korean hint, catalog fix_hints, and the SCHEMA of the referenced tables so column/table names can be corrected in place. status=needs_fix when a fix is required, executed_empty for zero rows (with zero_row_hints), executed on success. Same guardrails as run_sql_safely; this just reshapes the response to halve repair round-trips. Prefer this over run_sql_safely when you expect to iterate.", objectSchema(map[string]any{
			"sql":             str("SQL to validate/execute"),
			"question":        str("Original NL question (optional) — sharpens the schema context returned for repair"),
			"limit":           integer("Row limit (capped by the profile's max_rows)"),
			"profile":         str("DB profile id (or 'auto' to route); omit for dry-run validation only"),
			"timeout_seconds": integer("Query timeout override"),
			"fresh":           boolSchema("true → bypass the 60s result cache"),
			"approve_plan":    boolSchema("true → bypass the plan-approval gate after a prior plan phase; only with user approval"),
		}, []string{"sql"})),
		tool("list_metadata_sources", "List DB profiles usable as automated metadata-collection sources (source_id, name, type, masked connect target). Use a source_id with discover_metadata / run_metadata_sync / diff_metadata_snapshots. Physical metadata is auto-collected; business meaning stays approval-based.", objectSchema(map[string]any{}, nil)),
		tool("discover_metadata", "List the non-system schemas available on a metadata source database, so you can scope a sync. Read-only; queries information_schema only.", objectSchema(map[string]any{
			"source": str("metadata source id (a db profile id from list_metadata_sources)"),
		}, []string{"source"})),
		tool("run_metadata_sync", "Collect the physical model (schemas, tables, views, columns, PK/FK/unique/check constraints, indexes, comments, row-count estimates) from a source DB into a versioned snapshot, and return the change set versus the previous snapshot. Incremental by default: if the schema hash is unchanged it skips without storing a redundant snapshot. Deletions are reported as retire candidates, never applied immediately. This collects PHYSICAL facts only — it never writes business meaning (logical names, metrics) into the operational catalog.", objectSchema(map[string]any{
			"source":        str("metadata source id (db profile id)"),
			"schemas":       arrayOf("string", "Optional schema names to scope collection; omit for all non-system schemas"),
			"incremental":   boolSchema("true (default) → skip when the schema hash is unchanged; false → always snapshot and diff"),
			"include_views": boolSchema("Collect views/materialized views and their SQL (default false)"),
		}, []string{"source"})),
		tool("suggest_indexes", "Index advisor: mine the query audit log for slow, successful queries and propose candidate indexes for the WHERE/JOIN/ORDER-BY columns that lack one, ranked by impact (occurrences × avg latency), each with ready-to-review CREATE INDEX DDL and a sample query. Read-only and ADVISORY — it never creates anything; a DBA reviews the DDL (considering cardinality and write overhead). Uses the profile's workspace catalog when one exists.", objectSchema(map[string]any{
			"profile":        str("Optional DB profile id to scope the audit log; omit for all"),
			"min_elapsed_ms": integer("Slow-query threshold in ms (default 200)"),
			"days":           integer("How many days of audit log to scan (default 7)"),
			"verify":         boolSchema("When true and a profile is given, run a live EXPLAIN (read-only) on each top candidate's sample query to confirm a full/seq scan on the table — trims false positives; sets plan_confirms/plan_cost"),
		}, nil)),
		tool("lint_sql", "SQL anti-pattern linter: statically scan one statement for classic performance/correctness smells — SELECT *, leading-wildcard LIKE, NOT IN (subquery), function-wrapped indexed columns (non-sargable), inequality on an indexed column, implicit comma cross-join, OR in WHERE, ORDER BY without LIMIT, and DML without WHERE — each with a severity and a concrete fix suggestion. Catalog-aware (index coverage) and advisory: it flags, it does not rewrite. Read-only.", objectSchema(map[string]any{
			"sql":     str("SQL statement to lint"),
			"profile": str("Optional DB profile id: lint against that profile's catalog workspace (index coverage) instead of the active catalog"),
		}, []string{"sql"})),
		tool("explain_sql_in_words", "Describe a SQL statement in plain Korean: which tables (with catalog logical names), what it filters/joins/groups/orders on, and which aggregates it computes, plus a one-line prose summary. Static structural analysis — it does not execute the query. Useful for reviewing generated SQL or documenting a query's intent.", objectSchema(map[string]any{
			"sql":     str("SQL statement to explain"),
			"profile": str("Optional DB profile id: resolve table logical names from that profile's catalog workspace"),
		}, []string{"sql"})),
		tool("workload_report", "Workload report: aggregate the query audit log over a window into an operational profile — total/success/error counts and error rate, latency percentiles (avg/p50/p95/p99/max), slow-query count, hottest tables, top error codes, usage by tool and by profile, the slowest statements, and the busiest hour. Read-only summary of what already ran. Pair with suggest_indexes (index candidates) and lint_sql (per-statement smells).", objectSchema(map[string]any{
			"profile": str("Optional DB profile id to scope the report; omit for all"),
			"days":    integer("How many days of audit log to scan (default 7)"),
			"slow_ms": integer("Slow-query threshold in ms (default 200)"),
		}, nil)),
		tool("get_dba_digest", "Compact proactive DBA snapshot distilled from the workload report and index advisor: query volume, error rate, p95/max latency, slow-query count, hottest tables, and the top index candidates, with a one-line headline. Read-only. Same data the scheduler posts to the digest webhook each tick — use for a periodic 'what needs attention' glance.", objectSchema(map[string]any{
			"profile": str("Optional DB profile id to scope the digest; omit for all"),
			"days":    integer("How many days of audit log to summarize (default 7)"),
			"slow_ms": integer("Slow-query threshold in ms (default 200)"),
		}, nil)),
		tool("create_change_plan", "변경 실행 전 사전 상태·영향·검증·보상 작업을 구조화한 초안을 생성합니다. 이 호출은 DB를 변경하지 않습니다.", objectSchema(map[string]any{
			"id": str("Unique change request id"), "profile_id": str("Target DB profile id"), "target": str("Instance/database/schema/object target"),
			"reason": str("Business and operational reason"), "risk": str("low | medium | high | critical | emergency"),
			"pre_state": map[string]any{"description": "Observed pre-change evidence"}, "impact": map[string]any{"description": "Affected objects and services"},
			"expected_lock": str("Expected lock type and scope"), "estimated_duration": str("Estimated duration or unknown"),
			"preconditions": arrayOf("string", "Backup, replication, capacity, and session preconditions"), "maintenance_window": str("Allowed execution window"),
			"steps": arrayOfObjects("Ordered steps: {order, command, verification, compensation}"),
		}, []string{"id", "profile_id", "target", "reason", "risk", "steps"})),
		tool("evaluate_change_risk", "서버 정책으로 위험도와 필요한 승인 수를 평가합니다. 클라이언트가 승인 수를 낮출 수 없습니다.", objectSchema(map[string]any{
			"risk": str("low | medium | high | critical | emergency"),
		}, []string{"risk"})),
		tool("submit_change", "변경계획 초안을 분석/검토 상태로 제출합니다. 중간 이상 위험은 승인 전 실행할 수 없습니다.", objectSchema(map[string]any{"id": str("Change request id")}, []string{"id"})),
		tool("approve_change", "검토 대기 중인 변경을 현재 DBA 자격으로 승인합니다. 치명적 변경은 서로 다른 2인의 승인이 필요합니다.", objectSchema(map[string]any{"id": str("Change request id")}, []string{"id"})),
		tool("execute_approved_change", "승인 ID와 실행 직전 재검증을 거쳐 승인된 불변 단계만 실행하고 사후 검증합니다.", objectSchema(map[string]any{
			"id": str("Change request id"), "approval_id": str("Approval id returned by approve_change; required for approval-gated risk"),
		}, []string{"id"})),
		tool("verify_change", "변경 상태, 승인, 실행·검증 결과를 조회합니다. 이 호출은 추가 SQL을 실행하지 않습니다.", objectSchema(map[string]any{"id": str("Change request id")}, []string{"id"})),
		tool("rollback_change", "rollback_required 상태의 변경에 대해 보상 작업을 역순 실행합니다.", objectSchema(map[string]any{"id": str("Change request id")}, []string{"id"})),
		tool("cancel_change", "실행 전의 초안·검토·승인·예약 변경을 취소합니다.", objectSchema(map[string]any{"id": str("Change request id")}, []string{"id"})),
		tool("dba_overview", "DBA console overview for a profile: dialect, whether DBA credentials are configured, server version, and role/database counts. Read-only. Requires the dba/admin role and a DBA-enabled profile (db_profiles.dba).", objectSchema(map[string]any{
			"profile": str("DB profile id (must have dba credentials configured)"),
		}, []string{"profile"})),
		tool("dba_list_users", "List database users/roles and their attributes (superuser, createdb, createrole, login, connection limit). Read-only privileged query.", objectSchema(map[string]any{
			"profile": str("DB profile id"),
		}, []string{"profile"})),
		tool("dba_list_databases", "List databases with owner, encoding, collation, and size. Read-only privileged query.", objectSchema(map[string]any{
			"profile": str("DB profile id"),
		}, []string{"profile"})),
		tool("dba_list_settings", "List server configuration parameters (pg_settings / global_variables), optionally filtered by name substring. Read-only.", objectSchema(map[string]any{
			"profile": str("DB profile id"),
			"filter":  str("Optional name substring filter (e.g. 'work_mem')"),
		}, []string{"profile"})),
		tool("dba_list_sessions", "List active sessions/backends (pid, user, database, state, duration, current query). Read-only. Use dba_terminate_session to cancel or kill one.", objectSchema(map[string]any{
			"profile": str("DB profile id"),
		}, []string{"profile"})),
		tool("dba_create_user", "Create a database user/role. Postgres: CREATE ROLE with LOGIN/SUPERUSER/CREATEDB/CREATEROLE attributes. MySQL/MariaDB: CREATE USER 'name'@'%'. Password (if given) is set and redacted in the audit log. WRITE — audited.", objectSchema(map[string]any{
			"profile":    str("DB profile id"),
			"username":   str("Role/user name to create"),
			"password":   str("Optional password"),
			"can_login":  boolSchema("Postgres: LOGIN (default true)"),
			"superuser":  boolSchema("Grant SUPERUSER (postgres)"),
			"createdb":   boolSchema("Grant CREATEDB (postgres)"),
			"createrole": boolSchema("Grant CREATEROLE (postgres)"),
		}, []string{"profile", "username"})),
		tool("dba_alter_user", "Alter a user/role: change password and/or attributes. Postgres alters LOGIN/SUPERUSER/CREATEDB/CREATEROLE and password; MySQL/MariaDB supports password change. WRITE — audited.", objectSchema(map[string]any{
			"profile":    str("DB profile id"),
			"username":   str("Role/user name to alter"),
			"password":   str("New password (optional)"),
			"can_login":  boolSchema("Set LOGIN/NOLOGIN (postgres)"),
			"superuser":  boolSchema("Set SUPERUSER/NOSUPERUSER (postgres)"),
			"createdb":   boolSchema("Set CREATEDB/NOCREATEDB (postgres)"),
			"createrole": boolSchema("Set CREATEROLE/NOCREATEROLE (postgres)"),
		}, []string{"profile", "username"})),
		tool("dba_drop_user", "Drop a user/role (IF EXISTS). Destructive — requires confirm=true. WRITE — audited.", objectSchema(map[string]any{
			"profile":  str("DB profile id"),
			"username": str("Role/user name to drop"),
			"confirm":  boolSchema("Must be true to proceed"),
		}, []string{"profile", "username"})),
		tool("dba_grant", "Grant or revoke privileges. privileges e.g. 'SELECT, INSERT' or 'ALL PRIVILEGES'; object e.g. 'DATABASE mydb', 'schema.table', or 'mydb.*'; grantee is a role/user. Set revoke=true to REVOKE. WRITE — audited. Note: object/privileges are passed through as written — supply exactly the SQL grant target.", objectSchema(map[string]any{
			"profile":    str("DB profile id"),
			"privileges": str("Privilege list, e.g. 'SELECT, INSERT' or 'ALL PRIVILEGES'"),
			"object":     str("Grant target, e.g. 'DATABASE mydb' / 'public.mytable' / 'mydb.*'"),
			"grantee":    str("Role/user receiving the grant"),
			"revoke":     boolSchema("REVOKE instead of GRANT"),
			"with_grant": boolSchema("Add WITH GRANT OPTION (grant only)"),
		}, []string{"profile", "privileges", "object", "grantee"})),
		tool("dba_create_database", "Create a database. Postgres: OWNER/ENCODING options; MySQL/MariaDB: CHARACTER SET. WRITE — audited. For postgres, point the profile's dba.connect_string at the 'postgres' maintenance DB so this does not run inside a user database.", objectSchema(map[string]any{
			"profile":  str("DB profile id"),
			"name":     str("New database name"),
			"owner":    str("Owner role (postgres, optional)"),
			"encoding": str("Encoding/charset (optional)"),
		}, []string{"profile", "name"})),
		tool("dba_drop_database", "Drop a database (IF EXISTS). Destructive — requires confirm=true. WRITE — audited.", objectSchema(map[string]any{
			"profile": str("DB profile id"),
			"name":    str("Database name to drop"),
			"confirm": boolSchema("Must be true to proceed"),
		}, []string{"profile", "name"})),
		tool("dba_set_parameter", "Change a server configuration parameter. Postgres: ALTER SYSTEM SET (persisted to postgresql.auto.conf) then pg_reload_conf(); restart-only params are flagged pending_restart. MySQL/MariaDB: SET GLOBAL/SESSION. WRITE — audited.", objectSchema(map[string]any{
			"profile":   str("DB profile id"),
			"parameter": str("Bare setting name, e.g. 'work_mem'"),
			"value":     str("New value"),
			"scope":     str("MySQL only: GLOBAL (default) or SESSION"),
		}, []string{"profile", "parameter", "value"})),
		tool("dba_terminate_session", "Terminate or cancel a session/backend by pid. Postgres: pg_terminate_backend (or pg_cancel_backend when cancel_only=true). MySQL/MariaDB: KILL (or KILL QUERY). WRITE — audited.", objectSchema(map[string]any{
			"profile":     str("DB profile id"),
			"pid":         integer("Backend pid / process id (from dba_list_sessions)"),
			"cancel_only": boolSchema("Cancel the running query but keep the connection (postgres pg_cancel_backend / mysql KILL QUERY)"),
		}, []string{"profile", "pid"})),
		tool("dba_run_maintenance", "Run a maintenance operation. Postgres: VACUUM / ANALYZE / REINDEX. MySQL/MariaDB: ANALYZE / OPTIMIZE (table). target is a qualified table/index/database name. WRITE — audited.", objectSchema(map[string]any{
			"profile":   str("DB profile id"),
			"operation": str("VACUUM | ANALYZE | REINDEX (postgres) or ANALYZE | OPTIMIZE (mysql)"),
			"target":    str("Target object (table/index/database); required for REINDEX and mysql ops"),
		}, []string{"profile", "operation"})),
		tool("dba_execute", "Escape hatch: run an ARBITRARY privileged SQL statement (DDL/DCL/maintenance) through the DBA connection. Requires confirm=true. The statement is audited verbatim. Use the structured dba_* tools when one fits; use this for anything they do not cover.", objectSchema(map[string]any{
			"profile": str("DB profile id"),
			"sql":     str("The privileged statement to execute (single statement)"),
			"confirm": boolSchema("Must be true to proceed"),
		}, []string{"profile", "sql"})),
		tool("db_health_report", "DBA health check: run read-only system-catalog diagnostics against a connected DB profile and flag classic issues — tables without a primary key (high), foreign-key columns lacking a supporting index (medium), unused indexes (low), stale/absent planner statistics (medium), and the largest tables with comment coverage (info). PostgreSQL runs all checks; MySQL/MariaDB run the portable subset and mark the rest unsupported. Read-only (pg_catalog/information_schema); nothing is changed — remediation (CREATE INDEX, ANALYZE) is left to the DBA.", objectSchema(map[string]any{
			"profile": str("DB profile id to diagnose"),
		}, []string{"profile"})),
		tool("describe_db_schema", "Introspect a connected DB profile's LIVE schema (information_schema) so you can generate SQL for tables that are not in the catalog. Catalog-first: tables/columns already registered are annotated with their logical names/descriptions, and each table is flagged in_catalog (true = curated metadata, false = live-only physical structure). Read-only, nothing persisted. Use it to ground SQL, then run_sql_safely/execute_with_repair(profile=...) to execute. To make live-only tables pass catalog validation, apply_metadata_sync them.", objectSchema(map[string]any{
			"profile":       str("DB profile id to introspect"),
			"schemas":       arrayOf("string", "Optional schema names to scope; omit for all non-system schemas"),
			"table":         str("Optional single table (name or schema.table) to describe"),
			"include_views": boolSchema("Include views/materialized views (default false)"),
		}, []string{"profile"})),
		tool("list_profile_catalogs", "List every usable DB profile with whether it has its own catalog metadata workspace (under <data>/profiles/<profile>/) and, if so, its table/relation counts and build time. Per-profile workspaces let you view and manage catalog JSON separately for each connected database.", objectSchema(map[string]any{}, nil)),
		tool("get_profile_catalog", "View a DB profile's catalog workspace: catalog summary, dataset-file inventory (with per-file counts/issues), and health. Read-only.", objectSchema(map[string]any{
			"profile": str("DB profile id"),
		}, []string{"profile"})),
		tool("build_profile_catalog", "ADMIN: build/refresh a DB profile's catalog workspace from its LIVE schema — collect the physical model and write meta_physical_models.json + topology_relations.json under <data>/profiles/<profile>/ (existing workspace descriptions preserved; deletions kept as retire candidates unless prune=true). After building, manage business metadata with put_profile_dataset.", objectSchema(map[string]any{
			"profile": str("DB profile id to build from"),
			"schemas": arrayOf("string", "Optional schemas to scope; omit for all non-system schemas"),
			"prune":   boolSchema("true → remove workspace rows absent from the live schema; false (default) → keep as retire candidates"),
		}, []string{"profile"})),
		tool("get_profile_dataset", "Read one metadata JSON file (e.g. overrides, glossary, meta_physical_models) from a DB profile's catalog workspace. Read-only.", objectSchema(map[string]any{
			"profile": str("DB profile id"),
			"dataset": str("dataset name (e.g. overrides, glossary, physical_models, metrics, relations)"),
		}, []string{"profile", "dataset"})),
		tool("put_profile_dataset", "ADMIN: write one metadata JSON file into a DB profile's catalog workspace (validated, previous file backed up, rolled back if the workspace fails to compile). Use to manage per-profile business metadata (logical names via overrides, glossary, metrics, ...).", objectSchema(map[string]any{
			"profile": str("DB profile id"),
			"dataset": str("dataset name to replace"),
			"content": map[string]any{"description": "the full JSON content for the dataset file"},
		}, []string{"profile", "dataset", "content"})),
		tool("import_openmetadata_to_profile", "ADMIN: import OpenMetadata's curated business metadata (display names → logical names, descriptions, PII tags, glossary) into a specific profile's catalog WORKSPACE (not the global catalog) — so each database's business metadata comes from OpenMetadata into that database's own workspace. Gaps only; existing workspace values preserved. apply=false previews, apply=true merges. Build the workspace first (build_profile_catalog).", objectSchema(map[string]any{
			"profile": str("DB profile id whose workspace to import into"),
			"scope":   str("OpenMetadata database/schema FQN to scope; omit for all"),
			"apply":   boolSchema("true → merge into the workspace; false → preview only (default)"),
		}, []string{"profile"})),
		tool("build_all_profile_catalogs", "ADMIN: build/refresh catalog workspaces for MANY profiles at once from their live schemas (all usable profiles when 'profiles' is omitted). Per-profile permission-checked; failures are reported per-profile without aborting the batch. Handy for onboarding many databases.", objectSchema(map[string]any{
			"profiles": arrayOf("string", "Profile ids to build; omit for all usable profiles"),
			"prune":    boolSchema("true → remove workspace rows absent from each live schema; false (default) → keep as retire candidates"),
		}, nil)),
		tool("get_active_catalog", "Report which catalog is currently serving NL2SQL: the default (boot -data dir) or a hot-swapped profile workspace, plus the fixed operational dir (where DB profiles / audit / workspaces stay). Read-only.", objectSchema(map[string]any{}, nil)),
		tool("set_active_catalog", "ADMIN: hot-swap the active NL2SQL catalog to a profile's workspace (or back to the default when profile is empty) WITHOUT restarting. Search / prepare_sql_context / validate then use that workspace's metadata. DB profiles, audit, and workspaces stay at the operational dir (unaffected). Standalone mode only; reverts to -data on restart.", objectSchema(map[string]any{
			"profile": str("profile id to activate; empty or 'default' → the boot -data catalog"),
		}, nil)),
		tool("apply_metadata_sync", "ADMIN: reflect a source's latest collected snapshot into the operational catalog — merge the PHYSICAL model (columns, types, nullability, PK/FK, FK relations) into meta_physical_models.json / topology_relations.json (with backups) and hot-reload. Physical facts are auto-applied; existing column/table descriptions (business meaning) are PRESERVED; dropped tables/columns are retire candidates and are NOT removed unless prune=true. Run run_metadata_sync first to collect a snapshot.", objectSchema(map[string]any{
			"source": str("metadata source id (db profile id) whose latest snapshot to apply"),
			"prune":  boolSchema("true → also remove physical rows for tables/columns absent from the snapshot (within collected schemas); false (default) → keep them as retire candidates"),
		}, []string{"source"})),
		tool("get_sync_status", "List stored metadata snapshots for a source (newest first) with collection time, schema hash, and object counts. Use the snapshot ids with diff_metadata_snapshots.", objectSchema(map[string]any{
			"source": str("metadata source id (db profile id)"),
		}, []string{"source"})),
		tool("diff_metadata_snapshots", "Compute the change set (table/column add/remove, type/nullability/key/comment/index/view-SQL changes, each with severity and disposition) between two stored snapshots of a source. Deletions surface as retire candidates.", objectSchema(map[string]any{
			"source": str("metadata source id (db profile id)"),
			"from":   str("baseline snapshot id"),
			"to":     str("target snapshot id"),
		}, []string{"source", "from", "to"})),
		tool("profile_metadata_assets", "Compute column statistics (row/null/distinct counts, min/max, top values, format patterns) for a source's tables, with bounded cost and privacy protection. Modes: fast (null presence over a 2k-row sample), standard (default; null ratio, distinct, min/max, top values over a 100k-row sample), deep (full scan). SENSITIVE columns (names matching PII/credit patterns, or listed in pii_columns) never store raw values, min/max, or top values — only length range, format pattern, null ratio, and distinct count. Results are reviewable candidates and are NOT written into the operational catalog's column_stats.", objectSchema(map[string]any{
			"source":       str("metadata source id (db profile id)"),
			"tables":       arrayOf("string", "Schema-qualified tables to profile; omit for all tables in the latest snapshot (capped)"),
			"mode":         str("fast | standard (default) | deep"),
			"sample_limit": integer("Override the mode's sample-row cap (0 = mode default)"),
			"pii_columns":  arrayOf("string", "Extra sensitive columns: \"schema.table.col\" or \"*.col\""),
		}, []string{"source"})),
		tool("record_feedback", "Append bounded NL2SQL feedback to the review queue. Actor/session/dataset scope and trust state are server-owned; feedback never affects prompts, retrieval, or learning until an administrator approves it with review_feedback.", objectSchema(map[string]any{
			"question":          str("Original question"),
			"analysis":          map[string]any{"type": "object", "description": "analyze_question output", "additionalProperties": true},
			"tables":            arrayOf("string", "Candidate/used tables"),
			"columns":           arrayOf("string", "Candidate/used columns"),
			"generated_sql":     str("Generated SQL"),
			"validation_errors": map[string]any{"description": "validate_sql errors, if any"},
			"final_sql":         str("Accepted or corrected SQL"),
			"executed":          boolSchema("Whether the SQL was actually executed"),
			"adopted":           boolSchema("Whether the user adopted the result"),
			"outcome":           str("success, failure, corrected, rejected"),
			"duration_ms":       map[string]any{"type": "number", "description": "End-to-end latency in ms"},
			"result_rows":       integer("Row count of the executed result"),
			"failure_cause":     str("Failure cause classification"),
			"notes":             str("Manual correction memo"),
		}, []string{"question", "outcome"})),
		tool("review_feedback", "ADMIN: list pending feedback, or approve/reject one record. Approval is the only path that marks feedback trusted and eligible for few-shot reuse, retrieval priors, and learn_from_feedback.", objectSchema(map[string]any{
			"feedback_id": str("Feedback id to review; omit to list the pending queue"),
			"decision":    str("approve or reject; required with feedback_id"),
			"notes":       str("Optional administrator review note"),
			"limit":       integer("Queue page size when feedback_id is omitted (default 50, max 200)"),
		}, nil)),
		tool("get_catalog_health", "Return catalog compilation status: load/validation issues, metadata coverage gaps, PII columns, and dictionary sizes.", objectSchema(map[string]any{}, nil)),
		tool("get_metadata_quality", "Score every table's metadata quality (completeness, consistency, relationship, profiling, metric linkage, usability, security) 0–100 with an A–E grade, aggregate by schema/domain, and list the top improvement targets. Pass gate=true to instead evaluate the release gate: whether blocking conditions (load errors, broken metrics/certified joins, unclassified PII, quality below the release floor) would stop a catalog release.", objectSchema(map[string]any{
			"gate": boolSchema("true → evaluate the release gate (pass/fail + blocking violations) instead of the full per-table report"),
		}, nil)),
		tool("suggest_semantic_metadata", "Generate REVIEWABLE candidate logical names, semantic types, and descriptions for columns that are missing them, each with evidence and a confidence score. Rule-based and offline (glossary terms, cross-table reuse, abbreviation expansion, name/type patterns) — never applied to the operational catalog. Returns a paste-ready overrides.json columns[] snippet for high-confidence items. As an LLM you can refine these candidates before an operator approves them.", objectSchema(map[string]any{
			"tables": arrayOf("string", "Schema-qualified tables to enrich; omit for all tables"),
			"kinds":  arrayOf("string", "Restrict to logical_name | semantic_type | description; omit for all"),
		}, nil)),
		tool("suggest_model_candidates", "Generate REVIEWABLE code-dictionary, metric, and relation candidates. Rule-based and offline: code dictionaries seeded from profiled top values of low-cardinality code columns; aggregate metrics (SUM/AVG) for AMOUNT/COUNT/RATIO/SCORE columns not already covered; foreign-key relations inferred from identifier naming + PK-name/table-name match + type compatibility. Each candidate carries evidence + confidence and is NEVER applied to the operational catalog — an operator or LLM confirms labels, expressions, and cardinality first.", objectSchema(map[string]any{
			"tables": arrayOf("string", "Schema-qualified tables to analyze; omit for all tables"),
			"kinds":  arrayOf("string", "Restrict to code_dict | metric | relation; omit for all"),
		}, nil)),
		tool("analyze_impact", "Trace the dependency footprint (lineage/impact) of a table or column before changing or retiring it: which metrics, relations, preferred/forbidden joins, golden queries, overrides, glossary terms, and one-hop downstream tables depend on it. Read-only analysis over the loaded catalog; returns an impact_level (high if metrics or preferred joins would break) and the full dependent list so you can plan a safe change.", objectSchema(map[string]any{
			"table":  str("Schema-qualified table to analyze (schema.table)"),
			"column": str("Optional single column to scope the analysis to"),
		}, []string{"table"})),
		tool("review_candidates", "List the metadata candidate review queue: every enrichment (logical name/semantic type/description) and model candidate (code dictionary/metric/relation) joined with its stored approve/reject decision. Each item has a stable id you pass to decide_candidates. Filter by status pending|approved|rejected. This is the human-in-the-loop gate — candidates are only ever applied after approval.", objectSchema(map[string]any{
			"tables": arrayOf("string", "Restrict to these schema-qualified tables; omit for all"),
			"kinds":  arrayOf("string", "Restrict to logical_name|semantic_type|description|code_dict|metric|relation; omit for all"),
			"status": str("Filter to pending | approved | rejected; omit for all"),
		}, nil)),
		tool("decide_candidates", "Approve or reject metadata candidates by their review-queue id (from review_candidates). Decisions are persisted with reviewer + timestamp + notes. Approved items compile into a paste-ready overrides/metrics/relations snippet (get_approved_overrides) — nothing is applied to the operational catalog automatically. Use this to record a human/operator review decision.", objectSchema(map[string]any{
			"decisions": arrayOfObjects("Each: {id, decision: approved|rejected, notes?}"),
			"reviewer":  str("Who is making the decision (name or id)"),
		}, []string{"decisions"})),
		tool("get_metadata_digest", "One compact operational-health snapshot of the catalog: metadata quality score + release-gate status, the candidate review backlog (pending/approved/rejected), golden-promotion candidate count, and catalog size + load warnings, with a one-line headline. Read-only; use for a daily ops glance or to drive an alert.", objectSchema(map[string]any{}, nil)),
		tool("openmetadata_status", "Test connectivity and auth to the configured OpenMetadata server and report its version. Use before import/export to confirm the -openmetadata-url / token are set.", objectSchema(map[string]any{}, nil)),
		tool("import_openmetadata", "Import curated business metadata (table/column display names → logical names, descriptions, PII tags → pii/semantic_type, glossary terms) from OpenMetadata into jamypg. Proposes candidates for GAPS ONLY (never overwrites operator curation), matched to catalog tables by schema.table. apply=false (default) previews; apply=true merges into overrides.json/glossary.json with backups and reloads the catalog (admin); to_review=true stages logical-name/description gaps into the review queue for human approval (review_candidates → decide_candidates → apply_approved_candidates).", objectSchema(map[string]any{
			"scope":            str("OpenMetadata database/schema FQN to scope the import (e.g. 'service.db' or 'service.db.schema'); omit for all"),
			"max_tables":       integer("Max tables to fetch (default 500)"),
			"include_glossary": boolSchema("Also import glossary terms (default true)"),
			"apply":            boolSchema("true → merge into dataset files + reload (admin); false → preview only (default)"),
			"to_review":        boolSchema("true → stage logical-name/description gaps into the review queue instead of applying"),
		}, nil)),
		tool("openmetadata_drift", "Reconciliation report: compare sqlon's catalog against OpenMetadata and classify every logical-name/description/PII divergence as jamypg_gap (sqlon empty, OM has → import candidate), conflict (both differ → human decision), or ext_gap (sqlon has, OM empty → export candidate). Read-only, writes nothing. Use for governance / keeping the two catalogs aligned.", objectSchema(map[string]any{
			"scope":      str("OpenMetadata database/schema FQN to scope; omit for all"),
			"max_tables": integer("Max tables to compare (default 500)"),
		}, nil)),
		tool("export_lineage_to_openmetadata", "Push sqlon's relation graph to OpenMetadata as table-level lineage edges (fromEntity = referenced/parent table, toEntity = base/child table). Maps sqlon FK-style relationships to OpenMetadata relationship lineage (NOT ETL data-flow). dry_run=true (default) returns the plan; dry_run=false performs PUT /api/v1/lineage writes (admin). Edges whose tables are absent in OpenMetadata are skipped and reported.", objectSchema(map[string]any{
			"scope":      str("OpenMetadata database/schema FQN to scope table id resolution; omit for all"),
			"max_tables": integer("Max tables to resolve ids from (default 500)"),
			"dry_run":    boolSchema("true → plan only (default); false → write lineage to OpenMetadata (admin)"),
		}, nil)),
		tool("export_to_openmetadata", "Push sqlon-owned column descriptions (explicit, or composed from logical names) BACK to OpenMetadata for columns that lack a description there. Never overwrites existing OpenMetadata descriptions. dry_run=true (default) returns the plan; dry_run=false performs JSON-Patch writes (admin).", objectSchema(map[string]any{
			"scope":      str("OpenMetadata database/schema FQN to scope; omit for all"),
			"max_tables": integer("Max tables to scan (default 500)"),
			"dry_run":    boolSchema("true → plan only (default); false → write to OpenMetadata (admin)"),
		}, nil)),
		tool("get_approved_overrides", "Return all approved metadata candidates compiled into paste-ready fragments grouped by destination file: overrides.json columns[], metrics.json entries, relations.json entries, and code-dictionary bindings. Apply these and reload/restart to make the approved candidates live.", objectSchema(map[string]any{}, nil)),
		tool("apply_approved_candidates", "ONE-CLICK APPLY: merge every approved-but-not-yet-applied candidate into the dataset files (overrides.json columns[], metrics.json, topology_relations.json, meta_code_dict.json) with automatic per-file backups, then hot-reload the catalog. Idempotent — applied records are stamped applied_at and skipped on re-run; merges also dedupe against file content, and existing operator-curated values are never overwritten. Approval remains the explicit human gate; this is the second explicit act that makes approved metadata live.", objectSchema(map[string]any{}, nil)),
		tool("find_filter_columns", "Map literal values from the question (e.g. 서울, 정상, 개인사업자) onto filter columns via code dictionaries, top values, and sample values, with suggested predicates.", objectSchema(map[string]any{
			"values": arrayOf("string", "Literal values or labels mentioned in the question"),
			"tables": arrayOf("string", "Optional tables to restrict the search"),
			"top_k":  integer("Maximum candidates"),
		}, []string{"values"})),
		tool("resolve_time", "Parse Korean/English temporal expressions (오늘, 지난달, 최근 3개월, 2025년 6월, 상반기, 전월 대비...) into date ranges, and render column-type-aware SQL conditions for a table's date columns.", objectSchema(map[string]any{
			"question": str("Question or phrase containing temporal expressions"),
			"table":    str("Optional table whose date columns should get rendered conditions"),
		}, []string{"question"})),
		tool("run_evaluation", "Run the golden-query evaluation set (table/column/metric/join/SQL-validity accuracy). Pass a db profile to additionally execute expected_sql for execution success rate and row-count sanity. Pass retrieval=true for retrieval-stage-only metrics: Table/Column Recall@k, Join Path Recall, Value Evidence Recall, and the graph-vs-plain recall gain.", objectSchema(map[string]any{
			"golden_path": str("Optional path to golden_queries.json; defaults to <data>/golden_queries.json"),
			"top_k":       integer("Search depth used for table-selection accuracy"),
			"profile":     str("Optional db profile id for execution-based checks (postgres/mysql/mariadb)"),
			"retrieval":   boolSchema("true → retrieval-stage-only recall metrics (no SQL/execution checks)"),
		}, nil)),
		tool("learn_from_feedback", "ADMIN: promote repeated patterns from trusted, operator-approved, in-scope feedback plus the DB execution audit. Pending/untrusted/foreign-scope and duplicate feedback is skipped. Persists learned_rules.json and hot-applies search penalties and LEARNED_* validation warnings.", objectSchema(map[string]any{
			"min_occurrences": integer("Minimum repetitions before a pattern becomes a rule (default 3)"),
		}, nil)),
		tool("suggest_golden_from_feedback", "List golden-query CANDIDATES derived from approved + successful + executed feedback that is not already in the golden set (deduped by normalized question and SQL). Each candidate carries the question, expected SQL/tables/columns and its source feedback_id. Only trust-boundary-approved feedback is eligible (fail-closed). Use to grow the evaluation set from real production traffic.", objectSchema(map[string]any{
			"limit": integer("Max candidates (default 50, cap 200)"),
		}, nil)),
		tool("promote_golden_queries", "ADMIN: append chosen feedback-derived candidates (by feedback_id, from suggest_golden_from_feedback) to golden_queries.json with a backup, then hot-reload the catalog. Duplicates are skipped. This is the explicit operator act that turns approved feedback into evaluation truth.", objectSchema(map[string]any{
			"feedback_ids": arrayOf("string", "feedback_id values to promote (from suggest_golden_from_feedback)"),
		}, []string{"feedback_ids"})),
		tool("prepare_sql_context", "★ ONE-CALL PIPELINE: runs analyze_question → search_schema → get_metric_definition → get_schema_context → get_join_paths → build_sql_skeleton in a single call and returns one bundle (analysis, selected_tables, metrics, schema_context, join_paths, skeleton, expected_output_columns, next_step). Start HERE for most questions, then just fill the skeleton's /* SLOT */ markers, call validate_sql, and optionally run_sql_safely. Saves orchestrating 6+ calls and prevents skipping a step.", objectSchema(map[string]any{
			"question":          str("Natural language question (Korean/English)"),
			"tables":            arrayOf("string", "Optional explicit tables; defaults to top search hits"),
			"limit":             integer("Row bound for the skeleton's LIMIT"),
			"clarifications":    map[string]any{"type": "object", "description": "Answers from a previous needs_clarification response, keyed by clarification id; value may be free text or an option key.", "additionalProperties": map[string]any{"type": "string"}},
			"previous_question": str("For follow-up turns ('그중 서울만', '이걸 월별로'): the prior question this one refines. Merges context so short refinements work."),
			"previous_sql":      str("Optional: the SQL produced for previous_question — returned back as previous_sql to refine instead of regenerating from scratch."),
			"profile":           str("Optional DB profile id: when that profile has a catalog workspace, prepare against IT instead of the active catalog (per-request multi-DB; no global switch). Falls back to the active catalog when no workspace exists."),
		}, []string{"question"})),
		tool("build_sql_skeleton", "Assemble a draft DB SQL frame from vetted parts only: catalog join conditions with aliases, dictionary metric expressions, semantic-type-aware time predicates, and policy filters. Fill the /* SLOT */ comments; do not restructure. → prepare_sql_context already calls this; use it directly only for incremental refinement.", objectSchema(map[string]any{
			"question": str("Natural language question (drives time/metric/pattern detection)"),
			"tables":   arrayOf("string", "Selected tables in join order; defaults to top search hits"),
			"limit":    integer("Row bound for LIMIT"),
		}, []string{"question"})),
		tool("rank_candidates", "Rank multiple candidate SQLs with objective server-side signals (validation errors, policy warnings, risk estimate, result-schema coverage, metric conformity) and return them best-first. Use for complex questions: generate 2-3 candidates, then pick best_sql.", objectSchema(map[string]any{
			"question":         str("Original question, for the audit trail"),
			"candidates":       arrayOf("string", "Candidate SQL statements (2-5 recommended)"),
			"expected_outputs": arrayOf("string", "Business terms the SELECT list must cover"),
			"metrics":          arrayOf("string", "Dictionary metric names the SQL should implement"),
			"limit":            integer("Row bound used during validation"),
		}, []string{"candidates"})),
		tool("suggest_joins", "Discover relation-graph edges missing from the catalog (single-column-PK masters referenced by other tables), ranked by FK/index/type/co-occurrence evidence. Returns overrides.json snippets for OPERATOR REVIEW ONLY — suggestions are never auto-applied.", objectSchema(map[string]any{
			"tables": arrayOf("string", "Optional scope: only suggest edges touching these tables"),
			"top_k":  integer("Maximum suggestions (default 20)"),
		}, nil)),
		tool("list_datasets", "Describe every JSON dataset this server references: purpose, schema, consuming tools, required/editable flags, live status (present, size, loaded entries, load issues).", objectSchema(map[string]any{}, nil)),
		tool("get_dataset", "Inspect one dataset: registry description plus the head of its current content.", objectSchema(map[string]any{
			"name":        str("Dataset name from list_datasets (e.g. glossary, metrics, overrides)"),
			"sample_rows": integer("Sample entries to return (default 5, max 50)"),
		}, []string{"name"})),
		tool("put_dataset", "Replace a dataset's content: validates JSON shape, backs up the current file, writes, recompiles the catalog, and hot-swaps it. Rolls back automatically if compilation fails or (without force) introduces new errors.", objectSchema(map[string]any{
			"name":    str("Dataset name from list_datasets"),
			"content": map[string]any{"description": "Full new content (JSON array or object per the dataset schema)"},
			"force":   boolSchema("Apply even if the new content introduces load errors"),
		}, []string{"name", "content"})),
		tool("remove_dataset", "Remove an optional dataset file (backed up first) and hot-reload the catalog. Required or system-managed datasets are refused.", objectSchema(map[string]any{
			"name": str("Dataset name from list_datasets"),
		}, []string{"name"})),
		tool("reload_catalog", "Recompile the catalog from the files on disk and hot-swap it. Use after editing dataset files directly (e.g. via a mounted volume).", objectSchema(map[string]any{}, nil)),
	}
	return annotateTools(publicTools(list))
}

func publicTools(list []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, definition := range list {
		name, _ := definition["name"].(string)
		if internalDBAExecutors[name] {
			continue
		}
		out = append(out, definition)
	}
	return out
}

// annotateTools attaches MCP tool annotations (readOnlyHint / destructiveHint /
// idempotentHint / openWorldHint) so clients can render hazard cues and pick
// tools safely. Default is a pure read-only, closed-world catalog lookup;
// exceptions are the DB-touching tools (openWorldHint) and the operator tools
// that mutate catalog/feedback state (not read-only).
func annotateTools(list []map[string]any) []map[string]any {
	// operator tools that mutate server-side state (not read-only)
	writers := map[string]struct {
		destructive bool
		idempotent  bool
	}{
		"put_dataset":                    {destructive: true, idempotent: false}, // overwrites a dataset
		"remove_dataset":                 {destructive: true, idempotent: false}, // deletes a dataset
		"reload_catalog":                 {destructive: false, idempotent: true}, // recompiles from disk
		"learn_from_feedback":            {destructive: false, idempotent: true},
		"record_feedback":                {destructive: false, idempotent: false}, // appends a queue record
		"review_feedback":                {destructive: false, idempotent: false}, // changes review state/audit time
		"decide_candidates":              {destructive: false, idempotent: false}, // persists review decisions
		"apply_approved_candidates":      {destructive: true, idempotent: true},   // merges into dataset files (backed up)
		"promote_golden_queries":         {destructive: true, idempotent: true},   // appends to golden_queries.json (backed up)
		"import_openmetadata":            {destructive: true, idempotent: true},   // merges external metadata (apply=true)
		"export_to_openmetadata":         {destructive: true, idempotent: true},   // writes to OpenMetadata (dry_run=false)
		"export_lineage_to_openmetadata": {destructive: true, idempotent: true},   // writes lineage to OpenMetadata
		"apply_metadata_sync":            {destructive: true, idempotent: true},   // merges physical model into catalog
		"build_profile_catalog":          {destructive: true, idempotent: true},   // builds a per-profile workspace
		"put_profile_dataset":            {destructive: true, idempotent: false},  // writes a per-profile dataset
		"set_active_catalog":             {destructive: true, idempotent: true},   // hot-swaps the active catalog
		"build_all_profile_catalogs":     {destructive: true, idempotent: true},   // batch-builds workspaces
		"import_openmetadata_to_profile": {destructive: true, idempotent: true},   // imports OM metadata into a workspace
		"create_change_plan":             {destructive: false, idempotent: true},
		"submit_change":                  {destructive: false, idempotent: false},
		"approve_change":                 {destructive: false, idempotent: false},
		"execute_approved_change":        {destructive: true, idempotent: false},
		"rollback_change":                {destructive: true, idempotent: false},
		"cancel_change":                  {destructive: false, idempotent: true},
	}
	for _, t := range list {
		name, _ := t["name"].(string)
		ann := map[string]any{"title": name}
		if w, ok := writers[name]; ok {
			ann["readOnlyHint"] = false
			ann["destructiveHint"] = w.destructive
			ann["idempotentHint"] = w.idempotent
			ann["openWorldHint"] = false
		} else {
			ann["readOnlyHint"] = true
			ann["destructiveHint"] = false
			ann["idempotentHint"] = true
			ann["openWorldHint"] = dbProfileTools[name]
		}
		t["annotations"] = ann
	}
	return list
}

// adminOnlyTools are MCP tools that mutate shared server state (catalog,
// datasets, learned rules) and therefore require the admin role when auth is
// enabled — the same rule the REST endpoints enforce via requireAdmin.
var adminOnlyTools = map[string]bool{
	"put_dataset":                    true,
	"remove_dataset":                 true,
	"reload_catalog":                 true,
	"learn_from_feedback":            true,
	"review_feedback":                true,
	"decide_candidates":              true,
	"apply_approved_candidates":      true,
	"promote_golden_queries":         true,
	"import_openmetadata":            true,
	"export_to_openmetadata":         true,
	"export_lineage_to_openmetadata": true,
	"apply_metadata_sync":            true,
	"build_profile_catalog":          true,
	"put_profile_dataset":            true,
	"set_active_catalog":             true,
	"build_all_profile_catalogs":     true,
	"import_openmetadata_to_profile": true,
}

// dbaTools are privileged DBA operations. They require the dba/admin capability
// (toolActorIsDBA) and a DBA-enabled profile; all mutate live DB state through
// the write-capable admin pool and are audited. Read-only inspection tools
// (dba_overview / dba_list_*) are included so the whole suite is role-gated as
// one coherent surface.
var dbaTools = map[string]bool{
	"create_change_plan":      true,
	"evaluate_change_risk":    true,
	"submit_change":           true,
	"approve_change":          true,
	"execute_approved_change": true,
	"verify_change":           true,
	"rollback_change":         true,
	"cancel_change":           true,
	"dba_overview":            true,
	"dba_list_users":          true,
	"dba_list_databases":      true,
	"dba_list_settings":       true,
	"dba_list_sessions":       true,
	"dba_create_user":         true,
	"dba_alter_user":          true,
	"dba_drop_user":           true,
	"dba_grant":               true,
	"dba_create_database":     true,
	"dba_drop_database":       true,
	"dba_set_parameter":       true,
	"dba_terminate_session":   true,
	"dba_run_maintenance":     true,
	"dba_execute":             true,
}

// internalDBAExecutors are retained as implementation helpers during the
// migration window, but are neither advertised nor callable over MCP.
var internalDBAExecutors = map[string]bool{
	"dba_create_user": true, "dba_alter_user": true, "dba_drop_user": true,
	"dba_grant": true, "dba_create_database": true, "dba_drop_database": true,
	"dba_set_parameter": true, "dba_terminate_session": true,
	"dba_run_maintenance": true, "dba_execute": true,
}

// dbProfileTools is the single registry for MCP tools that can reach an
// external DB when their profile argument is set. Besides driving the MCP
// open-world annotation, it ensures standalone HTTP applies the same master
// token gate to every such tool. Calls without a profile are catalog-only.
var dbProfileTools = map[string]bool{
	"get_fleet_health":        true,
	"list_sessions":           true,
	"get_lock_tree":           true,
	"get_workload_summary":    true,
	"get_top_sql":             true,
	"get_storage_status":      true,
	"run_sql_safely":          true,
	"execute_with_repair":     true,
	"explain_sql":             true,
	"run_evaluation":          true,
	"route_db_profile":        true,
	"discover_metadata":       true,
	"run_metadata_sync":       true,
	"profile_metadata_assets": true,
	"describe_db_schema":      true,
	"build_profile_catalog":   true,
	"db_health_report":        true,
	"suggest_indexes":         true,
	"workload_report":         true,
	"get_dba_digest":          true,
}

// authorizeDBProfileTool closes token-gate gaps between DB-touching tools.
// toolActorIsAdmin deliberately preserves the established transport policy:
// a configured standalone HTTP request carries ctxKeyHTTPAdmin and must pass
// the token, while stdio and standalone HTTP with no configured token remain
// locally trusted. Meta mode proceeds to its existing per-profile ACL checks.
func (s *Server) authorizeDBProfileTool(ctx context.Context, name string, arguments json.RawMessage) (map[string]any, error) {
	if !dbProfileTools[name] {
		return nil, nil
	}
	var a struct {
		Profile   string `json:"profile"`
		Source    string `json:"source"`
		Retrieval bool   `json:"retrieval"`
	}
	if err := decodeArgs(arguments, &a); err != nil {
		return nil, err
	}
	// route_db_profile and run_sql_safely(profile="auto") probe every usable
	// profile's live inventory, so they always touch the DB regardless of an
	// explicit profile value. The metadata-sync tools address the DB by
	// `source` and always touch it.
	probesAll := name == "route_db_profile" ||
		name == "get_fleet_health" ||
		name == "discover_metadata" || name == "run_metadata_sync" || name == "profile_metadata_assets" ||
		(name == "run_sql_safely" && strings.EqualFold(strings.TrimSpace(a.Profile), "auto"))
	// Retrieval-only evaluation never opens a DB, even if a client happens to
	// include a profile value.
	if !probesAll && (a.Profile == "" || (name == "run_evaluation" && a.Retrieval)) {
		return nil, nil
	}
	if s.authEnabled() || s.toolActorIsAdmin(ctx) {
		return nil, nil
	}
	return map[string]any{
		"status": "forbidden",
		"error":  "tool '" + name + "' access to a db profile requires the admin token",
		"notice": "pass the master admin token (X-Admin-Token or Authorization: Bearer <token>).",
	}, nil
}

func (s *Server) callTool(ctx context.Context, params json.RawMessage) (any, error) {
	var req struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, err
	}
	if internalDBAExecutors[req.Name] {
		return map[string]any{
			"status": "deprecated",
			"error":  "direct DBA mutation tools are disabled; create and execute an approved ChangePlan",
		}, nil
	}
	// Admin-only tools (dataset mutations, learning) must enforce admin over
	// MCP too, matching the REST layer. In meta mode this is the admin role;
	// in standalone mode with -admin-token set it is the master-token flag
	// (stdio / no-token = locally trusted).
	if adminOnlyTools[req.Name] && !s.toolActorIsAdmin(ctx) {
		return map[string]any{
			"status": "forbidden",
			"error":  "tool '" + req.Name + "' requires admin privileges",
		}, nil
	}
	// DBA tools run privileged, write-capable statements against the target DB.
	// Require the dba (or admin) capability AND a profile that opted in with
	// DBA credentials (checked at the exec layer). This is a stricter gate than
	// adminOnlyTools (which only guards shared catalog state).
	if dbaTools[req.Name] && !s.toolActorIsDBA(ctx) {
		return map[string]any{
			"status": "forbidden",
			"error":  "tool '" + req.Name + "' requires the dba or admin role",
			"notice": "DBA 도구는 dba/admin 역할과 프로파일의 dba 자격증명(db_profiles.dba)이 모두 필요합니다.",
		}, nil
	}
	if denied, err := s.authorizeDBProfileTool(ctx, req.Name, req.Arguments); err != nil {
		return nil, err
	} else if denied != nil {
		return denied, nil
	}
	switch req.Name {
	case "analyze_question":
		var a catalog.AnalyzeRequest
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.cat().AnalyzeQuestion(a), nil
	case "retrieve_context":
		var a struct {
			Question string `json:"question"`
			TopK     int    `json:"top_k"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.cat().RetrieveContext(a.Question, a.TopK), nil
	case "suggest_join_relations":
		var a struct {
			GoldenPath string `json:"golden_path"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.cat().SuggestJoinRelations(a.GoldenPath)
	case "search_schema":
		var a catalog.SearchRequest
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.cat().SearchSchema(a), nil
	case "get_schema_context":
		var a struct {
			Question           string   `json:"question"`
			Tables             []string `json:"tables"`
			MaxColumnsPerTable int      `json:"max_columns_per_table"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.cat().SchemaContext(a.Question, a.Tables, a.MaxColumnsPerTable), nil
	case "get_join_paths":
		var a catalog.JoinPathRequest
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.cat().GetJoinPaths(a)
	case "get_metric_definition":
		var a struct {
			MetricName string `json:"metric_name"`
			TopK       int    `json:"top_k"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.cat().MetricDefinition(a.MetricName, a.TopK), nil
	case "get_column_stats":
		var a struct {
			Table  string `json:"table"`
			Column string `json:"column"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.cat().ColumnStats(a.Table, a.Column), nil
	case "search_examples":
		var a struct {
			Question string `json:"question"`
			TopK     int    `json:"top_k"`
			Table    string `json:"table"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.cat().SearchSamples(a.Question, a.TopK, a.Table), nil
	case "validate_sql":
		// Accept `expected_output_columns` as an alias for `expected_outputs`,
		// since prepare_sql_context returns the former and models often echo it.
		var a struct {
			catalog.ValidateRequest
			ExpectedOutputColumns []string `json:"expected_output_columns,omitempty"`
			MetricNames           []string `json:"metric_names,omitempty"`
			Profile               string   `json:"profile,omitempty"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		if len(a.ExpectedOutputs) == 0 && len(a.ExpectedOutputColumns) > 0 {
			a.ExpectedOutputs = a.ExpectedOutputColumns
		}
		if len(a.Metrics) == 0 && len(a.MetricNames) > 0 {
			a.Metrics = a.MetricNames
		}
		vcat, source := s.catalogFor(a.Profile)
		res := vcat.ValidateSQL(a.ValidateRequest)
		if source != "active" {
			// annotate which catalog validated (keeps the response shape)
			res.Hints = append(res.Hints, "validated against "+source)
		}
		return res, nil
	case "explain_sql":
		var a struct {
			SQL     string `json:"sql"`
			Limit   int    `json:"limit"`
			Profile string `json:"profile"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		res := s.cat().ExplainSQL(catalog.ValidateRequest{SQL: a.SQL, Limit: a.Limit})
		if a.Profile != "" {
			if err := s.canUseProfileID(ctx, userFrom(ctx), a.Profile); err != nil {
				res["live_plan_error"] = err.Error()
				return res, nil
			}
			plan, err := s.DB.ExplainPlan(ctx, a.Profile, a.SQL)
			if err != nil {
				res["live_plan_error"] = err.Error()
			} else {
				res["live_plan"] = plan
				// 실측 위험이 정적 추정보다 높으면 권장 조치를 승격
				if plan.Risk == "high" {
					res["risk"] = "high"
					res["recommended_action"] = "regenerate_with_constraints"
				}
				res["execution_notice"] = "live_plan은 실제 DB EXPLAIN(JSON) 결과입니다. risk=high면 실행하지 말고 suggestions를 반영해 재생성하세요."
			}
		}
		return res, nil
	case "list_db_profiles":
		return s.mcpListProfiles(ctx), nil
	case "list_database_instances":
		profiles, err := s.usableProfiles(ctx)
		if err != nil {
			return map[string]any{"status": "error", "warnings": []string{err.Error()}, "data": []any{}}, nil
		}
		return fleet.New(s.DB).InventoryProfiles(profiles), nil
	case "get_fleet_health":
		profiles, err := s.usableProfiles(ctx)
		if err != nil {
			return map[string]any{"status": "error", "warnings": []string{err.Error()}, "data": []any{}}, nil
		}
		return fleet.NewWithOperations(s.DB, s.Collector).HealthProfiles(ctx, profiles), nil
	case "list_sessions":
		var a struct {
			Profile string `json:"profile"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		profiles, err := s.usableProfiles(ctx)
		if err != nil {
			return nil, err
		}
		profile, ok := allowedProfile(profiles, a.Profile)
		if !ok {
			return map[string]any{"status": "not_found", "warnings": []string{"db profile not found or not permitted"}}, nil
		}
		return observability.New(s.DB).Sessions(ctx, profile), nil
	case "get_lock_tree":
		var a struct {
			Profile string `json:"profile"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		profiles, err := s.usableProfiles(ctx)
		if err != nil {
			return nil, err
		}
		profile, ok := allowedProfile(profiles, a.Profile)
		if !ok {
			return map[string]any{"status": "not_found", "warnings": []string{"db profile not found or not permitted"}}, nil
		}
		return observability.New(s.DB).Locks(ctx, profile), nil
	case "get_workload_summary", "get_top_sql", "get_storage_status":
		var a struct {
			Profile string `json:"profile"`
			Fresh   bool   `json:"fresh"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		kind := map[string]string{"get_workload_summary": "workload", "get_top_sql": "top_sql", "get_storage_status": "capacity"}[req.Name]
		return s.mcpOperational(ctx, a.Profile, kind, a.Fresh), nil
	case "route_db_profile":
		var a struct {
			SQL string `json:"sql"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		dec, err := s.routeProfile(ctx, a.SQL)
		if err != nil {
			return map[string]any{"error": err.Error()}, nil
		}
		return routeResult(dec), nil
	case "list_metadata_sources":
		return s.mcpMetadataSources(ctx), nil
	case "discover_metadata":
		var a struct {
			Source string `json:"source"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.mcpDiscoverMetadata(ctx, a.Source), nil
	case "run_metadata_sync":
		var a struct {
			Source       string   `json:"source"`
			Schemas      []string `json:"schemas"`
			Incremental  *bool    `json:"incremental"`
			IncludeViews bool     `json:"include_views"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		incremental := a.Incremental == nil || *a.Incremental
		return s.mcpRunMetadataSync(ctx, a.Source, a.Schemas, incremental, a.IncludeViews), nil
	case "describe_db_schema":
		var a struct {
			Profile      string   `json:"profile"`
			Schemas      []string `json:"schemas"`
			Table        string   `json:"table"`
			IncludeViews bool     `json:"include_views"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		if a.Profile == "" {
			return map[string]any{"status": "error", "error": "profile is required"}, nil
		}
		if err := s.canUseProfileID(ctx, userFrom(ctx), a.Profile); err != nil {
			return map[string]any{"status": "forbidden", "error": err.Error()}, nil
		}
		return s.mcpDescribeDBSchema(ctx, a.Profile, a.Schemas, a.Table, a.IncludeViews), nil
	case "db_health_report":
		var a struct {
			Profile string `json:"profile"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		if a.Profile == "" {
			return map[string]any{"status": "error", "error": "profile is required"}, nil
		}
		if err := s.canUseProfileID(ctx, userFrom(ctx), a.Profile); err != nil {
			return map[string]any{"status": "forbidden", "error": err.Error()}, nil
		}
		return s.mcpDBHealthReport(ctx, a.Profile), nil
	case "suggest_indexes":
		var a struct {
			Profile      string `json:"profile"`
			MinElapsedMs int    `json:"min_elapsed_ms"`
			Days         int    `json:"days"`
			Verify       bool   `json:"verify"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		if a.Profile != "" {
			if err := s.canUseProfileID(ctx, userFrom(ctx), a.Profile); err != nil {
				return map[string]any{"status": "forbidden", "error": err.Error()}, nil
			}
		}
		return s.suggestIndexesVerified(ctx, a.Profile, a.MinElapsedMs, a.Days, a.Verify), nil
	case "lint_sql":
		var a struct {
			SQL     string `json:"sql"`
			Profile string `json:"profile"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		if strings.TrimSpace(a.SQL) == "" {
			return map[string]any{"status": "error", "error": "sql is required"}, nil
		}
		vcat, src := s.catalogFor(a.Profile)
		findings := vcat.LintSQL(a.SQL)
		return map[string]any{
			"findings":       findings,
			"count":          len(findings),
			"catalog_source": src,
			"note":           "정적 안티패턴 점검(권고용). 실행하지 않으며 SQL을 자동 수정하지 않습니다.",
		}, nil
	case "explain_sql_in_words":
		var a struct {
			SQL     string `json:"sql"`
			Profile string `json:"profile"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		if strings.TrimSpace(a.SQL) == "" {
			return map[string]any{"status": "error", "error": "sql is required"}, nil
		}
		vcat, _ := s.catalogFor(a.Profile)
		return vcat.ExplainSQLWords(a.SQL), nil
	case "workload_report":
		var a struct {
			Profile string `json:"profile"`
			Days    int    `json:"days"`
			SlowMs  int    `json:"slow_ms"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		if a.Profile != "" {
			if err := s.canUseProfileID(ctx, userFrom(ctx), a.Profile); err != nil {
				return map[string]any{"status": "forbidden", "error": err.Error()}, nil
			}
		}
		return s.mcpWorkloadReport(a.Profile, a.Days, a.SlowMs), nil
	case "get_dba_digest":
		var a struct {
			Profile string `json:"profile"`
			Days    int    `json:"days"`
			SlowMs  int    `json:"slow_ms"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		if a.Profile != "" {
			if err := s.canUseProfileID(ctx, userFrom(ctx), a.Profile); err != nil {
				return map[string]any{"status": "forbidden", "error": err.Error()}, nil
			}
		}
		return s.mcpDBADigest(a.Profile, a.Days, a.SlowMs), nil
	case "create_change_plan":
		var p change.Plan
		if err := decodeArgs(req.Arguments, &p); err != nil {
			return nil, err
		}
		created, err := s.Changes.Create(p, p.ID)
		if err != nil {
			return map[string]any{"status": "error", "error": err.Error()}, nil
		}
		return map[string]any{"status": "ok", "data": created, "collected_at": time.Now().UTC()}, nil
	case "evaluate_change_risk":
		var a struct {
			Risk change.Risk `json:"risk"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		required, err := change.ApprovalRequirement(a.Risk)
		if err != nil {
			return map[string]any{"status": "error", "error": err.Error()}, nil
		}
		return map[string]any{"status": "ok", "risk": a.Risk, "required_approvals": required}, nil
	case "submit_change":
		var a struct {
			ID string `json:"id"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		p, err := s.Changes.Submit(a.ID)
		if err != nil {
			return map[string]any{"status": "error", "error": err.Error()}, nil
		}
		return map[string]any{"status": "ok", "data": p}, nil
	case "approve_change":
		var a struct {
			ID string `json:"id"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		actor := "dba"
		if u := userFrom(ctx); u != nil {
			actor = u.Username
		}
		p, err := s.Changes.Approve(a.ID, actor)
		if err != nil {
			return map[string]any{"status": "error", "error": err.Error()}, nil
		}
		return map[string]any{"status": "ok", "data": p, "approval": p.Approvals[len(p.Approvals)-1]}, nil
	case "execute_approved_change":
		var a struct {
			ID         string `json:"id"`
			ApprovalID string `json:"approval_id"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		p, ok := s.Changes.Get(a.ID)
		if !ok {
			return map[string]any{"status": "error", "error": "change plan not found"}, nil
		}
		if p.RequiredApprovals > 0 {
			valid := false
			for _, approval := range p.Approvals {
				if approval.ID == a.ApprovalID {
					valid = true
					break
				}
			}
			if !valid {
				return map[string]any{"status": "forbidden", "error": "a valid approval_id is required"}, nil
			}
		}
		p, err := s.Changes.Execute(ctx, a.ID, approvedChangeRunner{server: s})
		if err != nil {
			return map[string]any{"status": "error", "error": err.Error(), "data": p}, nil
		}
		return map[string]any{"status": "ok", "data": p}, nil
	case "verify_change":
		var a struct {
			ID string `json:"id"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		p, ok := s.Changes.Get(a.ID)
		if !ok {
			return map[string]any{"status": "error", "error": "change plan not found"}, nil
		}
		return map[string]any{"status": "ok", "data": p}, nil
	case "rollback_change":
		var a struct {
			ID string `json:"id"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		p, err := s.Changes.Rollback(ctx, a.ID, approvedChangeRunner{server: s})
		if err != nil {
			return map[string]any{"status": "error", "error": err.Error(), "data": p}, nil
		}
		return map[string]any{"status": "ok", "data": p}, nil
	case "cancel_change":
		var a struct {
			ID string `json:"id"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		p, err := s.Changes.Cancel(a.ID)
		if err != nil {
			return map[string]any{"status": "error", "error": err.Error()}, nil
		}
		return map[string]any{"status": "ok", "data": p}, nil
	case "dba_overview":
		var a struct {
			Profile string `json:"profile"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.dbaOverview(ctx, a.Profile), nil
	case "dba_list_users":
		var a struct {
			Profile string `json:"profile"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.dbaListUsers(ctx, a.Profile), nil
	case "dba_list_databases":
		var a struct {
			Profile string `json:"profile"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.dbaListDatabases(ctx, a.Profile), nil
	case "dba_list_settings":
		var a struct {
			Profile string `json:"profile"`
			Filter  string `json:"filter"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.dbaListSettings(ctx, a.Profile, a.Filter), nil
	case "dba_list_sessions":
		var a struct {
			Profile string `json:"profile"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.dbaListSessions(ctx, a.Profile), nil
	case "dba_create_user":
		var a struct {
			Profile    string `json:"profile"`
			Username   string `json:"username"`
			Password   string `json:"password"`
			CanLogin   *bool  `json:"can_login"`
			Superuser  *bool  `json:"superuser"`
			CreateDB   *bool  `json:"createdb"`
			CreateRole *bool  `json:"createrole"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.dbaCreateUser(ctx, a.Profile, dbaUserOpts{
			Username: a.Username, Password: a.Password, CanLogin: a.CanLogin,
			Superuser: a.Superuser, CreateDB: a.CreateDB, CreateRole: a.CreateRole,
		}), nil
	case "dba_alter_user":
		var a struct {
			Profile    string `json:"profile"`
			Username   string `json:"username"`
			Password   string `json:"password"`
			CanLogin   *bool  `json:"can_login"`
			Superuser  *bool  `json:"superuser"`
			CreateDB   *bool  `json:"createdb"`
			CreateRole *bool  `json:"createrole"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.dbaAlterUser(ctx, a.Profile, dbaUserOpts{
			Username: a.Username, Password: a.Password, CanLogin: a.CanLogin,
			Superuser: a.Superuser, CreateDB: a.CreateDB, CreateRole: a.CreateRole,
		}), nil
	case "dba_drop_user":
		var a struct {
			Profile  string `json:"profile"`
			Username string `json:"username"`
			Confirm  bool   `json:"confirm"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.dbaDropUser(ctx, a.Profile, a.Username, a.Confirm), nil
	case "dba_grant":
		var a struct {
			Profile    string `json:"profile"`
			Privileges string `json:"privileges"`
			Object     string `json:"object"`
			Grantee    string `json:"grantee"`
			Revoke     bool   `json:"revoke"`
			WithGrant  bool   `json:"with_grant"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.dbaGrant(ctx, a.Profile, a.Privileges, a.Object, a.Grantee, a.Revoke, a.WithGrant), nil
	case "dba_create_database":
		var a struct {
			Profile  string `json:"profile"`
			Name     string `json:"name"`
			Owner    string `json:"owner"`
			Encoding string `json:"encoding"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.dbaCreateDatabase(ctx, a.Profile, a.Name, a.Owner, a.Encoding), nil
	case "dba_drop_database":
		var a struct {
			Profile string `json:"profile"`
			Name    string `json:"name"`
			Confirm bool   `json:"confirm"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.dbaDropDatabase(ctx, a.Profile, a.Name, a.Confirm), nil
	case "dba_set_parameter":
		var a struct {
			Profile   string `json:"profile"`
			Parameter string `json:"parameter"`
			Value     string `json:"value"`
			Scope     string `json:"scope"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.dbaSetParameter(ctx, a.Profile, a.Parameter, a.Value, a.Scope), nil
	case "dba_terminate_session":
		var a struct {
			Profile    string `json:"profile"`
			PID        int64  `json:"pid"`
			CancelOnly bool   `json:"cancel_only"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.dbaTerminateSession(ctx, a.Profile, a.PID, a.CancelOnly), nil
	case "dba_run_maintenance":
		var a struct {
			Profile   string `json:"profile"`
			Operation string `json:"operation"`
			Target    string `json:"target"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.dbaRunMaintenance(ctx, a.Profile, a.Operation, a.Target), nil
	case "dba_execute":
		var a struct {
			Profile string `json:"profile"`
			SQL     string `json:"sql"`
			Confirm bool   `json:"confirm"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.dbaExecute(ctx, a.Profile, a.SQL, a.Confirm), nil
	case "list_profile_catalogs":
		return s.listProfileCatalogs(ctx), nil
	case "get_profile_catalog":
		var a struct {
			Profile string `json:"profile"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.getProfileCatalog(a.Profile), nil
	case "build_profile_catalog":
		var a struct {
			Profile string   `json:"profile"`
			Schemas []string `json:"schemas"`
			Prune   bool     `json:"prune"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		if err := s.canUseProfileID(ctx, userFrom(ctx), a.Profile); err != nil {
			return map[string]any{"status": "forbidden", "error": err.Error()}, nil
		}
		return s.buildProfileCatalog(ctx, a.Profile, a.Schemas, a.Prune), nil
	case "get_profile_dataset":
		var a struct {
			Profile string `json:"profile"`
			Dataset string `json:"dataset"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.getProfileDataset(a.Profile, a.Dataset), nil
	case "put_profile_dataset":
		var a struct {
			Profile string          `json:"profile"`
			Dataset string          `json:"dataset"`
			Content json.RawMessage `json:"content"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.putProfileDataset(a.Profile, a.Dataset, a.Content), nil
	case "build_all_profile_catalogs":
		var a struct {
			Profiles []string `json:"profiles"`
			Prune    bool     `json:"prune"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.buildAllProfileCatalogs(ctx, a.Profiles, a.Prune), nil
	case "import_openmetadata_to_profile":
		var a struct {
			Profile string `json:"profile"`
			Scope   string `json:"scope"`
			Apply   bool   `json:"apply"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.omImportToProfile(ctx, a.Profile, a.Scope, a.Apply), nil
	case "get_active_catalog":
		return s.activeCatalogInfo(), nil
	case "set_active_catalog":
		var a struct {
			Profile string `json:"profile"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.setActiveCatalog(a.Profile), nil
	case "apply_metadata_sync":
		var a struct {
			Source string `json:"source"`
			Prune  bool   `json:"prune"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.mcpApplyMetadataSync(a.Source, a.Prune), nil
	case "get_sync_status":
		var a struct {
			Source string `json:"source"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.mcpSyncStatus(a.Source), nil
	case "diff_metadata_snapshots":
		var a struct {
			Source string `json:"source"`
			From   string `json:"from"`
			To     string `json:"to"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.mcpDiffSnapshots(a.Source, a.From, a.To), nil
	case "profile_metadata_assets":
		var a struct {
			Source      string   `json:"source"`
			Tables      []string `json:"tables"`
			Mode        string   `json:"mode"`
			SampleLimit int      `json:"sample_limit"`
			PIIColumns  []string `json:"pii_columns"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.mcpProfileMetadata(ctx, a.Source, a.Tables, a.Mode, a.SampleLimit, a.PIIColumns), nil
	case "execute_with_repair":
		var a repairArgs
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.executeWithRepair(ctx, a)
	case "run_sql_safely":
		var a struct {
			SQL            string `json:"sql"`
			Limit          int    `json:"limit"`
			Profile        string `json:"profile"`
			TimeoutSeconds int    `json:"timeout_seconds"`
			Fresh          bool   `json:"fresh"`
			ApprovePlan    bool   `json:"approve_plan"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		// profile="auto" → resolve via the router; execute only when decisive,
		// otherwise hand back candidates for an explicit choice (never guess).
		if strings.EqualFold(strings.TrimSpace(a.Profile), "auto") {
			dec, err := s.routeProfile(ctx, a.SQL)
			if err != nil {
				return map[string]any{"status": "routing_failed", "error": err.Error()}, nil
			}
			if !dec.Decisive {
				out := routeResult(dec)
				out["status"] = "profile_choice_required"
				out["notice"] = "여러 프로파일이 이 쿼리를 처리할 수 있어 자동 선택하지 않았습니다. candidates 중 하나의 profile_id를 사용자와 확정한 뒤 run_sql_safely(profile=<id>)로 다시 호출하세요."
				return out, nil
			}
			a.Profile = dec.Selected
		}
		if ids := s.pendingClarifications(ctx); len(ids) > 0 && a.Profile != "" {
			return map[string]any{
				"status":  "clarification_required",
				"error":   "이 세션에 답변되지 않은 blocking 재질문이 있습니다. 실행 전에 사용자 확인이 필요합니다.",
				"pending": ids,
				"notice":  "clarifications의 질문을 사용자에게 전달해 답을 받은 뒤 prepare_sql_context(question, clarifications={...})를 다시 호출하세요. status=ready를 받으면 실행이 허용됩니다.",
			}, nil
		}
		// Authorization before validation: an unauthorized caller must not
		// receive catalog-validation service (or its error detail) for a
		// profile-scoped execution request.
		if a.Profile != "" {
			if err := s.canUseProfileID(ctx, userFrom(ctx), a.Profile); err != nil {
				return map[string]any{
					"status": "forbidden",
					"error":  err.Error(),
					"notice": "discover usable profile ids with list_db_profiles; request access from the profile owner/admin.",
				}, nil
			}
		}
		// validate against the profile's own workspace catalog when it has one
		// (the right catalog for that DB); fall back to the active catalog.
		vcat, _ := s.catalogFor(a.Profile)
		v := vcat.ValidateSQL(catalog.ValidateRequest{SQL: a.SQL, Limit: a.Limit})
		if a.Profile == "" {
			return map[string]any{
				"status":       "dry_run_only",
				"validation":   v,
				"bounded_sql":  v.BoundedSQL,
				"rows":         []any{},
				"notice":       "No db profile given. Configure profiles in /admin/db (dataset db_profiles) and pass profile to execute for real.",
				"request_time": time.Now().Format(time.RFC3339),
			}, nil
		}
		if !v.Valid {
			return map[string]any{
				"status":     "blocked",
				"error":      "catalog validation failed",
				"validation": v,
				"notice":     "invalid SQL is never executed. Apply validation.fix_hints and retry (max 2).",
			}, nil
		}
		mcpUser := userFrom(ctx)
		execAs := "mcp"
		if mcpUser != nil {
			execAs = mcpUser.Username
		}
		result, masked, cached, err := s.executeGuarded(ctx, a.Profile, a.SQL, dbconn.ExecOptions{
			MaxRows:        a.Limit,
			TimeoutSeconds: a.TimeoutSeconds,
			User:           execAs,
			ApprovePlan:    a.ApprovePlan,
		}, a.Fresh)
		if err != nil {
			var cost *dbconn.PlanCostError
			if errors.As(err, &cost) {
				return map[string]any{
					"status":     "blocked_cost_ceiling",
					"validation": map[string]any{"valid": true, "warnings": len(v.Warnings)},
					"live_plan":  cost.Plan,
					"measure":    cost.Measure,
					"limit":      cost.Limit,
					"estimated":  cost.Actual,
					"error":      err.Error(),
					"notice":     "예상 실행 비용이 프로파일의 절대 상한을 초과했습니다. approve_plan으로 우회할 수 없습니다. 기간·LIMIT·필터로 쿼리를 좁히세요(상한 조정은 관리자 프로파일 정책 변경 필요).",
				}, nil
			}
			var gate *dbconn.PlanGateError
			if errors.As(err, &gate) {
				return map[string]any{
					"status":     "plan_approval_required",
					"validation": map[string]any{"valid": true, "warnings": len(v.Warnings)},
					"live_plan":  gate.Plan,
					"threshold":  gate.Threshold,
					"error":      err.Error(),
					"notice":     "실행계획 위험도가 임계값 이상입니다. live_plan의 risk_factors/suggestions를 반영해 기간·LIMIT 조건을 좁혀 재생성하는 것을 우선하세요. 위험을 감수하고 그대로 실행해야 한다면 사용자 승인을 받은 뒤 approve_plan=true로 다시 호출하세요.",
				}, nil
			}
			out := map[string]any{
				"status":     "execution_failed",
				"validation": v,
				"error":      err.Error(),
			}
			if h := dbHint(err.Error()); h != "" {
				out["hint"] = h
			}
			return out, nil
		}
		out := map[string]any{
			"status":     "executed",
			"validation": map[string]any{"valid": true, "warnings": len(v.Warnings)},
			"result":     result,
		}
		if len(v.Lint) > 0 {
			out["lint_warnings"] = v.Lint
		}
		if cached {
			out["cached"] = true
		}
		if len(masked) > 0 {
			out["masked_columns"] = masked
			out["notice"] = "PII 컬럼 값은 마스킹되었습니다."
		}
		if d := s.diagnoseResult(a.SQL, result); d != nil {
			out["result_diagnosis"] = d
		}
		return out, nil
	case "record_feedback":
		var a map[string]any
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.recordFeedback(ctx, a, "client")
	case "review_feedback":
		var a struct {
			FeedbackID string `json:"feedback_id"`
			Decision   string `json:"decision"`
			Notes      string `json:"notes"`
			Limit      int    `json:"limit"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.reviewFeedback(ctx, a.FeedbackID, a.Decision, a.Notes, a.Limit)
	case "get_catalog_health":
		return s.cat().Health(), nil
	case "get_metadata_quality":
		var a struct {
			Gate bool `json:"gate"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		if a.Gate {
			return s.cat().QualityGate(), nil
		}
		return s.cat().QualityReport(), nil
	case "suggest_semantic_metadata":
		var a struct {
			Tables []string `json:"tables"`
			Kinds  []string `json:"kinds"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.cat().SuggestSemanticMetadata(a.Tables, a.Kinds), nil
	case "suggest_model_candidates":
		var a struct {
			Tables []string `json:"tables"`
			Kinds  []string `json:"kinds"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.cat().SuggestModelCandidates(a.Tables, a.Kinds), nil
	case "analyze_impact":
		var a struct {
			Table  string `json:"table"`
			Column string `json:"column"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.cat().AnalyzeImpact(a.Table, a.Column), nil
	case "review_candidates":
		var a struct {
			Tables []string `json:"tables"`
			Kinds  []string `json:"kinds"`
			Status string   `json:"status"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.cat().ReviewCandidates(a.Tables, a.Kinds, a.Status), nil
	case "decide_candidates":
		var a struct {
			Decisions []catalog.DecideCandidate `json:"decisions"`
			Reviewer  string                    `json:"reviewer"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		reviewer := a.Reviewer
		if reviewer == "" {
			reviewer = "mcp"
		}
		return s.cat().DecideCandidates(a.Decisions, reviewer, time.Now()), nil
	case "get_metadata_digest":
		return s.MetadataDigest(), nil
	case "openmetadata_status":
		return s.omStatus(ctx), nil
	case "import_openmetadata":
		var a struct {
			Scope           string `json:"scope"`
			MaxTables       int    `json:"max_tables"`
			IncludeGlossary *bool  `json:"include_glossary"`
			Apply           bool   `json:"apply"`
			ToReview        bool   `json:"to_review"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		includeGlossary := a.IncludeGlossary == nil || *a.IncludeGlossary
		return s.omImport(ctx, a.Scope, a.MaxTables, includeGlossary, a.Apply, a.ToReview), nil
	case "export_to_openmetadata":
		var a struct {
			Scope     string `json:"scope"`
			MaxTables int    `json:"max_tables"`
			DryRun    *bool  `json:"dry_run"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		dryRun := a.DryRun == nil || *a.DryRun
		return s.omExport(ctx, a.Scope, a.MaxTables, dryRun), nil
	case "openmetadata_drift":
		var a struct {
			Scope     string `json:"scope"`
			MaxTables int    `json:"max_tables"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.omDrift(ctx, a.Scope, a.MaxTables), nil
	case "export_lineage_to_openmetadata":
		var a struct {
			Scope     string `json:"scope"`
			MaxTables int    `json:"max_tables"`
			DryRun    *bool  `json:"dry_run"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		dryRun := a.DryRun == nil || *a.DryRun
		return s.omExportLineage(ctx, a.Scope, a.MaxTables, dryRun), nil
	case "get_approved_overrides":
		return s.cat().ApprovedOverrides(), nil
	case "apply_approved_candidates":
		return s.applyApprovedCandidates()
	case "find_filter_columns":
		var a struct {
			Values []string `json:"values"`
			Tables []string `json:"tables"`
			TopK   int      `json:"top_k"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.cat().FindFilterColumns(a.Values, a.Tables, a.TopK), nil
	case "resolve_time":
		var a struct {
			Question string `json:"question"`
			Table    string `json:"table"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.cat().ResolveTime(a.Question, a.Table, time.Now()), nil
	case "run_evaluation":
		var a struct {
			GoldenPath string `json:"golden_path"`
			TopK       int    `json:"top_k"`
			Profile    string `json:"profile"`
			Retrieval  bool   `json:"retrieval"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		if a.Retrieval {
			// retrieval-stage-only metrics: table/column/join/value recall
			return s.cat().EvaluateRetrieval(a.GoldenPath, a.TopK)
		}
		var counter catalog.RowCounter
		if a.Profile != "" {
			if err := s.canUseProfileID(ctx, userFrom(ctx), a.Profile); err != nil {
				return nil, err
			}
			counter = func(cctx context.Context, sql string) (int64, error) {
				return s.DB.CountRows(cctx, a.Profile, sql)
			}
		}
		return s.cat().RunEvaluationExec(ctx, a.GoldenPath, a.TopK, counter)
	case "learn_from_feedback":
		var a struct {
			MinOccurrences int `json:"min_occurrences"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.cat().LearnFromFeedback(a.MinOccurrences)
	case "suggest_golden_from_feedback":
		var a struct {
			Limit int `json:"limit"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.cat().SuggestGoldenFromFeedback(a.Limit), nil
	case "promote_golden_queries":
		var a struct {
			FeedbackIDs []string `json:"feedback_ids"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		res := s.cat().PromoteGolden(a.FeedbackIDs, time.Now())
		if applied, _ := res["applied"].(int); applied > 0 {
			// meta-DB mode: persist before reload or the promotion is reverted
			if err := s.persistDatasetsToDB("golden_queries.json"); err != nil {
				res["persist_error"] = "file applied but meta DB write failed: " + err.Error()
				return res, nil
			}
			if reload, err := s.reloadCatalog(); err == nil {
				res["reloaded"] = reload
			} else {
				res["reload_error"] = err.Error()
			}
		}
		return res, nil
	case "prepare_sql_context":
		var a struct {
			Question         string            `json:"question"`
			Tables           []string          `json:"tables"`
			Limit            int               `json:"limit"`
			Clarifications   map[string]string `json:"clarifications"`
			PreviousQuestion string            `json:"previous_question"`
			PreviousSQL      string            `json:"previous_sql"`
			Profile          string            `json:"profile"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		pcat, catSource := s.catalogFor(a.Profile)
		bundle := pcat.PrepareFollowup(a.Question, a.PreviousQuestion, a.PreviousSQL, a.Tables, a.Limit, time.Now(), a.Clarifications)
		if catSource != "active" {
			bundle["catalog_source"] = catSource
		}
		s.trackPendingClarifications(ctx, bundle)
		// learning loop: a clarification round the user answered is a strong
		// relevance signal — record it as "corrected" feedback so the chosen
		// tables gain retrieval usage prior on the next catalog reload.
		if len(a.Clarifications) > 0 && bundle["status"] == "ready" {
			if sel, ok := bundle["selected_tables"].([]string); ok && len(sel) > 0 {
				fb := map[string]any{
					"question": a.Question, "tables": sel, "outcome": "corrected",
					"source": "clarification", "clarifications": a.Clarifications,
				}
				if _, err := s.recordFeedback(ctx, fb, "clarification"); err != nil {
					log.Printf("clarification feedback record failed: %v", err)
				}
			}
		}
		return bundle, nil
	case "build_sql_skeleton":
		var a struct {
			Question string   `json:"question"`
			Tables   []string `json:"tables"`
			Limit    int      `json:"limit"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.cat().BuildSQLSkeleton(a.Question, a.Tables, a.Limit, time.Now()), nil
	case "rank_candidates":
		var a struct {
			Question        string   `json:"question"`
			Candidates      []string `json:"candidates"`
			ExpectedOutputs []string `json:"expected_outputs"`
			Metrics         []string `json:"metrics"`
			Limit           int      `json:"limit"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		if len(a.Candidates) == 0 {
			return nil, errors.New("candidates must contain at least one SQL")
		}
		return s.cat().RankCandidates(a.Question, a.Candidates, a.ExpectedOutputs, a.Metrics, a.Limit), nil
	case "suggest_joins":
		var a struct {
			Tables []string `json:"tables"`
			TopK   int      `json:"top_k"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.cat().SuggestJoins(a.Tables, a.TopK), nil
	case "list_datasets":
		return map[string]any{
			"data_dir": s.cat().DataDir,
			"datasets": s.cat().DatasetStatus(),
			"how_to":   "put_dataset으로 교체(자동 백업·검증·핫스왑), remove_dataset으로 제거, 파일을 직접 수정했다면 reload_catalog 호출. required=true는 제거 불가, editable=false는 시스템 관리 대상.",
		}, nil
	case "get_dataset":
		var a struct {
			Name       string `json:"name"`
			SampleRows int    `json:"sample_rows"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.cat().DatasetSample(a.Name, a.SampleRows)
	case "put_dataset":
		var a struct {
			Name    string          `json:"name"`
			Content json.RawMessage `json:"content"`
			Force   bool            `json:"force"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.putDataset(a.Name, a.Content, a.Force)
	case "remove_dataset":
		var a struct {
			Name string `json:"name"`
		}
		if err := decodeArgs(req.Arguments, &a); err != nil {
			return nil, err
		}
		return s.removeDataset(a.Name)
	case "reload_catalog":
		return s.reloadCatalog()
	default:
		return nil, errors.New("unknown tool: " + req.Name)
	}
}

// putDataset replaces a dataset file and hot-swaps the recompiled catalog.
// The previous file is backed up first; if the new content fails to compile,
// or introduces new load errors for that file (and force is false), the
// backup is restored and the old catalog stays active.
func (s *Server) putDataset(name string, content json.RawMessage, force bool) (map[string]any, error) {
	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	return s.putDatasetLocked(name, content, force)
}

// putDatasetLocked is the body of putDataset for callers that already hold
// dataMu (e.g. restoreDataset).
func (s *Server) putDatasetLocked(name string, content json.RawMessage, force bool) (map[string]any, error) {
	dataDir := s.cat().DataDir
	before := datasetErrorCount(s.cat(), name)
	d, backup, err := catalog.ReplaceDataset(dataDir, name, content)
	if err != nil {
		return nil, err
	}
	rollback := func(reason string, issues any) (map[string]any, error) {
		if backup != "" {
			if rerr := catalog.RestoreDatasetBackup(dataDir, d.File, backup); rerr != nil {
				return nil, fmt.Errorf("%s; ROLLBACK ALSO FAILED (%v) — restore %s manually", reason, rerr, backup)
			}
		} else {
			_ = os.Remove(filepath.Join(dataDir, d.File))
		}
		if cat, lerr := catalog.Load(dataDir); lerr == nil {
			s.setCatalog(cat)
		}
		return map[string]any{
			"applied": false, "dataset": d.Name, "reason": reason,
			"issues": issues, "backup": backup,
			"hint": "content를 스키마에 맞게 수정하거나, 오류를 감수하려면 force=true로 재시도하세요. 스키마: " + d.Schema,
		}, nil
	}
	cat, err := catalog.Load(dataDir)
	if err != nil {
		return rollback("new content failed catalog compilation: "+err.Error(), nil)
	}
	var newIssues []catalog.LoadIssue
	for _, i := range cat.Issues {
		if i.Source == d.File && i.Level == "error" {
			newIssues = append(newIssues, i)
		}
	}
	if len(newIssues) > before && !force {
		return rollback("new content introduces load errors", newIssues)
	}
	s.setCatalog(cat)
	// meta mode: persist the confirmed content to the DB (source of truth).
	// The file already holds it (used for validation); only commit to DB on
	// success so rollback stays purely file-based.
	if s.datasetsInDB() {
		if perr := s.Meta.Store.PutDataset(context.Background(), d.Name, content, "admin"); perr != nil {
			return nil, fmt.Errorf("catalog updated but meta DB write failed: %w", perr)
		}
	}
	res := map[string]any{
		"applied": true, "dataset": d.Name, "file": d.File,
		"backup": backup,
		"loaded": cat.Summary(),
		"status": "catalog hot-swapped; no restart needed",
		"stored": storedNote(s.datasetsInDB()),
	}
	if len(newIssues) > 0 {
		res["issues"] = newIssues
		res["warning"] = "load errors present (force applied)"
	}
	return res, nil
}

func storedNote(inDB bool) string {
	if inDB {
		return "postgres meta DB (jamypg_datasets)"
	}
	return "file (db_profiles dir)"
}

func (s *Server) removeDataset(name string) (map[string]any, error) {
	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	dataDir := s.cat().DataDir
	d, backup, err := catalog.RemoveDataset(dataDir, name)
	if err != nil {
		return nil, err
	}
	cat, err := catalog.Load(dataDir)
	if err != nil {
		if backup != "" {
			_ = catalog.RestoreDatasetBackup(dataDir, d.File, backup)
			if restored, rerr := catalog.Load(dataDir); rerr == nil {
				s.setCatalog(restored)
			}
		}
		return nil, fmt.Errorf("catalog reload failed after removal, file restored: %w", err)
	}
	s.setCatalog(cat)
	if s.datasetsInDB() {
		_ = s.Meta.Store.DeleteDataset(context.Background(), d.Name)
	}
	return map[string]any{
		"removed": true, "dataset": d.Name, "file": d.File,
		"backup": backup,
		"loaded": cat.Summary(),
		"status": "catalog hot-swapped; restore by re-uploading via put_dataset or copying the backup back",
	}, nil
}

// applyApprovedCandidates merges approved review decisions into the dataset
// files and hot-reloads the catalog so they take effect immediately.
func (s *Server) applyApprovedCandidates() (map[string]any, error) {
	cat := s.cat()
	res := cat.ApplyApproved(cat.DataDir, time.Now())
	if errMsg, _ := res["error"].(string); errMsg != "" {
		return res, nil // structured failure with backup paths — not a transport error
	}
	if applied, _ := res["applied"].(int); applied == 0 {
		return res, nil // nothing written; skip the reload
	}
	// meta-DB mode: persist before reload or the merge is reverted
	if err := s.persistDatasetsToDB("overrides.json", "metrics.json", "topology_relations.json", "meta_code_dict.json"); err != nil {
		res["persist_error"] = "files applied but meta DB write failed: " + err.Error()
		return res, nil
	}
	reload, err := s.reloadCatalog()
	if err != nil {
		res["reload_error"] = err.Error()
		res["note"] = "파일 병합은 완료됐지만 카탈로그 리로드가 실패했습니다. 이전 카탈로그가 유지 중이니 원인 수정 후 reload_catalog를 호출하세요."
		return res, nil
	}
	res["reloaded"] = reload
	return res, nil
}

func (s *Server) reloadCatalog() (map[string]any, error) {
	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	// meta mode: refresh the on-disk cache from the DB first so a reload picks
	// up any out-of-band DB changes and the DB stays authoritative.
	if s.datasetsInDB() {
		if err := s.materializeDatasets(context.Background()); err != nil {
			return nil, fmt.Errorf("reload failed materializing datasets from meta DB: %w", err)
		}
	}
	cat, err := catalog.Load(s.cat().DataDir)
	if err != nil {
		return nil, fmt.Errorf("reload failed, previous catalog stays active: %w", err)
	}
	s.setCatalog(cat)
	errCount := 0
	for _, i := range cat.Issues {
		if i.Level == "error" {
			errCount++
		}
	}
	return map[string]any{
		"reloaded": true,
		"loaded":   cat.Summary(),
		"errors":   errCount,
		"warnings": len(cat.Issues) - errCount,
		"hint":     "상세 이슈는 get_catalog_health로 확인하세요.",
	}, nil
}

func datasetErrorCount(c *catalog.Catalog, name string) int {
	d, ok := findRegistryFile(name)
	if !ok {
		return 0
	}
	n := 0
	for _, i := range c.Issues {
		if i.Source == d && i.Level == "error" {
			n++
		}
	}
	return n
}

func findRegistryFile(name string) (string, bool) {
	for _, d := range catalog.DatasetRegistry {
		if d.Name == strings.ToLower(strings.TrimSpace(name)) || d.File == name {
			return d.File, true
		}
	}
	return "", false
}

// auditToolCall appends every MCP tool invocation to an append-only JSONL so
// SQL-generation steps are traceable. Failures to write never block a call.
func (s *Server) auditToolCall(params json.RawMessage, dur time.Duration, callErr error) {
	var req struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	_ = json.Unmarshal(params, &req)
	args := string(req.Arguments)
	if len(args) > 4000 {
		args = args[:4000] + "...(truncated)"
	}
	entry := map[string]any{
		"ts":          time.Now().Format(time.RFC3339Nano),
		"tool":        req.Name,
		"arguments":   json.RawMessage(nullIfEmpty(args)),
		"duration_ms": dur.Milliseconds(),
		"is_error":    callErr != nil,
	}
	if callErr != nil {
		entry["error"] = callErr.Error()
	}
	s.recordToolMetric(req.Name, dur.Milliseconds(), callErr != nil)
	s.appendAudit(entry)
}

func nullIfEmpty(s string) string {
	if strings.TrimSpace(s) == "" || !json.Valid([]byte(s)) {
		return "null"
	}
	return s
}

func (s *Server) resources() []map[string]any {
	return []map[string]any{
		resource("guide://getting-started", "Getting started", "How to go from a question to safe DB SQL with these tools — read this first."),
		resource("metadata://catalog/summary", "Catalog summary", "Counts, schemas, sample totals, and load time."),
		resource("metadata://catalog/tables", "Table list", "Compact list of compiled tables."),
		resource("metadata://catalog/relations", "Join relations", "Compiled join graph relations."),
		resource("metadata://catalog/policies", "NL2SQL policy hints", "Read-only, row-limit, and SQL safety rules."),
		resource("metadata://catalog/prompts", "Stored SQL prompts", "Active prompt templates from the dataset prompts.json."),
		resource("metadata://catalog/examples", "Golden SQL examples", "First 100 examples from sql_datasets.json."),
	}
}

func (s *Server) resourceTemplates() []map[string]any {
	return []map[string]any{
		{
			"uriTemplate": "metadata://catalog/table/{schema}.{table}",
			"name":        "Table detail",
			"description": "Full compiled metadata for one table.",
			"mimeType":    "application/json",
		},
	}
}

func (s *Server) readResource(params json.RawMessage) (map[string]any, error) {
	var req struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, err
	}
	if req.URI == "guide://getting-started" {
		return map[string]any{
			"contents": []map[string]any{{
				"uri":      req.URI,
				"mimeType": "text/markdown",
				"text":     gettingStartedGuide,
			}},
		}, nil
	}
	var data any
	switch req.URI {
	case "metadata://catalog/summary":
		data = s.cat().Summary()
	case "metadata://catalog/tables":
		data = s.tableList()
	case "metadata://catalog/relations":
		data = s.cat().Relations
	case "metadata://catalog/policies":
		data = map[string]any{
			"read_only":        true,
			"default_limit":    catalog.DefaultLimit,
			"blocked_keywords": []string{"INSERT", "UPDATE", "DELETE", "MERGE", "DROP", "ALTER", "TRUNCATE", "CREATE", "GRANT", "REVOKE", "EXECUTE"},
			"validation":       "Use validate_sql before run_sql_safely or any external execution.",
		}
	case "metadata://catalog/prompts":
		data = s.promptListResource()
	case "metadata://catalog/examples":
		n := len(s.cat().Samples)
		if n > 100 {
			n = 100
		}
		data = s.cat().Samples[:n]
	default:
		const prefix = "metadata://catalog/table/"
		if strings.HasPrefix(req.URI, prefix) {
			name := strings.TrimPrefix(req.URI, prefix)
			t, ok := s.cat().ResolveTable(name)
			if !ok {
				return nil, fmt.Errorf("table not found: %s", name)
			}
			data = t
		} else {
			return nil, fmt.Errorf("unknown resource: %s", req.URI)
		}
	}
	b, _ := json.MarshalIndent(data, "", "  ")
	return map[string]any{
		"contents": []map[string]any{{
			"uri":      req.URI,
			"mimeType": "application/json",
			"text":     string(b),
		}},
	}, nil
}

func (s *Server) tableList() []map[string]any {
	out := make([]map[string]any, 0, len(s.cat().Tables))
	for _, t := range s.cat().Tables {
		out = append(out, map[string]any{
			"table":        t.FQN,
			"schema":       t.Schema,
			"name":         t.Name,
			"logical_name": t.LogicalName,
			"description":  t.Description,
			"columns":      len(t.Columns),
			"primary_keys": t.PrimaryKeys,
			"foreign_keys": t.ForeignKeys,
		})
	}
	sort.Slice(out, func(i, j int) bool { return fmt.Sprint(out[i]["table"]) < fmt.Sprint(out[j]["table"]) })
	return out
}

func (s *Server) prompts() []map[string]any {
	out := []map[string]any{
		{
			"name":        "text2sql_workflow",
			"description": "Workflow prompt that instructs the client to retrieve schema, joins, examples, validate, then produce SQL.",
			"arguments": []map[string]any{
				{"name": "question", "description": "User question", "required": true},
			},
		},
		{
			"name":        "db_sql_generation",
			"description": "DB SQL generation prompt with optional schema context, join context, and few-shot examples.",
			"arguments": []map[string]any{
				{"name": "question", "required": true},
				{"name": "schema_context", "required": false},
				{"name": "join_context", "required": false},
				{"name": "examples", "required": false},
			},
		},
	}
	for _, p := range s.cat().Prompts {
		if p.Name == "" || (!p.IsActive && strings.EqualFold(p.Category, "SQL")) {
			continue
		}
		out = append(out, map[string]any{
			"name":        promptName(p.Name),
			"description": firstNonEmpty(p.Description, p.Name),
			"arguments":   []map[string]any{},
		})
	}
	return out
}

func (s *Server) getPrompt(params json.RawMessage) (map[string]any, error) {
	var req struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, err
	}
	switch req.Name {
	case "text2sql_workflow":
		q := req.Arguments["question"]
		return promptResult("Metadata-compiled NL2SQL workflow", "Use this workflow for question: "+q+"\n\n1. Call prepare_sql_context(question) — it runs analyze → search → metric definitions → schema context → join paths → SQL skeleton in one call and returns a single bundle. (Fall back to the individual tools — analyze_question, search_schema, search_examples, find_filter_columns, resolve_time, get_metric_definition, get_schema_context, get_join_paths, build_sql_skeleton — only to refine one part.)\n1b. If the response has status=needs_clarification, DO NOT generate SQL. Relay each clarifications[].question to the user verbatim; when options exist, present them and mark the recommended one. Then re-call prepare_sql_context(question, clarifications={id: answer-or-option-key}). Items under advisory carry safe defaults — proceed, but list them as assumptions in the final answer. If a metric's source=inferred, confirm the formula with the user.\n2. Fill only the skeleton's /* SLOT */ comments to complete one DB SELECT, using solely the bundle's tables, columns, dictionary metric expressions, and join conditions. Never expose pii columns. Always bound rows.\n3. Call validate_sql with metrics=metric_names and expected_outputs=expected_output_columns from the bundle; apply fix_hints and retry at most twice. Never execute invalid SQL. For hard questions, generate 2-3 candidates and pick rank_candidates best_sql.\n4. Call explain_sql; if risk is high, regenerate with period/limit constraints instead of executing.\n5. To execute, call run_sql_safely(sql, profile) — read-only; discover profile ids with list_db_profiles. It refuses with status=clarification_required while blocking clarifications remain unanswered in this session.\n6. Return the final answer as JSON: {sql, used_tables, used_columns, applied_metrics, applied_join_paths, applied_filters, assumptions, cautions, validation_result, executable}.\n7. Call record_feedback with the outcome."), nil
	case "db_sql_generation":
		text := "Generate one DB SQL SELECT for the user question.\n\nQuestion:\n" + req.Arguments["question"] + "\n\nSchema context:\n" + req.Arguments["schema_context"] + "\n\nJoin context:\n" + req.Arguments["join_context"] + "\n\nFew-shot examples:\n" + req.Arguments["examples"] + "\n\nRules:\n- Use schema-qualified table names.\n- Do not invent columns.\n- Apply each table's operator-configured policy filters (see policy_hints) only when the corresponding columns exist.\n- Return SQL plus a brief Korean explanation."
		return promptResult("DB SQL generation", text), nil
	default:
		for _, p := range s.cat().Prompts {
			if promptName(p.Name) == req.Name {
				return promptResult(firstNonEmpty(p.Description, p.Name), replacePromptArgs(p.Content, req.Arguments)), nil
			}
		}
		return nil, fmt.Errorf("prompt not found: %s", req.Name)
	}
}

func (s *Server) promptListResource() []map[string]any {
	out := []map[string]any{}
	for _, p := range s.cat().Prompts {
		out = append(out, map[string]any{
			"name":        promptName(p.Name),
			"source_name": p.Name,
			"role":        p.Role,
			"category":    p.Category,
			"description": p.Description,
			"is_active":   p.IsActive,
		})
	}
	return out
}

func promptResult(description, text string) map[string]any {
	return map[string]any{
		"description": description,
		"messages": []map[string]any{{
			"role": "user",
			"content": map[string]any{
				"type": "text",
				"text": text,
			},
		}},
	}
}

func resultResponse(id *json.RawMessage, result any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: rawID(id), Result: result}
}

func errorResponse(id *json.RawMessage, code int, message string, data any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: rawID(id), Error: &rpcError{Code: code, Message: message, Data: data}}
}

func rawID(id *json.RawMessage) json.RawMessage {
	if id == nil {
		return json.RawMessage("null")
	}
	return *id
}

func toolResult(data any) map[string]any {
	b, _ := json.MarshalIndent(data, "", "  ")
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(b)}},
		"structuredContent": data,
		"isError":           false,
	}
}

func toolError(message string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": message}},
		"isError": true,
	}
}

func tool(name, description string, inputSchema map[string]any) map[string]any {
	return map[string]any{"name": name, "description": description, "inputSchema": inputSchema}
}

const gettingStartedGuide = `# JASQL — Getting started

JASQL turns a Korean/English question into **safe, read-only DB SQL**,
grounded in compiled catalog metadata. You never have to guess table or column
names — the tools hand you vetted parts.

## The fast path (recommended)

1. **prepare_sql_context(question)** — one call runs the whole front half of the
   pipeline (analyze → search → metric definitions → schema context → join paths
   → SQL skeleton) and returns a single bundle:
   - ` + "`analysis`" + ` — intent, dimensions, filters, expected_output_columns, ambiguities
   - ` + "`selected_tables`" + ` + ` + "`search_candidates`" + ` — which tables and why
   - ` + "`metrics`" + ` — dictionary metric expressions (use verbatim)
   - ` + "`schema_context`" + ` — the columns you may reference
   - ` + "`join_paths`" + ` — the only ON conditions you may use
   - ` + "`skeleton.skeleton_sql`" + ` — a draft SQL frame with ` + "`/* SLOT */`" + ` markers
   - ` + "`metric_names`" + `, ` + "`expected_output_columns`" + ` — pass these straight to validate_sql
1b. **If ` + "`status`" + ` is ` + "`needs_clarification`" + `**, the skeleton is withheld: the
   server judged the question ambiguous (undefined metric, near-tie table or
   filter-column choice, too vague). DO NOT generate SQL — relay each
   ` + "`clarifications[].question`" + ` to the user verbatim (present options, mark the
   recommended one), then re-call
   ` + "`prepare_sql_context(question, clarifications={id: answer-or-option-key})`" + `.
   Items under ` + "`advisory`" + ` carry safe defaults — proceed, but list them as
   assumptions in your final answer. run_sql_safely refuses to execute while
   blocking clarifications remain unanswered in the session.
2. **Fill only the ` + "`/* SLOT */`" + ` markers** to complete one DB SELECT.
   Use only the bundle's tables, columns, metric expressions, and joins. Never
   output PII columns; always bound rows.
3. **validate_sql(sql, metrics=metric_names, expected_outputs=expected_output_columns)** —
   apply ` + "`fix_hints`" + ` and retry at most twice. Invalid SQL is never executed.
4. **explain_sql(sql, profile?)** — if risk=high, add period/limit constraints
   and regenerate instead of executing.
5. **run_sql_safely(sql, profile)** — execute read-only. Discover profile ids
   with **list_db_profiles**.
6. Return your answer as JSON: {sql, used_tables, used_columns, applied_metrics,
   applied_join_paths, applied_filters, assumptions, cautions, validation_result,
   executable}, then **record_feedback**.

## When to drop to individual tools

Only to refine one part: analyze_question, search_schema, find_filter_columns,
resolve_time, get_metric_definition, get_schema_context, get_join_paths,
build_sql_skeleton, rank_candidates.

## Rules that never bend

- Read-only only (SELECT/WITH). DML/DDL/PLSQL are rejected by validate_sql and
  by the connector.
- Take JOIN conditions only from join_paths / the skeleton — never invent them.
- Use dictionary metric expressions verbatim; if a metric's source=inferred,
  confirm the formula with the user.
- If analysis.ambiguities has entries with no clear default, ask before generating.
`

func resource(uri, name, description string) map[string]any {
	return map[string]any{"uri": uri, "name": name, "description": description, "mimeType": "application/json"}
}

func objectSchema(props map[string]any, required []string) map[string]any {
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func integer(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description, "minimum": 0}
}

func boolSchema(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

func arrayOf(itemType, description string) map[string]any {
	return map[string]any{"type": "array", "description": description, "items": map[string]any{"type": itemType}}
}

func arrayOfObjects(description string) map[string]any {
	return map[string]any{"type": "array", "description": description, "items": map[string]any{"type": "object"}}
}

func decodeArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "" || strings.TrimSpace(string(raw)) == "null" {
		return nil
	}
	return json.Unmarshal(raw, dst)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) writeSSEResponse(w http.ResponseWriter, r *http.Request, resp rpcResponse) {
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		sessionID = "stateless"
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "close")
	s.writeSSEEvent(w, sessionID, resp)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) writeSSEEvent(w http.ResponseWriter, sessionID string, data any) {
	id := s.nextEventID(sessionID)
	b, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "id: %s\n", id)
	_, _ = fmt.Fprint(w, "event: message\n")
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
}

func (s *Server) nextEventID(sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events[sessionID]++
	return fmt.Sprintf("%s-%d", sessionID, s.events[sessionID])
}

// trackPendingClarifications records/clears blocking re-questions for the MCP
// session that produced this prepare_sql_context bundle. No session id (stdio,
// stateless clients) → no tracking; the gate is a best-effort second line of
// defense, not the primary mechanism (the withheld skeleton is).
func (s *Server) trackPendingClarifications(ctx context.Context, bundle map[string]any) {
	sid := sessionFrom(ctx)
	if sid == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if status, _ := bundle["status"].(string); status == "needs_clarification" {
		ids := []string{}
		if cls, ok := bundle["clarifications"].([]catalog.Clarification); ok {
			for _, cl := range cls {
				ids = append(ids, cl.ID)
			}
		}
		if len(s.pendingClar) > 1000 { // runaway backstop for never-DELETEd sessions
			s.pendingClar = map[string][]string{}
		}
		s.pendingClar[sid] = ids
		return
	}
	delete(s.pendingClar, sid)
}

// pendingClarifications returns the unanswered blocking clarification ids for
// the calling MCP session, if any.
func (s *Server) pendingClarifications(ctx context.Context) []string {
	sid := sessionFrom(ctx)
	if sid == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingClar[sid]
}

func (s *Server) newSession() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	id := base64.RawURLEncoding.EncodeToString(b[:])
	s.mu.Lock()
	s.sessions[id] = time.Now()
	s.mu.Unlock()
	return id
}

// touchSession refreshes a known session's last-seen time; missing or
// unknown session IDs are ignored on purpose (lenient session policy).
func (s *Server) touchSession(r *http.Request) {
	_, _ = s.sessionFromRequest(r)
}

func (s *Server) sessionFromRequest(r *http.Request) (string, bool) {
	id := r.Header.Get("Mcp-Session-Id")
	if id == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return "", false
	}
	s.sessions[id] = time.Now()
	return id, true
}

func (s *Server) validateOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		host = u.Hostname()
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	for _, allowed := range s.Options.AllowedOrigins {
		if strings.EqualFold(origin, strings.TrimSpace(allowed)) {
			return true
		}
	}
	return false
}

func (s *Server) writeCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" || !s.validateOrigin(r) {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept, Mcp-Session-Id, MCP-Protocol-Version, Last-Event-ID")
}

func validateProtocolHeader(r *http.Request) error {
	v := r.Header.Get("MCP-Protocol-Version")
	if v == "" {
		return nil
	}
	switch v {
	case ProtocolVersion, "2025-03-26":
		return nil
	default:
		return fmt.Errorf("unsupported MCP-Protocol-Version: %s", v)
	}
}

func wantsSSE(r *http.Request) bool {
	if strings.EqualFold(r.URL.Query().Get("stream"), "1") || strings.EqualFold(r.URL.Query().Get("stream"), "true") {
		return true
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Prefer")), "text/event-stream")
}

func accepts(r *http.Request, typ string) bool {
	accept := strings.ToLower(r.Header.Get("Accept"))
	if accept == "" {
		return false
	}
	for _, part := range strings.Split(accept, ",") {
		if strings.Contains(strings.TrimSpace(part), strings.ToLower(typ)) {
			return true
		}
	}
	return false
}

func promptName(name string) string {
	s := strings.ToLower(name)
	s = regexp.MustCompile(`[^a-z0-9_-]+`).ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "prompt"
	}
	return "ds_" + s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func replacePromptArgs(text string, args map[string]string) string {
	for k, v := range args {
		text = strings.ReplaceAll(text, "{"+k+"}", v)
	}
	return text
}

func Serve(addr string, c *catalog.Catalog, opts Options) error {
	return ServeServer(addr, NewServer(c, opts))
}

// ServeServer serves a pre-configured Server (e.g. with EnableMeta applied).
func ServeServer(addr string, srv *Server) error {
	mux := http.NewServeMux()
	srv.Register(mux)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("sqlon NL2SQL MCP listening on http://%s%s", addr, srv.Options.Endpoint)
	return httpServer.ListenAndServe()
}
