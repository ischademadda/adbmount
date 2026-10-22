package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
)

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func (a *adb) volumeUsage(p string) string {
	bsize, blocks, _, bavail, err := a.statfs(p)
	if err != nil || blocks == 0 {
		return "?"
	}
	return fmt.Sprintf("%s free of %s", humanSize(int64(bavail*bsize)), humanSize(int64(blocks*bsize)))
}

func runDevices(args []string) error {
	if _, err := parseInterspersed(flagSetFor("devices"), args); err != nil {
		return err
	}
	bin, err := adbPath()
	if err != nil {
		return err
	}
	devs, err := listDevices(bin)
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		fmt.Println("No devices. Plug in the phone with USB debugging on, or run `adbmount wifi connect HOST`.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SERIAL\tVIA\tSTATE\tDEVICE")
	for _, d := range devs {
		desc := d.model()
		if d.State == "device" {
			a := &adb{bin: bin, serial: d.Serial}
			if man, model, rel := a.props(); model != "" {
				desc = strings.TrimSpace(man+" "+model) + ", Android " + rel
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Serial, d.transport(), d.State, desc)
	}
	tw.Flush()
	for _, d := range devs {
		if d.State != "device" {
			continue
		}
		a := &adb{bin: bin, serial: d.Serial}
		fmt.Printf("\n%s\n  internal  /sdcard  %s\n", d.Serial, a.volumeUsage("/sdcard"))
		if sd := a.sdCardRoot(); sd != "" {
			fmt.Printf("  SD card   %s  %s\n", sd, a.volumeUsage(sd))
		}
	}
	return nil
}

// ---- doctor ------------------------------------------------------------------

type checker struct{ bad int }

func (c *checker) ok(format string, a ...any)   { fmt.Printf("  ✓ %s\n", fmt.Sprintf(format, a...)) }
func (c *checker) warn(format string, a ...any) { fmt.Printf("  ! %s\n", fmt.Sprintf(format, a...)) }
func (c *checker) bad_(format string, a ...any) {
	c.bad++
	fmt.Printf("  ✗ %s\n", fmt.Sprintf(format, a...))
}

func fuseTInstalled() bool {
	for _, p := range []string{"/usr/local/lib/libfuse-t.dylib", "/Library/Application Support/fuse-t", "/Applications/fuse-t.app"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

func runDoctor(args []string) error {
	if _, err := parseInterspersed(flagSetFor("doctor"), args); err != nil {
		return err
	}
	var c checker
	fmt.Println("adbmount", buildVersion())

	bin, err := adbPath()
	if err != nil {
		c.bad_("%v", err)
	} else {
		v, _ := exec.Command(bin, "version").Output()
		first, _, _ := strings.Cut(strings.TrimSpace(string(v)), "\n")
		c.ok("%s (%s)", first, bin)
	}

	if fuseTInstalled() {
		c.ok("FUSE-T is installed")
	} else {
		c.bad_("FUSE-T not found: brew install --cask fuse-t")
	}

	if bin != "" {
		devs, err := listDevices(bin)
		switch {
		case err != nil:
			c.bad_("%v", err)
		case len(devs) == 0:
			c.bad_("no devices: plug in the phone with USB debugging on, or `adbmount wifi connect HOST`")
		}
		for _, d := range devs {
			if d.State != "device" {
				c.bad_("%s is %s%s", d.Serial, d.State, stateHint(d.State))
				continue
			}
			c.ok("%s (%s) is ready", d.Serial, d.transport())
			a := &adb{bin: bin, serial: d.Serial}
			if _, err := a.stat("/sdcard"); err != nil {
				c.bad_("%s: cannot read /sdcard: %v (storage permission? phone locked?)", d.Serial, err)
			}
			if sd := a.sdCardRoot(); sd != "" {
				c.ok("%s: SD card at %s", d.Serial, sd)
			} else {
				c.warn("%s: no SD card detected (only internal storage will be mounted)", d.Serial)
			}
			if d.isNetwork() {
				c.warn("%s runs over Wi-Fi: expect lower and less steady throughput than USB", d.Serial)
			}
		}
	}

	if err := os.MkdirAll(mountBase(), 0o755); err != nil {
		c.bad_("cannot create mount directory %s: %v", mountBase(), err)
	} else {
		c.ok("mount directory %s", mountBase())
	}
	for _, n := range knownMounts() {
		if s, ok := loadState(n); ok && !isMounted(s.Mount) {
			c.warn("%s: stale state (daemon pid %d, not mounted); `adbmount down` cleans it", n, s.Pid)
		}
	}
	if c.bad > 0 {
		return fail("%d problem(s) found", c.bad)
	}
	fmt.Println("All good.")
	return nil
}
