package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/winfsp/cgofuse/fuse"
	"golang.org/x/text/unicode/norm"
)

// ---- in-memory phone ---------------------------------------------------------

type memNode struct {
	dir   bool
	data  []byte
	mtime int64
}

type memDevice struct {
	mu          sync.Mutex
	nodes       map[string]*memNode
	opens       []string // "path@off+n" for every open call
	readBytes   int64    // bytes handed out through open() readers
	failUploads int      // next N uploads fail
	uploads     int
}

var _ device = (*memDevice)(nil)

func newMemDevice() *memDevice {
	return &memDevice{nodes: map[string]*memNode{"/sdcard": {dir: true}}}
}

// put creates a file (and its parent directories).
func (m *memDevice) put(p string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for d := path.Dir(p); d != "/" && d != "."; d = path.Dir(d) {
		if _, ok := m.nodes[d]; !ok {
			m.nodes[d] = &memNode{dir: true}
		}
	}
	m.nodes[p] = &memNode{data: append([]byte(nil), data...), mtime: 1700000000}
}

func (m *memDevice) get(p string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[p]
	if !ok || n.dir {
		return nil, false
	}
	return append([]byte(nil), n.data...), true
}

func (m *memDevice) has(p string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.nodes[p]
	return ok
}

func (m *memDevice) entryOf(p string, n *memNode) entry {
	e := entry{Name: path.Base(p), Size: int64(len(n.data)), Mtime: n.mtime, Mode: 0100644}
	if n.dir {
		e.Mode, e.Size = 0040755, 0
	}
	return e
}

func (m *memDevice) list(dir string) ([]entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	dir = strings.TrimRight(dir, "/")
	var res []entry
	for p, n := range m.nodes {
		if path.Dir(p) == dir && p != dir {
			res = append(res, m.entryOf(p, n))
		}
	}
	return res, nil
}

func (m *memDevice) stat(p string) (entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[p]
	if !ok {
		return entry{}, errors.New("not found")
	}
	return m.entryOf(p, n), nil
}

type countingReader struct {
	r   io.Reader
	dev *memDevice
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.dev.mu.Lock()
	c.dev.readBytes += int64(n)
	c.dev.mu.Unlock()
	return n, err
}
func (c *countingReader) Close() error { return nil }

func (m *memDevice) open(p string, off, n int64) (io.ReadCloser, error) {
	m.mu.Lock()
	node, ok := m.nodes[p]
	m.opens = append(m.opens, fmt.Sprintf("%s@%d+%d", p, off, n))
	m.mu.Unlock()
	if !ok || node.dir {
		return nil, errors.New("not a file")
	}
	data := node.data
	if off > int64(len(data)) {
		off = int64(len(data))
	}
	data = data[off:]
	if n >= 0 && n < int64(len(data)) {
		data = data[:n]
	}
	return &countingReader{r: bytes.NewReader(data), dev: m}, nil
}

func (m *memDevice) upload(r io.Reader, remote string, mtime int64) error {
	m.mu.Lock()
	if m.failUploads > 0 {
		m.failUploads--
		m.mu.Unlock()
		return errors.New("simulated upload failure")
	}
	m.mu.Unlock()
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.nodes[path.Dir(remote)]; !ok || !p.dir {
		return errors.New("no such directory")
	}
	if mtime == 0 {
		mtime = time.Now().Unix()
	}
	m.nodes[remote] = &memNode{data: data, mtime: mtime}
	m.uploads++
	return nil
}

func (m *memDevice) create(remote string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.nodes[path.Dir(remote)]; !ok || !p.dir {
		return errors.New("no such directory")
	}
	m.nodes[remote] = &memNode{mtime: time.Now().Unix()}
	return nil
}

func (m *memDevice) mkdir(remote string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nodes[remote]; ok {
		return errors.New("exists")
	}
	if p, ok := m.nodes[path.Dir(remote)]; !ok || !p.dir {
		return errors.New("no such directory")
	}
	m.nodes[remote] = &memNode{dir: true}
	return nil
}

func (m *memDevice) rmdir(remote string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[remote]
	if !ok || !n.dir {
		return errors.New("not a directory")
	}
	for p := range m.nodes {
		if path.Dir(p) == remote {
			return errors.New("directory not empty")
		}
	}
	delete(m.nodes, remote)
	return nil
}

