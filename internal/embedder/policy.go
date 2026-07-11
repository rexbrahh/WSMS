package embedder

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	DefaultMaxBatchSize            = 32
	DefaultMaxDocumentBytes        = 16 * 1024
	DefaultMaxQueryBytes           = 4 * 1024
	DefaultMaxDocumentPayloadBytes = DefaultMaxDocumentBytes + MaxTemplateBytes + 1
	DefaultMaxQueryPayloadBytes    = DefaultMaxQueryBytes + MaxInstructionBytes + 1
	DefaultMaxDimensions           = 8192
	CurrentRedactionVersion        = "redaction/v0.1.0"
)

var (
	// ErrAdmissionDenied reports text that must not be embedded or cached.
	ErrAdmissionDenied = errors.New("embedding admission denied")
	// ErrBatchTooLarge reports a batch exceeding the configured bound.
	ErrBatchTooLarge = errors.New("embedding batch too large")
)

var (
	privateKeyRE = regexp.MustCompile(`(?i)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	openAIKeyRE  = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`)
	githubPATRE  = regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9_]{20,}\b`)
	awsKeyRE     = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)
	secretKVRE   = regexp.MustCompile(`(?i)\b(password|passwd|api[_-]?key|secret|token)\s*[:=]\s*['"]?[^'"\s]{8,}`)
	base64BlobRE = regexp.MustCompile(`\b[A-Za-z0-9+/]{512,}={0,2}\b`)
)

// AdmissionPolicy decides which text may be embedded.
type AdmissionPolicy struct {
	MaxDocumentBytes        int
	MaxQueryBytes           int
	MaxDocumentPayloadBytes int
	MaxQueryPayloadBytes    int
	DeniedPathMarkers       []string
}

// DefaultAdmissionPolicy returns the conservative local-first Phase 7D policy.
func DefaultAdmissionPolicy() AdmissionPolicy {
	return AdmissionPolicy{
		MaxDocumentBytes:        DefaultMaxDocumentBytes,
		MaxQueryBytes:           DefaultMaxQueryBytes,
		MaxDocumentPayloadBytes: DefaultMaxDocumentPayloadBytes,
		MaxQueryPayloadBytes:    DefaultMaxQueryPayloadBytes,
		DeniedPathMarkers: []string{
			"/.env", "\\.env", "\n.env", "path: .env", "path:.env",
			".ssh/", ".ssh\\", "id_rsa", "id_ed25519",
			"/secrets/", "\\secrets\\", "/private_keys/", "\\private_keys\\",
			"keychain/login.keychain", "credentials.json",
		},
	}
}

func (p AdmissionPolicy) withDefaults() AdmissionPolicy {
	if p.MaxDocumentBytes <= 0 {
		p.MaxDocumentBytes = DefaultMaxDocumentBytes
	}
	if p.MaxQueryBytes <= 0 {
		p.MaxQueryBytes = DefaultMaxQueryBytes
	}
	if p.MaxDocumentPayloadBytes <= 0 {
		p.MaxDocumentPayloadBytes = DefaultMaxDocumentPayloadBytes
	}
	if p.MaxQueryPayloadBytes <= 0 {
		p.MaxQueryPayloadBytes = DefaultMaxQueryPayloadBytes
	}
	if len(p.DeniedPathMarkers) == 0 {
		p.DeniedPathMarkers = DefaultAdmissionPolicy().DeniedPathMarkers
	}
	return p
}

func (p AdmissionPolicy) canonicalDocument(text string) (string, error) {
	p = p.withDefaults()
	return p.canonicalize(text, p.MaxDocumentBytes, RoleDocument)
}

func (p AdmissionPolicy) canonicalQuery(text string) (string, error) {
	p = p.withDefaults()
	return p.canonicalize(text, p.MaxQueryBytes, RoleQuery)
}

func (p AdmissionPolicy) validatePayload(payload string, role Role) error {
	p = p.withDefaults()
	limit := p.MaxDocumentPayloadBytes
	if role == RoleQuery {
		limit = p.MaxQueryPayloadBytes
	}
	if payload == "" {
		return fmt.Errorf("%w: empty payload", ErrAdmissionDenied)
	}
	if len(payload) > limit {
		return fmt.Errorf("%w: %s payload exceeds %d bytes", ErrAdmissionDenied, role, limit)
	}
	if err := admitCanonicalText(payload, p, role); err != nil {
		return err
	}
	return nil
}

