package devices

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/mobile-next/mobilecli/types"
	"github.com/mobile-next/mobilecli/utils"
)

// Semantics-tree dump: one RPC instead of ~3.5 per render object.
//
// dumpRenderTree walks the render tree node by node, which on a real app means
// thousands of round trips (one screen of a production app measured 2,421
// render objects and ~11,800 calls, to return 30 elements). Every one of them
// waits on the app's isolate, so the cost is
// call-count times a latency that moves with how busy the app is — seconds on
// an iOS simulator, tens of seconds through an adb-forwarded port, and worse
// whenever the app is decoding images or animating.
//
// Flutter already publishes the accessible view of the same tree through a
// service extension, and that answers in a single call (measured: 1.6-3.3ms,
// 15.7KB for the same screen, with no run-to-run spread). It carries what an
// accessibility dump carries — label, value, identifier, flags, role, rect —
// so it is a drop-in source for anything that works off labels. What it does
// not carry is the render-object type and the unlabeled nodes, so it does not
// replace dumpRenderTree for callers that need those.
//
// The output is the debug text of SemanticsNode.toStringDeep, so this file
// parses it. Two properties of that format matter:
//
//   - Nesting is by indentation: each node is printed under tree guides, and a
//     deeper content column means a deeper node.
//   - Each node's rect is expressed in its parent's coordinate space, so an
//     absolute rect is the sum of the ancestors' origins. Pure translations are
//     baked in and not annotated; the only transform note on a real tree is the
//     device-pixel-ratio scale on the view's child, and since we accumulate
//     from that child down, everything below it is in logical pixels (which is
//     what the dpr argument then converts).

// semanticsDumpExtension returns the accessible tree of the running app.
const semanticsDumpExtension = "ext.flutter.debugDumpSemanticsTreeInTraversalOrder"

// dumpSemanticsTree fetches and parses the semantics tree. dpr follows the same
// convention as dumpRenderTree: 1.0 where the platform reports logical points
// (iOS), the device pixel ratio where it reports physical pixels (Android).
func (vm *flutterVM) dumpSemanticsTree(dpr float64) ([]types.ScreenElement, error) {
	raw, err := vm.call(semanticsDumpExtension, map[string]any{"isolateId": vm.isolateID})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", semanticsDumpExtension, err)
	}
	var r struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("unexpected %s response: %w", semanticsDumpExtension, err)
	}
	if strings.TrimSpace(r.Data) == "" {
		// Semantics are off (no accessibility client and nobody called
		// ensureSemantics), so there is nothing to read.
		return nil, fmt.Errorf("%s returned an empty tree", semanticsDumpExtension)
	}

	elements := parseSemanticsTree(r.Data, dpr)
	if len(elements) == 0 {
		return nil, fmt.Errorf("semantics dump produced no elements")
	}
	utils.Verbose("flutter: semantics dump produced %d elements from %d bytes", len(elements), len(r.Data))
	return elements, nil
}

// treeGuides are the characters toStringDeep draws the tree with. Everything up
// to the first character outside this set is layout, not content.
const treeGuides = " │╎├└─┌┆┊╽╿"

// contentAt strips the tree guides from a line and reports the rune column the
// content starts at. Columns are only ever compared with each other, so the
// exact width of a guide does not matter as long as it is counted consistently.
func contentAt(line string) (content string, col int) {
	for i, r := range []rune(line) {
		if !strings.ContainsRune(treeGuides, r) {
			return string([]rune(line)[i:]), i
		}
	}
	return "", -1
}

// semNode is one SemanticsNode as printed, before coordinates are resolved.
type semNode struct {
	col    int
	parent int // index into the node slice, -1 for the root

	hasRect            bool
	left, top          float64
	right, bottom      float64
	absLeft, absTop    float64
	label, value       string
	identifier, role   string
	flags, actions     string
	hidden             bool
	awaitingLabelValue *string // set while a quoted value spans lines
}

// parseSemanticsTree flattens the dump into elements, skipping nodes that carry
// nothing addressable (no label, value, identifier or role) and nodes Flutter
// marked HIDDEN, which are the offscreen and merged-away ones an accessibility
// dump also leaves out.
func parseSemanticsTree(dump string, dpr float64) []types.ScreenElement {
	var nodes []semNode
	var stack []int // indices of the open ancestors, by increasing column

	for _, line := range strings.Split(dump, "\n") {
		content, col := contentAt(line)
		if content == "" {
			continue
		}

		// A value that started on an earlier line keeps consuming lines until
		// its closing quote, so check that before anything else.
		if len(nodes) > 0 {
			if dst := nodes[len(nodes)-1].awaitingLabelValue; dst != nil {
				done := false
				*dst, done = appendQuoted(*dst, content)
				if done {
					nodes[len(nodes)-1].awaitingLabelValue = nil
				}
				continue
			}
		}

		if strings.HasPrefix(content, "SemanticsNode#") {
			for len(stack) > 0 && nodes[stack[len(stack)-1]].col >= col {
				stack = stack[:len(stack)-1]
			}
			parent := -1
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			nodes = append(nodes, semNode{col: col, parent: parent})
			stack = append(stack, len(nodes)-1)
			continue
		}

		if len(nodes) == 0 {
			continue
		}
		applySemanticsProperty(&nodes[len(nodes)-1], content)
	}

	// Parents are always printed before their children, so a single forward
	// pass resolves every absolute origin.
	for i := range nodes {
		n := &nodes[i]
		n.absLeft, n.absTop = n.left, n.top
		if n.parent >= 0 {
			n.absLeft += nodes[n.parent].absLeft
			n.absTop += nodes[n.parent].absTop
		}
	}

	elements := make([]types.ScreenElement, 0, len(nodes))
	for i := range nodes {
		if el, ok := nodes[i].element(dpr); ok {
			elements = append(elements, el)
		}
	}
	return elements
}

