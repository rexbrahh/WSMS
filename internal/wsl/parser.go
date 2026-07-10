package wsl

import (
	"fmt"
	"strconv"
	"strings"

	wsmserrors "wsms/internal/errors"
	"wsms/internal/types"
)

// Parse parses WSL v0 text into records.
func Parse(src string) ([]Record, error) {
	lines := strings.Split(src, "\n")
	var (
		recs    []Record
		curKind Kind
		header  map[string]string
		body    map[string]string
		id      string
		start   int
		flush   bool
	)

	emit := func(endLine int) error {
		if curKind == "" {
			return nil
		}
		rec, err := buildRecord(curKind, id, header, body)
		if err != nil {
			return &wsmserrors.ParseError{Line: start, Message: err.Error()}
		}
		recs = append(recs, rec)
		curKind = ""
		header = nil
		body = nil
		id = ""
		_ = endLine
		return nil
	}

	for i, raw := range lines {
		lineNo := i + 1
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if err := emit(lineNo); err != nil {
				return nil, err
			}
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "@") {
			if err := emit(lineNo); err != nil {
				return nil, err
			}
			kind, rid, attrs, err := parseHeader(trimmed)
			if err != nil {
				return nil, &wsmserrors.ParseError{Line: lineNo, Message: err.Error()}
			}
			curKind = kind
			id = rid
			header = attrs
			body = map[string]string{}
			start = lineNo
			flush = true
			_ = flush
			continue
		}
		if curKind == "" {
			return nil, &wsmserrors.ParseError{Line: lineNo, Message: "body line without record header"}
		}
		k, v, err := parseBodyLine(trimmed)
		if err != nil {
			return nil, &wsmserrors.ParseError{Line: lineNo, Message: err.Error()}
		}
		body[k] = v
	}
	if err := emit(len(lines)); err != nil {
		return nil, err
	}
	return recs, nil
}

func parseHeader(line string) (Kind, string, map[string]string, error) {
	// @type [id] [bare-token...] key=value...
	// Bare tokens (e.g. hard|soft on @constraint) become attrs with empty value.
	parts := strings.Fields(line)
	if len(parts) < 1 {
		return "", "", nil, fmt.Errorf("empty header")
	}
	typeTok := strings.TrimPrefix(parts[0], "@")
	kind := Kind(typeTok)
	if !ValidKinds[kind] {
		return "", "", nil, fmt.Errorf("unknown record type %q", typeTok)
	}
	attrs := map[string]string{}
	id := ""
	start := 1
	if kind != KindNext && len(parts) > 1 && !strings.Contains(parts[1], "=") {
		// First bare token after type is the id (except @next).
		id = parts[1]
		start = 2
	}
	for _, p := range parts[start:] {
		if k, v, ok := strings.Cut(p, "="); ok {
			attrs[k] = v
			continue
		}
		// Bare flag token, e.g. hard / soft
		attrs[p] = ""
	}
	return kind, id, attrs, nil
}

func parseBodyLine(line string) (string, string, error) {
	k, v, ok := strings.Cut(line, ":")
	if !ok {
		return "", "", fmt.Errorf("expected key: value")
	}
	key := strings.TrimSpace(k)
	val := strings.TrimSpace(v)
	if val != "" && (val[0] == '"' || val[0] == '`') {
		unquoted, err := strconv.Unquote(val)
		if err != nil {
			return "", "", fmt.Errorf("invalid quoted value for %s: %w", key, err)
		}
		val = unquoted
	}
	return key, val, nil
}

func buildRecord(kind Kind, id string, header, body map[string]string) (Record, error) {
	switch kind {
	case KindTask:
		if id == "" {
			return nil, fmt.Errorf("task requires id")
		}
		return &TaskRecord{
			IDValue:  id,
			Phase:    header["phase"],
			Priority: types.Priority(header["priority"]),
			Goal:     body["goal"],
			Branch:   body["branch"],
			Commit:   body["commit"],
			Dirty:    body["dirty"],
		}, nil
	case KindConstraint:
		if id == "" {
			return nil, fmt.Errorf("constraint requires id")
		}
		// Support: @constraint C7 hard source=user
		str := headerStrength(header)
		return &ConstraintRecord{
			IDValue:  id,
			Strength: str,
			Source:   types.Source(header["source"]),
			Text:     body["text"],
			Scope:    types.Scope(body["scope"]),
		}, nil
	case KindFailure:
		if id == "" {
			return nil, fmt.Errorf("failure requires id")
		}
		exit := 0
		if body["exit"] != "" {
			n, err := strconv.Atoi(body["exit"])
			if err != nil {
				return nil, fmt.Errorf("invalid exit: %w", err)
			}
			exit = n
		}
		return &FailureRecord{
			IDValue:  id,
			Cmd:      body["cmd"],
			Exit:     exit,
			Err:      body["err"],
			FileHint: body["file_hint"],
			Raw:      body["raw"],
		}, nil
	case KindDecision:
		if id == "" {
			return nil, fmt.Errorf("decision requires id")
		}
		return &DecisionRecord{
			IDValue: id,
			Chosen:  body["chosen"],
			Because: body["because"],
			Refs:    body["refs"],
			Scope:   types.Scope(body["scope"]),
		}, nil
	case KindAvoid:
		if id == "" {
			return nil, fmt.Errorf("avoid requires id")
		}
		return &AvoidRecord{
			IDValue: id,
			Reason:  header["reason"],
			Text:    body["text"],
			Ref:     body["ref"],
		}, nil
	case KindAssumption:
		if id == "" {
			return nil, fmt.Errorf("assumption requires id")
		}
		return &AssumptionRecord{
			IDValue:  id,
			Status:   header["status"],
			Text:     body["text"],
			Evidence: body["evidence"],
		}, nil
	case KindInvalidated:
		if id == "" {
			return nil, fmt.Errorf("invalidated requires id")
		}
		return &InvalidatedRecord{
			IDValue: id,
			Target:  body["target"],
			Reason:  body["reason"],
		}, nil
	case KindNext:
		return &NextRecord{
			Action:   body["action"],
			Target:   body["target"],
			Question: body["question"],
		}, nil
	case KindPage:
		if id == "" {
			return nil, fmt.Errorf("page requires id")
		}
		return &PageRecord{
			IDValue: id,
			KindStr: header["kind"],
			Tier:    header["tier"],
			Summary: body["summary"],
			Refs:    body["refs"],
			Scope:   types.Scope(body["scope"]),
			Branch:  body["branch"],
		}, nil
	case KindFault:
		return &FaultRecord{
			IDValue: id,
			KindStr: firstNonEmpty(header["kind"], body["kind"]),
			Target:  firstNonEmpty(header["target"], body["target"]),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported kind %s", kind)
	}
}

func headerStrength(header map[string]string) types.Strength {
	// @constraint C7 hard source=user  → we need to capture bare "hard"
	// Our header parser only keeps key=value. Bare tokens after id are handled separately in Parse.
	if v, ok := header["strength"]; ok {
		return types.Strength(v)
	}
	if _, ok := header["hard"]; ok {
		return types.StrengthHard
	}
	if _, ok := header["soft"]; ok {
		return types.StrengthSoft
	}
	return types.StrengthSoft
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
