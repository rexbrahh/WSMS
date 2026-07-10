package wsl

import "fmt"

// Diff summarizes added/removed/changed ids between two states.
type Diff struct {
	Added   []string
	Removed []string
	Changed []string
}

// DiffStates compares a and b by id.
func DiffStates(a, b *WorkingState) Diff {
	am := a.KnownIDs()
	bm := b.KnownIDs()
	// strip event ids from known if only in eventOK — use Records for structural
	aIDs := map[string]Record{}
	for _, r := range a.Records() {
		aIDs[r.ID()] = r
	}
	bIDs := map[string]Record{}
	for _, r := range b.Records() {
		bIDs[r.ID()] = r
	}
	_ = am
	_ = bm
	var d Diff
	for id, ar := range aIDs {
		br, ok := bIDs[id]
		if !ok {
			d.Removed = append(d.Removed, id)
			continue
		}
		if Serialize([]Record{ar}) != Serialize([]Record{br}) {
			d.Changed = append(d.Changed, id)
		}
	}
	for id := range bIDs {
		if _, ok := aIDs[id]; !ok {
			d.Added = append(d.Added, id)
		}
	}
	return d
}

func (d Diff) String() string {
	return fmt.Sprintf("added=%v removed=%v changed=%v", d.Added, d.Removed, d.Changed)
}