func (m *memDevice) remove(remote string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n, ok := m.nodes[remote]; ok && n.dir {
		return errors.New("is a directory")
	}
	delete(m.nodes, remote)
	return nil
}

func (m *memDevice) rename(oldp, newp string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nodes[oldp]; !ok {
		return errors.New("no such file")
	}
	moved := map[string]*memNode{}
	for p, n := range m.nodes {
		if p == oldp || strings.HasPrefix(p, oldp+"/") {
			moved[newp+strings.TrimPrefix(p, oldp)] = n
			delete(m.nodes, p)
		}
	}
	for p, n := range moved {
		m.nodes[p] = n
	}
	return nil
}

func (m *memDevice) touch(remote string, mtime int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[remote]
	if !ok {
		return errors.New("no such file")
	}
	n.mtime = mtime
	return nil
}

func (m *memDevice) statfs(p string) (uint64, uint64, uint64, uint64, error) {
	return 4096, 1000, 400, 300, nil
}

// ---- helpers -----------------------------------------------------------------

func newTestFS(t *testing.T, ro, skipJunk bool) (*AdbFS, *memDevice) {
	t.Helper()
	dev := newMemDevice()
	return newAdbFS(dev, "/sdcard", t.TempDir(), ro, skipJunk), dev
}

func ok(t *testing.T, what string, rc int) {
	t.Helper()
	if rc != 0 {
		t.Fatalf("%s: rc=%d, want 0", what, rc)
	}
}

func wantRC(t *testing.T, what string, rc, want int) {
	t.Helper()
	if rc != want {
		t.Fatalf("%s: rc=%d, want %d", what, rc, want)
	}
}

func openRW(t *testing.T, fs *AdbFS, p string, flags int) uint64 {
	t.Helper()
	rc, fh := fs.Open(p, flags)
	ok(t, "open "+p, rc)
	return fh
}

func readAll(t *testing.T, fs *AdbFS, p string) []byte {
	t.Helper()
	rc, fh := fs.Open(p, fuse.O_RDONLY)
	ok(t, "open "+p, rc)
	defer fs.Release(p, fh)
	var st fuse.Stat_t
	ok(t, "getattr", fs.Getattr(p, &st, noFh))
	var out []byte
	buf := make([]byte, 32768)
	for off := int64(0); off < st.Size; {
		n := fs.Read(p, buf, off, fh)
		if n < 0 {
			t.Fatalf("read %s@%d: rc=%d", p, off, n)
		}
		if n == 0 {
			break
		}
		out = append(out, buf[:n]...)
		off += int64(n)
	}
	return out
}

func writeFile(t *testing.T, fs *AdbFS, p string, data []byte) {
	t.Helper()
	rc, fh := fs.Create(p, fuse.O_WRONLY|fuse.O_CREAT, 0644)
	ok(t, "create "+p, rc)
	if len(data) > 0 {
		if n := fs.Write(p, data, 0, fh); n != len(data) {
			t.Fatalf("write short: %d", n)
		}
	}
	ok(t, "flush", fs.Flush(p, fh))
	ok(t, "release", fs.Release(p, fh))
}

func readdirAll(t *testing.T, fs *AdbFS, p string, pageSize int) []string {
	t.Helper()
	var names []string
	ofst := int64(0)
	for guard := 0; guard < 1000; guard++ {
		var page []string
		var last int64
		fill := func(name string, st *fuse.Stat_t, o int64) bool {
			if len(page) >= pageSize {
				return false
			}
			page = append(page, name)
			last = o
			return true
		}
		ok(t, "readdir", fs.Readdir(p, fill, ofst, noFh))
		if len(page) == 0 {
			return names
		}
		names = append(names, page...)
		ofst = last
	}
	t.Fatal("readdir did not terminate")
	return nil
}

// ---- tests -------------------------------------------------------------------

func TestReaddirPagingReturnsEverythingOnce(t *testing.T) {
	fs, dev := newTestFS(t, false, true)
	var want []string
	for i := 0; i < 11; i++ {
		n := fmt.Sprintf("file%02d.flac", i)
		dev.put("/sdcard/"+n, []byte("x"))
		want = append(want, n)
	}
	dev.put("/sdcard/album/a.flac", []byte("x"))
	want = append(want, "album", neverIndex)
	sort.Strings(want)

	got := readdirAll(t, fs, "/", 3)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("paged listing differs:\n got %v\nwant %v", got, want)
	}
}