func (p AdmissionPolicy) canonicalize(text string, maxBytes int, role Role) (string, error) {
	if err := admitRawText(text, p, role); err != nil {
		return "", err
	}
	canonical := collapseWhitespace(strings.TrimSpace(text))
	if canonical == "" {
		return "", fmt.Errorf("%w: empty text", ErrAdmissionDenied)
	}
	if len(canonical) > maxBytes {
		return "", fmt.Errorf("%w: text exceeds %d bytes", ErrAdmissionDenied, maxBytes)
	}
	if err := admitCanonicalText(canonical, p, role); err != nil {
		return "", err
	}
	return canonical, nil
}

func admitRawText(text string, policy AdmissionPolicy, role Role) error {
	if !utf8.ValidString(text) {
		return fmt.Errorf("%w: invalid utf-8", ErrAdmissionDenied)
	}
	if strings.ContainsRune(text, 0) {
		return fmt.Errorf("%w: binary artifact marker", ErrAdmissionDenied)
	}
	return admitCanonicalText(text, policy, role)
}

func admitCanonicalText(text string, policy AdmissionPolicy, role Role) error {
	lower := strings.ToLower(text)
	if privateKeyRE.MatchString(text) || openAIKeyRE.MatchString(text) || githubPATRE.MatchString(text) ||
		awsKeyRE.MatchString(text) || secretKVRE.MatchString(text) {
		return fmt.Errorf("%w: secret-like content", ErrAdmissionDenied)
	}
	if strings.Contains(lower, "top_secret_raw_output") ||
		strings.Contains(lower, "sensitive-marker") ||
		strings.Contains(lower, "sensitive_marker") ||
		strings.Contains(lower, "sensitive marker") {
		return fmt.Errorf("%w: sensitive marker", ErrAdmissionDenied)
	}
	for _, marker := range policy.DeniedPathMarkers {
		if marker != "" && strings.Contains(lower, strings.ToLower(marker)) {
			return fmt.Errorf("%w: denied path marker", ErrAdmissionDenied)
		}
	}
	if looksLikeRawTranscript(lower) {
		return fmt.Errorf("%w: raw transcript", ErrAdmissionDenied)
	}
	if looksLikeArtifact(lower, text) {
		return fmt.Errorf("%w: artifact-like content", ErrAdmissionDenied)
	}
	_ = role
	return nil
}

func looksLikeRawTranscript(lower string) bool {
	if strings.Contains(lower, "begin transcript") || strings.Contains(lower, "<transcript") {
		return true
	}
	lines := strings.Split(lower, "\n")
	var roleLines int
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "user:") || strings.HasPrefix(line, "assistant:") ||
			strings.HasPrefix(line, "system:") || strings.HasPrefix(line, "tool:") {
			roleLines++
		}
	}
	return roleLines >= 3
}

func looksLikeArtifact(lower, text string) bool {
	if strings.Contains(lower, "begin artifact") ||
		strings.Contains(lower, "artifact bytes") ||
		strings.Contains(lower, "content-transfer-encoding: base64") {
		return true
	}
	return base64BlobRE.MatchString(text)
}

func collapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
			continue
		}
		b.WriteRune(r)
		inSpace = false
	}
	return strings.TrimSpace(b.String())
}

func documentPayload(profile NamespaceProfile, canonical string) string {
	template := strings.TrimSpace(profile.DocumentTemplate)
	switch {
	case strings.Contains(template, "{{.SearchText}}"):
		return strings.ReplaceAll(template, "{{.SearchText}}", canonical)
	case strings.Contains(template, "{{search_text}}"):
		return strings.ReplaceAll(template, "{{search_text}}", canonical)
	default:
		return template + "\n" + canonical
	}
}

func queryPayload(profile NamespaceProfile, canonical string) string {
	instruction := strings.TrimSpace(profile.QueryInstruction)
	if strings.HasSuffix(instruction, "\nQuery:") {
		return instruction + " " + canonical
	}
	return instruction + "\n" + canonical
}
