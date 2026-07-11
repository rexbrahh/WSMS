// Command wsms-tui is the demo frontend: a two-pane terminal UI that drives the
// pi harness and renders the live WSMS memory hierarchy from `wsms serve`.
package main

import (
	"flag"
	"fmt"
	"os"

	"wsms/internal/tui"
)

func main() {
	fs := flag.NewFlagSet("wsms-tui", flag.ContinueOnError)
	coreURL := fs.String("core", "http://127.0.0.1:7673", "wsms serve base URL")
	pi := fs.String("pi", "", "pi launch command (e.g. \"pi\" or \"node dist/cli.js\"); empty runs viz-only")
	ext := fs.String("extension", "", "bridge extension path to load into pi via -e")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: wsms-tui [--core URL] [--pi \"<command>\"] [--extension path]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	err := tui.Run(tui.Options{
		CoreURL:   *coreURL,
		Token:     os.Getenv("WSMS_SERVE_TOKEN"),
		PiCommand: *pi,
		Extension: *ext,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "wsms-tui:", err)
		os.Exit(1)
	}
}