func TestCreateWriteFlushUploadsAndGetattrSeesDirtySize(t *testing.T) {
	fs, dev := newTestFS(t, false, true)
	rc, fh := fs.Create("/new.txt", fuse.O_WRONLY|fuse.O_CREAT, 0644)
	ok(t, "create", rc)
	if !dev.has("/sdcard/new.txt") {
		t.Fatal("create must make the file exist on the device immediately")
	}
	fs.Write("/new.txt", []byte("hello"), 0, fh)
	fs.Write("/new.txt", []byte(" world"), 5, fh)

	var st fuse.Stat_t
	ok(t, "getattr while dirty", fs.Getattr("/new.txt", &st, noFh))
	if st.Size != 11 {
		t.Fatalf("size while dirty = %d, want 11", st.Size)
	}
	if data, _ := dev.get("/sdcard/new.txt"); len(data) != 0 {
		t.Fatalf("data reached the device before flush: %q", data)
	}
	ok(t, "flush", fs.Flush("/new.txt", fh))
	if data, _ := dev.get("/sdcard/new.txt"); string(data) != "hello world" {
		t.Fatalf("device has %q", data)
	}
	ok(t, "release", fs.Release("/new.txt", fh))
}

func TestRewriteInPlacePreservesRest(t *testing.T) {
	fs, dev := newTestFS(t, false, true)
	orig := bytes.Repeat([]byte("0123456789"), 10)
	dev.put("/sdcard/a.bin", orig)
	fh := openRW(t, fs, "/a.bin", fuse.O_RDWR)
	fs.Write("/a.bin", []byte("XYZ"), 10, fh)
	ok(t, "release", fs.Release("/a.bin", fh))

	want := append([]byte(nil), orig...)
	copy(want[10:], "XYZ")
	if got, _ := dev.get("/sdcard/a.bin"); !bytes.Equal(got, want) {
		t.Fatalf("in-place write corrupted file: %q", got)
	}
}

func TestTruncateByPathAndByHandle(t *testing.T) {
	fs, dev := newTestFS(t, false, true)
	dev.put("/sdcard/a.bin", bytes.Repeat([]byte("a"), 100))

	ok(t, "truncate shrink", fs.Truncate("/a.bin", 10, noFh))
	if got, _ := dev.get("/sdcard/a.bin"); len(got) != 10 {
		t.Fatalf("shrunk to %d, want 10", len(got))
	}
	ok(t, "truncate grow", fs.Truncate("/a.bin", 20, noFh))
	got, _ := dev.get("/sdcard/a.bin")
	if len(got) != 20 || !bytes.Equal(got[10:], make([]byte, 10)) {
		t.Fatalf("grow must zero-fill, got %q", got)
	}

	fh := openRW(t, fs, "/a.bin", fuse.O_WRONLY)
	ok(t, "ftruncate", fs.Truncate("/a.bin", 5, fh))
	ok(t, "release", fs.Release("/a.bin", fh))
	if got, _ := dev.get("/sdcard/a.bin"); len(got) != 5 {
		t.Fatalf("ftruncate left %d bytes", len(got))
	}
}

func TestOpenWithTruncEmptiesFileEvenWithoutWrites(t *testing.T) {
	fs, dev := newTestFS(t, false, true)
	dev.put("/sdcard/a.bin", []byte("old content"))
	fh := openRW(t, fs, "/a.bin", fuse.O_WRONLY|fuse.O_TRUNC)
	ok(t, "release", fs.Release("/a.bin", fh))
	if got, _ := dev.get("/sdcard/a.bin"); len(got) != 0 {
		t.Fatalf("O_TRUNC left %q", got)
	}
}

func TestRenameFileDirAndOverwrite(t *testing.T) {
	fs, dev := newTestFS(t, false, true)
	dev.put("/sdcard/dir/x.flac", []byte("x"))
	dev.put("/sdcard/y.flac", []byte("y"))
	dev.put("/sdcard/z.flac", []byte("z"))

	ok(t, "rename file", fs.Rename("/dir/x.flac", "/dir/x2.flac"))
	if dev.has("/sdcard/dir/x.flac") || !dev.has("/sdcard/dir/x2.flac") {
		t.Fatal("file rename not applied")
	}
	ok(t, "rename dir", fs.Rename("/dir", "/moved"))
	if !dev.has("/sdcard/moved/x2.flac") || dev.has("/sdcard/dir") {
		t.Fatal("directory rename must carry its children")
	}
	ok(t, "rename over existing file", fs.Rename("/y.flac", "/z.flac"))
	if got, _ := dev.get("/sdcard/z.flac"); string(got) != "y" || dev.has("/sdcard/y.flac") {
		t.Fatalf("overwrite rename wrong, z=%q", got)
	}

	dev.put("/sdcard/a/f", []byte("1"))
	dev.put("/sdcard/b/g", []byte("2"))
	wantRC(t, "dir onto non-empty dir", fs.Rename("/a", "/b"), -fuse.ENOTEMPTY)
	wantRC(t, "missing source", fs.Rename("/nope", "/x"), -fuse.ENOENT)
}

