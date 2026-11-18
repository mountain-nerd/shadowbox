package effect_test

import (
	"testing"

	"github.com/mountain-nerd/shadowbox/internal/effect"
)

func ptr(b bool) *bool { return &b }

func TestRegistry_UnregisteredToolIsUnknownAndTreatedAsWrite(t *testing.T) {
	r := effect.NewRegistry(map[string]effect.Class{"lookup_order": effect.ReadOnly})

	got := r.Classify("issue_refund")

	if got != effect.Unknown {
		t.Fatalf("Classify(unregistered) = %v, want Unknown", got)
	}
	if !got.IsWrite() {
		t.Fatalf("Unknown.IsWrite() = false, want true (fail-safe)")
	}
}

func TestClass_IsWrite(t *testing.T) {
	cases := []struct {
		class effect.Class
		want  bool
	}{
		{effect.ReadOnly, false},
		{effect.Idempotent, true},
		{effect.Irreversible, true},
		{effect.Unknown, true},
	}
	for _, c := range cases {
		if got := c.class.IsWrite(); got != c.want {
			t.Errorf("%v.IsWrite() = %v, want %v", c.class, got, c.want)
		}
	}
}

func TestFromHints_FollowsMCPDefaults(t *testing.T) {
	cases := []struct {
		name  string
		hints effect.Hints
		want  effect.Class
	}{
		{"read-only hint", effect.Hints{ReadOnly: ptr(true)}, effect.ReadOnly},
		{"no hints: MCP defaults destructive=true", effect.Hints{}, effect.Irreversible},
		{"non-destructive idempotent", effect.Hints{Destructive: ptr(false), Idempotent: ptr(true)}, effect.Idempotent},
		{"non-destructive, not idempotent", effect.Hints{Destructive: ptr(false), Idempotent: ptr(false)}, effect.Irreversible},
		{"read-only wins over destructive", effect.Hints{ReadOnly: ptr(true), Destructive: ptr(true)}, effect.ReadOnly},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := effect.FromHints(c.hints); got != c.want {
				t.Fatalf("FromHints(%+v) = %v, want %v", c.hints, got, c.want)
			}
		})
	}
}

func TestMergeHints_UntrustedHintsCannotLoosenRegistry(t *testing.T) {
	r := effect.NewRegistry(map[string]effect.Class{"issue_refund": effect.Irreversible})

	merged := r.MergeHints(map[string]effect.Hints{"issue_refund": {ReadOnly: ptr(true)}}, false)

	if got := merged.Classify("issue_refund"); got != effect.Irreversible {
		t.Fatalf("Classify after loosening hint = %v, want Irreversible", got)
	}
}

func TestMergeHints_UntrustedHintsTightenRegistry(t *testing.T) {
	r := effect.NewRegistry(map[string]effect.Class{"sync_crm": effect.ReadOnly})

	merged := r.MergeHints(map[string]effect.Hints{"sync_crm": {}}, false)

	if got := merged.Classify("sync_crm"); got != effect.Irreversible {
		t.Fatalf("Classify after destructive-default hint = %v, want Irreversible", got)
	}
}

func TestMergeHints_UntrustedHintsDoNotClassifyUnregisteredTools(t *testing.T) {
	r := effect.NewRegistry(nil)

	merged := r.MergeHints(map[string]effect.Hints{"search_kb": {ReadOnly: ptr(true)}}, false)

	if got := merged.Classify("search_kb"); got != effect.Unknown {
		t.Fatalf("Classify(unregistered, untrusted read-only hint) = %v, want Unknown", got)
	}
}

func TestMergeHints_TrustedHintsClassifyUnregisteredTools(t *testing.T) {
	r := effect.NewRegistry(nil)

	merged := r.MergeHints(map[string]effect.Hints{"search_kb": {ReadOnly: ptr(true)}}, true)

	if got := merged.Classify("search_kb"); got != effect.ReadOnly {
		t.Fatalf("Classify(unregistered, trusted read-only hint) = %v, want ReadOnly", got)
	}
}

func TestMergeHints_DoesNotMutateOriginal(t *testing.T) {
	r := effect.NewRegistry(map[string]effect.Class{"sync_crm": effect.ReadOnly})

	_ = r.MergeHints(map[string]effect.Hints{"sync_crm": {}}, false)

	if got := r.Classify("sync_crm"); got != effect.ReadOnly {
		t.Fatalf("original registry changed to %v, want ReadOnly", got)
	}
}

func TestParseClass(t *testing.T) {
	cases := map[string]effect.Class{"read-only": effect.ReadOnly, "idempotent": effect.Idempotent, "irreversible": effect.Irreversible}
	for in, want := range cases {
		got, err := effect.ParseClass(in)
		if err != nil || got != want {
			t.Errorf("ParseClass(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := effect.ParseClass("safe"); err == nil {
		t.Errorf("ParseClass(\"safe\") err = nil, want error")
	}
}
