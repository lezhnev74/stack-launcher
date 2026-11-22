package ledger

import "testing"

func TestStateLock(t *testing.T) {
	dir := t.TempDir()
	first, err := LockState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := LockState(dir); err == nil {
		_ = second.Close()
		t.Fatal("second lock succeeded")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := LockState(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = second.Close()
}
