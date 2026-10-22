package main

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// endpoint is one side of a copy: the phone or the local disk. Paths on both
// sides are slash-separated absolute paths.
type endpoint struct {
	stat  func(p string) (entry, error)
	list  func(dir string) ([]entry, error)
	mkdir func(p string) error
}

func remoteEndpoint(dev device) endpoint {
	return endpoint{stat: dev.stat, list: dev.list, mkdir: dev.mkdir}
}

func localEntry(fi os.FileInfo) entry {
	mode := uint32(0100644)
	if fi.IsDir() {
		mode = 0040755
	}
	return entry{Name: fi.Name(), Mode: mode, Size: fi.Size(), Mtime: fi.ModTime().Unix()}
}

func localEndpoint() endpoint {
	return endpoint{
		stat: func(p string) (entry, error) {
			fi, err := os.Stat(p)
			if err != nil {
				return entry{}, err
			}
			return localEntry(fi), nil
		},
		list: func(dir string) ([]entry, error) {
			des, err := os.ReadDir(dir)
			if err != nil {
				return nil, err
			}
			var res []entry
			for _, de := range des {
				if fi, err := os.Stat(filepath.Join(dir, de.Name())); err == nil { // follows symlinks, drops broken ones
					res = append(res, localEntry(fi))
				}
			}
			return res, nil
		},
		mkdir: func(p string) error { return os.Mkdir(p, 0o755) },
	}
}

// ---- planning ----------------------------------------------------------------

type job struct {
	src, dst string
	size     int64
	mtime    int64
}

type plan struct {
	dirs         []string // destination directories to ensure, parents before children
	jobs         []job
	skipped      int
	skippedBytes int64
}

func (p *plan) bytes() (n int64) {
	for _, j := range p.jobs {
		n += j.size
	}
	return
}

// sameFile is the "unchanged" test. Android FAT/exFAT stores mtimes with 2 s resolution.
func sameFile(a, b entry) bool {
	d := a.Mtime - b.Mtime
	if d < 0 {
		d = -d
	}
	return a.Size == b.Size && d <= 2
}

type planner struct {
	src, dst endpoint
	force    bool
	skipJunk bool
	p        plan
	dstDirs  map[string]map[string]entry // memoised listings of existing destination dirs
	seenDirs map[string]bool
	dstStat  entry
}

// existing returns the destination directory's children (empty if it does not exist yet).
func (pl *planner) existing(dir string) map[string]entry {
	if m, ok := pl.dstDirs[dir]; ok {
		return m
	}
	m := map[string]entry{}
	if e, err := pl.dst.stat(dir); err == nil && e.IsDir() {
		if es, err := pl.dst.list(dir); err == nil {
			for _, e := range es {
				m[e.Name] = e
			}
		}
	}
	pl.dstDirs[dir] = m
	return m
}

func (pl *planner) addDir(dir string) {
	if !pl.seenDirs[dir] {
		pl.seenDirs[dir] = true
		pl.p.dirs = append(pl.p.dirs, dir)
	}
}

func (pl *planner) addFile(srcPath, dstPath string, e entry) {
	if pl.skipJunk && isJunk(path.Base(srcPath)) {
		return
	}
	dir := path.Dir(dstPath)
	pl.addDir(dir)
	if old, ok := pl.existing(dir)[path.Base(dstPath)]; ok && !old.IsDir() && !pl.force && sameFile(e, old) {
		pl.p.skipped++
		pl.p.skippedBytes += e.Size
		return
	}
	pl.p.jobs = append(pl.p.jobs, job{src: srcPath, dst: dstPath, size: e.Size, mtime: e.Mtime})
}

