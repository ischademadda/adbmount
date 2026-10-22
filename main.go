package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/winfsp/cgofuse/fuse"
)

// tempBase is where per-process download/upload copies live. A crash (kill -9,
// power loss) cannot run deferred cleanup, so leftovers are swept on the next start.
func tempBase() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "adbfs")
	}
	return filepath.Join(os.TempDir(), "adbfs")
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// cleanStaleTemp removes "<pid>-*" directories whose owner process is gone and
// returns how many were removed. Directories not named that way are left alone.
func cleanStaleTemp(base string, alive func(int) bool) int {
	entries, err := os.ReadDir(base)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pidStr, _, found := strings.Cut(e.Name(), "-")
		pid, err := strconv.Atoi(pidStr)
		if !found || err != nil || pid <= 0 || alive(pid) {
			continue
		}
		if os.RemoveAll(filepath.Join(base, e.Name())) == nil {
			removed++
		}
	}
	return removed
}

func main() {
	var (
		mountpoint = flag.String("mount", os.ExpandEnv("$HOME/mnt/android"), "local mount point (created if missing)")
		root       = flag.String("root", "/sdcard", "remote directory to expose")
		serial     = flag.String("serial", "", "adb device serial (default device if empty)")
		adbBin     = flag.String("adb", "adb", "path to adb")
		volname    = flag.String("volname", "Android", "volume name shown in Finder")
		ro         = flag.Bool("ro", false, "mount read-only")
		allowJunk  = flag.Bool("allow-junk", false, "store macOS ._* / .DS_Store files on the device instead of discarding them")
	)
	flag.Parse()

	path, err := exec.LookPath(*adbBin)
	if err != nil {
		log.Fatalf("adb not found: %v", err)
	}
	if err := os.MkdirAll(*mountpoint, 0o755); err != nil {
		log.Fatal(err)
	}

	base := tempBase()
	if err := os.MkdirAll(base, 0o755); err != nil {
		log.Fatal(err)
	}
	if n := cleanStaleTemp(base, pidAlive); n > 0 {
		log.Printf("removed %d stale temp dir(s) from earlier crashed runs", n)
	}
	tmp, err := os.MkdirTemp(base, fmt.Sprintf("%d-", os.Getpid()))
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	a := &adb{bin: path, serial: *serial}
	if _, err := a.stat(*root); err != nil {
		log.Fatalf("cannot stat %s on device: %v", *root, err)
	}

	fs := newAdbFS(a, *root, tmp, *ro, !*allowJunk)
	host := fuse.NewFileSystemHost(fs)
	fmt.Printf("mounting %s at %s\n", *root, *mountpoint)
	if !host.Mount(*mountpoint, []string{"-o", "volname=" + *volname}) {
		log.Fatal("mount failed")
	}
}
