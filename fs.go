package main

import (
	"io"
	"log"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/winfsp/cgofuse/fuse"
	"golang.org/x/text/unicode/norm"
)

var trace = os.Getenv("ADBFS_TRACE") != ""

// tr logs the duration of a filesystem operation when ADBFS_TRACE is set.
func tr(op, p string, extra ...interface{}) func() {
	if !trace {
		return func() {}
	}
	t := time.Now()
	return func() { log.Printf("%-8s %6.0fms %s %v", op, float64(time.Since(t).Microseconds())/1000, p, extra) }
}

const (
	cacheTTL    = 10 * time.Second
	seekAhead   = 2 << 20 // reads further ahead than this bypass the sequential download
	readAhead   = 8 << 20 // how far the sequential download may run past the last read
	copyBufSize = 256 << 10
	noFh        = ^uint64(0)
)

type dirCache struct {
	at      time.Time
	entries map[string]entry
}

// download is the local copy of one remote file. Read-only users stream it in
// the background; writers wait for the whole file, modify the local copy and
// push it back on flush.
type download struct {
	remote string // absolute path on the device
	rel    string // path relative to the mount root (real on-device spelling)
	tmp    *os.File
	rc     io.ReadCloser // background download stream (nil for local-only copies)

	mu       sync.Mutex
	cond     *sync.Cond
	size     int64
	done     int64
	maxReq   int64 // highest offset requested so far; paces the download
	closed   bool
	finished bool
	err      error
	refs     int // guarded by AdbFS.mu

	dirty   bool
	ver     uint64 // bumped on every modification
	discard bool   // never pushed (macOS junk, deleted files)
	junk    bool   // exists only in memory, not on the device
	mtime   int64  // mtime to apply after the next push, 0 = leave as is
	pushMu  sync.Mutex
}

type handle struct {
	d     *download
	write bool
}

type AdbFS struct {
	fuse.FileSystemBase
	dev      device
	root     string // remote directory exposed as the mount root
	tmpDir   string
	ro       bool
	skipJunk bool
	uid      uint32
	gid      uint32
	listMu   sync.Mutex // serialises directory listings
	mu       sync.Mutex // guards the maps below and download.refs
	dirs     map[string]*dirCache
	handles  map[uint64]*handle
	byPath   map[string]*download
	nextFh   uint64
	fsAt     time.Time
	fsInfo   [4]uint64
	statfsMu sync.Mutex
}

func newAdbFS(dev device, root, tmpDir string, ro, skipJunk bool) *AdbFS {
	return &AdbFS{
		dev:      dev,
		root:     strings.TrimRight(root, "/"),
		tmpDir:   tmpDir,
		ro:       ro,
		skipJunk: skipJunk,
		uid:      uint32(os.Getuid()),
		gid:      uint32(os.Getgid()),
		dirs:     map[string]*dirCache{},
		handles:  map[uint64]*handle{},
		byPath:   map[string]*download{},
		nextFh:   1,
	}
}

func (f *AdbFS) remote(p string) string {
	if p == "/" {
		return f.root
	}
	return f.root + p
}

// isJunk reports macOS metadata files that only pollute an exFAT card.
func isJunk(name string) bool {
	switch name {
	case ".DS_Store", ".Spotlight-V100", ".Trashes", ".fseventsd", ".TemporaryItems", ".localized":
		return true
	}
	return strings.HasPrefix(name, "._")
}

// ---- listing and lookup ----------------------------------------------------

func (f *AdbFS) invalidate(dir string) {
	f.mu.Lock()
	delete(f.dirs, dir)
	f.mu.Unlock()
}

func (f *AdbFS) invalidateAll() {
	f.mu.Lock()
	f.dirs = map[string]*dirCache{}
	f.mu.Unlock()
}

// listDir returns the (cached) children of a directory keyed by name.
func (f *AdbFS) listDir(p string) (map[string]entry, error) {
	f.mu.Lock()
	dc := f.dirs[p]
	f.mu.Unlock()
	if dc != nil && time.Since(dc.at) < cacheTTL {
		return dc.entries, nil
	}

	f.listMu.Lock()
	defer f.listMu.Unlock()
	// another goroutine may have refreshed it while we waited
	f.mu.Lock()
	dc = f.dirs[p]
	f.mu.Unlock()
	if dc != nil && time.Since(dc.at) < cacheTTL {
		return dc.entries, nil
	}

	list, err := f.dev.list(f.remote(p))
	if err != nil {
		return nil, err
	}
	m := make(map[string]entry, len(list))
	for _, e := range list {
		m[e.Name] = e
	}
	f.mu.Lock()
	f.dirs[p] = &dirCache{at: time.Now(), entries: m}
	f.mu.Unlock()
	return m, nil
}