func TestRenameFlushesPendingWritesFirst(t *testing.T) {
	fs, dev := newTestFS(t, false, true)
	rc, fh := fs.Create("/tmp.part", fuse.O_WRONLY|fuse.O_CREAT, 0644)
	ok(t, "create", rc)
	fs.Write("/tmp.part", []byte("payload"), 0, fh)
	ok(t, "rename while open and dirty", fs.Rename("/tmp.part", "/final.flac"))
	ok(t, "release", fs.Release("/final.flac", fh))
	if got, _ := dev.get("/sdcard/final.flac"); string(got) != "payload" {
		t.Fatalf("final.flac = %q", got)
	}
	if dev.has("/sdcard/tmp.part") {
		t.Fatal("old name must be gone")
	}
}

func TestUnlinkOfOpenDirtyFileNeverPushesIt(t *testing.T) {
	fs, dev := newTestFS(t, false, true)
	rc, fh := fs.Create("/gone.bin", fuse.O_WRONLY|fuse.O_CREAT, 0644)
	ok(t, "create", rc)
	fs.Write("/gone.bin", []byte("data"), 0, fh)
	ok(t, "unlink", fs.Unlink("/gone.bin"))
	fs.Flush("/gone.bin", fh)
	fs.Release("/gone.bin", fh)
	if dev.has("/sdcard/gone.bin") {
		t.Fatal("deleted file was resurrected by a late flush")
	}
	var st fuse.Stat_t
	wantRC(t, "getattr after unlink", fs.Getattr("/gone.bin", &st, noFh), -fuse.ENOENT)
}

func TestMkdirRmdir(t *testing.T) {
	fs, dev := newTestFS(t, false, true)
	ok(t, "mkdir", fs.Mkdir("/d", 0755))
	wantRC(t, "mkdir again", fs.Mkdir("/d", 0755), -fuse.EEXIST)
	wantRC(t, "mkdir without parent", fs.Mkdir("/x/y", 0755), -fuse.ENOENT)
	writeFile(t, fs, "/d/f.txt", []byte("1"))
	wantRC(t, "rmdir non-empty", fs.Rmdir("/d"), -fuse.ENOTEMPTY)
	ok(t, "unlink", fs.Unlink("/d/f.txt"))
	ok(t, "rmdir", fs.Rmdir("/d"))
	if dev.has("/sdcard/d") {
		t.Fatal("dir still on device")
	}
}

func TestMacJunkIsDiscardedButUsableWhileOpen(t *testing.T) {
	fs, dev := newTestFS(t, false, true)
	for _, name := range []string{"/._song.flac", "/.DS_Store"} {
		rc, fh := fs.Create(name, fuse.O_WRONLY|fuse.O_CREAT, 0644)
		ok(t, "create "+name, rc)
		fs.Write(name, []byte("appledouble"), 0, fh)
		var st fuse.Stat_t
		ok(t, "getattr while open", fs.Getattr(name, &st, noFh))
		if st.Size != 11 {
			t.Fatalf("junk size = %d", st.Size)
		}
		ok(t, "flush", fs.Flush(name, fh))
		ok(t, "release", fs.Release(name, fh))
		if dev.has("/sdcard" + name) {
			t.Fatalf("%s reached the device", name)
		}
		wantRC(t, "gone after close", fs.Getattr(name, &st, noFh), -fuse.ENOENT)
	}

	// with the filter off the same file is stored
	fs2, dev2 := newTestFS(t, false, false)
	writeFile(t, fs2, "/._keep", []byte("x"))
	if !dev2.has("/sdcard/._keep") {
		t.Fatal("-allow-junk must store ._ files")
	}
}

