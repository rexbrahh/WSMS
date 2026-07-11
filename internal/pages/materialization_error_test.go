package pages

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"wsms/internal/artifacts"
	wsmserrors "wsms/internal/errors"
	"wsms/internal/ledger"
)

type errorEventReader struct{ err error }

func (r errorEventReader) Get(context.Context, string) (ledger.Event, error) {
	return ledger.Event{}, r.err
}

type errorArtifactReader struct{ err error }

func (r errorArtifactReader) VerifyArtifact(context.Context, string) error { return r.err }

type errorFileReader struct{ err error }

func (r errorFileReader) ReadFileSlice(context.Context, string, string, int, int) ([]byte, error) {
	return nil, r.err
}

func TestResolveRefSeparatesMissingEvidenceFromAuthorityReadFailure(t *testing.T) {
	operational := errors.New("authoritative store unavailable")
	change := LedgerChange{Event: ledger.Event{ID: "E0002", SessionID: "session", Seq: 2}}

	tests := []struct {
		name        string
		change      LedgerChange
		ref         PageRef
		wantCause   error
		wantMissing bool
	}{
		{
			name: "ledger operational", change: func() LedgerChange {
				c := change
				c.Events = errorEventReader{err: operational}
				return c
			}(), ref: PageRef{Kind: RefEvent, ID: "E0001"}, wantCause: operational,
		},
		{
			name: "ledger missing", change: func() LedgerChange {
				c := change
				c.Events = errorEventReader{err: wsmserrors.ErrNotFound}
				return c
			}(), ref: PageRef{Kind: RefEvent, ID: "E0001"}, wantMissing: true,
		},
		{
			name: "artifact operational", change: func() LedgerChange {
				c := change
				c.Artifacts = errorArtifactReader{err: operational}
				return c
			}(), ref: PageRef{Kind: RefArtifact, ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, wantCause: operational,
		},
		{
			name: "artifact missing", change: func() LedgerChange {
				c := change
				c.Artifacts = errorArtifactReader{err: artifacts.ErrArtifactNotFound}
				return c
			}(), ref: PageRef{Kind: RefArtifact, ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, wantMissing: true,
		},
		{
			name: "file operational", change: func() LedgerChange {
				c := change
				c.Files = errorFileReader{err: operational}
				return c
			}(), ref: PageRef{Kind: RefFileSlice, Path: "a.go", Commit: "abc1234", StartLine: 1, EndLine: 1}, wantCause: operational,
		},
		{
			name: "file missing", change: func() LedgerChange {
				c := change
				c.Files = errorFileReader{err: fs.ErrNotExist}
				return c
			}(), ref: PageRef{Kind: RefFileSlice, Path: "a.go", Commit: "abc1234", StartLine: 1, EndLine: 1}, wantMissing: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveRef(context.Background(), tt.change, tt.ref)
			if tt.wantMissing {
				if !errors.Is(err, ErrUnmaterializableRef) {
					t.Fatalf("error=%v, want missing-evidence classification", err)
				}
				return
			}
			if !errors.Is(err, tt.wantCause) || errors.Is(err, ErrUnmaterializableRef) {
				t.Fatalf("error=%v, want operational cause %v", err, tt.wantCause)
			}
		})
	}
}

func TestResolveRefPreservesCancellationAsOperational(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := resolveRef(ctx, LedgerChange{}, PageRef{Kind: RefEvent, ID: "E0001"})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnmaterializableRef) {
		t.Fatalf("cancellation error=%v", err)
	}
}
