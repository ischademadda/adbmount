package main

import (
	"flag"
	"fmt"
	"os"
)

func flagSetFor(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("adbmount "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// parseInterspersed parses flags that may appear before, between or after
// positional arguments (the stdlib stops at the first positional).
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// serialFlag registers -s/--serial, defaulting to $ANDROID_SERIAL.
func serialFlag(fs *flag.FlagSet) *string {
	s := fs.String("s", os.Getenv("ANDROID_SERIAL"), "device serial (see `adbmount devices`)")
	fs.StringVar(s, "serial", *s, "same as -s")
	return s
}

func fail(format string, a ...any) error { return fmt.Errorf(format, a...) }
