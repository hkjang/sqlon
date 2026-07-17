package dbconn

import (
	"testing"
)

func TestStaticIndexSimulation(t *testing.T) {
	mgr := &Manager{}

	res, err := mgr.staticIndexSimulation("mysql", "orders", []string{"user_id"}, "SELECT * FROM orders WHERE user_id = 42", 1000, "")
	if err != nil {
		t.Fatal(err)
	}

	if !res.Used {
		t.Error("expected proposed index to be reported as used for matching query")
	}

	if res.SavingsPct != 75.0 {
		t.Errorf("expected savings percent to be 75.0, got %f", res.SavingsPct)
	}

	if res.SimulatedCost != 250 {
		t.Errorf("expected simulated cost to be 250, got %d", res.SimulatedCost)
	}

	// Test non-matching query
	res2, err := mgr.staticIndexSimulation("mysql", "orders", []string{"user_id"}, "SELECT * FROM users WHERE age = 30", 1000, "")
	if err != nil {
		t.Fatal(err)
	}

	if res2.Used {
		t.Error("expected index not to be used for non-matching query")
	}
}
