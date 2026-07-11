package indexer_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"wsms/internal/indexer"
	"wsms/internal/pages"
)

func TestAuthorityCompleteEmptyIsHealthyEmptyInBothChannels(t *testing.T) {
	idx := openDenseIndex(t, 2)
	ctx := context.Background()
	page := indexedSearchPage(t, 1, "authority-empty", "authority empty needle", []string{"src/empty.go"}, []pages.PageRef{{Kind: pages.RefEvent, ID: "EEmpty"}})
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{denseRecord(page, "", []float64{1, 0})}); err != nil {
		t.Fatal(err)
	}

	query := indexer.SearchQuery{
		SessionID: "authority-empty", Text: "authority empty needle", Limit: 5,
		EligibilityComplete: true,
	}
	lexical, err := idx.SearchLexical(ctx, query)
	if err != nil || len(lexical) != 0 {
		t.Fatalf("complete-empty lexical=%#v err=%v", lexical, err)
	}
	dense, err := idx.SearchDense(ctx, query, []float64{1, 0})
	if err != nil || len(dense) != 0 {
		t.Fatalf("complete-empty dense=%#v err=%v", dense, err)
	}

	query.EligibilityComplete = false
	legacy, err := idx.SearchLexical(ctx, query)
	if err != nil || len(legacy) != 1 || legacy[0].Page.ID != page.ID {
		t.Fatalf("legacy inspection lexical=%#v err=%v", legacy, err)
	}
	query.EligiblePageTuples = []indexer.PageTuple{pageTuple(page)}
	if _, err := idx.SearchLexical(ctx, query); !errors.Is(err, indexer.ErrInvalidPageTuple) {
		t.Fatalf("incomplete eligibility error=%v", err)
	}
}

func TestExactAuthorityAppliesBeforeLexicalLimitAndDenseKNN(t *testing.T) {
	idx := openDenseIndex(t, 2)
	ctx := context.Background()
	const session = "authority-prelimit"

	mutations := make([]pages.PageMutation, 0, 122)
	vectors := make([]indexer.VectorRecord, 0, 122)
	for i := 1; i <= 121; i++ {
		page := indexedSearchPage(t, i, session, "exact authority starvation needle", []string{"src/stale.go"}, []pages.PageRef{{Kind: pages.RefEvent, ID: fmt.Sprintf("EStale%d", i)}})
		page.ScopeEpoch = 73 // deliberately collides with the authorized path epoch
		mutations = append(mutations, pages.PageMutation{Op: pages.MutationUpsert, Page: page})
		vectors = append(vectors, denseRecord(page, "", []float64{1, 0}))
	}
	target := indexedSearchPage(t, 122, session, "exact authority starvation needle", []string{"src/current.go"}, []pages.PageRef{
		{Kind: pages.RefArtifact, ID: strings.Repeat("f", 64)},
		{Kind: pages.RefEvent, ID: "ECurrent"},
		{Kind: pages.RefWSLRecord, ID: "WCurrent"},
	})
	target.ScopeEpoch = 73
	mutations = append(mutations, pages.PageMutation{Op: pages.MutationUpsert, Page: target})
	vectors = append(vectors, denseRecord(target, "", []float64{0, 1}))
	if err := idx.ApplyWithWatermark(ctx, mutations, session, 1, int64(target.Version)); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, vectors); err != nil {
		t.Fatal(err)
	}

	snapshot, err := idx.ActivePageSnapshot(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ServingGeneration <= 0 || snapshot.Watermark != (indexer.SearchWatermark{SessionID: session, LastSourceSeq: 1, LastPageVersion: target.Version}) {
		t.Fatalf("snapshot metadata=%#v", snapshot)
	}
	if len(snapshot.Pages) != len(mutations) {
		t.Fatalf("snapshot pages=%d want %d", len(snapshot.Pages), len(mutations))
	}
	var authority indexer.PageAuthority
	for _, page := range snapshot.Pages {
		if page.Tuple.PageID == target.ID {
			authority = page
			break
		}
	}
	if authority.Tuple.PageID == "" {
		t.Fatal("target missing from active page snapshot")
	}
	if strings.Join(authority.RefIDs, ",") != "ECurrent,WCurrent" {
		t.Fatalf("logical refs=%v", authority.RefIDs)
	}
	if authority.Scope != target.Scope || authority.Branch != target.Branch || authority.Commit != target.Commit ||
		strings.Join(authority.PathScope, ",") != strings.Join(target.PathScope, ",") {
		t.Fatalf("authority descriptor=%#v page=%#v", authority, target)
	}

	query := indexer.SearchQuery{
		SessionID: session, Text: "exact authority starvation needle", Limit: 1,
		EligibilityComplete: true, EligiblePageTuples: []indexer.PageTuple{authority.Tuple},
	}
	lexical, err := idx.SearchLexical(ctx, query)
	if err != nil || len(lexical) != 1 || lexical[0].Page.ID != target.ID {
		t.Fatalf("prelimit lexical=%#v err=%v", lexical, err)
	}
	if !strings.Contains(lexical[0].Explanation, "page-authority") {
		t.Fatalf("lexical explanation=%q", lexical[0].Explanation)
	}
	dense, err := idx.SearchDense(ctx, query, []float64{1, 0})
	if err != nil || len(dense) != 1 || dense[0].Page.ID != target.ID {
		t.Fatalf("pre-KNN dense=%#v err=%v", dense, err)
	}
	if dense[0].Tuple.ScopeEpoch != target.ScopeEpoch {
		t.Fatalf("dense tuple=%#v", dense[0].Tuple)
	}
}