func (pl *planner) walk(srcDir, dstDir string) error {
	pl.addDir(dstDir)
	es, err := pl.src.list(srcDir)
	if err != nil {
		return fmt.Errorf("list %s: %w", srcDir, err)
	}
	sort.Slice(es, func(i, j int) bool { return es[i].Name < es[j].Name })
	for _, e := range es {
		s, d := path.Join(srcDir, e.Name), path.Join(dstDir, e.Name)
		if e.IsDir() {
			if pl.skipJunk && isJunk(e.Name) {
				continue
			}
			if err := pl.walk(s, d); err != nil {
				return err
			}
			continue
		}
		pl.addFile(s, d, e)
	}
	return nil
}

// buildPlan decides what to copy, with rsync semantics so that running the same
// command twice is a no-op: a directory source lands as DEST/<name>, and a
// trailing slash on the source copies its contents into DEST instead. A file
// source goes inside DEST when DEST is a directory (or ends in a slash), else
// DEST is its new name.
func buildPlan(src, dst endpoint, srcArgs []string, dstArg string, force, skipJunk bool) (*plan, error) {
	pl := &planner{src: src, dst: dst, force: force, skipJunk: skipJunk,
		dstDirs: map[string]map[string]entry{}, seenDirs: map[string]bool{}}
	dstTrailing := strings.HasSuffix(dstArg, "/")
	dstPath := path.Clean(dstArg)
	de, derr := dst.stat(dstPath)
	dstIsDir := derr == nil && de.IsDir()
	multi := len(srcArgs) > 1
	intoDst := dstIsDir || dstTrailing || multi // a source keeps its own name inside dst

	for _, arg := range srcArgs {
		trailing := strings.HasSuffix(arg, "/")
		sp := path.Clean(arg)
		e, err := src.stat(sp)
		if err != nil {
			return nil, fmt.Errorf("%s: not found", arg)
		}
		if !e.IsDir() {
			target := dstPath
			if intoDst {
				target = path.Join(dstPath, path.Base(sp))
			}
			pl.addFile(sp, target, e)
			continue
		}
		root := dstPath
		if !trailing { // rsync semantics: repeatable, always lands as dst/<name>
			root = path.Join(dstPath, path.Base(sp))
		}
		if err := pl.walk(sp, root); err != nil {
			return nil, err
		}
	}
	return &pl.p, nil
}

func mkdirAll(ep endpoint, p string, made map[string]bool) error {
	if made[p] || p == "/" || p == "." {
		return nil
	}
	if e, err := ep.stat(p); err == nil {
		if !e.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", p)
		}
		made[p] = true
		return nil
	}
	if err := mkdirAll(ep, path.Dir(p), made); err != nil {
		return err
	}
	if err := ep.mkdir(p); err != nil {
		return fmt.Errorf("mkdir %s: %w", p, err)
	}
	made[p] = true
	return nil
}

// ---- progress ----------------------------------------------------------------

type progress struct {
	totalBytes, doneBytes atomic.Int64
	totalFiles, doneFiles atomic.Int64
	start                 time.Time
}

type countReader struct {
	r io.Reader
	p *progress
	n int64
}

func (c *countReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	c.p.doneBytes.Add(int64(n))
	return n, err
}

// undo takes back what a failed attempt had counted, so a retry does not overshoot.
func (c *countReader) undo() { c.p.doneBytes.Add(-c.n); c.n = 0 }

