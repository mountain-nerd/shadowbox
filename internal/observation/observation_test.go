package observation_test

import (
	"reflect"
	"testing"

	"github.com/mountain-nerd/shadowbox/internal/observation"
)

func TestTraceIDFromTraceparent(t *testing.T) {
	cases := map[string]string{
		"00-aaaabbbbccccddddeeeeffff00001111-1111222233334444-01": "aaaabbbbccccddddeeeeffff00001111",
		"":                             "",
		"garbage":                      "",
		"00-short-1111222233334444-01": "",
		"00-00000000000000000000000000000000-1111222233334444-01": "", // all-zero trace id is invalid per W3C
	}
	for in, want := range cases {
		if got := observation.TraceIDFromTraceparent(in); got != want {
			t.Errorf("TraceIDFromTraceparent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecision_ToolNamesAreSortedAndUnique(t *testing.T) {
	d := observation.Decision{Kind: observation.ToolCalls, ToolCalls: []observation.ToolCall{
		{Name: "lookup_order"}, {Name: "issue_refund"}, {Name: "lookup_order"},
	}}

	got := d.ToolNames()

	want := []string{"issue_refund", "lookup_order"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ToolNames() = %v, want %v", got, want)
	}
}

func TestUsage_Total(t *testing.T) {
	u := observation.Usage{InputTokens: 120, OutputTokens: 30, Known: true}
	if got := u.Total(); got != 150 {
		t.Fatalf("Total() = %d, want 150", got)
	}
}
