package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---- state gathered in the background ----------------------------------------

type mountRow struct {
	name    string
	mounted bool
	ro      bool
	path    string
	pid     int
	serial  string
}

type devDetail struct {
	desc     string
	internal string
	sd       string
	sdRoot   string
	fetched  time.Time
}

type snapMsg struct {
	devs   []devInfo
	mounts []mountRow
	err    error
}

type detailMsg struct {
	serial string
	d      devDetail
}

type actionMsg struct {
	text string
	err  error
}

type tickMsg time.Time

func gatherMounts() []mountRow {
	var rows []mountRow
	for _, n := range defaultMounts {
		r := mountRow{name: n, path: mountPointOf(n)}
		if s, ok := loadState(n); ok {
			r.ro, r.pid, r.serial = s.RO, s.Pid, s.Serial
			if s.Mount != "" {
				r.path = s.Mount
			}
		}
		r.mounted = isMounted(r.path)
		rows = append(rows, r)
	}
	// android first, then sdcard, whatever order defaultMounts has
	if rows[0].name != "android" {
		rows[0], rows[1] = rows[1], rows[0]
	}
	return rows
}

func snapshotCmd(bin string) tea.Cmd {
	return func() tea.Msg {
		devs, err := listDevices(bin)
		return snapMsg{devs: devs, mounts: gatherMounts(), err: err}
	}
}

func detailCmd(bin, serial string) tea.Cmd {
	return func() tea.Msg {
		a := &adb{bin: bin, serial: serial}
		man, model, rel := a.props()
		d := devDetail{fetched: time.Now()}
		if model != "" {
			d.desc = strings.TrimSpace(man+" "+model) + " · Android " + rel
		}
		d.internal = a.volumeUsage("/sdcard")
		if d.sdRoot = a.sdCardRoot(); d.sdRoot != "" {
			d.sd = a.volumeUsage(d.sdRoot)
		}
		return detailMsg{serial: serial, d: d}
	}
}

// ---- model -------------------------------------------------------------------

type panel int

const (
	panelDevices panel = iota
	panelMounts
)

type tuiModel struct {
	bin     string
	cfg     config
	devs    []devInfo
	details map[string]devDetail
	fetch   map[string]bool // detail fetch in flight
	mounts  []mountRow
	sel     string // chosen serial; empty = automatic
	focus   panel
	cursor  [2]int
	working string // description of the running action, empty when idle
	notice  string
	noticeE bool
	listErr error
	showLog bool
	input   textinput.Model
	asking  bool
	width   int
}

func newTUIModel(bin string) tuiModel {
	ti := textinput.New()
	ti.Prompt = "connect to (host[:port]): "
	ti.CharLimit = 64
	return tuiModel{bin: bin, cfg: loadConfig(), details: map[string]devDetail{}, fetch: map[string]bool{},
		focus: panelMounts, input: ti, width: 80}
}

// current is the device actions apply to: the chosen one, else the first ready
// device, preferring USB (faster and steadier than Wi-Fi).
func (m tuiModel) current() (devInfo, bool) {
	if m.sel != "" {
		for _, d := range m.devs {
			if d.Serial == m.sel {
				return d, true
			}
		}
	}
	var best *devInfo
	for i, d := range m.devs {
		if d.State != "device" {
			continue
		}
		if best == nil || (best.transport() != "usb" && d.transport() == "usb") {
			best = &m.devs[i]
		}
	}
	if best != nil {
		return *best, true
	}
	return devInfo{}, false
}

func tick() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(snapshotCmd(m.bin), tick())
}