// neverIndex is served as an empty file at the root so Spotlight skips the volume.
const neverIndex = ".metadata_never_index"

// lookup finds p and returns its entry plus the real on-device path. Files
// being written are reported with their in-memory size.
func (f *AdbFS) lookup(p string) (entry, string, bool) {
	if p == "/" {
		return entry{Name: "/", Mode: 0040755}, p, true
	}
	if p == "/"+neverIndex {
		return entry{Name: neverIndex, Mode: 0100444}, p, true
	}

	f.mu.Lock()
	d := f.byPath[p]
	f.mu.Unlock()
	if d != nil && d.junk {
		return entry{Name: path.Base(p), Mode: 0100644, Size: d.getSize(), Mtime: time.Now().Unix()}, p, true
	}

	e, real, ok := f.lookupRemote(p)
	if ok && d != nil && d.isDirty() {
		e.Size = d.getSize()
	}
	return e, real, ok
}

// lookupRemote resolves p against device listings. macOS may hand us names in
// NFC or NFD while the phone stores one form, so when an exact match fails we
// walk the path comparing NFC-normalised names.
func (f *AdbFS) lookupRemote(p string) (entry, string, bool) {
	m, err := f.listDir(path.Dir(p))
	if err != nil {
		log.Printf("list %s: %v", path.Dir(p), err)
		return entry{}, "", false
	}
	if e, ok := m[path.Base(p)]; ok {
		return e, p, true
	}
	if !needsNorm(p) {
		return entry{}, "", false
	}
	return f.resolveNorm(p)
}

func needsNorm(p string) bool {
	for i := 0; i < len(p); i++ {
		if p[i] >= 0x80 {
			return true
		}
	}
	return false
}

func (f *AdbFS) resolveNorm(p string) (entry, string, bool) {
	cur := "/"
	var e entry
	for _, comp := range strings.Split(strings.Trim(p, "/"), "/") {
		m, err := f.listDir(cur)
		if err != nil {
			return entry{}, "", false
		}
		real, found := m[comp]
		name := comp
		if !found {
			want := norm.NFC.String(comp)
			for k, v := range m {
				if norm.NFC.String(k) == want {
					real, name, found = v, k, true
					break
				}
			}
		}
		if !found {
			return entry{}, "", false
		}
		e, cur = real, path.Join(cur, name)
	}
	return e, cur, true
}

func (f *AdbFS) fill(st *fuse.Stat_t, e entry) {
	*st = fuse.Stat_t{}
	st.Uid, st.Gid = f.uid, f.gid
	mt := fuse.Timespec{Sec: e.Mtime}
	st.Mtim, st.Atim, st.Ctim, st.Birthtim = mt, mt, mt, mt
	dirMode, fileMode := uint32(0755), uint32(0644)
	if f.ro || e.Name == neverIndex {
		dirMode, fileMode = 0555, 0444
	}
	if e.IsDir() {
		st.Mode = fuse.S_IFDIR | dirMode
		st.Nlink = 2
	} else {
		st.Mode = fuse.S_IFREG | fileMode
		st.Nlink = 1
		st.Size = e.Size
		st.Blksize = 4096
		st.Blocks = (e.Size + 511) / 512
	}
}

func (f *AdbFS) Getattr(p string, st *fuse.Stat_t, fh uint64) int {
	defer tr("getattr", p)()
	e, _, ok := f.lookup(p)
	if !ok {
		return -fuse.ENOENT
	}
	f.fill(st, e)
	return 0
}

