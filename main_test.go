package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanStaleTempRemovesOnlyDeadOwners(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"111-abc", "222-def", "notapid-x", "333"} {
		if err := os.MkdirAll(filepath.Join(base, name, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(base, "444-file"), []byte("x"), 0o644) // a file, not a dir

	alive := func(pid int) bool { return pid == 222 }
	if n := cleanStaleTemp(base, alive); n != 1 {
		t.Fatalf("removed %d, want 1 (only 111-abc)", n)
	}
	for name, want := range map[string]bool{"111-abc": false, "222-def": true, "notapid-x": true, "333": true, "444-file": true} {
		_, err := os.Stat(filepath.Join(base, name))
		if (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", name, err == nil, want)
		}
	}
}

func TestCleanStaleTempMissingBaseIsFine(t *testing.T) {
	if n := cleanStaleTemp(filepath.Join(t.TempDir(), "nope"), func(int) bool { return false }); n != 0 {
		t.Fatalf("removed %d from a missing dir", n)
	}
}

func TestPidAlive(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Fatal("own pid must be alive")
	}
	if pidAlive(1<<22 + 12345) {
		t.Fatal("absurd pid must not be alive")
	}
}
