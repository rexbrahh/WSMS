package config

import (
	"reflect"
	"testing"

	"wsms/internal/memory"
)

func TestDefaultCarriesExplicitResidencyPolicy(t *testing.T) {
	if got, want := Default().ResidencyPolicy, memory.DefaultPolicy(); !reflect.DeepEqual(got, want) {
		t.Fatalf("default residency policy = %#v, want %#v", got, want)
	}
}
