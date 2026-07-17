package fleet

import (
	"context"
	"sqlon/internal/dbconn"
	"testing"
)

func TestHealthReportsUnavailableProfileAsFailed(t *testing.T) {
	dir := t.TempDir()
	if err := dbconn.SaveProfiles(dir, []dbconn.Profile{{ID: "missing", Type: "postgres", ConnectString: "127.0.0.1:1/db", Username: "u", PasswordRef: "plain:p"}}); err != nil {
		t.Fatal(err)
	}
	h := New(dbconn.NewManager(dir)).Health(context.Background())
	if len(h.Data) != 1 || h.Data[0].Status != StatusFailed {
		t.Fatalf("unexpected health: %+v", h)
	}
}
