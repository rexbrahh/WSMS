package wsl

import (
	"os"
	"testing"
)

func TestSerializeRoundTrip(t *testing.T) {
	data, err := os.ReadFile(testdata("sample_session.wsl"))
	if err != nil {
		t.Fatal(err)
	}
	recs, err := Parse(string(data))
	if err != nil {
		t.Fatal(err)
	}
	out := Serialize(recs)
	recs2, err := Parse(out)
	if err != nil {
		t.Fatalf("reparse: %v\n---\n%s", err, out)
	}
	if len(recs2) != len(recs) {
		t.Fatalf("len %d vs %d\n%s", len(recs2), len(recs), out)
	}
	// Semantic checks
	if recs2[0].(*TaskRecord).Goal != recs[0].(*TaskRecord).Goal {
		t.Fatal("goal mismatch")
	}
	if recs2[1].(*ConstraintRecord).Text != recs[1].(*ConstraintRecord).Text {
		t.Fatal("constraint text mismatch")
	}
	if recs2[2].(*FailureRecord).Cmd != recs[2].(*FailureRecord).Cmd {
		t.Fatal("cmd mismatch")
	}
	if recs2[2].(*FailureRecord).Err != recs[2].(*FailureRecord).Err {
		t.Fatal("err mismatch")
	}
}
