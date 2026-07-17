package mysql

import "testing"

func TestWaitClassNormalization(t *testing.T) {
	for event, want := range map[string]string{"wait/io/file/sql/binlog": "io", "wait/lock/table/sql/handler": "lock", "wait/synch/mutex/x": "sync", "unknown": "other"} {
		if got := waitClass(event); got != want {
			t.Fatalf("%s: got %s want %s", event, got, want)
		}
	}
}
