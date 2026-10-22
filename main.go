package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
)

// version is set at release time with -ldflags "-X main.version=v1.2.3".
var version = ""

func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

type command struct {
	name  string
	usage string // arguments, for the help text
	short string
	run   func(args []string) error
}

func commands() []command {
	return []command{
		{"up", "[--ro] [--only android|sdcard] [-s SERIAL]", "mount internal storage and SD card", runUp},
		{"down", "[android|sdcard ...]", "unmount (everything by default)", runDown},
		{"status", "", "show what is mounted", runStatus},
		{"log", "[-f] android|sdcard", "show the daemon log of a mount", runLog},
		{"devices", "", "list connected phones with their storage volumes", runDevices},
		{"wifi", "enable|connect|pair|disconnect|usb|scan", "use adb over Wi-Fi instead of a cable", runWifi},
		{"ls", "[-l] [-s SERIAL] [PATH]", "list a directory on the phone", runLs},
		{"pull", "[-n] [-f] [-j N] REMOTE... LOCAL", "copy from the phone, skipping unchanged files", runPull},
		{"push", "[-n] [-f] [-j N] LOCAL... REMOTE", "copy to the phone, skipping unchanged files", runPush},
		{"doctor", "", "check that everything needed is installed and working", runDoctor},
		{"serve", "-root DIR -mount DIR [...]", "run the filesystem daemon in the foreground (used by up)", runServe},
		{"version", "", "print the version", func([]string) error { fmt.Println("adbmount", buildVersion()); return nil }},
	}
}

func usage() {
	var b strings.Builder
	b.WriteString("adbmount: mount and manage an Android phone from macOS over adb\n\nUsage: adbmount <command> [options]\n\nCommands:\n")
	for _, c := range commands() {
		fmt.Fprintf(&b, "  %-9s %s\n", c.name, c.short)
	}
	b.WriteString("\nRun `adbmount <command> -h` for the options of a command.\n")
	b.WriteString("PATH arguments accept `sd:` (SD card root) and `int:` (internal storage) prefixes.\n")
	b.WriteString("Select a device with -s SERIAL or ANDROID_SERIAL; Wi-Fi devices look like 192.168.1.20:5555.\n")
	fmt.Fprint(os.Stderr, b.String())
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	name := os.Args[1]
	switch name {
	case "-h", "--help", "help":
		usage()
		return
	case "-v", "--version":
		name = "version"
	}
	for _, c := range commands() {
		if c.name != name {
			continue
		}
		if err := c.run(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "adbmount %s: %v\n", name, err)
			os.Exit(1)
		}
		return
	}
	fmt.Fprintf(os.Stderr, "adbmount: unknown command %q\n\n", name)
	usage()
	os.Exit(2)
}