func (f *AdbFS) Readdir(p string, fill func(string, *fuse.Stat_t, int64) bool, ofst int64, fh uint64) int {
	defer tr("readdir", p, ofst)()
	if _, real, ok := f.lookup(p); ok {
		p = real
	}
	m, err := f.listDir(p)
	if err != nil {
		log.Printf("readdir %s: %v", p, err)
		return -fuse.EIO
	}
	if p == "/" {
		m = withNeverIndex(m)
	}
	// Files that exist only in memory (just created) are not in the listing yet.
	f.mu.Lock()
	for fp, d := range f.byPath {
		if d.junk && path.Dir(fp) == p {
			m = withEntry(m, entry{Name: path.Base(fp), Mode: 0100644, Size: d.getSize(), Mtime: time.Now().Unix()})
		}
	}
	f.mu.Unlock()

	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)

	// "." and ".." are synthesised by the FUSE-T NFS layer; emitting them
	// here makes it swallow the first real entry.
	// The kernel reads directories in pages: resume at ofst and hand out
	// 1-based offsets so the next call knows where to continue.
	for i := int(ofst); i < len(names); i++ {
		st := new(fuse.Stat_t)
		f.fill(st, m[names[i]])
		if !fill(names[i], st, int64(i+1)) {
			break
		}
	}
	return 0
}

func withEntry(m map[string]entry, e entry) map[string]entry {
	c := make(map[string]entry, len(m)+1)
	for k, v := range m {
		c[k] = v
	}
	c[e.Name] = e
	return c
}

func withNeverIndex(m map[string]entry) map[string]entry {
	return withEntry(m, entry{Name: neverIndex, Mode: 0100444})
}

// ---- download state --------------------------------------------------------

func (d *download) getSize() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.size
}

func (d *download) isDirty() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dirty
}

// waitComplete blocks until the whole remote file is in the local copy.
func (d *download) waitComplete() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.maxReq = d.size
	d.cond.Broadcast()
	for !d.finished {
		d.cond.Wait()
	}
	return d.err
}

func (d *download) setSize(size int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.tmp.Truncate(size); err != nil {
		return err
	}
	d.size, d.done = size, size
	d.dirty = true
	d.ver++
	return nil
}

func (f *AdbFS) newLocal(rel string, discard bool) (*download, error) {
	tmp, err := os.CreateTemp(f.tmpDir, "dl-*")
	if err != nil {
		return nil, err
	}
	d := &download{remote: f.remote(rel), rel: rel, tmp: tmp, finished: true, discard: discard, junk: discard}
	d.cond = sync.NewCond(&d.mu)
	return d, nil
}

func (f *AdbFS) startDownload(rel string, e entry) (*download, error) {
	d, err := f.newLocal(rel, false)
	if err != nil {
		return nil, err
	}
	d.size = e.Size
	if e.Size == 0 {
		return d, nil
	}
	d.finished = false
	rc, err := f.dev.open(d.remote, 0, -1)
	if err != nil {
		d.tmp.Close()
		os.Remove(d.tmp.Name())
		return nil, err
	}
	d.rc = rc
	go d.pump(rc)
	return d, nil
}

func (d *download) pump(r io.ReadCloser) {
	defer r.Close()
	buf := make([]byte, copyBufSize)
	for {
		d.mu.Lock()
		for d.done-d.maxReq >= readAhead && !d.closed {
			d.cond.Wait() // stall the adb pipe until a reader catches up
		}
		stop := d.closed
		d.mu.Unlock()
		if stop {
			return
		}
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := d.tmp.WriteAt(buf[:n], d.done); werr != nil {
				err = werr
			} else {
				d.mu.Lock()
				d.done += int64(n)
				d.cond.Broadcast()
				d.mu.Unlock()
			}
		}
		if err != nil {
			d.mu.Lock()
			if err != io.EOF {
				d.err = err
			} else if d.done < d.size {
				d.err = io.ErrUnexpectedEOF
			}
			d.finished = true
			d.cond.Broadcast()
			d.mu.Unlock()
			return
		}
	}
}

// ---- open / read / write ---------------------------------------------------

// acquire returns the shared local copy of a file with its refcount raised.
func (f *AdbFS) acquire(p, real string, e entry, write bool) (*download, error) {
	f.mu.Lock()
	d := f.byPath[p]
	if d == nil || (!d.discard && !d.isDirty() && d.getSize() != e.Size) {
		var err error
		if d, err = f.startDownload(real, e); err != nil {
			f.mu.Unlock()
			return nil, err
		}
		f.byPath[p] = d
	}
	d.refs++
	f.mu.Unlock()

	if write {
		if err := d.waitComplete(); err != nil {
			f.release(p, d)
			return nil, err
		}
	}
	return d, nil
}

