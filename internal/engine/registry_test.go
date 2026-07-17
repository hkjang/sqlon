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

func TestDefaultRegistryDeclaresAllSupportedEngines(t *testing.T) {
	r := NewDefaultRegistry()
	want := []string{"mariadb", "mysql", "oracle", "postgres"}
	got := r.Names()
	if len(got) != len(want) {
		t.Fatalf("engines = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("engines = %v, want %v", got, want)
		}
	}
	oracle, ok := r.Get("oracle")
	if !ok || !oracle.Capabilities.Multitenant || !oracle.Capabilities.RAC {
		t.Fatalf("oracle capabilities = %+v", oracle.Capabilities)
	}
}
