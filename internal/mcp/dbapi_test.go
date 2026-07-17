package mcp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestDBProfileAPILifecycle(t *testing.T) {
	_, mux := newAdminMux(t, "")

	// empty list + driver status
	rec := doReq(t, mux, "GET", "/api/db-profiles", "", nil)
	if rec.Code != 200 {
		t.Fatalf("list: %d", rec.Code)
	}
	var list struct {
		Profiles        []map[string]any `json:"profiles"`
		DriverAvailable bool             `json:"driver_available"`
		DriverNote      string           `json:"driver_note"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Profiles) != 0 || list.DriverNote == "" {
		t.Fatalf("unexpected initial list: %+v", list)
	}

	// create
	body := `{"id":"dev-01","name":"개발","connect_string":"db-host:1521/ORCLPDB1?connect_timeout=5","username":"APP_READONLY","password_ref":"env:ORA_DEV_PW"}`
	rec = doReq(t, mux, "POST", "/api/db-profiles", body, nil)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	// invalid create refused (schemeless password)
	rec = doReq(t, mux, "POST", "/api/db-profiles", `{"id":"bad","connect_string":"h:1/S","username":"U","password_ref":"raw"}`, nil)
	if rec.Code != 400 {
		t.Fatalf("invalid create must be 400: %d", rec.Code)
	}
	// list shows masked-safe fields and defaults
	rec = doReq(t, mux, "GET", "/api/db-profiles", "", nil)
	if !strings.Contains(rec.Body.String(), `"env:ORA_DEV_PW"`) || !strings.Contains(rec.Body.String(), `"max_rows":1000`) {
		t.Fatalf("list after create: %s", rec.Body.String())
	}
	// update
	rec = doReq(t, mux, "PUT", "/api/db-profiles/dev-01",
		`{"connect_string":"db-vip:1521/ORCLPDB1?connect_timeout=5&expire_time=2","username":"APP_READONLY","password_ref":"env:ORA_DEV_PW","name":"개발 VIP"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	// test endpoint: stub driver → ok=false with actionable note
	rec = doReq(t, mux, "POST", "/api/db-profiles/dev-01/test", "", nil)
	if rec.Code != 200 {
		t.Fatalf("test: %d", rec.Code)
	}
	var ping struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ping)
	// Pure build → stub driver note ("-tags oracle"); oracle build → the real
	// driver fails resolving the unset password ref. Either is an actionable
	// ok=false, which is what this asserts.
	if ping.OK || (!strings.Contains(ping.Error, "-tags oracle") && !strings.Contains(ping.Error, "circuit") &&
		!strings.Contains(ping.Error, "ORA_DEV_PW")) {
		t.Fatalf("ping should fail with an actionable note: %+v", ping)
	}

	// query validate: catalog + connector guard
	rec = doReq(t, mux, "POST", "/api/query/validate",
		`{"sql":"DELETE FROM TS.TBL1","profile_id":"dev-01"}`, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "only SELECT") {
		t.Fatalf("validate: %d %s", rec.Code, rec.Body.String())
	}

	// execute: catalog-invalid SQL is never executed
	rec = doReq(t, mux, "POST", "/api/query/execute",
		`{"profile_id":"dev-01","sql":"SELECT X.NOPE FROM NO.SUCH_TABLE X"}`, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"executed":false`) ||
		!strings.Contains(rec.Body.String(), "catalog validation failed") {
		t.Fatalf("execute invalid: %d %s", rec.Code, rec.Body.String())
	}
	// execute: valid SQL reaches the driver, which is absent in default build
	rec = doReq(t, mux, "POST", "/api/query/execute",
		`{"profile_id":"dev-01","sql":"SELECT T1.CUST_NO FROM TS.TBL1 T1 FETCH FIRST 5 ROWS ONLY"}`, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"executed":false`) {
		t.Fatalf("execute stub: %d %s", rec.Code, rec.Body.String())
	}

	// explain: static always returned; live plan unavailable on stub build
	rec = doReq(t, mux, "POST", "/api/query/explain",
		`{"profile_id":"dev-01","sql":"SELECT T1.CUST_NO FROM TS.TBL1 T1 FETCH FIRST 5 ROWS ONLY"}`, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"static"`) ||
		!strings.Contains(rec.Body.String(), "live_plan_error") {
		t.Fatalf("explain: %d %s", rec.Code, rec.Body.String()[:300])
	}

	// history + metrics endpoints respond
	rec = doReq(t, mux, "GET", "/api/query/history", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "history") {
		t.Fatalf("history: %d", rec.Code)
	}
	rec = doReq(t, mux, "GET", "/api/metrics", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "driver_available") {
		t.Fatalf("metrics: %d", rec.Code)
	}
	rec = doReq(t, mux, "GET", "/metrics", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "db_query_total") {
		t.Fatalf("prometheus: %d", rec.Code)
	}

	// cancel unknown execution → 404
	rec = doReq(t, mux, "POST", "/api/query/cancel/exec-nope", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cancel unknown: %d", rec.Code)
	}

	// delete
	rec = doReq(t, mux, "DELETE", "/api/db-profiles/dev-01", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"removed":true`) {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, mux, "DELETE", "/api/db-profiles/dev-01", "", nil)
	if rec.Code != 400 {
		t.Fatalf("double delete must be 400: %d", rec.Code)
	}
}

func TestDBAPITokenEnforcement(t *testing.T) {
	_, mux := newAdminMux(t, "sekrit")
	// reads open
	if rec := doReq(t, mux, "GET", "/api/db-profiles", "", nil); rec.Code != 200 {
		t.Fatalf("profile list should be open: %d", rec.Code)
	}
	// mutations and data access require token
	for _, c := range [][2]string{
		{"POST", "/api/db-profiles"},
		{"PUT", "/api/db-profiles/x"},
		{"DELETE", "/api/db-profiles/x"},
		{"POST", "/api/query/execute"},
		{"POST", "/api/query/preview"},
		{"POST", "/api/query/metadata"},
		{"POST", "/api/query/explain"},
		{"POST", "/api/query/cancel/x"},
	} {
		rec := doReq(t, mux, c[0], c[1], `{}`, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s should require token, got %d", c[0], c[1], rec.Code)
		}
	}
	// db.html page served
	rec := doReq(t, mux, "GET", "/admin/db", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "DB 연결 관리") {
		t.Fatalf("/admin/db: %d", rec.Code)
	}
}
