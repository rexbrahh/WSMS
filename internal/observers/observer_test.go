package observers

import (
	"sync"
	"testing"
)

func TestSeqIDGenSnapshotRestore(t *testing.T) {
	ids := NewSeqIDGen()
	if got := ids.Next("C"); got != "C1" {
		t.Fatalf("first constraint id=%q", got)
	}
	checkpoint := ids.Snapshot()
	if got := ids.Next("C"); got != "C2" {
		t.Fatalf("tentative constraint id=%q", got)
	}
	if got := ids.Next("F"); got != "F1" {
		t.Fatalf("tentative failure id=%q", got)
	}

	ids.Restore(checkpoint)
	checkpoint["C"] = 99 // Restore must not retain the caller's map.
	if got := ids.Next("C"); got != "C2" {
		t.Fatalf("restored constraint id=%q, want C2", got)
	}
	if got := ids.Next("F"); got != "F1" {
		t.Fatalf("restored failure id=%q, want F1", got)
	}
}

func TestSeqIDGenConcurrentNextIsUnique(t *testing.T) {
	ids := NewSeqIDGen()
	const count = 64
	results := make(chan string, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- ids.Next("F")
		}()
	}
	wg.Wait()
	close(results)

	seen := make(map[string]bool, count)
	for id := range results {
		if seen[id] {
			t.Fatalf("duplicate concurrent id %q", id)
		}
		seen[id] = true
	}
	if len(seen) != count {
		t.Fatalf("unique id count=%d, want %d", len(seen), count)
	}
}
