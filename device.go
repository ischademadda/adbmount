package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// devInfo is one line of `adb devices -l`.
type devInfo struct {
	Serial string
	State  string // device, offline, unauthorized, ...
	Props  map[string]string
}

// isNetwork reports serials of the form host:port (adb over Wi-Fi).
func (d devInfo) isNetwork() bool { return netSerial.MatchString(d.Serial) }

var netSerial = regexp.MustCompile(`^[\w.\-\[\]:]+:\d+$`)

func (d devInfo) transport() string {
	switch {
	case d.isNetwork():
		return "wifi"
	case strings.HasPrefix(d.Serial, "emulator-"):
		return "emulator"
	default:
		return "usb"
	}
}

func (d devInfo) model() string {
	if m := d.Props["model"]; m != "" {
		return strings.ReplaceAll(m, "_", " ")
	}
	return "-"
}

func parseDevices(out string) []devInfo {
	var res []devInfo
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "List of devices") || strings.HasPrefix(line, "*") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		d := devInfo{Serial: f[0], State: f[1], Props: map[string]string{}}
		for _, kv := range f[2:] {
			if k, v, ok := strings.Cut(kv, ":"); ok {
				d.Props[k] = v
			}
		}
		res = append(res, d)
	}
	return res
}

// pickDevice chooses the device a command should talk to. An empty serial means
// "the only usable device".
func pickDevice(devs []devInfo, serial string) (devInfo, error) {
	if serial != "" {
		for _, d := range devs {
			if d.Serial == serial {
				if d.State != "device" {
					return d, fmt.Errorf("device %s is %s%s", serial, d.State, stateHint(d.State))
				}
				return d, nil
			}
		}
		return devInfo{}, fmt.Errorf("device %s not found (see `adbmount devices`; for Wi-Fi try `adbmount wifi connect`)", serial)
	}
	var ready []devInfo
	for _, d := range devs {
		if d.State == "device" {
			ready = append(ready, d)
		}
	}
	switch len(ready) {
	case 1:
		return ready[0], nil
	case 0:
		if len(devs) > 0 {
			return devs[0], fmt.Errorf("device %s is %s%s", devs[0].Serial, devs[0].State, stateHint(devs[0].State))
		}
		return devInfo{}, fmt.Errorf("no phone visible: check the cable and USB debugging, or use `adbmount wifi connect HOST`")
	}
	var names []string
	for _, d := range ready {
		names = append(names, fmt.Sprintf("%s (%s, %s)", d.Serial, d.transport(), d.model()))
	}
	return devInfo{}, fmt.Errorf("several devices connected, choose one with -s or ANDROID_SERIAL:\n  %s", strings.Join(names, "\n  "))
}

func stateHint(state string) string {
	switch state {
	case "unauthorized":
		return ": accept the \"Allow USB debugging?\" prompt on the phone"
	case "offline":
		return ": reconnect the cable or run `adb kill-server`"
	}
	return ""
}

func adbPath() (string, error) {
	p, err := exec.LookPath("adb")
	if err != nil {
		return "", fmt.Errorf("adb not found in PATH (brew install android-platform-tools)")
	}
	return p, nil
}

func listDevices(bin string) ([]devInfo, error) {
	var out, errb bytes.Buffer
	c := exec.Command(bin, "devices", "-l")
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return nil, fmt.Errorf("adb devices: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return parseDevices(out.String()), nil
}

// connectDevice resolves serial (or the only device) and returns an adb handle bound to it.
func connectDevice(serial string) (*adb, devInfo, error) {
	bin, err := adbPath()
	if err != nil {
		return nil, devInfo{}, err
	}
	devs, err := listDevices(bin)
	if err != nil {
		return nil, devInfo{}, err
	}
	d, err := pickDevice(devs, serial)
	if err != nil {
		return nil, d, err
	}
	return &adb{bin: bin, serial: d.Serial}, d, nil
}

// ---- storage volumes ---------------------------------------------------------

var sdUUID = regexp.MustCompile(`^[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}$`)

// sdCardRoot returns /storage/XXXX-XXXX for the first removable volume, or "".
func (a *adb) sdCardRoot() string {
	out, err := a.run("ls /storage")
	if err != nil {
		return ""
	}
	return pickSD(string(out))
}

func pickSD(lsOutput string) string {
	for _, name := range strings.Fields(strings.ReplaceAll(lsOutput, "\r", "")) {
		if sdUUID.MatchString(name) {
			return "/storage/" + name
		}
	}
	return ""
}

// resolveRemote expands the sd: / int: shortcuts and makes the path absolute.
func resolveRemote(a *adb, p string) (string, error) {
	switch {
	case p == "" || p == "int:" || p == "internal:":
		return "/sdcard", nil
	case strings.HasPrefix(p, "int:"), strings.HasPrefix(p, "internal:"):
		_, rest, _ := strings.Cut(p, ":")
		return "/sdcard/" + strings.TrimLeft(rest, "/"), nil
	case strings.HasPrefix(p, "sd:"):
		root := a.sdCardRoot()
		if root == "" {
			return "", fmt.Errorf("no SD card found on the phone")
		}
		return strings.TrimRight(root+"/"+strings.TrimLeft(strings.TrimPrefix(p, "sd:"), "/"), "/"), nil
	case strings.HasPrefix(p, "/"):
		return p, nil
	}
	return "", fmt.Errorf("remote path %q must be absolute or start with sd: / int:", p)
}

// props reads manufacturer, model and Android release in one call.
func (a *adb) props() (manufacturer, model, release string) {
	out, err := a.run("getprop ro.product.manufacturer; getprop ro.product.model; getprop ro.build.version.release")
	if err != nil {
		return
	}
	lines := strings.Split(strings.ReplaceAll(string(out), "\r", ""), "\n")
	get := func(i int) string {
		if i < len(lines) {
			return strings.TrimSpace(lines[i])
		}
		return ""
	}
	return get(0), get(1), get(2)
}
