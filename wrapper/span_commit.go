package main

// Chip-preserving instruction enhancement: plan and commit primitives.
//
// Contract (measured, see temp/goa-attachment/probe_span/EVIDENCE_MANIFEST.txt):
// the native composer keeps a pasted block as an opaque atom backed by its own
// store. The Ctrl+G editor roundtrip exports expanded text and rebuilds it on
// import, destroying the atom for ANY changed payload (even a byte-identical
// roundtrip). The only supported writer that preserves the atom is keystroke
// mutation of the ordinary text around it, via the CLI's own input path.
//
// The shadow draft (draftRunes) holds the EXPANDED pasted payload, not the
// marker, so the atom body is an exact rune subsequence of it. This file plans
// which ordinary-text regions beside the body can be rewritten, with the proven
// geometry-independent primitive for each, and rebuilds the draft after commit.
// It performs no I/O and calls no model: it is the deterministic core, unit
// tested for the real invariants (body located, regions correct, chip body
// preserved, offset/length exact for the forward-delete / shift-left loops).

import "strings"

// editPrimitive is the keystroke maneuver that replaces one ordinary region
// without ever crossing into the atom.
type editPrimitive int

const (
	// primReplaceFromStart targets ordinary text BEFORE the chip: Ctrl+Home, type
	// the replacement, then forward-Delete exactly len(original) runes. Proven to
	// remove one rune per press (ASCII, 2-byte CJK, 4-byte/2-UTF16 emoji alike).
	primReplaceFromStart editPrimitive = iota
	// primReplaceFromEnd targets ordinary text AFTER the chip: Ctrl+End, Shift+Left
	// exactly len(original) runes, then type. Proven not to cross the atom.
	primReplaceFromEnd
)

func (p editPrimitive) String() string {
	if p == primReplaceFromEnd {
		return "replace-from-end"
	}
	return "replace-from-start"
}

// spanRegion is one contiguous ordinary-text region beside the atom.
type spanRegion struct {
	text string
	prim editPrimitive
}

// spanPlan is the full, validated plan for one chip-bearing draft.
type spanPlan struct {
	chipBody string // exact expanded payload runes, preserved untouched
	regions  []spanRegion
	ok       bool
}

// findChipBody returns the index of the exact payload rune-subsequence in draft,
// or -1. Unambiguous first match; the caller treats -1 as "cannot safely plan".
func findChipBodyIndex(draft []rune, payload string) int {
	p := []rune(payload)
	if len(p) == 0 || len(draft) < len(p) {
		return -1
	}
	for i := 0; i+len(p) <= len(draft); i++ {
		match := true
		for j := range p {
			if draft[i+j] != p[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// countChipBodyOccurrences counts how many positions the payload matches in draft.
// A chip body that matches more than once is AMBIGUOUS: the regions derived from
// the first match could be the wrong ones, so the plan must be rejected rather
// than guessed. Bounded: stops counting at 2 (enough to detect ambiguity).
func countChipBodyOccurrences(draft []rune, payload string) int {
	p := []rune(payload)
	if len(p) == 0 || len(draft) < len(p) {
		return 0
	}
	n := 0
	for i := 0; i+len(p) <= len(draft); i++ {
		match := true
		for j := range p {
			if draft[i+j] != p[j] {
				match = false
				break
			}
		}
		if match {
			n++
			if n >= 2 {
				return n
			}
		}
	}
	return n
}

// buildSpanPlan locates the atom in the shadow draft and derives the ordinary
// regions on each side. ok=false means "do not mutate" (nothing to enhance, body
// not locatable, or an ambiguous/over-long payload at the truncation limit).
func buildSpanPlan(draft []rune, payload string) spanPlan {
	if len(draft) == 0 || payload == "" {
		return spanPlan{}
	}
	// A payload captured at the truncation limit is a suffix of the true paste, so
	// it cannot be located reliably.
	if len([]rune(payload)) >= pastePayloadLimit {
		return spanPlan{}
	}
	idx := findChipBodyIndex(draft, payload)
	if idx < 0 {
		return spanPlan{}
	}
	// Reject an AMBIGUOUS payload: if the exact body occurs more than once, the
	// first match's surrounding regions may not be the real ones. Guessing would
	// rewrite the wrong text; refuse instead of claiming a completed plan.
	if countChipBodyOccurrences(draft, payload) != 1 {
		return spanPlan{}
	}
	pLen := len([]rune(payload))
	pre := string(draft[:idx])
	suf := string(draft[idx+pLen:])
	var regions []spanRegion
	if strings.TrimSpace(pre) != "" {
		regions = append(regions, spanRegion{text: pre, prim: primReplaceFromStart})
	}
	if strings.TrimSpace(suf) != "" {
		regions = append(regions, spanRegion{text: suf, prim: primReplaceFromEnd})
	}
	if len(regions) == 0 {
		return spanPlan{chipBody: payload}
	}
	return spanPlan{chipBody: payload, regions: regions, ok: true}
}

// splitEdgeWS separates leading/trailing whitespace so the model works on the core
// text while the composer's boundary spacing (which keeps the instruction from
// merging into the chip) is preserved exactly.
func splitEdgeWS(s string) (lead, core, trail string) {
	r := []rune(s)
	i := 0
	for i < len(r) && isSpaceRune(r[i]) {
		i++
	}
	j := len(r)
	for j > i && isSpaceRune(r[j-1]) {
		j--
	}
	return string(r[:i]), string(r[i:j]), string(r[j:])
}

func isSpaceRune(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }

// rebuildDraftWithRegions reconstructs the draft after commit. It replaces each
// original region string with its committed replacement, leaving the chip body
// byte-for-byte in place (the body is the substring between the two regions and
// is never touched). regions and replacements are parallel.
func rebuildDraftWithRegions(draft []rune, plan spanPlan, replacements []string) []rune {
	full := string(draft)
	// Replace the suffix first so that editing it cannot shift the position used
	// to find the prefix region (prefix is earlier in the string).
	order := make([]int, 0, len(plan.regions))
	for i := range plan.regions {
		order = append(order, i)
	}
	// Process regions from last to first by their position in the original text,
	// which is safe because each region's original text is unique enough to locate
	// and we always replace the later occurrence first.
	idxOf := make([]int, len(plan.regions))
	searchFrom := 0
	valid := true
	for i := range plan.regions {
		j := strings.Index(full[searchFrom:], plan.regions[i].text)
		if j < 0 {
			valid = false
			break
		}
		idxOf[i] = searchFrom + j
		searchFrom = idxOf[i] + len(plan.regions[i].text)
	}
	if !valid {
		return draft
	}
	for i := len(order) - 1; i >= 0; i-- {
		k := order[i]
		full = full[:idxOf[k]] + replacements[k] + full[idxOf[k]+len(plan.regions[k].text):]
	}
	return []rune(full)
}

// chipBodyAfter is a test/inspection helper: the substring between the committed
// regions in the rebuilt draft, which must equal the original payload.
func chipBodyAfter(rebuilt []rune, plan spanPlan, replacements []string) string {
	if len(plan.regions) == 0 {
		return string(rebuilt)
	}
	s := string(rebuilt)
	var prefixLen int
	if plan.regions[0].prim == primReplaceFromStart {
		prefixLen = len(replacements[0])
	}
	// The body sits after the prefix replacement and before the suffix replacement.
	end := len(s)
	for i := range plan.regions {
		if plan.regions[i].prim == primReplaceFromEnd {
			end = len(s) - len(replacements[i])
		}
	}
	if prefixLen > len(s) || end < prefixLen {
		return ""
	}
	return s[prefixLen:end]
}