func (m *tuiModel) say(text string, err error) {
	m.notice, m.noticeE = text, err != nil
	if err != nil {
		m.notice = err.Error()
	}
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tickMsg:
		cmds := []tea.Cmd{tick(), snapshotCmd(m.bin)}
		if d, ok := m.current(); ok && d.State == "device" {
			if det, have := m.details[d.Serial]; !m.fetch[d.Serial] && (!have || time.Since(det.fetched) > 20*time.Second) {
				m.fetch[d.Serial] = true
				cmds = append(cmds, detailCmd(m.bin, d.Serial))
			}
		}
		return m, tea.Batch(cmds...)
	case snapMsg:
		m.devs, m.mounts, m.listErr = msg.devs, msg.mounts, msg.err
		if len(m.devs) > 0 && m.cursor[panelDevices] >= len(m.devs) {
			m.cursor[panelDevices] = len(m.devs) - 1
		}
		if d, ok := m.current(); ok && d.State == "device" {
			if _, have := m.details[d.Serial]; !have && !m.fetch[d.Serial] {
				m.fetch[d.Serial] = true
				return m, detailCmd(m.bin, d.Serial)
			}
		}
	case detailMsg:
		m.details[msg.serial] = msg.d
		delete(m.fetch, msg.serial)
	case actionMsg:
		m.working = ""
		m.say(msg.text, msg.err)
		return m, snapshotCmd(m.bin)
	case tea.KeyMsg:
		if m.asking {
			return m.updateInput(msg)
		}
		return m.updateKey(msg)
	}
	return m, nil
}

func (m tuiModel) updateInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.asking = false
		return m, nil
	case "enter":
		target := strings.TrimSpace(m.input.Value())
		m.asking = false
		if target == "" {
			return m, nil
		}
		m.working = "connecting to " + withPort(target)
		return m, connectCmd(m.bin, withPort(target))
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m tuiModel) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch key {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "tab":
		m.focus = 1 - m.focus
		return m, nil
	case "up", "k":
		if m.cursor[m.focus] > 0 {
			m.cursor[m.focus]--
		}
		return m, nil
	case "down", "j":
		n := len(m.mounts)
		if m.focus == panelDevices {
			n = len(m.devs)
		}
		if m.cursor[m.focus] < n-1 {
			m.cursor[m.focus]++
		}
		return m, nil
	case "r":
		m.cfg.ReadOnly = !m.cfg.ReadOnly
		m.say("read-only on next mount: "+onOff(m.cfg.ReadOnly)+" (unmount and mount again to apply)", m.cfg.save())
		return m, nil
	case "f":
		m.cfg.Open = !m.cfg.Open
		m.say("open in Finder after mounting: "+onOff(m.cfg.Open), m.cfg.save())
		return m, nil
	case "l":
		m.showLog = !m.showLog
		return m, nil
	case "c":
		m.asking = true
		m.input.SetValue("")
		return m, m.input.Focus()
	}
	if m.working != "" {
		m.say("busy: "+m.working, nil)
		return m, nil
	}
	dev, haveDev := m.current()
	switch key {
	case "enter", " ":
		if m.focus == panelDevices {
			if len(m.devs) > 0 {
				m.sel = m.devs[m.cursor[panelDevices]].Serial
				m.say("selected "+m.sel, nil)
			}
			return m, nil
		}
		if len(m.mounts) == 0 {
			return m, nil
		}
		return m.toggleMount(m.mounts[m.cursor[panelMounts]], dev, haveDev)
	case "a":
		return m.mountAll(dev, haveDev)
	case "u":
		m.working = "unmounting everything"
		return m, func() tea.Msg {
			var parts []string
			for _, n := range defaultMounts {
				parts = append(parts, stopMount(n))
			}
			return actionMsg{text: strings.Join(parts, "; ")}
		}
	case "o":
		if len(m.mounts) > 0 {
			r := m.mounts[m.cursor[panelMounts]]
			if !r.mounted {
				m.say(r.name+" is not mounted", errors.New(r.name+" is not mounted"))
				return m, nil
			}
			exec.Command("open", r.path).Run()
		}
		return m, nil
	case "w":
		if !haveDev {
			m.say("", errors.New("no device"))
			return m, nil
		}
		if dev.isNetwork() {
			m.say("", errors.New("already over Wi-Fi"))
			return m, nil
		}
		m.working = "switching " + dev.Serial + " to Wi-Fi"
		return m, wifiEnableCmd(m.bin, dev.Serial)
	case "x":
		if m.focus == panelDevices && len(m.devs) > 0 && m.devs[m.cursor[panelDevices]].isNetwork() {
			serial := m.devs[m.cursor[panelDevices]].Serial
			m.working = "disconnecting " + serial
			return m, func() tea.Msg {
				out, _ := adbOutput(m.bin, "disconnect", serial)
				return actionMsg{text: out}
			}
		}
		m.say("", errors.New("select a Wi-Fi device in the Devices panel (tab), then x"))
	}
	return m, nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func connectCmd(bin, target string) tea.Cmd {
	return func() tea.Msg {
		out, ok := adbConnect(bin, target)
		if !ok {
			return actionMsg{err: errors.New(out)}
		}
		return actionMsg{text: out}
	}
}

