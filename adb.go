package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// entry is one remote file as reported by toybox `stat`.
type entry struct {
	Name  string
	Mode  uint32
	Size  int64
	Mtime int64
}

func (e entry) IsDir() bool { return e.Mode&0170000 == 0040000 }

// device is everything the filesystem needs from the phone. The real
// implementation talks to adb; tests use an in-memory fake.
type device interface {
	list(dir string) ([]entry, error)
	stat(p string) (entry, error)
	// open streams the byte range [off, off+n); n < 0 means until EOF.
	// Closing the reader aborts the transfer.
	open(p string, off, n int64) (io.ReadCloser, error)
	// upload atomically replaces remote with the contents of r; a non-zero
	// mtime is applied afterwards.
	upload(r io.Reader, remote string, mtime int64) error
	create(remote string) error
	mkdir(remote string) error
	rmdir(remote string) error
	remove(remote string) error
	rename(oldp, newp string) error
	touch(remote string, mtime int64) error
	statfs(p string) (bsize, blocks, bfree, bavail uint64, err error)
}

// adb implements device on top of the adb command line client.
type adb struct {
	bin    string
	serial string // optional, empty = default device
}

var _ device = (*adb)(nil)

func (a *adb) cmd(args ...string) *exec.Cmd {
	if a.serial != "" {
		args = append([]string{"-s", a.serial}, args...)
	}
	return exec.Command(a.bin, args...)
}

// shQuote quotes s for the Android shell (single quotes).
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ---- resilience -------------------------------------------------------------

// transient reports adb errors that usually mean "the cable/daemon hiccuped,
// try again" rather than "your command is wrong".
func transient(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"device offline", "no devices", "device not found", "error: closed",
		"protocol fault", "device still authorizing", "connection reset", "daemon not running",
		"cannot connect to daemon", "unauthorized"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

const retryAttempts = 6

// retry runs fn, retrying with growing pauses while the failure looks transient.
func retry(fn func() error) error {
	var err error
	for attempt := 0; attempt < retryAttempts; attempt++ {
		if err = fn(); !transient(err) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	return err
}

// ---- command execution ------------------------------------------------------

// run executes a shell command via exec-out (clean binary-safe stdout) and
// returns stdout. exec-out does NOT report the remote exit status.
func (a *adb) run(script string) ([]byte, error) {
	var out []byte
	err := retry(func() error {
		c := a.cmd("exec-out", script)
		var o, e bytes.Buffer
		c.Stdout, c.Stderr = &o, &e
		if err := c.Run(); err != nil {
			return fmt.Errorf("adb: %w: %s", err, strings.TrimSpace(e.String()))
		}
		out = o.Bytes()
		return nil
	})
	return out, err
}

// shell runs a command via `adb shell`, which (unlike exec-out) reports the
// remote exit status. Use it for anything that mutates the device.
func (a *adb) shell(script string) error {
	return retry(func() error {
		c := a.cmd("shell", script)
		var out bytes.Buffer
		c.Stdout, c.Stderr = &out, &out
		if err := c.Run(); err != nil {
			return fmt.Errorf("adb: %w: %s", err, strings.TrimSpace(out.String()))
		}
		return nil
	})
}

const statFmt = `%f|%s|%Y|%n`

func parseStat(line string) (entry, bool) {
	parts := strings.SplitN(line, "|", 4)
	if len(parts) != 4 {
		return entry{}, false
	}
	mode, err1 := strconv.ParseUint(parts[0], 16, 32)
	size, err2 := strconv.ParseInt(parts[1], 10, 64)
	mtime, err3 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return entry{}, false
	}
	return entry{Name: parts[3], Mode: uint32(mode), Size: size, Mtime: mtime}, true
}

// list returns the children of a remote directory with a single adb call.
func (a *adb) list(dir string) ([]entry, error) {
	script := fmt.Sprintf(
		`find %s -mindepth 1 -maxdepth 1 -exec stat -L -c %s {} + 2>/dev/null; true`,
		shQuote(strings.TrimRight(dir, "/")+"/"), shQuote(statFmt)) // trailing slash: follow a symlinked root like /sdcard
	out, err := a.run(script)
	if err != nil {
		return nil, err
	}
	var res []entry
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		e, ok := parseStat(sc.Text())
		if !ok {
			continue
		}
		// %n is the full path as given to stat; keep only the base name.
		if i := strings.LastIndexByte(e.Name, '/'); i >= 0 {
			e.Name = e.Name[i+1:]
		}
		res = append(res, e)
	}
	return res, nil
}

