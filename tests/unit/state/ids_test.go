package state_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

func TestIDs_FormatAndParse(t *testing.T) {
	t.Parallel()
	random := testkit.NewRandom("ids")
	tests := []struct {
		name   string
		prefix string
		make   func() (string, error)
		parse  func(string) (string, error)
	}{
		{
			"operation", "op-", func() (string, error) { id, err := state.NewOperationID(epoch, random); return id.String(), err },
			func(s string) (string, error) { id, err := state.ParseOperationID(s); return id.String(), err },
		},
		{
			"scope", "sc-", func() (string, error) { id, err := state.NewScopeUID(epoch, random); return id.String(), err },
			func(s string) (string, error) { id, err := state.ParseScopeUID(s); return id.String(), err },
		},
		{
			"realization", "rz-", func() (string, error) { id, err := state.NewRealizationID(epoch, random); return id.String(), err },
			func(s string) (string, error) { id, err := state.ParseRealizationID(s); return id.String(), err },
		},
		{
			"secret version", "sv-", func() (string, error) { id, err := state.NewSecretVersionID(epoch, random); return id.String(), err },
			func(s string) (string, error) { id, err := state.ParseSecretVersionID(s); return id.String(), err },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			text, err := test.make()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(text, test.prefix) || len(text) != len(test.prefix)+26 {
				t.Fatalf("id %q is not %s plus 26 characters", text, test.prefix)
			}
			if body := text[len(test.prefix):]; body[0] > '7' {
				t.Errorf("a ULID starts with 0-7, got %q", body[:1])
			}
			back, err := test.parse(text)
			if err != nil || back != text {
				t.Fatalf("parse(%q) = %q, %v", text, back, err)
			}
			for _, bad := range []string{"", test.prefix, text[:len(text)-1], strings.ToLower(text), "xx-" + text[3:], text + "0", strings.Replace(text, text[4:5], "U", 1)} {
				if _, err := test.parse(bad); !errors.Is(err, state.ErrInvalidID) {
					t.Errorf("parse(%q) error = %v, want ErrInvalidID", bad, err)
				}
			}
		})
	}
}

func TestIDs_AreTimeOrderedAndComparable(t *testing.T) {
	t.Parallel()
	random := testkit.NewRandom("order")
	first, _ := state.NewOperationID(epoch, random)
	second, _ := state.NewOperationID(epoch.Add(time.Second), random)
	if first.String() >= second.String() {
		t.Errorf("later ID %s does not sort after %s", second, first)
	}
	seen := map[state.OperationID]bool{first: true}
	if seen[second] {
		t.Error("distinct IDs compared equal")
	}
}

func TestScopeUID_SuffixComesFromTheRandomPart(t *testing.T) {
	t.Parallel()
	a, _ := state.NewScopeUID(epoch, testkit.NewRandom("a"))
	b, _ := state.NewScopeUID(epoch, testkit.NewRandom("b"))
	if len(a.Suffix()) != 6 || a.Suffix() != strings.ToLower(a.Suffix()) {
		t.Fatalf("suffix %q is not six lowercase characters", a.Suffix())
	}
	if a.Suffix() == b.Suffix() {
		t.Errorf("two scopes created in the same instant share the suffix %q", a.Suffix())
	}
	timePart := strings.ToLower(a.String()[len("sc-") : len("sc-")+10])
	if strings.Contains(a.Suffix(), timePart[:6]) && a.Suffix() == timePart[:6] {
		t.Errorf("suffix %q comes from the time prefix", a.Suffix())
	}
	later, _ := state.NewScopeUID(epoch.Add(time.Hour), testkit.NewRandom("a"))
	if later.Suffix() != a.Suffix() {
		t.Errorf("the suffix depends on the time: %q then %q", a.Suffix(), later.Suffix())
	}
	if (state.ScopeUID{}).Suffix() != "" {
		t.Error("zero scope uid has a suffix")
	}
}

func TestIDs_MarshalText(t *testing.T) {
	t.Parallel()
	id, _ := state.NewOperationID(epoch, testkit.NewRandom("text"))
	text, err := id.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	var back state.OperationID
	if err := back.UnmarshalText(text); err != nil || back != id {
		t.Fatalf("round trip = %v, %v", back, err)
	}
	if err := back.UnmarshalText([]byte("nope")); err == nil {
		t.Error("UnmarshalText accepted garbage")
	}
}
