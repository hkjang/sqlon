package catalog

import "testing"

func TestPredicateColumns(t *testing.T) {
	// orders(order_id PK, customer_id [no index], status [leading index], amount)
	orders := &Table{
		Schema: "S", Name: "ORDERS", FQN: "S.ORDERS",
		Columns: []*Column{
			{Name: "ORDER_ID", IsPK: true},
			{Name: "CUSTOMER_ID"},
			{Name: "STATUS"},
			{Name: "AMOUNT"},
		},
		ColumnMap: map[string]*Column{
			"ORDER_ID":    {Name: "ORDER_ID", IsPK: true},
			"CUSTOMER_ID": {Name: "CUSTOMER_ID"},
			"STATUS":      {Name: "STATUS"},
			"AMOUNT":      {Name: "AMOUNT"},
		},
		Indexes: []IndexDef{
			{IndexName: "idx_status", ColumnName: "STATUS", Seq: 1},
		},
	}
	c := &Catalog{
		Tables: map[string]*Table{"S.ORDERS": orders},
		ByName: map[string][]*Table{"ORDERS": {orders}},
	}

	cols := c.PredicateColumns("SELECT amount FROM s.orders WHERE customer_id = 10 AND status = 'A' ORDER BY amount")
	got := map[string]bool{}
	for _, pc := range cols {
		got[pc.Column] = pc.Indexed
	}
	if _, ok := got["CUSTOMER_ID"]; !ok {
		t.Fatalf("expected CUSTOMER_ID as a predicate column, got %+v", cols)
	}
	if got["CUSTOMER_ID"] {
		t.Fatal("CUSTOMER_ID has no index and must be reported un-indexed")
	}
	if _, ok := got["STATUS"]; !ok || !got["STATUS"] {
		t.Fatalf("STATUS is a leading index column and must be reported indexed, got %+v", cols)
	}
	if _, ok := got["ORDER_ID"]; ok {
		// order_id not used in predicates → should not appear
		t.Fatalf("ORDER_ID was not a predicate and must not appear, got %+v", cols)
	}
	// AMOUNT appears only in ORDER BY → predicate column, un-indexed
	if indexed, ok := got["AMOUNT"]; !ok || indexed {
		t.Fatalf("AMOUNT (ORDER BY, no index) should be reported un-indexed, got %+v", cols)
	}
}