// applySemanticsProperty reads one property line into the node.
func applySemanticsProperty(n *semNode, content string) {
	switch {
	case strings.HasPrefix(content, "Rect.fromLTRB("):
		n.setRect(content)
		return
	case content == "HIDDEN":
		n.hidden = true
		return
	}

	key, rest, ok := strings.Cut(content, ":")
	if !ok {
		return
	}
	rest = strings.TrimSpace(rest)

	var dst *string
	switch key {
	case "label":
		dst = &n.label
	case "value":
		dst = &n.value
	case "identifier":
		// Printed quoted, like label and value.
		dst = &n.identifier
	case "role":
		n.role = rest
		return
	case "flags":
		n.flags = rest
		return
	case "actions":
		n.actions = rest
		return
	default:
		return
	}

	// label, value and identifier are printed as quoted strings, either inline
	// or starting on the next line, and they may span lines when the text does.
	if rest == "" {
		*dst = ""
		n.awaitingLabelValue = dst
		return
	}
	done := false
	*dst, done = appendQuoted("", rest)
	if !done {
		n.awaitingLabelValue = dst
	}
}

// appendQuoted adds one line of a possibly multi-line quoted value to acc and
// reports whether the closing quote was reached. The opening quote is dropped
// on the first line; lines are joined with the newline the text had.
func appendQuoted(acc, content string) (string, bool) {
	if acc == "" {
		content = strings.TrimPrefix(content, `"`)
	} else {
		acc += "\n"
	}
	if trimmed, closed := strings.CutSuffix(content, `"`); closed {
		return acc + trimmed, true
	}
	return acc + content, false
}

// setRect parses "Rect.fromLTRB(l, t, r, b)". Anything after the closing paren
// is a transform note ("scaled by 3.0x" on the view's child); it is deliberately
// ignored, see the file comment.
func (n *semNode) setRect(content string) {
	inner := content[len("Rect.fromLTRB("):]
	inner, ok := cutBefore(inner, ")")
	if !ok {
		return
	}
	parts := strings.Split(inner, ",")
	if len(parts) != 4 {
		return
	}
	vals := make([]float64, 4)
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return
		}
		vals[i] = v
	}
	n.left, n.top, n.right, n.bottom = vals[0], vals[1], vals[2], vals[3]
	n.hasRect = true
}

func cutBefore(s, sep string) (string, bool) {
	before, _, ok := strings.Cut(s, sep)
	return before, ok
}

// element converts the node, or reports ok=false when it is not addressable.
func (n *semNode) element(dpr float64) (types.ScreenElement, bool) {
	if n.hidden || !n.hasRect {
		return types.ScreenElement{}, false
	}
	w, h := n.right-n.left, n.bottom-n.top
	if w <= 0 || h <= 0 {
		return types.ScreenElement{}, false
	}
	if n.label == "" && n.value == "" && n.identifier == "" && n.role == "" {
		return types.ScreenElement{}, false
	}

	el := types.ScreenElement{
		Type: n.elementType(),
		Rect: types.ScreenElementRect{
			X:      int(n.absLeft*dpr + 0.5),
			Y:      int(n.absTop*dpr + 0.5),
			Width:  int(w*dpr + 0.5),
			Height: int(h*dpr + 0.5),
		},
	}
	if n.label != "" {
		label := n.label
		el.Label = &label
	}
	if n.value != "" {
		value := n.value
		el.Value = &value
	}
	if n.identifier != "" {
		identifier := n.identifier
		el.Identifier = &identifier
	}
	if n.hasFlag("isSelected") {
		selected := true
		el.Selected = &selected
	}
	if n.hasFlag("isChecked") {
		checked := true
		el.Checked = &checked
	}
	if n.hasFlag("isFocused") {
		focused := true
		el.Focused = &focused
	}
	// Flutter only reports the enabled state for controls that have one, so a
	// missing hasEnabledState says nothing about the node.
	if n.hasFlag("hasEnabledState") && !n.hasFlag("isEnabled") {
		enabled := false
		el.Enabled = &enabled
	}
	return el, true
}

// elementType names the node the way the accessibility dumps do, preferring the
// role Flutter assigned and falling back to the flags.
func (n *semNode) elementType() string {
	if n.role != "" {
		return strings.ToUpper(n.role[:1]) + n.role[1:]
	}
	switch {
	case n.hasFlag("isButton"):
		return "Button"
	case n.hasFlag("isImage"):
		return "Image"
	case n.hasFlag("isTextField"):
		return "TextField"
	case n.hasFlag("isHeader"):
		return "Heading"
	case n.label != "" || n.value != "":
		return "StaticText"
	}
	return "Other"
}

func (n *semNode) hasFlag(flag string) bool {
	for _, f := range strings.Split(n.flags, ",") {
		if strings.TrimSpace(f) == flag {
			return true
		}
	}
	return false
}
