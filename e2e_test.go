package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// e2e drives the real binary against testdata/fakeadb, which serves a local
// directory as the phone. FUSE mounting is not covered (CI cannot mount).
type e2e struct {
	t    *testing.T
	bin  string
	root string // fake phone: sdcard/ and storage/
	env  []string
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("fakeadb shims use BSD stat")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	wd, _ := os.Getwd()
	bin := filepath.Join(t.TempDir(), "adbmount")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	root := t.TempDir()
	for _, d := range []string{"sdcard/Music/Album", "storage/ABCD-1234", "storage/emulated"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	env := append(os.Environ(),
		"FAKE_ADB_ROOT="+root,
		"PATH="+filepath.Join(wd, "testdata", "fakeadb")+":"+os.Getenv("PATH"),
		"ANDROID_SERIAL=",
		"ADBMOUNT_DIR="+filepath.Join(t.TempDir(), "mnt"),
		"ADBMOUNT_STATE_DIR="+t.TempDir(),
		"ADBMOUNT_CONFIG="+filepath.Join(t.TempDir(), "config.json"),
	)
	return &e2e{t: t, bin: bin, root: root, env: env}
}

func (e *e2e) run(args ...string) (string, error) {
	c := exec.Command(e.bin, args...)
	c.Env = e.env
	out, err := c.CombinedOutput()
	return string(out), err
}

func (e *e2e) must(args ...string) string {
	e.t.Helper()
	out, err := e.run(args...)
	if err != nil {
		e.t.Fatalf("adbmount %v: %v\n%s", args, err, out)
	}
	return out
}

func TestE2EDevicesLsDoctor(t *testing.T) {
	e := newE2E(t)
	os.WriteFile(filepath.Join(e.root, "sdcard", "Music", "a.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(e.root, "storage", "ABCD-1234", "card.txt"), []byte("sd"), 0o644)

	if out := e.must("devices"); !strings.Contains(out, "FAKE123") || !strings.Contains(out, "SD card") {
		t.Errorf("devices:\n%s", out)
	}
	if out := e.must("ls", "int:Music"); !strings.Contains(out, "Album/") || !strings.Contains(out, "a.txt") {
		t.Errorf("ls:\n%s", out)
	}
	if out := e.must("ls", "sd:"); strings.TrimSpace(out) != "card.txt" {
		t.Errorf("ls sd::\n%s", out)
	}
	if fuseTInstalled() {
		if out := e.must("doctor"); !strings.Contains(out, "All good") {
			t.Errorf("doctor:\n%s", out)
		}
	}
	if _, err := e.run("ls", "int:nope"); err == nil {
		t.Error("ls of a missing path must fail")
	}
}

func TestE2EPushPullRoundTrip(t *testing.T) {
	e := newE2E(t)
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "pics"), 0o755)
	os.WriteFile(filepath.Join(src, "pics", "1.bin"), []byte("one"), 0o644)
	os.WriteFile(filepath.Join(src, "pics", ".DS_Store"), []byte("junk"), 0o644)
	os.WriteFile(filepath.Join(src, "top.txt"), []byte("top"), 0o644)

	if out := e.must("push", "-n", src+"/", "int:Backup"); !strings.Contains(out, "Would copy 2 files") {
		t.Errorf("dry run:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(e.root, "sdcard", "Backup")); err == nil {
		t.Fatal("dry run created the destination")
	}
	if out := e.must("push", src+"/", "int:Backup"); !strings.Contains(out, "Copied 2 of 2") {
		t.Errorf("push:\n%s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(e.root, "sdcard", "Backup", "pics", "1.bin")); string(b) != "one" {
		t.Errorf("pushed content = %q", b)
	}
	if out := e.must("push", src+"/", "int:Backup"); !strings.Contains(out, "skipped 2 unchanged") {
		t.Errorf("second push should skip:\n%s", out)
	}

	back := t.TempDir()
	e.must("pull", "int:Backup/", back)
	if b, _ := os.ReadFile(filepath.Join(back, "top.txt")); string(b) != "top" {
		t.Errorf("pulled content = %q", b)
	}
}

func TestE2EWifiEnable(t *testing.T) {
	e := newE2E(t)
	out := e.must("wifi", "enable")
	if !strings.Contains(out, "192.168.7.42:5555") {
		t.Errorf("wifi enable should report the phone address:\n%s", out)
	}
}

func TestE2EUsageErrors(t *testing.T) {
	e := newE2E(t)
	if _, err := e.run("bogus"); err == nil {
		t.Error("unknown command must fail")
	}
	if out, err := e.run("pull", "only-one-arg"); err == nil || !strings.Contains(out, "usage") {
		t.Errorf("pull with one arg: %v\n%s", err, out)
	}
}

func TestE2ETwoTransportsOfOnePhoneAreNotAmbiguous(t *testing.T) {
	e := newE2E(t)
	os.WriteFile(filepath.Join(e.root, "sdcard", "f.txt"), []byte("x"), 0o644)
	e.env = append(e.env, "FAKE_ADB_SERIALS=USB1,10.0.0.5:5555", "FAKE_ADB_SAME_PHONE=1")
	if out := e.must("ls", "int:"); !strings.Contains(out, "f.txt") {
		t.Errorf("one phone on two transports should just work:\n%s", out)
	}
}

func TestE2ETwoDifferentPhonesAreAmbiguous(t *testing.T) {
	e := newE2E(t)
	e.env = append(e.env, "FAKE_ADB_SERIALS=PHONE_A,PHONE_B")
	out, err := e.run("ls", "int:")
	if err == nil || !strings.Contains(out, "several devices") || !strings.Contains(out, "PHONE_B") {
		t.Errorf("want an ambiguity error listing both, got err=%v\n%s", err, out)
	}
	if out, err := e.run("ls", "-s", "PHONE_B", "int:"); err != nil {
		t.Errorf("-s should resolve the ambiguity: %v\n%s", err, out)
	}
}
