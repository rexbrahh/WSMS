package pages_test

import (
	"strings"
	"testing"
	"time"

	"wsms/internal/pages"
	"wsms/internal/types"
)

func TestAuthorityDescriptorValidationMatchesPageMutationAdmission(t *testing.T) {
	base := pages.WarmPage{
		ID: pages.PageID("wp_" + strings.Repeat("d", 32)), Version: 1, SessionID: "descriptor-parity",
		RepoID: "repo", Branch: "main", Commit: "abcdef1", PathScope: []string{"src/z.go", "src/a.go"},
		Scope: types.ScopeFile, Kind: pages.KindFileContext, Trust: pages.TrustRepo, Status: pages.StatusActive,
		Salience: 0.5, SalienceReason: "descriptor parity",
		SearchText: "kind=file_context descriptor parity", Summary: "descriptor parity",
		Refs:         []pages.PageRef{{Kind: pages.RefEvent, ID: "EParity"}},
		SourceSeqMin: 1, SourceSeqMax: 1, SourceDigest: pages.SourceDigest(strings.Repeat("d", 64)),
		CompilerVersion: pages.CurrentCompilerVersion, CreatedAt: time.Unix(1, 0).UTC(),
	}
	tests := []struct {
		name   string
		mutate func(*pages.WarmPage)
	}{
		{name: "valid unsorted unique paths", mutate: func(*pages.WarmPage) {}},
		{name: "unknown kind", mutate: func(page *pages.WarmPage) { page.Kind = pages.PageKind("unknown") }},
		{name: "unknown trust", mutate: func(page *pages.WarmPage) { page.Trust = pages.Trust("unknown") }},
		{name: "invalid kind trust pairing", mutate: func(page *pages.WarmPage) { page.Kind = pages.KindFailureEpisode }},
		{name: "unknown scope", mutate: func(page *pages.WarmPage) { page.Scope = types.Scope("unknown") }},
		{name: "noncanonical path", mutate: func(page *pages.WarmPage) { page.PathScope = []string{"../escape"} }},
		{name: "duplicate path", mutate: func(page *pages.WarmPage) { page.PathScope = []string{"src/a.go", "src/a.go"} }},
		{name: "malformed branch", mutate: func(page *pages.WarmPage) { page.Branch = " main" }},
		{name: "malformed commit", mutate: func(page *pages.WarmPage) { page.Commit = "bad\ncommit" }},
		{name: "malformed ref", mutate: func(page *pages.WarmPage) { page.Refs = []pages.PageRef{{Kind: pages.RefEvent, ID: "bad/id"}} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			page := base
			page.PathScope = append([]string(nil), base.PathScope...)
			page.Refs = append([]pages.PageRef(nil), base.Refs...)
			test.mutate(&page)
			descriptorErr := pages.ValidateAuthorityDescriptor(
				page.Kind, page.Trust, page.Scope, page.RepoID, page.TaskID, page.Branch, page.Commit, page.PathScope, page.Refs,
			)
			mutationErr := (pages.PageMutation{Op: pages.MutationUpsert, Page: page}).Validate()
			if (descriptorErr == nil) != (mutationErr == nil) {
				t.Fatalf("descriptor error=%v mutation error=%v", descriptorErr, mutationErr)
			}
		})
	}
}
