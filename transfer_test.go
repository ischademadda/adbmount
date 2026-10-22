package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeLocal(t *testing.T, p string, data []byte, mtime int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Unix(mtime, 0)
	os.Chtimes(p, mt, mt)
}

func runPlan(t *testing.T, dir string, dev *memDevice, srcs []string, dst string, force bool) (*plan, endpoint, endpoint) {
	t.Helper()
	rem, loc := remoteEndpoint(dev), localEndpoint()
	var pl *plan
	var err error
	if dir == "pull" {
		pl, err = buildPlan(rem, loc, srcs, dst, force, false)
	} else {
		pl, err = buildPlan(loc, rem, srcs, dst, force, true)
	}
	if err != nil {
		t.Fatal(err)
	}
	return pl, rem, loc
}

func hasDst(pl *plan, dst string) bool {
	for _, j := range pl.jobs {
		if j.dst == dst {
			return true
		}
	}
	return false
}

func TestPullDirectoryIntoNewAndExistingDest(t *testing.T) {
	dev := newMemDevice()
	dev.put("/sdcard/Music/a.flac", []byte("AAAA"))
	dev.put("/sdcard/Music/Album/b.flac", []byte("BB"))

	// Destination does not exist yet: Music lands as dst/Music.
	tmp := t.TempDir()
	dst := filepath.Join(tmp, "copy")
	pl, rem, loc := runPlan(t, "pull", dev, []string{"/sdcard/Music"}, dst, false)
	if len(pl.jobs) != 2 {
		t.Fatalf("jobs: %+v", pl.jobs)
	}
	made := map[string]bool{}
	for _, d := range pl.dirs {
		if err := mkdirAll(loc, d, made); err != nil {
			t.Fatal(err)
		}
	}
	if failed := execute(pl, 3, pullFile(dev)); len(failed) != 0 {
		t.Fatalf("failed: %v", failed)
	}
	got, _ := os.ReadFile(filepath.Join(dst, "Music", "Album", "b.flac"))
	if string(got) != "BB" {
		t.Fatalf("content: %q", got)
	}
	fi, _ := os.Stat(filepath.Join(dst, "Music", "a.flac"))
	if fi.ModTime().Unix() != 1700000000 {
		t.Errorf("mtime not preserved: %v", fi.ModTime())
	}
	if _, err := os.Stat(filepath.Join(dst, "Music", "a.flac"+partSuffix)); err == nil {
		t.Error("temp part file left behind")
	}

	// Second run: everything is unchanged.
	pl2, _, _ := runPlan(t, "pull", dev, []string{"/sdcard/Music"}, dst, false)
	if len(pl2.jobs) != 0 || pl2.skipped != 2 {
		t.Fatalf("second run should skip all: jobs=%d skipped=%d", len(pl2.jobs), pl2.skipped)
	}
	// -f forces.
	pl3, _, _ := runPlan(t, "pull", dev, []string{"/sdcard/Music"}, dst, true)
	if len(pl3.jobs) != 2 {
		t.Fatalf("force should copy all, got %d", len(pl3.jobs))
	}
	// A changed file on the phone is picked up.
	dev.put("/sdcard/Music/a.flac", []byte("AAAAAA"))
	pl4, _, _ := runPlan(t, "pull", dev, []string{"/sdcard/Music"}, dst, false)
	if len(pl4.jobs) != 1 || pl4.jobs[0].dst != filepath.Join(dst, "Music", "a.flac") {
		t.Fatalf("changed file not detected: %+v", pl4.jobs)
	}
	_ = rem

	// Destination exists: Music lands inside it; a trailing slash copies contents.
	into := t.TempDir()
	pl5, _, _ := runPlan(t, "pull", dev, []string{"/sdcard/Music"}, into, false)
	if !hasDst(pl5, filepath.Join(into, "Music", "a.flac")) {
		t.Errorf("expected into/Music/..., got %+v", pl5.jobs)
	}
	pl6, _, _ := runPlan(t, "pull", dev, []string{"/sdcard/Music/"}, into, false)
	if !hasDst(pl6, filepath.Join(into, "a.flac")) {
		t.Errorf("trailing slash should copy contents, got %+v", pl6.jobs)
	}
}

