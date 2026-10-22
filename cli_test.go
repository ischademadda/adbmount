package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseDevices(t *testing.T) {
	out := `* daemon not running; starting now at tcp:5037
* daemon started successfully
List of devices attached
PBYP8TRSZHUS9PEM       device usb:1-1 product:rubens model:22041211AC device:rubens transport_id:1
192.168.1.20:5555      device product:rubens model:Redmi_Note device:rubens transport_id:2
R5CT1234               unauthorized usb:1-2 transport_id:3

`
	devs := parseDevices(out)
	if len(devs) != 3 {
		t.Fatalf("got %d devices: %+v", len(devs), devs)
	}
	if devs[0].Serial != "PBYP8TRSZHUS9PEM" || devs[0].State != "device" || devs[0].Props["model"] != "22041211AC" {
		t.Errorf("bad first device: %+v", devs[0])
	}
	if devs[0].transport() != "usb" || devs[1].transport() != "wifi" || !devs[1].isNetwork() {
		t.Errorf("transports: %s %s", devs[0].transport(), devs[1].transport())
	}
	if devs[1].model() != "Redmi Note" {
		t.Errorf("model = %q", devs[1].model())
	}
	if devs[2].State != "unauthorized" {
		t.Errorf("state = %q", devs[2].State)
	}
}

func TestPickDevice(t *testing.T) {
	usb := devInfo{Serial: "AAA", State: "device"}
	wifi := devInfo{Serial: "10.0.0.2:5555", State: "device"}
	bad := devInfo{Serial: "BBB", State: "unauthorized"}

	if d, err := pickDevice([]devInfo{usb, bad}, ""); err != nil || d.Serial != "AAA" {
		t.Errorf("single ready device should be picked: %v %v", d, err)
	}
	if _, err := pickDevice([]devInfo{usb, wifi}, ""); err == nil || !strings.Contains(err.Error(), "10.0.0.2:5555") {
		t.Errorf("ambiguity must list candidates, got %v", err)
	}
	if d, err := pickDevice([]devInfo{usb, wifi}, "10.0.0.2:5555"); err != nil || d.Serial != wifi.Serial {
		t.Errorf("explicit serial: %v %v", d, err)
	}
	if _, err := pickDevice([]devInfo{bad}, ""); err == nil || !strings.Contains(err.Error(), "Allow USB debugging") {
		t.Errorf("unauthorized hint missing: %v", err)
	}
	if _, err := pickDevice(nil, ""); err == nil || !strings.Contains(err.Error(), "no phone") {
		t.Errorf("empty list: %v", err)
	}
	if _, err := pickDevice([]devInfo{usb}, "ZZZ"); err == nil {
		t.Error("unknown serial must fail")
	}
}

func TestParseWlanIP(t *testing.T) {
	addr := `22: wlan0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc mq state UP qlen 3000
    inet 192.168.1.42/24 brd 192.168.1.255 scope global wlan0
       valid_lft forever preferred_lft forever
`
	if got := parseWlanIP(addr); got != "192.168.1.42" {
		t.Errorf("addr: %q", got)
	}
	route := "10.0.0.0/8 dev rmnet0 proto kernel scope link src 10.4.5.6\n192.168.0.0/24 dev wlan0 proto kernel scope link src 192.168.0.77\n"
	if got := parseWlanIP(route); got != "192.168.0.77" {
		t.Errorf("route: %q", got)
	}
	if got := parseWlanIP("nothing here"); got != "" {
		t.Errorf("empty: %q", got)
	}
}

func TestWithPort(t *testing.T) {
	for in, want := range map[string]string{"1.2.3.4": "1.2.3.4:5555", "1.2.3.4:37000": "1.2.3.4:37000", "phone.local": "phone.local:5555"} {
		if got := withPort(in); got != want {
			t.Errorf("withPort(%q) = %q, want %q", in, got, want)
		}
	}
	if !connectedOK("already connected to 1.2.3.4:5555") || !connectedOK("connected to 1.2.3.4:5555") || connectedOK("failed to connect to 1.2.3.4:5555") {
		t.Error("connectedOK misclassifies adb output")
	}
}

func TestParseMounts(t *testing.T) {
	out := `/dev/disk3s1s1 on / (apfs, sealed, local, read-only, journaled)
fuse-t:/SDCard on /Users/me/mnt/sd card (nfs, nodev, nosuid, mounted by me)
fuse-t:/Android on /Users/me/mnt/android (nfs, nodev)
`
	m := parseMounts(out)
	for _, want := range []string{"/", "/Users/me/mnt/sd card", "/Users/me/mnt/android"} {
		if !m[want] {
			t.Errorf("missing mount %q in %v", want, m)
		}
	}
	if len(m) != 3 {
		t.Errorf("got %v", m)
	}
}

func TestPickSD(t *testing.T) {
	if got := pickSD("emulated\r\nself\r\nABCD-1234\r\n"); got != "/storage/ABCD-1234" {
		t.Errorf("got %q", got)
	}
	if got := pickSD("emulated\nself\n"); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestResolveRemoteWithoutDevice(t *testing.T) {
	for in, want := range map[string]string{
		"": "/sdcard", "int:": "/sdcard", "int:Music": "/sdcard/Music", "internal:/DCIM": "/sdcard/DCIM", "/storage/emulated/0": "/storage/emulated/0",
	} {
		if got, err := resolveRemote(nil, in); err != nil || got != want {
			t.Errorf("resolveRemote(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := resolveRemote(nil, "relative/path"); err == nil {
		t.Error("relative path must be rejected")
	}
}

func TestParseInterspersed(t *testing.T) {
	fl := flagSetFor("t")
	n := fl.Bool("n", false, "")
	j := fl.Int("j", 1, "")
	pos, err := parseInterspersed(fl, []string{"a", "-n", "b", "-j", "8", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if !*n || *j != 8 || !reflect.DeepEqual(pos, []string{"a", "b", "c"}) {
		t.Errorf("n=%v j=%d pos=%v", *n, *j, pos)
	}
}

func TestHumanSize(t *testing.T) {
	for in, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 5 << 20: "5.0 MiB", 3 << 30: "3.0 GiB"} {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestCollapseSamePhone(t *testing.T) {
	wifi := devInfo{Serial: "10.0.0.2:5555", State: "device"}
	usb := devInfo{Serial: "AAA", State: "device"}
	other := devInfo{Serial: "BBB", State: "device"}
	unknown := devInfo{Serial: "CCC", State: "device"}
	hw := map[string]string{"10.0.0.2:5555": "SN1", "AAA": "SN1", "BBB": "SN2"}

	got := collapseSamePhone([]devInfo{wifi, usb, other, unknown}, hw)
	var serials []string
	for _, d := range got {
		serials = append(serials, d.Serial)
	}
	if !reflect.DeepEqual(serials, []string{"AAA", "BBB", "CCC"}) {
		t.Errorf("got %v; want the USB transport of SN1, then BBB and CCC", serials)
	}
}

func TestCompletionScripts(t *testing.T) {
	for _, sh := range []string{"zsh", "bash", "fish"} {
		s, err := completionScript(sh)
		if err != nil || !strings.Contains(s, "wifi") || strings.Contains(s, "serve") {
			t.Errorf("%s: err=%v\n%s", sh, err, s)
		}
	}
	if _, err := completionScript("tcsh"); err == nil {
		t.Error("unknown shell must fail")
	}
}