func wifiEnableCmd(bin, serial string) tea.Cmd {
	return func() tea.Msg {
		target, err := enableWifi(&adb{bin: bin, serial: serial}, 5555)
		if err != nil {
			return actionMsg{err: err}
		}
		return actionMsg{text: "Wi-Fi ready: " + target + " (you can unplug the cable)"}
	}
}

// volumeFor maps a mount name to what to expose on the phone.
func (m tuiModel) volumeFor(name string, dev devInfo) (root, vol string, err error) {
	if name == "android" {
		return "/sdcard", "Android", nil
	}
	d := m.details[dev.Serial]
	if d.sdRoot == "" {
		return "", "", errors.New("no SD card detected on " + dev.Serial)
	}
	return d.sdRoot, "SDCard", nil
}

func (m tuiModel) toggleMount(r mountRow, dev devInfo, haveDev bool) (tea.Model, tea.Cmd) {
	if r.mounted {
		m.working = "unmounting " + r.name
		return m, func() tea.Msg { return actionMsg{text: stopMount(r.name)} }
	}
	return m.mountNames(dev, haveDev, r.name)
}

func (m tuiModel) mountAll(dev devInfo, haveDev bool) (tea.Model, tea.Cmd) {
	var names []string
	for _, r := range m.mounts {
		if !r.mounted {
			names = append(names, r.name)
		}
	}
	return m.mountNames(dev, haveDev, names...)
}

func (m tuiModel) mountNames(dev devInfo, haveDev bool, names ...string) (tea.Model, tea.Cmd) {
	if !haveDev || dev.State != "device" {
		m.say("", errors.New("no ready device to mount from"))
		return m, nil
	}
	type job struct{ name, root, vol string }
	var jobs []job
	for _, n := range names {
		root, vol, err := m.volumeFor(n, dev)
		if err != nil {
			m.say("", err)
			return m, nil
		}
		jobs = append(jobs, job{n, root, vol})
	}
	if len(jobs) == 0 {
		return m, nil
	}
	m.working = "mounting " + strings.Join(names, ", ")
	bin, cfg := m.bin, m.cfg
	return m, func() tea.Msg {
		a := &adb{bin: bin, serial: dev.Serial}
		var parts []string
		for _, j := range jobs {
			text, err := startMount(a, j.name, j.root, j.vol, cfg.ReadOnly)
			if err != nil {
				return actionMsg{err: err}
			}
			parts = append(parts, text)
			if cfg.Open {
				exec.Command("open", mountPointOf(j.name)).Run()
			}
		}
		return actionMsg{text: strings.Join(parts, "; ")}
	}
}

// ---- view --------------------------------------------------------------------

var (
	styTitle = lipgloss.NewStyle().Bold(true)
	styDim   = lipgloss.NewStyle().Faint(true)
	styOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styErr   = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	stySel   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	styHead  = lipgloss.NewStyle().Bold(true).Underline(true)
)

func (m tuiModel) cursorMark(p panel, i int) string {
	if m.focus == p && m.cursor[p] == i {
		return stySel.Render("▸ ")
	}
	return "  "
}

