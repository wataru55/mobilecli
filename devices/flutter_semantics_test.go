package devices

import (
	"os"
	"strings"
	"testing"

	"github.com/mobile-next/mobilecli/types"
)

// fixture is a semantics dump in the exact shape SemanticsNode.toStringDeep
// prints, written to cover the parts of that format the parser has to get
// right: nesting by indentation, rects expressed in the parent's coordinate
// space, the device-pixel-ratio note on the view's child, quoted and
// multi-line labels, quoted identifiers, HIDDEN nodes, roles and flags.
func fixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/semantics_tree.txt")
	if err != nil {
		t.Fatalf("reading testdata: %v", err)
	}
	return string(b)
}

func TestParseSemanticsTreeResolvesAbsoluteRects(t *testing.T) {
	elements := parseSemanticsTree(fixture(t), 1.0)
	if len(elements) == 0 {
		t.Fatal("no elements parsed")
	}

	// Rects are printed relative to the parent, so these centers only come out
	// right if the ancestors' origins were accumulated: "Home" sits at (44, 0)
	// inside a bar at (0, 788), and the selected tab at (0, 1) inside a tab bar
	// at (60, 785).
	cases := []struct {
		label string
		wantX int
		wantY int
	}{
		{label: "Home", wantX: 83, wantY: 814},
		{label: "Mon\ntab: 1/2", wantX: 80, wantY: 807},
	}
	for _, tc := range cases {
		el := findByLabel(elements, tc.label)
		if el == nil {
			t.Errorf("label %q not found", tc.label)
			continue
		}
		gotX := el.Rect.X + el.Rect.Width/2
		gotY := el.Rect.Y + el.Rect.Height/2
		if gotX != tc.wantX || gotY != tc.wantY {
			t.Errorf("label %q center = (%d,%d), want (%d,%d)", tc.label, gotX, gotY, tc.wantX, tc.wantY)
		}
	}
}

func TestParseSemanticsTreeKeepsLeafElements(t *testing.T) {
	elements := parseSemanticsTree(fixture(t), 1.0)
	for _, label := range []string{"Section heading", "Home", "Library", "Mon\ntab: 1/2", "Tue\ntab: 2/2"} {
		if findByLabel(elements, label) == nil {
			t.Errorf("expected the output to include %q", label)
		}
	}
}

func TestParseSemanticsTreeDropsHiddenAndEmptyNodes(t *testing.T) {
	elements := parseSemanticsTree(fixture(t), 1.0)

	for _, el := range elements {
		// A node Flutter prints HIDDEN is offscreen or merged away, and an
		// accessibility dump leaves it out too.
		if el.Label != nil && strings.HasPrefix(*el.Label, "Offscreen card") {
			t.Errorf("HIDDEN node leaked into the output: %q", *el.Label)
		}
		if el.Rect.Width <= 0 || el.Rect.Height <= 0 {
			t.Errorf("element %q has a non-positive rect: %+v", el.Type, el.Rect)
		}
		if el.Label == nil && el.Value == nil && el.Identifier == nil && el.Type == "Other" {
			t.Errorf("element with nothing addressable was emitted: %+v", el)
		}
	}

	// The view, its scaled child and the unlabeled bottom bar carry nothing
	// addressable, so only the screen, the heading, the tab bar and its two
	// tabs, and the two bar items survive.
	if len(elements) != 7 {
		t.Errorf("got %d elements, want 7", len(elements))
	}
}

func TestParseSemanticsTreeReadsMultiLineLabels(t *testing.T) {
	elements := parseSemanticsTree(fixture(t), 1.0)
	el := findByLabel(elements, "Mon\ntab: 1/2")
	if el == nil {
		t.Fatal("multi-line label was not reassembled")
	}
	if el.Type != "Tab" {
		t.Errorf("type = %q, want %q (from role: tab)", el.Type, "Tab")
	}
	if el.Selected == nil || !*el.Selected {
		t.Error("the first tab is printed isSelected; expected Selected=true")
	}
}

func TestParseSemanticsTreeUnquotesIdentifiers(t *testing.T) {
	elements := parseSemanticsTree(fixture(t), 1.0)

	// Flutter prints identifier quoted ("NavHome"); the quotes are part of the
	// debug formatting, not of the value, and callers match on the value.
	want := map[string]string{
		"Home":    "NavHome",
		"Library": "NavLibrary",
	}
	for label, id := range want {
		el := findByLabel(elements, label)
		if el == nil {
			t.Errorf("label %q not found", label)
			continue
		}
		if el.Identifier == nil {
			t.Errorf("label %q has no identifier, want %q", label, id)
			continue
		}
		if *el.Identifier != id {
			t.Errorf("label %q identifier = %q, want %q", label, *el.Identifier, id)
		}
	}
	for _, el := range elements {
		if el.Identifier != nil && strings.Contains(*el.Identifier, `"`) {
			t.Errorf("identifier kept its debug quotes: %q", *el.Identifier)
		}
	}
}

func TestParseSemanticsTreeScalesByDevicePixelRatio(t *testing.T) {
	logical := parseSemanticsTree(fixture(t), 1.0)
	physical := parseSemanticsTree(fixture(t), 3.0)

	a := findByLabel(logical, "Library")
	b := findByLabel(physical, "Library")
	if a == nil || b == nil {
		t.Fatal("Library missing from one of the parses")
	}
	// Scaling happens on the parsed floats rather than on the rounded logical
	// pixels, so a half-pixel edge can land one off a naive multiplication —
	// this element starts at x=123.5, which rounds to 124 logical and 371
	// physical rather than 372. Allow that single pixel.
	if abs(b.Rect.X-a.Rect.X*3) > 1 || abs(b.Rect.Width-a.Rect.Width*3) > 1 {
		t.Errorf("dpr not applied: logical x=%d w=%d, physical x=%d w=%d",
			a.Rect.X, a.Rect.Width, b.Rect.X, b.Rect.Width)
	}
}

func TestContentAtStripsTreeGuides(t *testing.T) {
	cases := []struct {
		line    string
		content string
		col     int
	}{
		{"SemanticsNode#0", "SemanticsNode#0", 0},
		{"         ├─SemanticsNode#35", "SemanticsNode#35", 11},
		{"         │           │ Rect.fromLTRB(1.0, 2.0, 3.0, 4.0)", "Rect.fromLTRB(1.0, 2.0, 3.0, 4.0)", 23},
		{"   │   │", "", -1},
	}
	for _, tc := range cases {
		content, col := contentAt(tc.line)
		if content != tc.content || col != tc.col {
			t.Errorf("contentAt(%q) = (%q, %d), want (%q, %d)", tc.line, content, col, tc.content, tc.col)
		}
	}
}

// findByLabel returns the first element whose label matches exactly, or nil.
func findByLabel(elements []types.ScreenElement, label string) *types.ScreenElement {
	for i := range elements {
		if elements[i].Label != nil && *elements[i].Label == label {
			return &elements[i]
		}
	}
	return nil
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
