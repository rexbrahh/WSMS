// Package faults implements demand page-fault retrieval tools.
package faults

import (
	"context"
	"os"
	"strings"
)

// Request is a page-fault request.
type Request struct {
	Kind   string // page | raw_log | file_slice
	ID     string
	Path   string
	Start  int
	End    int
	Budget int
}

// Tools exposes harness-facing fault helpers.
type Tools struct {
	Resolver *Resolver
}

// ReadPage loads a page by id.
func (t *Tools) ReadPage(ctx context.Context, pageID string, budget int) (string, error) {
	return t.Resolver.Resolve(ctx, Request{Kind: "page", ID: pageID, Budget: budget})
}

// ReadRawLog loads raw event/artifact text by event or failure id.
func (t *Tools) ReadRawLog(ctx context.Context, id string, budget int) (string, error) {
	return t.Resolver.Resolve(ctx, Request{Kind: "raw_log", ID: id, Budget: budget})
}

// ReadFileSlice reads a 1-indexed inclusive line range from path.
func ReadFileSlice(path string, start, end int) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	if start < 1 {
		start = 1
	}
	if end < start {
		end = start
	}
	if start > len(lines) {
		return "", nil
	}
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start-1:end], "\n"), nil
}
