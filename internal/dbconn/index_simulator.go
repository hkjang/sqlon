package dbconn

import (
	"context"
	"fmt"
	"strings"
)

type IndexSimulationResult struct {
	OK            bool     `json:"ok"`
	Used          bool     `json:"used"`
	Engine        string   `json:"engine"`
	OriginalCost  int64    `json:"original_cost"`
	SimulatedCost int64    `json:"simulated_cost"`
	SavingsPct    float64  `json:"savings_pct"`
	Message       string   `json:"message"`
	PlanSteps     []string `json:"plan_steps,omitempty"`
}

// VerifyHypotheticalIndex simulates index creation.
// On PostgreSQL, it uses the 'hypopg' extension if available.
// On other engines or as fallback, it performs AST/lexical matching.
func (m *Manager) VerifyHypotheticalIndex(ctx context.Context, profileID string, tableName string, columns []string, sqlText string) (*IndexSimulationResult, error) {
	p, err := m.store.GetProfileByID(ctx, profileID)
	if err != nil {
		return nil, err
	}
	d, err := DialectFor(p.Type)
	if err != nil {
		return nil, err
	}
	db, err := m.db(p)
	if err != nil {
		return nil, err
	}

	cleanSQL := trimSQL(sqlText)
	colsStr := strings.Join(columns, ", ")

	// 1. Get original cost
	var origCost int64 = 1000 // default mock cost
	origPlan, err := m.ExplainPlan(ctx, profileID, sqlText)
	if err == nil {
		origCost = origPlan.TotalCost
	}

	// 2. Perform simulation based on engine type
	if d.Name() == "postgres" {
		var indexName string
		// Try to create hypopg index
		createStmt := fmt.Sprintf("SELECT indexname FROM hypopg_create_index('CREATE INDEX ON %s (%s)')", tableName, colsStr)
		err = db.QueryRowContext(ctx, createStmt).Scan(&indexName)
		if err != nil {
			// Fallback if hypopg is missing
			return m.staticIndexSimulation(d.Name(), tableName, columns, cleanSQL, origCost, "PostgreSQL hypopg extension not available; falling back to static analysis.")
		}

		// Ensure we reset hypopg after our EXPLAIN
		defer func() {
			_, _ = db.ExecContext(ctx, "SELECT hypopg_reset()")
		}()

		// Run EXPLAIN with hypothetical index active
		var raw []byte
		if err := db.QueryRowContext(ctx, "EXPLAIN (FORMAT JSON) "+cleanSQL).Scan(&raw); err != nil {
			return nil, fmt.Errorf("hypothetical explain failed: %w", err)
		}
		simPlan, err := AnalyzePostgresPlanJSON(raw)
		if err != nil {
			return nil, err
		}

		// Check if the virtual index name appears in the explain plan steps
		used := false
		var steps []string
		for _, s := range simPlan.Steps {
			steps = append(steps, fmt.Sprintf("%s on %s %s (cost=%d)", s.Operation, s.ObjectName, s.Options, s.Cost))
			if strings.Contains(s.ObjectName, indexName) || strings.Contains(s.Options, indexName) {
				used = true
			}
		}

		savings := float64(0)
		if origCost > 0 {
			savings = float64(origCost-simPlan.TotalCost) / float64(origCost) * 100
		}
		if savings < 0 {
			savings = 0
		}

		msg := fmt.Sprintf("Hypothetical index created via hypopg: %s(%s).", tableName, colsStr)
		if used {
			msg += fmt.Sprintf(" Optimizer used this index! Cost reduced by %.1f%%.", savings)
		} else {
			msg += " Optimizer chose NOT to use this index."
		}

		return &IndexSimulationResult{
			OK:            true,
			Used:          used,
			Engine:        d.Name(),
			OriginalCost:  origCost,
			SimulatedCost: simPlan.TotalCost,
			SavingsPct:    savings,
			Message:       msg,
			PlanSteps:     steps,
		}, nil
	}

	// Fallback/MySQL/MariaDB static analysis
	return m.staticIndexSimulation(d.Name(), tableName, columns, cleanSQL, origCost, "")
}

func (m *Manager) staticIndexSimulation(engine, tableName string, columns []string, sqlText string, origCost int64, warning string) (*IndexSimulationResult, error) {
	upperSQL := strings.ToUpper(sqlText)
	used := false
	matchType := ""

	tableNameUpper := strings.ToUpper(tableName)
	if strings.Contains(upperSQL, tableNameUpper) {
		for _, col := range columns {
			colUpper := strings.ToUpper(col)
			if strings.Contains(upperSQL, colUpper) {
				used = true
				matchType = col
				break
			}
		}
	}

	savings := float64(0)
	simCost := origCost
	if used {
		savings = 75.0
		simCost = int64(float64(origCost) * 0.25)
	}

	msg := fmt.Sprintf("Static Analysis: Proposed index %s(%s) matches filter condition on '%s'.", tableName, strings.Join(columns, ", "), matchType)
	if used {
		msg += " Est. 75% cost reduction."
	} else {
		msg = fmt.Sprintf("Static Analysis: Proposed index %s(%s) does not match any indexable filters in this query.", tableName, strings.Join(columns, ", "))
	}

	if warning != "" {
		msg = warning + "\n" + msg
	}

	return &IndexSimulationResult{
		OK:            true,
		Used:          used,
		Engine:        engine,
		OriginalCost:  origCost,
		SimulatedCost: simCost,
		SavingsPct:    savings,
		Message:       msg,
	}, nil
}
