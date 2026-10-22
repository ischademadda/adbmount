package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// mountState is what `up` remembers about a daemon it started.
type mountState struct {
	Name   string `json:"name"`
	Pid    int    `json:"pid"`
	Mount  string `json:"mount"`
	Root   string `json:"root"`
	Serial string `json:"serial"`
	RO     bool   `json:"ro"`
}

var defaultMounts = []string{"sdcard", "android"}

func stateDir() string {
	return filepath.Join(tempBase(), "run")
}

func mountBase() string {
	if d := os.Getenv("ADBMOUNT_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "mnt")
}

func mountPointOf(name string) string { return filepath.Join(mountBase(), name) }
func statePath(name string) string    { return filepath.Join(stateDir(), name+".json") }
func logPath(name string) string      { return filepath.Join(stateDir(), name+".log") }

func loadState(name string) (mountState, bool) {
	b, err := os.ReadFile(statePath(name))
	if err != nil {
		return mountState{}, false
	}
	var s mountState
	if json.Unmarshal(b, &s) != nil {
		return mountState{}, false
	}
	return s, true
}

func saveState(s mountState) error {
	b, _ := json.Marshal(s)
	return os.WriteFile(statePath(s.Name), b, 0o644)
}

// knownMounts lists the names we manage: the defaults plus anything with a state file.
func knownMounts() []string {
	seen := map[string]bool{}
	var names []string
	for _, n := range defaultMounts {
		seen[n] = true
		names = append(names, n)
	}
	files, _ := filepath.Glob(filepath.Join(stateDir(), "*.json"))
	for _, f := range files {
		n := strings.TrimSuffix(filepath.Base(f), ".json")
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	return names
}

// parseMounts extracts mount points from `mount` output ("src on /path (type, opts)").
func parseMounts(out string) map[string]bool {
	res := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		i := strings.LastIndex(line, " (")
		if i < 0 {
			continue
		}
		left := line[:i]
		if j := strings.Index(left, " on "); j >= 0 {
			res[left[j+4:]] = true
		}
	}
	return res
}

func isMounted(mnt string) bool {
	out, err := exec.Command("mount").Output()
	if err != nil {
		return false
	}
	// macOS reports the resolved path (/private/var vs /var), so compare both.
	mounts := parseMounts(string(out))
	if mounts[mnt] {
		return true
	}
	if r, err := filepath.EvalSymlinks(mnt); err == nil {
		return mounts[r]
	}
	return false
}

// isOurDaemon checks that pid is alive and really is the serve process for mnt,
// so a recycled pid is never killed.
func isOurDaemon(pid int, mnt string) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	out, err := exec.Command("ps", "-p", fmt.Sprint(pid), "-o", "command=").Output()
	cmdline := string(out)
	return err == nil && strings.Contains(cmdline, " serve ") && strings.Contains(cmdline, mnt)
}

