package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"wsms/internal/pirpc"
)

// Options configures a TUI run.
type Options struct {
	CoreURL   string // wsms serve base URL
	Token     string // optional bearer token for the core
	PiCommand string // pi launch command (space-split); empty runs viz-only
	Extension string // bridge extension path to load into pi via -e
	Provider  string // optional pi --provider (e.g. the offline mock)
	Model     string // optional pi --model
}

// Run starts the TUI. If PiCommand is set it spawns that command in pi RPC mode;
// otherwise the chat pane is inert and only the live memory panels update, which
// is the path used before a real pi runtime is installed.
func Run(opts Options) error {
	core := newCoreClient(opts.CoreURL, opts.Token)

	var a agent
	if cmd := strings.TrimSpace(opts.PiCommand); cmd != "" {
		fields := strings.Fields(cmd)
		args := append([]string{}, fields[1:]...)
		if opts.Extension != "" {
			args = append(args, "-e", opts.Extension)
		}
		if opts.Provider != "" {
			args = append(args, "--provider", opts.Provider)
		}
		if opts.Model != "" {
			args = append(args, "--model", opts.Model)
		}
		args = append(args, "--mode", "rpc")
		client, err := pirpc.Spawn(context.Background(), fields[0], args...)
		if err != nil {
			return fmt.Errorf("spawn pi: %w", err)
		}
		a = client
	}

	model := newModel(a, core)
	model.modelName = opts.Model
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := program.Run()
	if a != nil {
		_ = a.Close()
	}
	return err
}