// release drops one reference and frees the local copy after the last one.
func (f *AdbFS) release(p string, d *download) {
	f.mu.Lock()
	d.refs--
	last := d.refs <= 0
	if last && f.byPath[p] == d {
		delete(f.byPath, p)
	}
	f.mu.Unlock()
	if !last {
		return
	}
	d.mu.Lock()
	d.closed = true
	d.cond.Broadcast()
	d.mu.Unlock()
	if d.rc != nil {
		d.rc.Close() // aborts a transfer that is still running
	}
	d.tmp.Close()
	os.Remove(d.tmp.Name())
}

func (f *AdbFS) newHandle(d *download, write bool) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	fh := f.nextFh
	f.nextFh++
	f.handles[fh] = &handle{d: d, write: write}
	return fh
}

func (f *AdbFS) handle(fh uint64) *handle {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handles[fh]
}

func (f *AdbFS) Open(p string, flags int) (int, uint64) {
	defer tr("open", p)()
	write := flags&(fuse.O_WRONLY|fuse.O_RDWR) != 0
	if write && f.ro {
		return -fuse.EROFS, noFh
	}
	e, real, ok := f.lookup(p)
	if !ok {
		return -fuse.ENOENT, noFh
	}
	if e.IsDir() {
		return -fuse.EISDIR, noFh
	}
	d, err := f.acquire(p, real, e, write)
	if err != nil {
		log.Printf("open %s: %v", p, err)
		return -fuse.EIO, noFh
	}
	if write && flags&fuse.O_TRUNC != 0 {
		if err := d.setSize(0); err != nil {
			f.release(p, d)
			return -fuse.EIO, noFh
		}
	}
	return 0, f.newHandle(d, write)
}

func (f *AdbFS) Create(p string, flags int, mode uint32) (int, uint64) {
	if f.ro {
		return -fuse.EROFS, noFh
	}
	if _, _, exists := f.lookup(p); exists {
		if flags&fuse.O_EXCL != 0 {
			return -fuse.EEXIST, noFh
		}
		return f.Open(p, flags|fuse.O_WRONLY)
	}
	pe, realDir, ok := f.lookup(path.Dir(p))
	if !ok {
		return -fuse.ENOENT, noFh
	}
	if !pe.IsDir() {
		return -fuse.ENOTDIR, noFh
	}
	rel := path.Join(realDir, path.Base(p))
	junk := f.skipJunk && isJunk(path.Base(p))
	if !junk {
		// Make the file exist on the device right away so later lookups see it.
		if err := f.dev.create(f.remote(rel)); err != nil {
			log.Printf("create %s: %v", p, err)
			return -fuse.EIO, noFh
		}
		f.invalidate(realDir)
	}
	d, err := f.newLocal(rel, junk)
	if err != nil {
		return -fuse.EIO, noFh
	}
	f.mu.Lock()
	d.refs++
	f.byPath[p] = d
	f.mu.Unlock()
	return 0, f.newHandle(d, true)
}

func (f *AdbFS) Read(p string, buff []byte, ofst int64, fh uint64) int {
	defer tr("read", p, ofst, len(buff))()
	h := f.handle(fh)
	if h == nil {
		return -fuse.EBADF
	}
	d := h.d
	size := d.getSize()
	if ofst >= size {
		return 0
	}
	want := int64(len(buff))
	if ofst+want > size {
		want = size - ofst
	}

	d.mu.Lock()
	// Far seek past the sequential download: fetch just that range.
	if ofst > d.done+seekAhead && !d.finished {
		d.mu.Unlock()
		return f.readRange(d, buff[:want], ofst)
	}
	if ofst+want > d.maxReq {
		d.maxReq = ofst + want
		d.cond.Broadcast()
	}
	for d.done < ofst+want && !d.finished {
		d.cond.Wait()
	}
	err, avail := d.err, d.done
	d.mu.Unlock()

	if avail <= ofst {
		if err != nil {
			log.Printf("read %s: %v", p, err)
			return -fuse.EIO
		}
		return 0
	}
	if ofst+want > avail {
		want = avail - ofst
	}
	n, rerr := d.tmp.ReadAt(buff[:want], ofst)
	if rerr != nil && rerr != io.EOF {
		return -fuse.EIO
	}
	return n
}

func (f *AdbFS) readRange(d *download, buf []byte, ofst int64) int {
	rc, err := f.dev.open(d.remote, ofst, int64(len(buf)))
	if err != nil {
		return -fuse.EIO
	}
	defer rc.Close()
	n, _ := io.ReadFull(rc, buf)
	return n
}