func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d >= time.Hour {
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

func (p *progress) line() string {
	done, total := p.doneBytes.Load(), p.totalBytes.Load()
	el := time.Since(p.start)
	rate, eta := 0.0, "-"
	if el > 0 {
		rate = float64(done) / el.Seconds()
	}
	if rate > 0 && total > done {
		eta = humanDuration(time.Duration(float64(total-done) / rate * float64(time.Second)))
	}
	pct := 100
	if total > 0 {
		pct = int(done * 100 / total)
	}
	return fmt.Sprintf("%3d%%  %d/%d files  %s / %s  %s/s  ETA %s",
		pct, p.doneFiles.Load(), p.totalFiles.Load(), humanSize(done), humanSize(total), humanSize(int64(rate)), eta)
}

func stderrIsTTY() bool {
	fi, err := os.Stderr.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// ---- executing a plan --------------------------------------------------------

type copyFunc func(j job, p *progress) error

func execute(pl *plan, workers int, cp copyFunc) (failed []string) {
	prog := &progress{start: time.Now()}
	prog.totalBytes.Store(pl.bytes())
	prog.totalFiles.Store(int64(len(pl.jobs)))
	tty := stderrIsTTY()

	stop := make(chan struct{})
	var ui sync.WaitGroup
	if tty {
		ui.Add(1)
		go func() {
			defer ui.Done()
			t := time.NewTicker(250 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					fmt.Fprintf(os.Stderr, "\r\033[K%s", prog.line())
				}
			}
		}()
	}

	ch := make(chan job)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				if err := cp(j, prog); err != nil {
					mu.Lock()
					failed = append(failed, fmt.Sprintf("%s: %v", j.src, err))
					mu.Unlock()
					continue
				}
				prog.doneFiles.Add(1)
			}
		}()
	}
	for _, j := range pl.jobs {
		ch <- j
	}
	close(ch)
	wg.Wait()
	close(stop)
	ui.Wait()
	if tty {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
	el := time.Since(prog.start)
	fmt.Printf("Copied %d of %d files (%s) in %s, skipped %d unchanged, %d failed\n",
		prog.doneFiles.Load(), len(pl.jobs), humanSize(prog.doneBytes.Load()), humanDuration(el), pl.skipped, len(failed))
	return failed
}

const partSuffix = ".adbmount-part"

func pullFile(dev device) copyFunc {
	return func(j job, p *progress) (err error) {
		for attempt := 0; attempt < 2; attempt++ {
			if err = pullOnce(dev, j, p); err == nil {
				return nil
			}
		}
		return err
	}
}

func pullOnce(dev device, j job, p *progress) error {
	rc, err := dev.open(j.src, 0, -1)
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp := j.dst + partSuffix
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	cr := &countReader{r: rc, p: p}
	n, err := io.Copy(f, cr)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != j.size {
		err = fmt.Errorf("short read: got %d of %d bytes", n, j.size)
	}
	if err != nil {
		cr.undo()
		os.Remove(tmp)
		return err
	}
	mt := time.Unix(j.mtime, 0)
	os.Chtimes(tmp, mt, mt)
	return os.Rename(tmp, j.dst)
}

func pushFile(dev device) copyFunc {
	return func(j job, p *progress) (err error) {
		for attempt := 0; attempt < 2; attempt++ {
			if err = pushOnce(dev, j, p); err == nil {
				return nil
			}
		}
		return err
	}
}

func pushOnce(dev device, j job, p *progress) error {
	f, err := os.Open(j.src)
	if err != nil {
		return err
	}
	defer f.Close()
	cr := &countReader{r: f, p: p}
	err = dev.upload(cr, j.dst, j.mtime)
	if err == nil {
		// adb gives no checksum, so at least confirm the size arrived intact.
		var e entry
		if e, err = dev.stat(j.dst); err == nil && e.Size != j.size {
			err = fmt.Errorf("size on phone is %d, expected %d", e.Size, j.size)
		}
	}
	if err != nil {
		cr.undo()
	}
	return err
}

// ---- commands ----------------------------------------------------------------

// keepSlash resolves sd:/int: shortcuts but remembers a trailing slash (or a bare "sd:").
func keepSlash(a *adb, p string) (string, error) {
	r, err := resolveRemote(a, p)
	if err != nil {
		return "", err
	}
	if (strings.HasSuffix(p, "/") || strings.HasSuffix(p, ":")) && !strings.HasSuffix(r, "/") {
		r += "/"
	}
	return r, nil
}

func localArg(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(p, "/") && !strings.HasSuffix(abs, "/") {
		abs += "/"
	}
	return abs, nil
}

func runPull(args []string) error { return runTransfer("pull", args) }
func runPush(args []string) error { return runTransfer("push", args) }