func tailFile(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func startMount(a *adb, name, root, volname string, ro bool) error {
	mnt := mountPointOf(name)
	if isMounted(mnt) {
		fmt.Printf("  %s: already mounted (%s)\n", name, mnt)
		return nil
	}
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"serve", "-root", root, "-mount", mnt, "-volname", volname, "-serial", a.serial, "-adb", a.bin}
	if ro {
		args = append(args, "-ro")
	}
	logf, err := os.Create(logPath(name))
	if err != nil {
		return err
	}
	defer logf.Close()
	c := exec.Command(exe, args...)
	c.Stdout, c.Stderr = logf, logf
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survive the terminal that started us
	if err := c.Start(); err != nil {
		return err
	}
	exited := make(chan struct{})
	go func() { c.Wait(); close(exited) }()
	saveState(mountState{Name: name, Pid: c.Process.Pid, Mount: mnt, Root: root, Serial: a.serial, RO: ro})

	for i := 0; i < 40; i++ {
		if isMounted(mnt) {
			mode := "read-write"
			if ro {
				mode = "read-only"
			}
			fmt.Printf("  %s: %s → %s (%s)\n", name, root, mnt, mode)
			return nil
		}
		select {
		case <-exited:
			os.Remove(statePath(name))
			return fmt.Errorf("%s: adbmount serve exited during startup:\n%s", name, tailFile(logPath(name), 5))
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s: not mounted after 20 s (log: %s)", name, logPath(name))
}

func stopMount(name string) {
	mnt := mountPointOf(name)
	s, haveState := loadState(name)
	if haveState && s.Mount != "" {
		mnt = s.Mount
	}
	was := isMounted(mnt)
	if was {
		if exec.Command("diskutil", "unmount", "force", mnt).Run() != nil {
			exec.Command("umount", "-f", mnt).Run()
		}
	}
	if haveState && isOurDaemon(s.Pid, mnt) {
		syscall.Kill(s.Pid, syscall.SIGTERM)
		for i := 0; i < 20 && syscall.Kill(s.Pid, 0) == nil; i++ {
			time.Sleep(250 * time.Millisecond)
		}
		if syscall.Kill(s.Pid, 0) == nil {
			syscall.Kill(s.Pid, syscall.SIGKILL)
		}
	}
	os.Remove(statePath(name))
	switch {
	case isMounted(mnt):
		fmt.Printf("  %s: could NOT unmount (busy? close files on it and retry)\n", name)
	case was:
		fmt.Printf("  %s: unmounted\n", name)
	default:
		fmt.Printf("  %s: not mounted\n", name)
	}
}

// ---- commands ----------------------------------------------------------------

func runUp(args []string) error {
	fl := flagSetFor("up")
	ro := fl.Bool("ro", false, "mount read-only")
	only := fl.String("only", "", "mount just `android` (internal storage) or `sdcard`")
	open := fl.Bool("open", false, "open the mounted volumes in Finder")
	serial := serialFlag(fl)
	if _, err := parseInterspersed(fl, args); err != nil {
		return err
	}
	if *only != "" && *only != "android" && *only != "sdcard" {
		return fail("--only must be android or sdcard")
	}
	a, d, err := connectDevice(*serial)
	if err != nil {
		return err
	}
	fmt.Printf("Mounting from %s (%s)%s:\n", d.Serial, d.transport(), map[bool]string{true: ", read-only"}[*ro])
	if *only == "" || *only == "android" {
		if err := startMount(a, "android", "/sdcard", "Android", *ro); err != nil {
			return err
		}
	}
	if *only == "" || *only == "sdcard" {
		if sd := a.sdCardRoot(); sd != "" {
			if err := startMount(a, "sdcard", sd, "SDCard", *ro); err != nil {
				return err
			}
		} else {
			fmt.Println("  sdcard: no SD card found, skipping")
		}
	}
	if *open {
		for _, n := range defaultMounts {
			if isMounted(mountPointOf(n)) {
				exec.Command("open", mountPointOf(n)).Run()
			}
		}
	}
	return nil
}

func runDown(args []string) error {
	fl := flagSetFor("down")
	names, err := parseInterspersed(fl, args)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		names = knownMounts()
	}
	fmt.Println("Unmounting:")
	for _, n := range names {
		stopMount(n)
	}
	return nil
}

func runStatus(args []string) error {
	if _, err := parseInterspersed(flagSetFor("status"), args); err != nil {
		return err
	}
	for _, n := range knownMounts() {
		mnt := mountPointOf(n)
		s, ok := loadState(n)
		if ok && s.Mount != "" {
			mnt = s.Mount
		}
		if !isMounted(mnt) {
			fmt.Printf("%-8s not mounted\n", n)
			continue
		}
		mode, extra := "?", ""
		if ok {
			mode = map[bool]string{true: "ro", false: "rw"}[s.RO]
			extra = fmt.Sprintf("  device=%s pid=%d", s.Serial, s.Pid)
		}
		fmt.Printf("%-8s mounted (%s)  %s%s\n", n, mode, mnt, extra)
	}
	return nil
}

func runLog(args []string) error {
	fl := flagSetFor("log")
	follow := fl.Bool("f", false, "follow the log")
	pos, err := parseInterspersed(fl, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fail("usage: adbmount log [-f] android|sdcard")
	}
	p := logPath(pos[0])
	if _, err := os.Stat(p); err != nil {
		return fail("no log for %q", pos[0])
	}
	if !*follow {
		fmt.Println(tailFile(p, 50))
		return nil
	}
	c := exec.Command("tail", "-n", "50", "-f", p)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}
