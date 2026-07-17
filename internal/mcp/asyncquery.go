package mcp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"

	"sqlon/internal/dbconn"
)

// Async query jobs: submit returns immediately with a job id; the query runs
// in the background under its own timeout and the result is polled by id.
// For interactive use the sync endpoints stay primary — this exists for
// long-running analytical queries that outlive an HTTP request comfort zone.

const (
	jobResultTTL  = 10 * time.Minute
	jobMaxPerUser = 5 // concurrent running jobs per user
	jobMaxTotal   = 200
)

type asyncJob struct {
	ID         string              `json:"job_id"`
	ProfileID  string              `json:"profile_id"`
	SQL        string              `json:"sql"`
	User       string              `json:"user"`
	Status     string              `json:"status"` // running | done | failed
	Error      string              `json:"error,omitempty"`
	Hint       string              `json:"hint,omitempty"`
	Result     *dbconn.QueryResult `json:"result,omitempty"`
	Masked     []string            `json:"masked_columns,omitempty"`
	Diagnosis  map[string]any      `json:"result_diagnosis,omitempty"`
	StartedAt  time.Time           `json:"started_at"`
	FinishedAt *time.Time          `json:"finished_at,omitempty"`
	cancel     context.CancelFunc
}

type asyncJobStore struct {
	mu   sync.Mutex
	jobs map[string]*asyncJob
}

func newAsyncJobStore() *asyncJobStore { return &asyncJobStore{jobs: map[string]*asyncJob{}} }

func newJobID() string {
	var b [9]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "job_" + time.Now().Format("150405.000000")
	}
	return "job_" + base64.RawURLEncoding.EncodeToString(b[:])
}

// prune drops finished jobs past their TTL; called under lock.
func (st *asyncJobStore) prune() {
	now := time.Now()
	for id, j := range st.jobs {
		if j.FinishedAt != nil && now.Sub(*j.FinishedAt) > jobResultTTL {
			delete(st.jobs, id)
		}
	}
}

func (st *asyncJobStore) runningFor(user string) int {
	n := 0
	for _, j := range st.jobs {
		if j.Status == "running" && j.User == user {
			n++
		}
	}
	return n
}

// submitAsyncQuery validates limits, registers the job, and launches the
// background execution. The background context is detached from the HTTP
// request but bounded by the profile/query timeout inside the connector.
func (s *Server) submitAsyncQuery(profile, sql, user string, opts dbconn.ExecOptions) (*asyncJob, string) {
	st := s.asyncJobs
	st.mu.Lock()
	defer st.mu.Unlock()
	st.prune()
	if len(st.jobs) >= jobMaxTotal {
		return nil, "비동기 잡 저장소가 가득 찼습니다. 잠시 후 다시 시도하세요."
	}
	if st.runningFor(user) >= jobMaxPerUser {
		return nil, "사용자당 동시 실행 잡 한도(5)에 도달했습니다. 완료를 기다리거나 취소하세요."
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &asyncJob{
		ID: newJobID(), ProfileID: profile, SQL: sql, User: user,
		Status: "running", StartedAt: time.Now(), cancel: cancel,
	}
	st.jobs[job.ID] = job

	go func() {
		res, masked, _, err := s.executeGuarded(ctx, profile, sql, opts, true)
		now := time.Now()
		st.mu.Lock()
		defer st.mu.Unlock()
		job.FinishedAt = &now
		if err != nil {
			job.Status = "failed"
			job.Error = err.Error()
			job.Hint = dbHint(err.Error())
			return
		}
		job.Status = "done"
		job.Result = res
		job.Masked = masked
		job.Diagnosis = s.diagnoseResult(sql, res)
	}()
	return job, ""
}

// jobView returns a copy safe to serialize (no cancel-func data races).
func (st *asyncJobStore) jobView(id string) (*asyncJob, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	j, ok := st.jobs[id]
	if !ok {
		return nil, false
	}
	c := *j
	c.cancel = nil
	return &c, true
}

func (st *asyncJobStore) cancelJob(id, user string, isAdmin bool) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	j, ok := st.jobs[id]
	if !ok || (!isAdmin && j.User != user) {
		return false
	}
	if j.Status == "running" && j.cancel != nil {
		j.cancel()
	}
	return true
}
