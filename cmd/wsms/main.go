// Command wsms is a thin CLI for parse/lint/capsule helpers.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"wsms/internal/demo"
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

func usage() {
	fmt.Fprintln(os.Stderr, `wsms — Working State Management System CLI

Usage:
  wsms version
  wsms demo [--data-dir <directory>]
  wsms serve [--addr host:port] [--data-dir dir] [--session id] [--async-maintenance]
  wsms parse <file.wsl>
  wsms lint <file.wsl>
  wsms capsule <file.wsl>`)
}