func TestReadOnlyMountRejectsEveryMutation(t *testing.T) {
	fs, dev := newTestFS(t, true, true)
	dev.put("/sdcard/a.flac", []byte("abc"))
	rc, _ := fs.Create("/n", fuse.O_WRONLY|fuse.O_CREAT, 0644)
	wantRC(t, "create", rc, -fuse.EROFS)
	rc, _ = fs.Open("/a.flac", fuse.O_WRONLY)
	wantRC(t, "open for write", rc, -fuse.EROFS)
	wantRC(t, "mkdir", fs.Mkdir("/d", 0755), -fuse.EROFS)
	wantRC(t, "unlink", fs.Unlink("/a.flac"), -fuse.EROFS)
	wantRC(t, "rmdir", fs.Rmdir("/d"), -fuse.EROFS)
	wantRC(t, "rename", fs.Rename("/a.flac", "/b.flac"), -fuse.EROFS)
	wantRC(t, "truncate", fs.Truncate("/a.flac", 0, noFh), -fuse.EROFS)
	wantRC(t, "utimens", fs.Utimens("/a.flac", nil), -fuse.EROFS)
	if got := readAll(t, fs, "/a.flac"); string(got) != "abc" {
		t.Fatalf("read on ro mount broken: %q", got)
	}
	if got, _ := dev.get("/sdcard/a.flac"); string(got) != "abc" {
		t.Fatal("device changed on a read-only mount")
	}
}

func TestUnicodeNamesInNFCandNFD(t *testing.T) {
	fs, dev := newTestFS(t, false, true)
	nfc := norm.NFC.String("Майор Гром.txt")
	nfd := norm.NFD.String(nfc)
	if nfc == nfd {
		t.Fatal("test needs a name that differs between NFC and NFD")
	}
	dev.put("/sdcard/"+nfc, []byte("привет"))
	dev.put("/sdcard/"+norm.NFC.String("Папка")+"/ёж.txt", []byte("ёж"))

	var st fuse.Stat_t
	ok(t, "getattr NFC", fs.Getattr("/"+nfc, &st, noFh))
	ok(t, "getattr NFD", fs.Getattr("/"+nfd, &st, noFh))
	if got := readAll(t, fs, "/"+nfd); string(got) != "привет" {
		t.Fatalf("NFD read = %q", got)
	}
	// file inside a directory addressed with the other spelling
	nfdDir := norm.NFD.String("Папка")
	if got := readAll(t, fs, "/"+nfdDir+"/"+norm.NFD.String("ёж.txt")); string(got) != "ёж" {
		t.Fatalf("nested NFD read = %q", got)
	}
	// creating through the NFD spelling must land in the real (NFC) directory
	writeFile(t, fs, "/"+nfdDir+"/new.txt", []byte("n"))
	if !dev.has("/sdcard/" + norm.NFC.String("Папка") + "/new.txt") {
		t.Fatal("create via NFD dir did not use the real directory")
	}
}

func TestSequentialReadMatchesAndFarSeekUsesRange(t *testing.T) {
	fs, dev := newTestFS(t, false, true)
	data := make([]byte, 6<<20)
	rand.New(rand.NewSource(1)).Read(data)
	dev.put("/sdcard/big.flac", data)

	if got := readAll(t, fs, "/big.flac"); !bytes.Equal(got, data) {
		t.Fatal("sequential read differs from device data")
	}

	rc, fh := fs.Open("/big.flac", fuse.O_RDONLY)
	ok(t, "open", rc)
	defer fs.Release("/big.flac", fh)
	dev.mu.Lock()
	dev.opens = nil
	dev.mu.Unlock()
	off := int64(len(data) - 4097)
	buf := make([]byte, 4097)
	if n := fs.Read("/big.flac", buf, off, fh); n != 4097 || !bytes.Equal(buf, data[off:]) {
		t.Fatalf("far-seek read wrong (n=%d)", n)
	}
	dev.mu.Lock()
	defer dev.mu.Unlock()
	ranged := false
	for _, o := range dev.opens {
		if strings.HasSuffix(o, "+4097") {
			ranged = true
		}
	}
	if !ranged {
		t.Fatalf("far seek should use a ranged open, opens: %v", dev.opens)
	}
}

