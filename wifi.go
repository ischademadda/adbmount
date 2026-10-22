package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var (
	inetRe = regexp.MustCompile(`inet (\d+\.\d+\.\d+\.\d+)/`)
	srcRe  = regexp.MustCompile(`\bsrc (\d+\.\d+\.\d+\.\d+)`)
)

// parseWlanIP finds the phone's Wi-Fi address in the output of
// `ip -f inet addr show wlan0` or, as a fallback, of `ip route`.
func parseWlanIP(out string) string {
	if m := inetRe.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "wlan") {
			continue
		}
		if m := srcRe.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

func (a *adb) wlanIP() string {
	if out, err := a.run("ip -f inet addr show wlan0 2>/dev/null; true"); err == nil {
		if ip := parseWlanIP(string(out)); ip != "" {
			return ip
		}
	}
	if out, err := a.run("ip route 2>/dev/null; true"); err == nil {
		return parseWlanIP(string(out))
	}
	return ""
}

// withPort appends the default adb port when the host has none.
func withPort(hostport string) string {
	if strings.Contains(hostport, ":") && !strings.HasSuffix(hostport, "]") {
		return hostport
	}
	return hostport + ":5555"
}

// adbOutput runs adb with the given args and returns combined output.
func adbOutput(bin string, args ...string) (string, error) {
	var b bytes.Buffer
	c := exec.Command(bin, args...)
	c.Stdout, c.Stderr = &b, &b
	err := c.Run()
	return strings.TrimSpace(b.String()), err
}

// connectedOK recognises success in `adb connect` output, which exits 0 even on failure.
func connectedOK(out string) bool {
	return strings.Contains(out, "connected to") // also matches "already connected to"
}

// needsServerRestart recognises the failure a stale adb server gives for a host
// that is reachable from the shell (seen after the server outlived a network or
// permission change): a restart fixes it.
func needsServerRestart(out string) bool {
	return strings.Contains(out, "No route to host")
}

// adbConnect runs `adb connect`, restarting the adb server once if it reports a
// route failure.
func adbConnect(bin, target string) (string, bool) {
	out, _ := adbOutput(bin, "connect", target)
	if !connectedOK(out) && needsServerRestart(out) {
		adbOutput(bin, "kill-server")
		adbOutput(bin, "start-server")
		out, _ = adbOutput(bin, "connect", target)
	}
	return out, connectedOK(out)
}

func runWifi(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(os.Stderr, `usage: adbmount wifi <subcommand>

  enable [-s SERIAL] [--port N]   phone on USB: switch adb to TCP and connect over Wi-Fi
  connect HOST[:PORT]             connect to a phone that already listens (default port 5555)
  pair HOST:PORT [CODE]           Android 11+ "Wireless debugging": pair with the code from the phone
  scan                            list phones advertising wireless debugging on the network
  disconnect [HOST[:PORT]]        drop Wi-Fi connections (all if none given)
  usb [-s SERIAL]                 switch a phone back to USB-only adb

Then use the Wi-Fi serial everywhere: adbmount up -s 192.168.1.20:5555
`)
		return nil
	}
	sub, rest := args[0], args[1:]
	bin, err := adbPath()
	if err != nil {
		return err
	}
	switch sub {
	case "enable":
		return wifiEnable(bin, rest)
	case "connect":
		if len(rest) != 1 {
			return fail("usage: adbmount wifi connect HOST[:PORT]")
		}
		target := withPort(rest[0])
		out, ok := adbConnect(bin, target)
		if !ok {
			return fail("%s", out)
		}
		fmt.Println(out)
		return nil
	case "pair":
		if len(rest) < 1 || len(rest) > 2 {
			return fail("usage: adbmount wifi pair HOST:PORT [CODE]  (both shown under Developer options → Wireless debugging → Pair device with pairing code)")
		}
		c := exec.Command(bin, append([]string{"pair"}, rest...)...)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			return err
		}
		fmt.Println("Paired. Now connect to the (different) address shown on the main Wireless debugging screen:\n  adbmount wifi connect HOST:PORT")
		return nil
	case "scan":
		out, err := adbOutput(bin, "mdns", "services")
		if err != nil {
			return fail("%s", out)
		}
		fmt.Println(out)
		return nil
	case "disconnect":
		out, _ := adbOutput(bin, append([]string{"disconnect"}, rest...)...)
		fmt.Println(out)
		return nil
	case "usb":
		fl := flagSetFor("wifi usb")
		serial := serialFlag(fl)
		if _, err := parseInterspersed(fl, rest); err != nil {
			return err
		}
		a, d, err := connectDevice(*serial)
		if err != nil {
			return err
		}
		out, err := adbOutput(a.bin, "-s", d.Serial, "usb")
		if err != nil {
			return fail("%s", out)
		}
		fmt.Println(out)
		return nil
	}
	return fail("unknown subcommand %q (try `adbmount wifi -h`)", sub)
}

func wifiEnable(bin string, args []string) error {
	fl := flagSetFor("wifi enable")
	serial := serialFlag(fl)
	port := fl.Int("port", 5555, "TCP port for adbd")
	if _, err := parseInterspersed(fl, args); err != nil {
		return err
	}
	a, d, err := connectDevice(*serial)
	if err != nil {
		return err
	}
	if d.isNetwork() {
		return fail("%s is already connected over Wi-Fi", d.Serial)
	}
	ip := a.wlanIP()
	if ip == "" {
		return fail("could not find the phone's Wi-Fi address: is Wi-Fi on and connected?")
	}
	target := fmt.Sprintf("%s:%d", ip, *port)
	if out, err := adbOutput(bin, "-s", d.Serial, "tcpip", fmt.Sprint(*port)); err != nil {
		return fail("adb tcpip: %s", out)
	}
	// adbd restarts in TCP mode; give it a moment before connecting.
	var out string
	for i := 0; i < 10; i++ {
		time.Sleep(time.Second)
		var ok bool
		if out, ok = adbConnect(bin, target); ok {
			fmt.Printf("%s\n\nYou can unplug the cable now. Use it with:\n  adbmount up -s %s\n", out, target)
			fmt.Println("The setting lasts until the phone reboots; `adbmount wifi usb` reverts it.")
			return nil
		}
	}
	return fail("could not connect to %s: %s\n(the phone and this Mac must be on the same network)", target, out)
}