func runTransfer(dir string, args []string) error {
	fl := flagSetFor(dir)
	dry := fl.Bool("n", false, "dry run: show what would be copied")
	fl.BoolVar(dry, "dry-run", false, "same as -n")
	force := fl.Bool("f", false, "copy even when size and modification time match")
	workers := fl.Int("j", 4, "parallel file transfers")
	junk := fl.Bool("junk", false, "push: also copy macOS ._* / .DS_Store files")
	serial := serialFlag(fl)
	pos, err := parseInterspersed(fl, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		if dir == "pull" {
			return fail("usage: adbmount pull [-n] [-f] [-j N] REMOTE... LOCAL   (e.g. adbmount pull sd:Music ~/Music)")
		}
		return fail("usage: adbmount push [-n] [-f] [-j N] LOCAL... REMOTE   (e.g. adbmount push ~/Music sd:)")
	}
	if *workers < 1 {
		*workers = 1
	}
	a, _, err := connectDevice(*serial)
	if err != nil {
		return err
	}

	var src, dst endpoint
	var srcArgs []string
	var dstArg string
	var cp copyFunc
	srcPos, last := pos[:len(pos)-1], pos[len(pos)-1]
	rem := remoteEndpoint(a)
	if dir == "pull" {
		for _, p := range srcPos {
			r, err := keepSlash(a, p)
			if err != nil {
				return err
			}
			srcArgs = append(srcArgs, r)
		}
		if dstArg, err = localArg(last); err != nil {
			return err
		}
		src, dst, cp = rem, localEndpoint(), pullFile(a)
	} else {
		for _, p := range srcPos {
			l, err := localArg(p)
			if err != nil {
				return err
			}
			srcArgs = append(srcArgs, l)
		}
		if dstArg, err = keepSlash(a, last); err != nil {
			return err
		}
		src, dst, cp = localEndpoint(), rem, pushFile(a)
	}

	pl, err := buildPlan(src, dst, srcArgs, dstArg, *force, dir == "push" && !*junk)
	if err != nil {
		return err
	}
	if *dry {
		for _, j := range pl.jobs {
			fmt.Printf("%s → %s  (%s)\n", j.src, j.dst, humanSize(j.size))
		}
		fmt.Printf("Would copy %d files (%s), skip %d unchanged\n", len(pl.jobs), humanSize(pl.bytes()), pl.skipped)
		return nil
	}
	made := map[string]bool{}
	for _, d := range pl.dirs {
		if err := mkdirAll(dst, d, made); err != nil {
			return err
		}
	}
	if failed := execute(pl, *workers, cp); len(failed) > 0 {
		for _, f := range failed {
			fmt.Fprintln(os.Stderr, "  failed:", f)
		}
		return fail("%d file(s) failed", len(failed))
	}
	return nil
}

func runLs(args []string) error {
	fl := flagSetFor("ls")
	long := fl.Bool("l", false, "long format: size and modification time")
	serial := serialFlag(fl)
	pos, err := parseInterspersed(fl, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return fail("usage: adbmount ls [-l] [PATH]")
	}
	arg := "int:"
	if len(pos) == 1 {
		arg = pos[0]
	}
	a, _, err := connectDevice(*serial)
	if err != nil {
		return err
	}
	p, err := resolveRemote(a, arg)
	if err != nil {
		return err
	}
	e, err := a.stat(p)
	if err != nil {
		return fail("%s: not found", p)
	}
	entries := []entry{e}
	if e.IsDir() {
		if entries, err = a.list(p); err != nil {
			return err
		}
	} else {
		entries[0].Name = path.Base(p)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return entries[i].Name < entries[j].Name
	})
	for _, e := range entries {
		name := e.Name
		if e.IsDir() {
			name += "/"
		}
		if *long {
			fmt.Printf("%10s  %s  %s\n", humanSize(e.Size), time.Unix(e.Mtime, 0).Format("2006-01-02 15:04"), name)
		} else {
			fmt.Println(name)
		}
	}
	return nil
}