func TestReadAheadIsBoundedByBackpressure(t *testing.T) {
	fs, dev := newTestFS(t, false, true)
	dev.put("/sdcard/huge.flac", make([]byte, 64<<20))
	rc, fh := fs.Open("/huge.flac", fuse.O_RDONLY)
	ok(t, "open", rc)
	buf := make([]byte, 32768)
	if n := fs.Read("/huge.flac", buf, 0, fh); n != 32768 {
		t.Fatalf("read n=%d", n)
	}
	time.Sleep(300 * time.Millisecond)
	dev.mu.Lock()
	got := dev.readBytes
	dev.mu.Unlock()
	if limit := int64(readAhead + 3*copyBufSize); got > limit {
		t.Fatalf("downloaded %d bytes after one 32KB read, limit %d (no backpressure?)", got, limit)
	}
	if got == 0 {
		t.Fatal("nothing was downloaded")
	}
	fs.Release("/huge.flac", fh)
}

func TestFailedUploadKeepsFileDirtyAndRetries(t *testing.T) {
	fs, dev := newTestFS(t, false, true)
	rc, fh := fs.Create("/a.bin", fuse.O_WRONLY|fuse.O_CREAT, 0644)
	ok(t, "create", rc)
	fs.Write("/a.bin", []byte("precious"), 0, fh)
	dev.mu.Lock()
	dev.failUploads = 1
	dev.mu.Unlock()
	wantRC(t, "first flush fails", fs.Flush("/a.bin", fh), -fuse.EIO)
	ok(t, "second flush retries", fs.Flush("/a.bin", fh))
	ok(t, "release", fs.Release("/a.bin", fh))
	if got, _ := dev.get("/sdcard/a.bin"); string(got) != "precious" {
		t.Fatalf("device has %q after retry", got)
	}
}

func TestSecondFlushWithoutChangesDoesNotUploadAgain(t *testing.T) {
	fs, dev := newTestFS(t, false, true)
	rc, fh := fs.Create("/a.bin", fuse.O_WRONLY|fuse.O_CREAT, 0644)
	ok(t, "create", rc)
	fs.Write("/a.bin", []byte("x"), 0, fh)
	ok(t, "flush", fs.Flush("/a.bin", fh))
	ok(t, "flush again", fs.Flush("/a.bin", fh))
	ok(t, "release", fs.Release("/a.bin", fh))
	if dev.uploads != 1 {
		t.Fatalf("uploads = %d, want 1", dev.uploads)
	}
}

func TestReaderSeesWritesOfConcurrentWriter(t *testing.T) {
	fs, dev := newTestFS(t, false, true)
	dev.put("/sdcard/a.txt", []byte("0123456789"))
	rfh := openRW(t, fs, "/a.txt", fuse.O_RDONLY)
	wfh := openRW(t, fs, "/a.txt", fuse.O_WRONLY)
	fs.Write("/a.txt", []byte("AB"), 0, wfh)
	buf := make([]byte, 10)
	if n := fs.Read("/a.txt", buf, 0, rfh); n != 10 || string(buf) != "AB23456789" {
		t.Fatalf("reader saw %q (n=%d)", buf[:max(n, 0)], n)
	}
	fs.Release("/a.txt", wfh)
	fs.Release("/a.txt", rfh)
}

func TestUtimensImmediateAndDeferredForDirtyFiles(t *testing.T) {
	fs, dev := newTestFS(t, false, true)
	dev.put("/sdcard/a.txt", []byte("x"))
	ok(t, "utimens", fs.Utimens("/a.txt", []fuse.Timespec{{Sec: 1}, {Sec: 1600000000}}))
	if e, _ := dev.stat("/sdcard/a.txt"); e.Mtime != 1600000000 {
		t.Fatalf("mtime = %d", e.Mtime)
	}

	rc, fh := fs.Create("/b.txt", fuse.O_WRONLY|fuse.O_CREAT, 0644)
	ok(t, "create", rc)
	fs.Write("/b.txt", []byte("y"), 0, fh)
	ok(t, "utimens on dirty file", fs.Utimens("/b.txt", []fuse.Timespec{{Sec: 1}, {Sec: 1500000000}}))
	ok(t, "release", fs.Release("/b.txt", fh))
	// the mtime must be applied after the upload, not overwritten by it
	n := dev.nodes["/sdcard/b.txt"]
	if n == nil || n.mtime != 1500000000 {
		t.Fatalf("deferred mtime lost: %+v", n)
	}
}

