package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func testModel(t *testing.T) tuiModel {
	t.Helper()
	t.Setenv("ADBMOUNT_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("ADBMOUNT_STATE_DIR", t.TempDir())
	t.Setenv("ADBMOUNT_DIR", t.TempDir())
	return newTUIModel("adb")
}

func key(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestTUICurrentPrefersUSBAndHonoursSelection(t *testing.T) {
	m := testModel(t)
	m.devs = []devInfo{
		{Serial: "10.0.0.2:5555", State: "device"},
		{Serial: "AAA", State: "device"},
		{Serial: "BBB", State: "unauthorized"},
	}
	if d, ok := m.current(); !ok || d.Serial != "AAA" {
		t.Errorf("auto choice = %v %v, want the USB device AAA", d.Serial, ok)
	}
	m.sel = "10.0.0.2:5555"
	if d, _ := m.current(); d.Serial != "10.0.0.2:5555" {
		t.Errorf("explicit selection ignored: %s", d.Serial)
	}
	m.devs = []devInfo{{Serial: "BBB", State: "unauthorized"}}
	m.sel = ""
	if _, ok := m.current(); ok {
		t.Error("an unauthorized device must not be usable")
	}
}

func TestTUISettingsTogglePersist(t *testing.T) {
	m := testModel(t)
	next, _ := m.Update(key("r"))
	m = next.(tuiModel)
	if !m.cfg.ReadOnly || !loadConfig().ReadOnly {
		t.Fatal("read-only toggle not applied/saved")
	}
	next, _ = m.Update(key("f"))
	m = next.(tuiModel)
	if c := loadConfig(); !c.Open || !c.ReadOnly {
		t.Fatalf("saved config = %+v", c)
	}
}

func TestTUIMountWithoutDeviceReportsError(t *testing.T) {
	m := testModel(t)
	m.mounts = []mountRow{{name: "android"}, {name: "sdcard"}}
	next, cmd := m.Update(key("a"))
	m = next.(tuiModel)
	if cmd != nil || !m.noticeE || !strings.Contains(m.notice, "no ready device") {
		t.Errorf("notice=%q err=%v cmd=%v", m.notice, m.noticeE, cmd != nil)
	}
}

func TestTUIRefusesActionsWhileBusy(t *testing.T) {
	m := testModel(t)
	m.working = "mounting android"
	next, cmd := m.Update(key("u"))
	if cmd != nil || !strings.Contains(next.(tuiModel).notice, "busy") {
		t.Error("a second action must not start while one is running")
	}
}

func TestTUIViewShowsDeviceAndMounts(t *testing.T) {
	m := testModel(t)
	m.devs = []devInfo{{Serial: "AAA", State: "device", Props: map[string]string{"model": "Pixel_8"}}}
	m.mounts = []mountRow{{name: "android", path: "/tmp/x/android", mounted: true, ro: true}, {name: "sdcard", path: "/tmp/x/sdcard"}}
	v := m.View()
	for _, want := range []string{"AAA", "connected", "android", "mounted", "read-only", "not mounted", "read-only mounts"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
}

func TestTUIQuitKeys(t *testing.T) {
	m := testModel(t)
	if _, cmd := m.Update(key("q")); cmd == nil {
		t.Error("q must quit")
	}
}

func TestTUINoticeShortensHomeAndClips(t *testing.T) {
	m := testModel(t)
	home, _ := os.UserHomeDir()
	m.say("android: /sdcard → "+home+"/mnt/android (read-write)", nil)
	if !strings.Contains(m.notice, "~/mnt/android") || strings.Contains(m.notice, home) {
		t.Errorf("home not shortened: %q", m.notice)
	}
	if got := clip("abcdefghij", 6); got != "abcde…" {
		t.Errorf("clip = %q", got)
	}
	if got := clip("abc", 6); got != "abc" {
		t.Errorf("clip must not touch short text: %q", got)
	}
	m.width = 40
	m.notice = strings.Repeat("x", 100)
	for _, line := range strings.Split(m.View(), "\n") {
		if lipgloss.Width(line) > 80 {
			t.Errorf("line wider than 80 columns (%d): %q", lipgloss.Width(line), line)
		}
	}
}