func TestPullFileTargets(t *testing.T) {
	dev := newMemDevice()
	dev.put("/sdcard/x.txt", []byte("x"))
	dir := t.TempDir()
	pl, _, _ := runPlan(t, "pull", dev, []string{"/sdcard/x.txt"}, dir, false)
	if pl.jobs[0].dst != filepath.Join(dir, "x.txt") {
		t.Errorf("into dir: %s", pl.jobs[0].dst)
	}
	pl, _, _ = runPlan(t, "pull", dev, []string{"/sdcard/x.txt"}, filepath.Join(dir, "renamed.txt"), false)
	if pl.jobs[0].dst != filepath.Join(dir, "renamed.txt") {
		t.Errorf("rename: %s", pl.jobs[0].dst)
	}
	if _, err := buildPlan(remoteEndpoint(dev), localEndpoint(), []string{"/sdcard/missing"}, dir, false, false); err == nil {
		t.Error("missing source must be an error")
	}
}

func TestPushDirectorySkipsJunkAndUnchanged(t *testing.T) {
	dev := newMemDevice()
	src := t.TempDir()
	writeLocal(t, filepath.Join(src, "Album", "01.flac"), []byte("one"), 1700000100)
	writeLocal(t, filepath.Join(src, "Album", "._01.flac"), []byte("junk"), 1700000100)
	writeLocal(t, filepath.Join(src, ".DS_Store"), []byte("junk"), 1700000100)
	writeLocal(t, filepath.Join(src, "top.txt"), []byte("top"), 1700000100)

	// Remote destination does not exist yet: the source lands as /sdcard/Backup/<name>.
	root := "/sdcard/Backup/" + filepath.Base(src)
	pl, rem, _ := runPlan(t, "push", dev, []string{src}, "/sdcard/Backup", false)
	if len(pl.jobs) != 2 {
		t.Fatalf("junk must be skipped, jobs: %+v", pl.jobs)
	}
	made := map[string]bool{}
	for _, d := range pl.dirs {
		if err := mkdirAll(rem, d, made); err != nil {
			t.Fatalf("mkdirAll %s: %v", d, err)
		}
	}
	if failed := execute(pl, 2, pushFile(dev)); len(failed) != 0 {
		t.Fatalf("failed: %v", failed)
	}
	if got, ok := dev.get(root + "/Album/01.flac"); !ok || !bytes.Equal(got, []byte("one")) {
		t.Fatalf("file not uploaded: %q %v", got, ok)
	}
	if dev.has(root+"/Album/._01.flac") || dev.has(root+"/.DS_Store") {
		t.Error("junk was uploaded")
	}
	e, _ := dev.stat(root + "/top.txt")
	if e.Mtime != 1700000100 {
		t.Errorf("mtime not preserved: %d", e.Mtime)
	}

	// Re-run: nothing to do.
	pl2, _, _ := runPlan(t, "push", dev, []string{src}, "/sdcard/Backup", false)
	if len(pl2.jobs) != 0 || pl2.skipped != 2 {
		t.Fatalf("second push should skip all: jobs=%d skipped=%d", len(pl2.jobs), pl2.skipped)
	}
	// Trailing slash on the source: contents go straight into the destination.
	pl3, _, _ := runPlan(t, "push", dev, []string{src + "/"}, "/sdcard/Flat", false)
	if want := "/sdcard/Flat/top.txt"; pl3.jobs[len(pl3.jobs)-1].dst != want {
		t.Errorf("want %s, got %+v", want, pl3.jobs)
	}
}

func TestPushRetriesOnceAndReportsFailure(t *testing.T) {
	dev := newMemDevice()
	f := filepath.Join(t.TempDir(), "a.bin")
	writeLocal(t, f, []byte("payload"), 1700000200)
	pl, _, _ := runPlan(t, "push", dev, []string{f}, "/sdcard/", false)

	dev.failUploads = 1 // first attempt fails, the retry succeeds
	if failed := execute(pl, 1, pushFile(dev)); len(failed) != 0 {
		t.Fatalf("retry should have recovered: %v", failed)
	}
	if got, _ := dev.get("/sdcard/a.bin"); string(got) != "payload" {
		t.Fatalf("got %q", got)
	}

	dev2 := newMemDevice()
	dev2.failUploads = 5
	if failed := execute(pl, 1, pushFile(dev2)); len(failed) != 1 {
		t.Fatalf("persistent failure must be reported, got %v", failed)
	}
}

func TestMkdirAllRefusesFileInTheWay(t *testing.T) {
	dev := newMemDevice()
	dev.put("/sdcard/f", []byte("x"))
	if err := mkdirAll(remoteEndpoint(dev), "/sdcard/f/sub", map[string]bool{}); err == nil {
		t.Error("expected error when a file blocks the path")
	}
}
