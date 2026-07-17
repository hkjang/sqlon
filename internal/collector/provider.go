package collector

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"sqlon/internal/dbconn"
)

type SystemQueryer interface {
	SystemQuery(context.Context, string, string, ...any) ([]map[string]any, error)
}

type Provider interface {
	Collect(context.Context, SystemQueryer, dbconn.Profile) (Snapshot, error)
}

type Registry struct{ providers map[string]Provider }

func NewRegistry() *Registry {
	return &Registry{providers: map[string]Provider{
		"postgres": postgresProvider{}, "mysql": mysqlProvider{},
		"mariadb": mariadbProvider{}, "oracle": oracleProvider{},
	}}
}

func (r *Registry) Get(engine string) (Provider, bool) {
	p, ok := r.providers[strings.ToLower(strings.TrimSpace(engine))]
	return p, ok
}

func value(row map[string]any, name string) any {
	for key, v := range row {
		if strings.EqualFold(key, name) {
			return v
		}
	}
	return nil
}

func text(row map[string]any, name string) string {
	v := value(row, name)
	if v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func number(row map[string]any, name string) float64 {
	v := value(row, name)
	switch n := v.(type) {
	case int:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case float32:
		return float64(n)
	case float64:
		return n
	}
	n, _ := strconv.ParseFloat(strings.TrimSpace(fmt.Sprint(v)), 64)
	return n
}

func metric(row map[string]any, name, unit string) Metric {
	return Metric{Name: name, Value: number(row, name), Unit: unit, Cumulative: true}
}
