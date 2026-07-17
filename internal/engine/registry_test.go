package engine

import "testing"

func TestRegistryRejectsDuplicateAndNormalizesNames(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(Adapter{Name: "Oracle", Capabilities: CapabilitySet{Multitenant: true}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("oracle"); !ok {
		t.Fatal("normalized adapter not found")
	}
	if err := r.Register(Adapter{Name: "ORACLE"}); err == nil {
		t.Fatal("duplicate adapter accepted")
	}
}
