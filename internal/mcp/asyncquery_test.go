package mcp

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"sqlon/internal/meta"
)

func submitAsyncHTTP(t *testing.T, mux *http.ServeMux, token, profile string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"profile_id": profile, "sql": "SELECT CUST_NO FROM TS.TBL1"})
	if err != nil {
		t.Fatal(err)
	}
	rec := doReq(t, mux, "POST", "/api/query/submit", string(body), withCookie(token))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
	}
	var result struct {
		ID        string `json:"job_id"`
		Submitted bool   `json:"submitted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Submitted || result.ID == "" {
		t.Fatalf("submit response: %s", rec.Body.String())
	}
	return result.ID
}

// Observe completion without invoking either access path (and thus pruning).
func waitAsyncHTTPJob(t *testing.T, s *Server, id string) {
	t.Helper()
	// PostgreSQL AST validation compiles WASM on first use, which is slower under -race.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		s.asyncJobs.mu.Lock()
		j := s.asyncJobs.jobs[id]
		finished := j != nil && j.FinishedAt != nil && j.Status != "running"
		s.asyncJobs.mu.Unlock()
		if finished {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("job %s did not finish", id)
}

func TestAsyncHTTPExpiredFirstAccess(t *testing.T) {
	for _, method := range []string{"GET", "POST"} {
		t.Run(method, func(t *testing.T) {
			s, mux, admin, _ := newAuthServer(t)
			t.Cleanup(s.DB.Close)
			// The real background execution fails at profile lookup, without a DB server.
			id := submitAsyncHTTP(t, mux, admin, "missing-profile")
			waitAsyncHTTPJob(t, s, id)
			s.asyncJobs.mu.Lock()
			past := time.Now().Add(-jobResultTTL - time.Second)
			s.asyncJobs.jobs[id].FinishedAt = &past
			s.asyncJobs.mu.Unlock()

			// No submission or GET may clean up the job before this first access.
			path := "/api/query/job/" + id
			if method == "POST" {
				path += "/cancel"
			}
			rec := doReq(t, mux, method, path, "", withCookie(admin))
			if rec.Code != http.StatusNotFound {
				t.Errorf("first expired %s: got %d, want 404: %s", method, rec.Code, rec.Body.String())
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"sql", "result"} {
				if _, ok := body[key]; ok {
					t.Errorf("expired response exposes %s", key)
				}
			}
			if strings.Contains(rec.Body.String(), "SELECT CUST_NO FROM TS.TBL1") {
				t.Error("expired response exposes SQL")
			}
			s.asyncJobs.mu.Lock()
			_, exists := s.asyncJobs.jobs[id]
			s.asyncJobs.mu.Unlock()
			if exists {
				t.Error("expired job remains in store")
			}
		})
	}
}

func asyncHTTPProfile(t *testing.T, s *Server, connect string) {
	t.Helper()
	alice, err := s.Meta.Store.GetUserByUsername(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	definition, err := json.Marshal(map[string]any{"type": "postgres", "connect_string": connect, "username": "test", "password_ref": "plain:test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Meta.Store.UpsertProfile(t.Context(), &meta.ProfileRecord{ID: "async-test", OwnerID: alice.ID, Definition: definition, Visibility: meta.VisibilityPrivate}, true); err != nil {
		t.Fatal(err)
	}
}

func TestAsyncHTTPRetainedJobPermissions(t *testing.T) {
	s, mux, admin, alice := newAuthServer(t)
	t.Cleanup(s.DB.Close)
	asyncHTTPProfile(t, s, "postgres://localhost/test?host="+t.TempDir()+"&sslmode=disable")
	ownerID := submitAsyncHTTP(t, mux, alice, "async-test")
	adminID := submitAsyncHTTP(t, mux, admin, "missing-profile")
	// Submit everything before adjusting timestamps, so submit pruning cannot hide a regression.
	for _, id := range []string{ownerID, adminID} {
		waitAsyncHTTPJob(t, s, id)
		s.asyncJobs.mu.Lock()
		recent := time.Now().Add(-jobResultTTL + time.Minute)
		s.asyncJobs.jobs[id].FinishedAt = &recent
		s.asyncJobs.mu.Unlock()
	}
	for _, tc := range []struct {
		name, method, id, token string
		want                    int
	}{
		{"owner view", "GET", ownerID, alice, 200},
		{"admin view", "GET", ownerID, admin, 200},
		{"anonymous view", "GET", ownerID, "", 401},
		{"other user view", "GET", adminID, alice, 403},
		{"anonymous cancel", "POST", ownerID, "", 401},
		{"other user cancel", "POST", adminID, alice, 404},
		{"owner cancel completed", "POST", ownerID, alice, 200},
		{"admin cancel completed", "POST", ownerID, admin, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := "/api/query/job/" + tc.id
			if tc.method == "POST" {
				path += "/cancel"
			}
			rec := doReq(t, mux, tc.method, path, "", withCookie(tc.token))
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.method == "GET" && tc.want == 200 {
				var job asyncJob
				if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
					t.Fatal(err)
				}
				if job.ID != tc.id || job.SQL != "SELECT CUST_NO FROM TS.TBL1" || job.FinishedAt == nil || job.Status != "failed" || job.Error == "" {
					t.Fatalf("retained job: %+v", job)
				}
			}
		})
	}
}

func TestAsyncHTTPOldRunningJobSurvivesAccess(t *testing.T) {
	s, mux, admin, alice := newAuthServer(t)
	t.Cleanup(s.DB.Close)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	asyncHTTPProfile(t, s, "postgres://"+listener.Addr().String()+"/test?sslmode=disable&connect_timeout=30")
	id := submitAsyncHTTP(t, mux, alice, "async-test")
	t.Cleanup(func() {
		s.asyncJobs.cancelJob(id, "alice", false)
		waitAsyncHTTPJob(t, s, id)
	})
	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	// A real driver is blocked awaiting the local peer's PostgreSQL handshake.
	s.asyncJobs.mu.Lock()
	s.asyncJobs.jobs[id].StartedAt = time.Now().Add(-2 * jobResultTTL)
	s.asyncJobs.mu.Unlock()
	for _, token := range []string{alice, admin} {
		rec := doReq(t, mux, "GET", "/api/query/job/"+id, "", withCookie(token))
		if rec.Code != 200 {
			t.Fatalf("running view: %d %s", rec.Code, rec.Body.String())
		}
		var job asyncJob
		if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
			t.Fatal(err)
		}
		if job.Status != "running" || job.FinishedAt != nil {
			t.Fatalf("expected running job: %+v", job)
		}
	}
	rec := doReq(t, mux, "POST", "/api/query/job/"+id+"/cancel", "", withCookie(alice))
	if rec.Code != 200 {
		t.Fatalf("running cancel: %d %s", rec.Code, rec.Body.String())
	}
	conn.Close()
	waitAsyncHTTPJob(t, s, id)
}
