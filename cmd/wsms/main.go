// Command wsms is a thin CLI for parse/lint/capsule helpers.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"wsms/internal/demo"
	"wsms/internal/operator"
	"wsms/internal/renderer"
	"wsms/internal/serve"
	"wsms/internal/wsl"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println("wsms scaffold 0.1.0")
	case "demo":
		if err := runDemo(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "demo:", err)
			os.Exit(1)
		}
	case "serve":
		if err := runServe(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "serve:", err)
			os.Exit(1)
		}
	case "inspect":
		if err := runInspect(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "inspect:", err)
			os.Exit(1)
		}
	case "export":
		if err := runExport(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "export:", err)
			os.Exit(1)
		}
	case "delete":
		if err := runDelete(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "delete:", err)
			os.Exit(1)
		}
	case "purge":
		if err := runPurge(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "purge:", err)
			os.Exit(1)
		}
	case "parse":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: wsms parse <file.wsl>")
			os.Exit(2)
		}
		data, err := os.ReadFile(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		recs, err := wsl.Parse(string(data))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Print(wsl.Serialize(recs))
	case "lint":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: wsms lint <file.wsl>")
			os.Exit(2)
		}
		data, err := os.ReadFile(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		recs, err := wsl.Parse(string(data))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		st := wsl.NewWorkingState()
		if err := st.ApplyAll(recs); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("ok")
	case "capsule":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: wsms capsule <file.wsl>")
			os.Exit(2)
		}
		data, err := os.ReadFile(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		recs, err := wsl.Parse(string(data))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		st := wsl.NewWorkingState()
		if err := st.ApplyAll(recs); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Print(renderer.RenderCapsule(st, 512))
	default:
		usage()
		os.Exit(2)
	}
}

func runDemo(args []string) error {
	flags := flag.NewFlagSet("wsms demo", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	dataDir := flags.String("data-dir", "", "persist the demo ledger and artifacts in this directory")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: wsms demo [--data-dir <directory>]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", flags.Arg(0))
	}
	return demo.Run(context.Background(), os.Stdout, demo.Options{DataDir: *dataDir})
}

func runServe(args []string) error {
	flags := flag.NewFlagSet("wsms serve", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	addr := flags.String("addr", "127.0.0.1:7673", "loopback address to bind (host:port; :0 picks a free port)")
	dataDir := flags.String("data-dir", "", "persist the ledger and artifacts in this directory")
	sessionID := flags.String("session", "", "session id scoping the ledger")
	async := flags.Bool("async-maintenance", false, "apply the L3 warm index asynchronously")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: wsms serve [--addr host:port] [--data-dir dir] [--session id] [--async-maintenance]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", flags.Arg(0))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ready := make(chan string, 1)
	go func() {
		if bound, ok := <-ready; ok {
			fmt.Fprintf(os.Stderr, "wsms serve listening on http://%s\n", bound)
		}
	}()

	// The bearer token stays env-only so no secret is ever baked into a flag,
	// process listing, or committed file.
	return serve.Run(ctx, serve.Options{
		Addr:             *addr,
		DataDir:          *dataDir,
		SessionID:        *sessionID,
		Token:            os.Getenv("WSMS_SERVE_TOKEN"),
		AsyncMaintenance: *async,
		Ready:            ready,
	})
}

func runInspect(args []string) error {
	flags := flag.NewFlagSet("wsms inspect", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	dataDir := flags.String("data-dir", "", "data directory holding the ledger")
	sessionID := flags.String("session", "", "session id scoping the ledger")
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "usage: wsms inspect <%s> [id] [--data-dir dir] [--session id]\n",
			strings.Join(operator.InspectViews, "|"))
		flags.PrintDefaults()
	}
	// Positionals (view, id) may appear before or after flags; the stdlib flag
	// package stops at the first positional, so re-parse past each one.
	pos, err := parseInterspersed(flags, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		flags.Usage()
		return fmt.Errorf("a view is required")
	}
	arg := ""
	if len(pos) > 1 {
		arg = pos[1]
	}
	return operator.Inspect(context.Background(), operator.Options{DataDir: *dataDir, SessionID: *sessionID}, pos[0], arg, os.Stdout)
}

func runExport(args []string) error {
	flags := flag.NewFlagSet("wsms export", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	dataDir := flags.String("data-dir", "", "data directory holding the ledger")
	sessionID := flags.String("session", "", "session id scoping the ledger")
	out := flags.String("out", "", "write JSONL to this file instead of stdout")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: wsms export [--data-dir dir] [--session id] [--out file]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", flags.Arg(0))
	}

	w := os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		w = f
	}
	n, err := operator.Export(context.Background(), operator.Options{DataDir: *dataDir, SessionID: *sessionID}, w)
	if err != nil {
		return err
	}
	if *out != "" {
		fmt.Fprintf(os.Stderr, "exported %d events to %s\n", n, *out)
	}
	return nil
}

func runDelete(args []string) error {
	flags := flag.NewFlagSet("wsms delete", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	dataDir := flags.String("data-dir", "", "data directory holding the ledger")
	sessionID := flags.String("session", "", "session id scoping the ledger")
	kind := flags.String("kind", "", "target kind: record | event | page | path")
	target := flags.String("target", "", "target id or path to invalidate")
	reason := flags.String("reason", "", "reason: superseded | user_rejected | source_deleted | policy_changed | security_revoked")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: wsms delete --kind K --target T --reason R [--data-dir dir] [--session id]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", flags.Arg(0))
	}
	return operator.Delete(context.Background(),
		operator.Options{DataDir: *dataDir, SessionID: *sessionID},
		operator.DeleteSpec{Kind: *kind, Target: *target, Reason: *reason},
		os.Stdout)
}

func runPurge(args []string) error {
	flags := flag.NewFlagSet("wsms purge", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	dataDir := flags.String("data-dir", "", "data directory holding the ledger")
	sessionID := flags.String("session", "", "session id scoping the ledger")
	yes := flags.Bool("yes", false, "confirm the irreversible erasure (omit for a dry run)")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: wsms purge --session id [--data-dir dir] [--yes]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", flags.Arg(0))
	}
	return operator.Purge(context.Background(), operator.Options{DataDir: *dataDir, SessionID: *sessionID}, *yes, os.Stdout)
}

// parseInterspersed parses a flag set that also takes positional arguments,
// allowing flags and positionals in any order (stdlib flag stops at the first
// positional). Returns the positionals in order.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	for fs.NArg() > 0 {
		pos = append(pos, fs.Arg(0))
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return nil, err
		}
	}
	return pos, nil
}

func usage() {
	fmt.Fprintln(os.Stderr, `wsms — Working State Management System CLI

Usage:
  wsms version
  wsms demo [--data-dir <directory>]
  wsms serve [--addr host:port] [--data-dir dir] [--session id] [--async-maintenance]
  wsms inspect <sessions|events|event|state|capsule|page|residency> [id] [--data-dir dir] [--session id]
  wsms export [--data-dir dir] [--session id] [--out file]
  wsms delete --kind K --target T --reason R [--data-dir dir] [--session id]
  wsms purge --session id [--data-dir dir] [--yes]
  wsms parse <file.wsl>
  wsms lint <file.wsl>
  wsms capsule <file.wsl>`)
}