func TestStatfsReportsDeviceValues(t *testing.T) {
	fs, _ := newTestFS(t, false, true)
	var st fuse.Statfs_t
	ok(t, "statfs", fs.Statfs("/", &st))
	if st.Bsize != 4096 || st.Blocks != 1000 || st.Bfree != 400 || st.Bavail != 300 {
		t.Fatalf("statfs = %+v", st)
	}
}

// ---- pure helpers --------------------------------------------------------------

func TestParseStat(t *testing.T) {
	e, okp := parseStat("81b0|21453358|1778510704|/sdcard/11 Обрела Покой.flac")
	if !okp || e.Size != 21453358 || e.Mtime != 1778510704 || e.IsDir() || e.Name != "/sdcard/11 Обрела Покой.flac" {
		t.Fatalf("file: %+v ok=%v", e, okp)
	}
	d, _ := parseStat("41ff|4096|1|/sdcard/Music")
	if !d.IsDir() {
		t.Fatal("0x41ff must be a directory")
	}
	for _, bad := range []string{"", "garbage", "zz|1|2|/x", "81b0|x|2|/x", "81b0|1|y|/x"} {
		if _, okp := parseStat(bad); okp {
			t.Fatalf("parseStat accepted %q", bad)
		}
	}
	if e, _ := parseStat("81b0|1|2|/a|b"); e.Name != "/a|b" {
		t.Fatalf("pipe in a file name must survive: %q", e.Name)
	}
}

func TestShQuote(t *testing.T) {
	cases := map[string]string{
		"plain":      `'plain'`,
		"it's":       `'it'\''s'`,
		"a b;rm -rf": `'a b;rm -rf'`,
		"":           `''`,
	}
	for in, want := range cases {
		if got := shQuote(in); got != want {
			t.Errorf("shQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestIsJunk(t *testing.T) {
	for _, n := range []string{"._a.flac", ".DS_Store", ".Trashes", ".fseventsd", ".Spotlight-V100"} {
		if !isJunk(n) {
			t.Errorf("%q should be junk", n)
		}
	}
	for _, n := range []string{"a.flac", ".hidden-but-ours", "x._y", "cover.jpg"} {
		if isJunk(n) {
			t.Errorf("%q must not be junk", n)
		}
	}
}

func TestRangeScriptAlignment(t *testing.T) {
	cases := []struct{ off, n, lead, blocks int64 }{
		{0, 100, 0, 1},
		{4096, 4096, 0, 1},
		{4095, 2, 4095, 2},
		{332988416, 32768, 0, 8},
		{100, 4097, 100, 2},
	}
	for _, c := range cases {
		script, lead := rangeScript("/f", c.off, c.n)
		if lead != c.lead {
			t.Errorf("off=%d lead=%d want %d", c.off, lead, c.lead)
		}
		want := fmt.Sprintf("skip=%d count=%d", c.off/rangeBlock, c.blocks)
		if !strings.Contains(script, want) {
			t.Errorf("off=%d n=%d: %q lacks %q", c.off, c.n, script, want)
		}
		if got := (lead + c.n + rangeBlock - 1) / rangeBlock * rangeBlock; got < lead+c.n {
			t.Errorf("window too small")
		}
	}
}

func TestTransientClassificationAndRetry(t *testing.T) {
	for _, m := range []string{"error: device offline", "adb: exit status 1: error: no devices/emulators found",
		"error: closed", "adb server didn't ACK\n* cannot connect to daemon"} {
		if !transient(errors.New(m)) {
			t.Errorf("%q should be transient", m)
		}
	}
	for _, m := range []string{"rm: cannot remove: Permission denied", "stat: No such file"} {
		if transient(errors.New(m)) {
			t.Errorf("%q must not be retried", m)
		}
	}
	if transient(nil) {
		t.Error("nil is not transient")
	}

	calls := 0
	err := retry(func() error {
		calls++
		if calls == 1 {
			return errors.New("error: device offline")
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("retry: err=%v calls=%d", err, calls)
	}
	calls = 0
	err = retry(func() error { calls++; return errors.New("permission denied") })
	if err == nil || calls != 1 {
		t.Fatalf("non-transient errors must not be retried: calls=%d", calls)
	}
}

func TestNeedsNorm(t *testing.T) {
	if needsNorm("/plain/ascii") {
		t.Error("ascii paths need no normalisation")
	}
	if !needsNorm("/Пап") || !utf8.ValidString("/Пап") {
		t.Error("non-ascii paths do")
	}
}