func (f *AdbFS) Write(p string, buff []byte, ofst int64, fh uint64) int {
	h := f.handle(fh)
	if h == nil || !h.write {
		return -fuse.EBADF
	}
	d := h.d
	n, err := d.tmp.WriteAt(buff, ofst)
	if err != nil {
		log.Printf("write %s: %v", p, err)
		return -fuse.EIO
	}
	d.mu.Lock()
	if end := ofst + int64(n); end > d.size {
		d.size, d.done = end, end
	}
	d.dirty = true
	d.ver++
	d.mu.Unlock()
	return n
}

func (f *AdbFS) Truncate(p string, size int64, fh uint64) int {
	if f.ro {
		return -fuse.EROFS
	}
	if h := f.handle(fh); h != nil && h.write {
		if h.d.setSize(size) != nil {
			return -fuse.EIO
		}
		return 0
	}
	e, real, ok := f.lookup(p)
	if !ok {
		return -fuse.ENOENT
	}
	if e.IsDir() {
		return -fuse.EISDIR
	}
	d, err := f.acquire(p, real, e, true)
	if err != nil {
		return -fuse.EIO
	}
	defer f.release(p, d)
	if d.setSize(size) != nil {
		return -fuse.EIO
	}
	return f.push(d)
}

// push uploads a modified local copy to the device.
func (f *AdbFS) push(d *download) int {
	d.pushMu.Lock()
	defer d.pushMu.Unlock()
	d.mu.Lock()
	if !d.dirty || d.discard {
		d.mu.Unlock()
		return 0
	}
	ver, size, mtime := d.ver, d.size, d.mtime
	d.mu.Unlock()

	if err := f.dev.upload(io.NewSectionReader(d.tmp, 0, size), d.remote, mtime); err != nil {
		log.Printf("push: %v", err)
		return -fuse.EIO
	}
	d.mu.Lock()
	if d.ver == ver {
		d.dirty = false
	}
	d.mu.Unlock()
	f.invalidate(path.Dir(d.rel))
	return 0
}

func (f *AdbFS) Flush(p string, fh uint64) int {
	if h := f.handle(fh); h != nil && h.write {
		return f.push(h.d)
	}
	return 0
}

func (f *AdbFS) Fsync(p string, datasync bool, fh uint64) int {
	return f.Flush(p, fh)
}

func (f *AdbFS) Release(p string, fh uint64) int {
	defer tr("release", p)()
	f.mu.Lock()
	h := f.handles[fh]
	delete(f.handles, fh)
	f.mu.Unlock()
	if h == nil {
		return 0
	}
	if h.write {
		f.push(h.d) // no-op when Flush already did it
	}
	f.release(p, h.d)
	return 0
}

// ---- namespace operations --------------------------------------------------

func (f *AdbFS) Unlink(p string) int {
	if f.ro {
		return -fuse.EROFS
	}
	f.mu.Lock()
	d := f.byPath[p]
	delete(f.byPath, p)
	f.mu.Unlock()
	if d != nil {
		d.mu.Lock()
		d.dirty, d.discard = false, true // never push a deleted file
		d.mu.Unlock()
		if d.junk {
			return 0 // memory-only file, nothing on the device
		}
	}
	e, real, ok := f.lookupRemote(p)
	if !ok {
		return -fuse.ENOENT
	}
	if e.IsDir() {
		return -fuse.EISDIR
	}
	if err := f.dev.remove(f.remote(real)); err != nil {
		log.Printf("unlink %s: %v", p, err)
		return -fuse.EIO
	}
	f.invalidate(path.Dir(real))
	return 0
}

func (f *AdbFS) Mkdir(p string, mode uint32) int {
	if f.ro {
		return -fuse.EROFS
	}
	if _, _, exists := f.lookup(p); exists {
		return -fuse.EEXIST
	}
	pe, realDir, ok := f.lookup(path.Dir(p))
	if !ok {
		return -fuse.ENOENT
	}
	if !pe.IsDir() {
		return -fuse.ENOTDIR
	}
	if err := f.dev.mkdir(f.remote(path.Join(realDir, path.Base(p)))); err != nil {
		log.Printf("mkdir %s: %v", p, err)
		return -fuse.EIO
	}
	f.invalidate(realDir)
	return 0
}

