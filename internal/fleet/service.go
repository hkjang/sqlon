// Package fleet builds an evidence-bearing fleet overview from connection
// profiles. It is deliberately read-only: collectors own persistence later.
package fleet

import (
	"context"
	"sort"
	"time"

	"sqlon/internal/dbconn"
)

type Status string

const (
	StatusHealthy Status = "healthy"
	StatusFailed  Status = "failed"
	StatusUnknown Status = "unknown"
)

type Instance struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Engine       string          `json:"engine"`
	Status       Status          `json:"status"`
	CollectedAt  time.Time       `json:"collected_at"`
	ElapsedMS    int64           `json:"elapsed_ms,omitempty"`
	Error        string          `json:"error,omitempty"`
	Capabilities map[string]bool `json:"capabilities"`
}
type Health struct {
	Status      string     `json:"status"`
	Data        []Instance `json:"data"`
	CollectedAt time.Time  `json:"collected_at"`
	Warnings    []string   `json:"warnings,omitempty"`
	Limitations []string   `json:"limitations,omitempty"`
}
type Service struct{ DB *dbconn.Manager }

func New(db *dbconn.Manager) *Service { return &Service{DB: db} }

func (s *Service) Health(ctx context.Context) Health {
	now := time.Now().UTC()
	out := Health{Status: "ok", CollectedAt: now, Data: []Instance{}, Limitations: []string{"current snapshot uses connection probes; time-series collectors are not enabled yet"}}
	profiles, err := s.DB.Profiles(ctx)
	if err != nil {
		out.Status = "error"
		out.Warnings = []string{err.Error()}
		return out
	}
	for _, p := range profiles {
		ping := s.DB.Ping(ctx, p.ID)
		status := StatusHealthy
		if !ping.OK {
			status = StatusFailed
		}
		out.Data = append(out.Data, Instance{ID: p.ID, Name: p.Name, Engine: p.Type, Status: status, CollectedAt: now, ElapsedMS: ping.ElapsedMs, Error: ping.Error, Capabilities: capabilities(p.Type)})
	}
	sort.Slice(out.Data, func(i, j int) bool {
		if out.Data[i].Status != out.Data[j].Status {
			return out.Data[i].Status == StatusFailed
		}
		return out.Data[i].ID < out.Data[j].ID
	})
	return out
}
func capabilities(engine string) map[string]bool {
	c := map[string]bool{"sessions": true, "lock_tree": false, "workload": false, "query_plans": false, "storage": false, "replication": false}
	switch engine {
	case "postgres", "mysql", "mariadb":
		c["lock_tree"] = true
		c["query_plans"] = true
		c["storage"] = true
	case "oracle":
		c["lock_tree"] = true
		c["query_plans"] = true
		c["storage"] = true
		c["workload"] = true
	}
	return c
}