// stat returns info for a single remote path.
func (a *adb) stat(p string) (entry, error) {
	out, err := a.run(fmt.Sprintf(`stat -L -c %s %s`, shQuote(statFmt), shQuote(p)))
	if err != nil {
		return entry{}, err
	}
	e, ok := parseStat(strings.TrimSpace(string(out)))
	if !ok {
		return entry{}, fmt.Errorf("not found: %s", p)
	}
	return e, nil
}

// ---- streaming reads --------------------------------------------------------

// procReader exposes a running adb process's stdout; Close kills the process.
type procReader struct {
	io.Reader
	cmd  *exec.Cmd
	once sync.Once
}

func (p *procReader) Close() error {
	p.once.Do(func() {
		if p.cmd.Process != nil {
			p.cmd.Process.Kill()
		}
		p.cmd.Wait()
	})
	return nil
}

const rangeBlock = 4096

// rangeScript builds the dd command for [off, off+n). dd seeks instead of
// scanning (tail -c +N reads the file from the start: ~1s on a 300MB file). It
// works in whole blocks, so the first `lead` bytes of its output must be skipped.
func rangeScript(p string, off, n int64) (script string, lead int64) {
	lead = off % rangeBlock
	blocks := (lead + n + rangeBlock - 1) / rangeBlock
	return fmt.Sprintf("dd if=%s bs=%d skip=%d count=%d 2>/dev/null",
		shQuote(p), rangeBlock, off/rangeBlock, blocks), lead
}

func (a *adb) open(p string, off, n int64) (io.ReadCloser, error) {
	script, lead := "cat "+shQuote(p), int64(0)
	if off > 0 || n >= 0 {
		script, lead = rangeScript(p, off, n)
	}
	var pr *procReader
	var out io.Reader
	err := retry(func() error {
		c := a.cmd("exec-out", script)
		stdout, err := c.StdoutPipe()
		if err != nil {
			return err
		}
		if err := c.Start(); err != nil {
			return fmt.Errorf("adb: %w", err)
		}
		pr, out = &procReader{cmd: c}, stdout
		return nil
	})
	if err != nil {
		return nil, err
	}
	if lead > 0 {
		if _, err := io.CopyN(io.Discard, out, lead); err != nil {
			pr.Close()
			return nil, err
		}
	}
	if n >= 0 {
		out = io.LimitReader(out, n)
	}
	pr.Reader = out
	return pr, nil
}

// ---- mutations --------------------------------------------------------------

// uploadTmp is where upload stages remote before the atomic rename.
func uploadTmp(remote string) string {
	return path.Dir(remote) + "/." + path.Base(remote) + ".adbfs-tmp"
}

func (a *adb) upload(r io.Reader, remote string, mtime int64) error {
	tmp := uploadTmp(remote)
	script := fmt.Sprintf("cat > %s && mv -f %s %s", shQuote(tmp), shQuote(tmp), shQuote(remote))
	if mtime > 0 {
		script += fmt.Sprintf(" && touch -d @%d %s", mtime, shQuote(remote))
	}
	// A reader can only be consumed once, so uploads are not retried here: the
	// filesystem keeps the file dirty and the next flush uploads it again.
	c := a.cmd("shell", script)
	c.Stdin = r
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	if err := c.Run(); err != nil {
		a.shell("rm -f " + shQuote(tmp))
		return fmt.Errorf("upload %s: %w: %s", remote, err, strings.TrimSpace(out.String()))
	}
	return nil
}

func (a *adb) create(remote string) error { return a.shell(": > " + shQuote(remote)) }
func (a *adb) mkdir(remote string) error  { return a.shell("mkdir " + shQuote(remote)) }
func (a *adb) rmdir(remote string) error  { return a.shell("rmdir " + shQuote(remote)) }
func (a *adb) remove(remote string) error { return a.shell("rm -f " + shQuote(remote)) }
func (a *adb) rename(oldp, newp string) error {
	return a.shell("mv -f " + shQuote(oldp) + " " + shQuote(newp))
}
func (a *adb) touch(remote string, mtime int64) error {
	return a.shell("touch -d @" + strconv.FormatInt(mtime, 10) + " " + shQuote(remote))
}

// statfs returns fragment size, total, free and available blocks of the volume.
func (a *adb) statfs(p string) (bsize, blocks, bfree, bavail uint64, err error) {
	out, err := a.run(fmt.Sprintf(`stat -f -c '%%S %%b %%f %%a' %s`, shQuote(p)))
	if err != nil {
		return
	}
	_, err = fmt.Sscanf(strings.TrimSpace(string(out)), "%d %d %d %d", &bsize, &blocks, &bfree, &bavail)
	return
}