func TestExactAuthorityRejectsUpdatedTupleAndTreatsJSONAsData(t *testing.T) {
	idx := openDenseIndex(t, 2)
	ctx := context.Background()
	const session = "authority-update"
	page := indexedSearchPage(t, 1, session, "tuple update needle", []string{"src/update.go"}, []pages.PageRef{{Kind: pages.RefEvent, ID: "EUpdate"}})
	page.ScopeEpoch = 11
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{denseRecord(page, "", []float64{1, 0})}); err != nil {
		t.Fatal(err)
	}
	stale := pageTuple(page)

	updated := page
	updated.Version = 2
	updated.SourceSeqMin, updated.SourceSeqMax = 2, 2
	updated.SourceDigest = pages.SourceDigest(strings.Repeat("2", 64))
	updated.ScopeEpoch = 12
	updated.SearchText = "kind=" + string(updated.Kind) + " tuple update needle"
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: updated}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{denseRecord(updated, "", []float64{1, 0})}); err != nil {
		t.Fatal(err)
	}

	query := indexer.SearchQuery{
		SessionID: session, Text: "tuple update needle", Limit: 5,
		EligibilityComplete: true, EligiblePageTuples: []indexer.PageTuple{stale},
	}
	for channel, search := range map[string]func() ([]indexer.Candidate, error){
		"lexical": func() ([]indexer.Candidate, error) { return idx.SearchLexical(ctx, query) },
		"dense":   func() ([]indexer.Candidate, error) { return idx.SearchDense(ctx, query, []float64{1, 0}) },
	} {
		hits, err := search()
		if err != nil || len(hits) != 0 {
			t.Fatalf("%s stale tuple hits=%#v err=%v", channel, hits, err)
		}
	}

	injected := pageTuple(updated)
	injected.SourceDigest = pages.SourceDigest(`x' OR 1=1 --`)
	query.EligiblePageTuples = []indexer.PageTuple{injected}
	hits, err := idx.SearchLexical(ctx, query)
	if !errors.Is(err, indexer.ErrInvalidPageTuple) || len(hits) != 0 {
		t.Fatalf("injected tuple was not safely rejected: hits=%#v err=%v", hits, err)
	}
	query.EligiblePageTuples = []indexer.PageTuple{pageTuple(updated)}
	hits, err = idx.SearchLexical(ctx, query)
	if err != nil || len(hits) != 1 || hits[0].Page.ID != updated.ID {
		t.Fatalf("current tuple hits=%#v err=%v", hits, err)
	}
}

func TestActivePageSnapshotHonorsCancellation(t *testing.T) {
	idx := openTestIndex(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := idx.ActivePageSnapshot(ctx, "cancelled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("snapshot cancellation error=%v", err)
	}
}

func TestActivePageSnapshotAndExactSearchRaceWithPageUpdates(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()
	const session = "authority-race"
	page := indexedSearchPage(t, 1, session, "authority race needle", []string{"src/race.go"}, []pages.PageRef{{Kind: pages.RefEvent, ID: "ERace"}})
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for version := 2; version <= 40; version++ {
			updated := page
			updated.Version = pages.PageVersion(version)
			updated.SourceSeqMin, updated.SourceSeqMax = int64(version), int64(version)
			updated.SourceDigest = pages.SourceDigest(fmt.Sprintf("%064x", 10_000+version))
			updated.ScopeEpoch = pages.ScopeEpoch(version % 7)
			if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: updated}}); err != nil {
				errCh <- err
				return
			}
		}
	}()
	close(start)
	defer wg.Wait()

	for i := 0; i < 40; i++ {
		snapshot, err := idx.ActivePageSnapshot(ctx, session)
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Pages) != 1 {
			t.Fatalf("snapshot pages=%d", len(snapshot.Pages))
		}
		tuple := snapshot.Pages[0].Tuple
		hits, err := idx.SearchLexical(ctx, indexer.SearchQuery{
			SessionID: session, Text: "authority race needle", Limit: 1,
			EligibilityComplete: true, EligiblePageTuples: []indexer.PageTuple{tuple},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) > 1 || (len(hits) == 1 && hits[0].Tuple != tuple) {
			t.Fatalf("exact race result crossed tuple: snapshot=%#v hits=%#v", tuple, hits)
		}
	}
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}
}

func pageTuple(page pages.WarmPage) indexer.PageTuple {
	return indexer.PageTuple{
		PageID: page.ID, PageVersion: page.Version, SessionID: page.SessionID,
		SourceDigest: page.SourceDigest, CompilerVersion: page.CompilerVersion,
		ScopeEpoch: page.ScopeEpoch,
	}
}