func (f *AdbFS) Rmdir(p string) int {
	if f.ro {
		return -fuse.EROFS
	}
	e, real, ok := f.lookup(p)
	if !ok {
		return -fuse.ENOENT
	}
	if !e.IsDir() {
		return -fuse.ENOTDIR
	}
	if err := f.dev.rmdir(f.remote(real)); err != nil {
		return -fuse.ENOTEMPTY
	}
	f.invalidate(path.Dir(real))
	f.invalidate(real)
	return 0
}

func (f *AdbFS) Rename(oldp, newp string) int {
	if f.ro {
		return -fuse.EROFS
	}
	f.mu.Lock()
	d := f.byPath[oldp]
	f.mu.Unlock()

	// A memory-only junk file just changes its name.
	if d != nil && d.junk {
		f.mu.Lock()
		delete(f.byPath, oldp)
		f.byPath[newp] = d
		f.mu.Unlock()
		return 0
	}

	oe, oreal, ok := f.lookup(oldp)
	if !ok {
		return -fuse.ENOENT
	}
	pe, nrealDir, ok := f.lookup(path.Dir(newp))
	if !ok {
		return -fuse.ENOENT
	}
	if !pe.IsDir() {
		return -fuse.ENOTDIR
	}
	nreal := path.Join(nrealDir, path.Base(newp))

	if d != nil {
		f.push(d) // flush pending data before the file moves
	}
	if de, _, exists := f.lookup(newp); exists {
		switch {
		case de.IsDir() && !oe.IsDir():
			return -fuse.EISDIR
		case !de.IsDir() && oe.IsDir():
			return -fuse.ENOTDIR
		case de.IsDir():
			if err := f.dev.rmdir(f.remote(nreal)); err != nil {
				return -fuse.ENOTEMPTY
			}
		}
	}
	if err := f.dev.rename(f.remote(oreal), f.remote(nreal)); err != nil {
		log.Printf("rename %s -> %s: %v", oldp, newp, err)
		return -fuse.EIO
	}
	if d != nil {
		f.mu.Lock()
		delete(f.byPath, oldp)
		f.byPath[newp] = d
		f.mu.Unlock()
		d.mu.Lock()
		d.rel, d.remote = nreal, f.remote(nreal)
		d.mu.Unlock()
	}
	if oe.IsDir() {
		f.invalidateAll()
	} else {
		f.invalidate(path.Dir(oreal))
		f.invalidate(nrealDir)
	}
	return 0
}

func (f *AdbFS) Utimens(p string, tmsp []fuse.Timespec) int {
	if f.ro {
		return -fuse.EROFS
	}
	mtime := time.Now().Unix()
	if len(tmsp) == 2 && tmsp[1].Sec > 0 {
		mtime = tmsp[1].Sec
	}
	f.mu.Lock()
	d := f.byPath[p]
	f.mu.Unlock()
	if d != nil {
		d.mu.Lock()
		discard, dirty := d.junk, d.dirty
		if dirty {
			d.mtime = mtime // applied right after the pending upload
		}
		d.mu.Unlock()
		if discard || dirty {
			return 0
		}
	}
	_, real, ok := f.lookupRemote(p)
	if !ok {
		return -fuse.ENOENT
	}
	if err := f.dev.touch(f.remote(real), mtime); err != nil {
		return -fuse.EIO
	}
	f.invalidate(path.Dir(real))
	return 0
}

// Permissions and ownership are meaningless on the card's filesystem.
func (f *AdbFS) Chmod(p string, mode uint32) int            { return 0 }
func (f *AdbFS) Chown(p string, uid uint32, gid uint32) int { return 0 }

func (f *AdbFS) Statfs(p string, st *fuse.Statfs_t) int {
	f.statfsMu.Lock()
	if time.Since(f.fsAt) > 30*time.Second {
		if bs, bl, bf, ba, err := f.dev.statfs(f.root); err == nil {
			f.fsInfo = [4]uint64{bs, bl, bf, ba}
			f.fsAt = time.Now()
		}
	}
	info := f.fsInfo
	f.statfsMu.Unlock()
	if info[0] == 0 {
		info = [4]uint64{4096, 1 << 20, 1 << 19, 1 << 19}
	}
	st.Bsize, st.Frsize = info[0], info[0]
	st.Blocks, st.Bfree, st.Bavail = info[1], info[2], info[3]
	st.Namemax = 255
	return 0
}