func (m tuiModel) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n\n", styTitle.Render("adbmount"), styDim.Render(buildVersion()))

	cur, haveCur := m.current()
	b.WriteString(styHead.Render("Devices") + "\n")
	switch {
	case m.listErr != nil:
		b.WriteString("  " + styErr.Render("✗ "+m.listErr.Error()) + "\n")
	case len(m.devs) == 0:
		b.WriteString("  " + styWarn.Render("no device") + styDim.Render(" — plug in the phone with USB debugging on, or press c to connect over Wi-Fi") + "\n")
	}
	for i, d := range m.devs {
		dot := styOK.Render("●")
		if d.State != "device" {
			dot = styWarn.Render("●")
		}
		star := " "
		if haveCur && d.Serial == cur.Serial {
			star = styOK.Render("✓")
		}
		desc := m.details[d.Serial].desc
		if desc == "" {
			desc = d.model()
		}
		state := d.State
		if d.State == "device" {
			state = "connected"
		}
		fmt.Fprintf(&b, "%s%s %s %-20s %-5s %-12s %s\n", m.cursorMark(panelDevices, i), dot, star, d.Serial, d.transport(), state, desc)
		if h := stateHint(d.State); h != "" {
			b.WriteString("      " + styWarn.Render(strings.TrimPrefix(h, ": ")) + "\n")
		}
	}

	if haveCur {
		if det, ok := m.details[cur.Serial]; ok {
			b.WriteString("\n" + styHead.Render("Storage") + "\n")
			fmt.Fprintf(&b, "  internal  %s\n", det.internal)
			if det.sdRoot != "" {
				fmt.Fprintf(&b, "  SD card   %s  %s\n", det.sd, styDim.Render(det.sdRoot))
			} else {
				b.WriteString("  SD card   " + styDim.Render("not found") + "\n")
			}
		}
	}

	b.WriteString("\n" + styHead.Render("Mounts") + "\n")
	for i, r := range m.mounts {
		status := styDim.Render("not mounted")
		if r.mounted {
			mode := "read-write"
			if r.ro {
				mode = "read-only"
			}
			status = styOK.Render("mounted") + " " + mode
		}
		fmt.Fprintf(&b, "%s%-8s %-34s %s\n", m.cursorMark(panelMounts, i), r.name, shortHome(r.path), status)
	}

	b.WriteString("\n" + styHead.Render("Settings") + "\n")
	fmt.Fprintf(&b, "  [r] read-only mounts      %s\n", styOnOff(m.cfg.ReadOnly))
	fmt.Fprintf(&b, "  [f] open Finder after up  %s\n", styOnOff(m.cfg.Open))
	if p := configPath(); p != "" {
		b.WriteString("  " + styDim.Render("saved in "+shortHome(p)) + "\n")
	}

	if m.showLog && len(m.mounts) > 0 {
		r := m.mounts[m.cursor[panelMounts]]
		b.WriteString("\n" + styHead.Render("Log: "+r.name) + "\n")
		txt := tailFile(logPath(r.name), 8)
		if txt == "" {
			txt = "(empty)"
		}
		b.WriteString(styDim.Render(indent(txt)) + "\n")
	}

	b.WriteString("\n")
	switch {
	case m.asking:
		b.WriteString(m.input.View() + "\n")
	case m.working != "":
		b.WriteString(styWarn.Render("… "+m.working) + "\n")
	case m.notice != "" && m.noticeE:
		b.WriteString(styErr.Render("✗ "+m.notice) + "\n")
	case m.notice != "":
		b.WriteString(styOK.Render("✓ ") + m.notice + "\n")
	default:
		b.WriteString("\n")
	}
	b.WriteString(styDim.Render("↑↓ move · tab panel · enter mount/unmount or select device · a mount all · u unmount all · o open in Finder") + "\n")
	b.WriteString(styDim.Render("c connect Wi-Fi · w switch USB phone to Wi-Fi · x disconnect Wi-Fi · l log · q quit (mounts keep running)") + "\n")
	return b.String()
}

func styOnOff(v bool) string {
	if v {
		return styOK.Render("on")
	}
	return styDim.Render("off")
}

func shortHome(p string) string {
	if h, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, h) {
		return "~" + strings.TrimPrefix(p, h)
	}
	return p
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}

func runTUI(args []string) error {
	if _, err := parseInterspersed(flagSetFor("tui"), args); err != nil {
		return err
	}
	if fi, err := os.Stdout.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return fail("the interactive UI needs a terminal; use `adbmount status` / `adbmount devices` in scripts")
	}
	bin, err := adbPath()
	if err != nil {
		return err
	}
	_, err = tea.NewProgram(newTUIModel(bin), tea.WithAltScreen()).Run()
	return err
}
