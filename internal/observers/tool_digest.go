package observers

import (
	"context"
	"regexp"
	"strings"

	"wsms/internal/ledger"
	"wsms/internal/wsl"
)

var (
	fileHintRE = regexp.MustCompile(`([A-Za-z0-9_./\\-]+\.[A-Za-z0-9]+):(\d+)(?:-(\d+))?`)
	errLineRE  = regexp.MustCompile(`(?i)(error:|FAIL\b|panic:|fatal:|--- FAIL)`)
)

// ToolDigest extracts @failure records from command/tool outputs.
type ToolDigest struct {
	IDs IDGen
}

func (o *ToolDigest) Name() string { return "tool_digest" }

func (o *ToolDigest) Handle(ctx context.Context, ev ledger.Event) ([]wsl.Update, error) {
	_ = ctx
	switch ev.Type {
	case ledger.EventCommandOutput, ledger.EventTestResult, ledger.EventToolResult:
	default:
		return nil, nil
	}

	exit := ev.PayloadInt("exit", 0)
	if exit == 0 {
		// success — no failure record
		return nil, nil
	}

	cmd := ev.PayloadString("cmd")
	if cmd == "" {
		cmd = ev.PayloadString("command")
	}
	rawOut := ev.PayloadString("output")
	errSig := ev.PayloadString("err")
	if errSig == "" {
		errSig = extractErrorSignature(rawOut)
	}
	if errSig == "" {
		errSig = "command failed"
	}
	hint := ev.PayloadString("file_hint")
	if hint == "" {
		hint = extractFileHint(rawOut)
	}
	raw := ev.PayloadString("raw")
	if raw == "" && ev.ArtifactHash != "" {
		raw = "artifact:sha256:" + ev.ArtifactHash
	}

	id := o.IDs.Next("F")
	rec := &wsl.FailureRecord{
		IDValue:  id,
		Cmd:      cmd,
		Exit:     exit,
		Err:      errSig,
		FileHint: hint,
		Raw:      raw,
	}
	return []wsl.Update{{Op: "upsert", Record: rec}}, nil
}

func extractErrorSignature(output string) string {
	if output == "" {
		return ""
	}
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if errLineRE.MatchString(line) {
			if len(line) > 200 {
				return line[:200]
			}
			return line
		}
	}
	// fallback: first non-empty line
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			if len(line) > 200 {
				return line[:200]
			}
			return line
		}
	}
	return ""
}

func extractFileHint(output string) string {
	m := fileHintRE.FindStringSubmatch(output)
	if m == nil {
		return ""
	}
	if m[3] != "" {
		return m[1] + ":" + m[2] + "-" + m[3]
	}
	return m[1] + ":" + m[2]
}
