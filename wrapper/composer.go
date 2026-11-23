package main

import (
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// composerTracker keeps a minimal terminal grid for the CLI's own output so the
// wrapper can see what the composer currently renders. Attachment chips are
// drawn by the CLI itself ("[Pasted text #N ...]" for pasted blocks, "media
// attached" for images/video), and an image paste produces no byte the wrapper
// can intercept on stdin. The rendered marker is therefore the only observable
// source of truth for "the draft holds an opaque chip".
type composerTracker struct {
	mu         sync.Mutex
	cols, rows int
	grid       [][]rune
	curR, curC int
	savedR     int
	savedC     int
	pendingX   bool
	esc        []byte
}

func newComposerTracker(cols, rows int) *composerTracker {
	if cols < 20 {
		cols = 20
	}
	if rows < 5 {
		rows = 5
	}
	t := &composerTracker{cols: cols, rows: rows}
	t.clear()
	return t
}

func (t *composerTracker) clear() {
	t.grid = make([][]rune, t.rows)
	for i := range t.grid {
		t.grid[i] = make([]rune, t.cols)
		for j := range t.grid[i] {
			t.grid[i][j] = ' '
		}
	}
	t.curR = 0
	t.curC = 0
	t.savedR = 0
	t.savedC = 0
	t.pendingX = false
	t.esc = nil
}

func (t *composerTracker) scroll() {
	copy(t.grid, t.grid[1:])
	t.grid[t.rows-1] = make([]rune, t.cols)
	for j := range t.grid[t.rows-1] {
		t.grid[t.rows-1][j] = ' '
	}
}

func (t *composerTracker) put(r rune) {
	if t.pendingX {
		t.curC = 0
		t.curR++
		t.pendingX = false
		if t.curR >= t.rows {
			t.curR = t.rows - 1
			t.scroll()
		}
	}
	w := 1
	if r >= 0x1100 && !(r >= 0x2500 && r <= 0x257F) {
		w = 2
	}
	if w == 2 && t.curC == t.cols-1 {
		t.curC = 0
		t.curR++
		if t.curR >= t.rows {
			t.curR = t.rows - 1
			t.scroll()
		}
	}
	t.grid[t.curR][t.curC] = r
	if w == 2 && t.curC+1 < t.cols {
		t.grid[t.curR][t.curC+1] = 0
	}
	t.curC += w
	if t.curC >= t.cols {
		t.curC = t.cols - 1
		t.pendingX = true
	}
}

func (t *composerTracker) clamp() {
	if t.curR < 0 {
		t.curR = 0
	}
	if t.curR >= t.rows {
		t.curR = t.rows - 1
	}
	if t.curC < 0 {
		t.curC = 0
	}
	if t.curC >= t.cols {
		t.curC = t.cols - 1
	}
}

func (t *composerTracker) applyCSIfinal(final byte, params []int) {
	p := func(i, def int) int {
		if i < len(params) && params[i] > 0 {
			return params[i]
		}
		return def
	}
	switch final {
	case 'H', 'f':
		t.curR = p(0, 1) - 1
		t.curC = p(1, 1) - 1
		t.pendingX = false
	case 'A':
		t.curR -= p(0, 1)
	case 'B':
		t.curR += p(0, 1)
	case 'C':
		t.curC += p(0, 1)
	case 'D':
		t.curC -= p(0, 1)
	case 'G':
		t.curC = p(0, 1) - 1
	case 'd':
		t.curR = p(0, 1) - 1
	case 'K':
		switch p(0, 0) {
		case 0:
			for c := t.curC; c < t.cols; c++ {
				if c >= 0 {
					t.grid[t.curR][c] = ' '
				}
			}
		case 1:
			for c := 0; c <= t.curC && c < t.cols; c++ {
				t.grid[t.curR][c] = ' '
			}
		case 2:
			for c := 0; c < t.cols; c++ {
				t.grid[t.curR][c] = ' '
			}
		}
	case 'J':
		if mode := p(0, 0); mode == 2 || mode == 3 {
			for r := range t.grid {
				for c := range t.grid[r] {
					t.grid[r][c] = ' '
				}
			}
		}
	case 's':
		t.savedR, t.savedC = t.curR, t.curC
	case 'u':
		t.curR, t.curC = t.savedR, t.savedC
	}
	t.clamp()
}

// Feed consumes raw CLI output (ANSI included). Partial escape sequences are
// buffered until the next call so chunk boundaries never corrupt the grid.
func (t *composerTracker) Feed(data []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	buf := append(t.esc, data...)
	t.esc = nil
	i := 0
	for i < len(buf) {
		b := buf[i]
		if b == 0x1b {
			if i+1 >= len(buf) {
				t.esc = append(t.esc, buf[i:]...)
				return
			}
			if buf[i+1] == '[' {
				j := i + 2
				var params []int
				var cur []byte
				complete := false
				for j < len(buf) {
					c := buf[j]
					if c >= '0' && c <= '9' {
						cur = append(cur, c)
						j++
						continue
					}
					if c >= 0x20 && c <= 0x2f {
						j++
						continue
					}
					if c == ';' || c == '?' || c == '<' || c == '=' || c == '>' {
						if len(cur) > 0 {
							n, _ := strconv.Atoi(string(cur))
							params = append(params, n)
							cur = cur[:0]
						}
						j++
						continue
					}
					if c >= 0x40 && c <= 0x7e {
						complete = true
					}
					break
				}
				if !complete {
					t.esc = append(t.esc, buf[i:]...)
					return
				}
				if len(cur) > 0 {
					n, _ := strconv.Atoi(string(cur))
					params = append(params, n)
				}
				t.applyCSIfinal(buf[j], params)
				i = j + 1
				continue
			}
			if buf[i+1] == ']' {
				j := i + 2
				for j < len(buf) && buf[j] != 0x07 && buf[j] != 0x1b {
					j++
				}
				if j >= len(buf) {
					t.esc = append(t.esc, buf[i:]...)
					return
				}
				if buf[j] == 0x1b && j+1 < len(buf) && buf[j+1] == '\\' {
					j++
				}
				i = j + 1
				continue
			}
			if i+1 >= len(buf) {
				t.esc = append(t.esc, buf[i:]...)
				return
			}
			if buf[i+1] == '(' || buf[i+1] == ')' || buf[i+1] == '#' || buf[i+1] == '=' || buf[i+1] == '>' {
				if i+2 >= len(buf) {
					t.esc = append(t.esc, buf[i:]...)
					return
				}
				i += 3
				continue
			}
			if buf[i+1] == '7' {
				t.savedR, t.savedC = t.curR, t.curC
			} else if buf[i+1] == '8' {
				t.curR, t.curC = t.savedR, t.savedC
				t.clamp()
			}
			i += 2
			continue
		}
		switch b {
		case '\r':
			t.curC = 0
			t.pendingX = false
			i++
		case '\n':
			t.curR++
			if t.curR >= t.rows {
				t.curR = t.rows - 1
				t.scroll()
			}
			i++
		case '\b':
			if t.curC > 0 {
				t.curC--
			}
			i++
		case '\t':
			t.curC = (t.curC/8 + 1) * 8
			if t.curC >= t.cols {
				t.curC = t.cols - 1
			}
			i++
		case 0x07:
			i++
		default:
			r, size := utf8.DecodeRune(buf[i:])
			if r == utf8.RuneError && size <= 1 {
				if i+size >= len(buf) && !utf8.FullRune(buf[i:]) {
					t.esc = append(t.esc, buf[i:]...)
					return
				}
				i++
				continue
			}
			t.put(r)
			i += size
		}
	}
}

const (
	pastedChipMarker = "Pasted text #"
	mediaChipMarker  = "media attached"

	// composerBandRows is how far above the hint line the composer band reaches.
	// It bounds the chip scan, so it stays narrow: a chip marker in the
	// transcript above the composer must never arm the guard.
	composerBandRows = 7
)

// composerBandLocked locates the live composer body rows. It prefers the hint
// line the CLI draws under the prompt and falls back to the verified draft-box
// geometry, so ownership never depends on the content of the status row. An
// unknown result is unknown (callers defer); the screen is never scanned as a
// fallback. The caller must hold t.mu.
func (t *composerTracker) composerBandLocked() (int, int, bool) {
	hint := -1
	for r := t.rows - 1; r >= 0; r-- {
		line := string(t.grid[r])
		if indexOf(line, "Enhance Prompt") >= 0 || indexOf(line, "Enhance prompt sedang") >= 0 {
			hint = r
			break
		}
	}
	// One coordinate contract for every consumer: the band is
	// [start .. end] with end = the lower border row (composerBodyRows scans the
	// interval downwards for that border) and start clamped to the box interior,
	// so transcript rows above the top border can never be included.
	box, boxOK := t.boxByCaretLocked()
	band := func(hintRow int) (int, int, bool) {
		if hintRow < 0 {
			return 0, -1, false
		}
		end := hintRow - 1
		start := hintRow - composerBandRows
		if boxOK && start < box.top+1 {
			start = box.top + 1
		}
		if start < 0 {
			start = 0
		}
		if end < start {
			return 0, -1, false
		}
		return start, end, true
	}
	if boxOK {
		// Verified geometry owns the band outright: the FULL interior including the
		// lower border, so a tall composer never loses its top body rows to a fixed
		// seven-row window. A stale hint (adjacent or not) cannot narrow or shift it.
		return box.top + 1, box.bottom, true
	}
	// No verified box: fall back to the hint line, which must still be a plausible
	// composer hint. Unknown stays unknown rather than becoming a full-screen scan.
	return band(hint)
}

// boxByCaretLocked locates the live draft box from rendered geometry plus a
// validated caret position, so ownership never depends on the content of the
// status row: the wrapper may paint any toast there.
//
// The caret must either sit inside the box, or sit exactly in the owned status
// slot (bottom+1) while that row provably holds the wrapper's own paint. The
// second case exists because the overlay is painted into the slot row; it is
// validated against the live hint text, never accepted as "caret row+1".
func (t *composerTracker) boxByCaretLocked() (box composerBox, ok bool) {
	for bottom := t.rows - 2; bottom >= 2; bottom-- {
		if !t.rowIsBorder(bottom) {
			continue
		}
		for top := bottom - 2; top >= 0; top-- {
			if !t.rowIsBorder(top) {
				continue
			}
			if t.firstVisibleRune(top+1) != '>' {
				continue
			}
			if t.borderBetween(top, bottom) {
				continue
			}
			if tw, bw := t.borderRun(top), t.borderRun(bottom); tw-bw > 2 || bw-tw > 2 {
				continue
			}
			inside := t.curR > top && t.curR < bottom
			ownedSlot := false
			if !inside && t.curR == bottom+1 {
				ownedSlot = wrapperOwnsStatusText(strings.TrimSpace(t.rowString(bottom + 1)))
			}
			if !inside && !ownedSlot {
				continue
			}
			return composerBox{top: top, bottom: bottom}, true
		}
	}
	return composerBox{}, false
}

// composerBox is the verified geometry of the live draft box: the upper border
// row and the lower border row (both inclusive).
type composerBox struct {
	top    int
	bottom int
}

// HasChip reports confirmed chip presence or unknown band as conservative
// refusal. Unknown never becomes a counted chip; mutation paths must use
// chipMarkerSpans and defer when its known result is false.
func (t *composerTracker) HasChip() bool {
	spans, known := t.chipMarkerSpans()
	return !known || len(spans) > 0
}

// CursorPos reports where the CLI last parked its cursor, in 0-based screen
// coordinates, plus whether that position lies inside the composer band.
//
// It is not a text offset: the CLI repaints with absolute cursor addressing, so
// this is the position from the most recent frame. That is what a viewport
// restore needs — the same row/column the caret occupied before the walk moved
// the box.
func (t *composerTracker) CursorPos() (row, col int, inComposer bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	start, end, ok := t.composerBandLocked()
	if !ok {
		return t.curR, t.curC, false
	}
	return t.curR, t.curC, t.curR >= start && t.curR <= end
}

func containsMarker(line string) bool {
	return indexOf(line, pastedChipMarker) >= 0 || indexOf(line, mediaChipMarker) >= 0
}

// chipSpan is a coordinate-preserving span of one pasted-text chip marker in the
// composer grid: the grid (row, col) of the marker's first '[' and of its last
// matched rune. Rows are absolute tracker rows; cols index t.grid[row].
type chipSpan struct {
	StartRow, StartCol, EndRow, EndCol int
}

// chipMarkerToken is the literal prefix the recognizer matches. A pasted chip
// renders as "[Pasted text #N ...]"; matching this prefix locates the marker's
// left edge without depending on the trailing count/line suffix.
const chipMarkerToken = "[Pasted text #"

// chipSpanAt matches chipMarkerToken starting at grid cell (r,c0), crossing
// soft-wrap row boundaries WITHOUT inserting or collapsing any whitespace: a row
// boundary is crossed only from the last non-space cell of a row into the next
// row's content after its two-space renderer indent. Because the chip marker is a
// single atomic string the CLI never breaks with a hard newline, any boundary hit
// mid-token is a soft wrap; a wrap landing on the marker's own interior space
// satisfies that space. ok=false when the token does not match here.
func (t *composerTracker) chipSpanAt(r, c0, bandEnd int) (chipSpan, bool) {
	tok := []rune(chipMarkerToken)
	i, rr, cc := 0, r, c0
	startRow, startCol := r, c0
	for i < len(tok) {
		if rr > bandEnd || rr >= t.rows || rr < 0 {
			return chipSpan{}, false
		}
		row := t.grid[rr]
		if cc >= len(row) || rowTrailingBlank(row, cc) {
			rr++
			if rr > bandEnd || rr >= t.rows {
				return chipSpan{}, false
			}
			cc = 0
			next := t.grid[rr]
			if len(next) >= 2 && next[0] == ' ' && next[1] == ' ' {
				cc = 2 // renderer indent on a wrapped/continued row (composer chrome)
			}
			if tok[i] == ' ' {
				i++ // the soft wrap consumed the marker's interior space
				if cc < len(next) && next[cc] == ' ' {
					cc++ // boundary may render the consumed space literally
				}
			}
			continue
		}
		cell := row[cc]
		if cell == 0 { // continuation filler of a wide rune: not content
			cc++
			continue
		}
		if cell != tok[i] {
			return chipSpan{}, false
		}
		i++
		cc++
	}
	endCol := cc - 1
	if endCol < 0 {
		endCol = 0
	}
	return chipSpan{StartRow: startRow, StartCol: startCol, EndRow: rr, EndCol: endCol}, true
}

// rowTrailingBlank reports whether every cell from `from` to the row end is a
// space or wide-rune filler (i.e. the row's content has ended here).
func rowTrailingBlank(row []rune, from int) bool {
	for i := from; i < len(row); i++ {
		if row[i] != ' ' && row[i] != 0 {
			return false
		}
	}
	return true
}

// chipMarkerSpans returns marker spans only from confirmed composer band.
// known=false means spans are unknown, not an observed zero-chip result.
func (t *composerTracker) chipMarkerSpans() ([]chipSpan, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	start, end, known := t.composerBandLocked()
	if !known {
		return nil, false
	}
	var spans []chipSpan
	for r := start; r <= end && r < t.rows; r++ {
		row := t.grid[r]
		for c := 0; c < len(row); c++ {
			if row[c] != '[' {
				continue
			}
			if sp, ok := t.chipSpanAt(r, c, end); ok {
				spans = append(spans, sp)
				c = len(row) // this row is consumed; skip to next row
				break
			}
		}
	}
	return spans, true
}

func (t *composerTracker) chipMarkerCount() (int, bool) {
	spans, known := t.chipMarkerSpans()
	return len(spans), known
}

// rowString returns one grid row as text.
//
// A cell holding NUL is the continuation cell of a wide rune (the tracker writes
// it when a two-column glyph is painted). It is not content: the wide rune
// already occupies the display, so emitting the filler would put a NUL inside
// the row and break comparisons against real text.
func (t *composerTracker) rowString(r int) string {
	if r < 0 || r >= t.rows {
		return ""
	}
	var b strings.Builder
	for _, c := range t.grid[r] {
		if c == 0 {
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// composerBodyRows returns body rows only when the composer band is known (hint
// row or verified draft-box geometry); an unknown band is unknown, not an
// empty composer box.
func (t *composerTracker) composerBodyRows() ([]string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	start, end, known := t.composerBandLocked()
	if !known {
		return nil, false
	}
	bottom := -1
	for r := end; r >= start; r-- {
		if t.rowIsBorder(r) {
			bottom = r
			break
		}
	}
	if bottom < 0 {
		return nil, false
	}
	top := -1
	for r := bottom - 1; r >= 0; r-- {
		if t.rowIsBorder(r) {
			top = r
			break
		}
	}
	if top < 0 {
		return nil, false
	}
	var rows []string
	for r := top + 1; r < bottom; r++ {
		rows = append(rows, t.rowString(r))
	}
	return rows, true
}

// DraftBodyText returns the rendered composer text as one string, one row per
// line. It is the display-side view of the same extraction the gate uses: chip
// marker rows are dropped, the composer's own prompt cell and row indentation
// are removed, and every character the user typed is preserved.
func (t *composerTracker) DraftBodyText() string {
	rows, ok := t.DraftBodyRows()
	if !ok {
		return ""
	}
	return draftBodyFromRows(rows)
}

// DraftBodyRows returns the live composer box rows that carry ordinary draft
// text: the structural band rows with chip-marker rows filtered out. Rows are
// returned verbatim (still carrying the renderer's prompt cell and indentation)
// so the gate can apply its own row-aware comparison without losing the
// characters it must prove are present.
func (t *composerTracker) DraftBodyRows() ([]string, bool) {
	rows, ok := t.composerBodyRows()
	if !ok {
		return nil, false
	}
	kept := make([]string, 0, len(rows))
	for _, row := range rows {
		if containsMarker(row) {
			continue
		}
		kept = append(kept, strings.TrimRight(row, " "))
	}
	return kept, true
}

// HasDraftText reports whether the live composer box currently renders
// ordinary draft content. A just-pasted chip renders as a marker line only, so
// its presence must not be treated as ordinary text.
func (t *composerTracker) HasDraftText() bool {
	return len([]rune(t.DraftBodyText())) > 0
}

func indexOf(s, sub string) int {
	n, m := len(s), len(sub)
	if m == 0 || n < m {
		return -1
	}
	for i := 0; i+m <= n; i++ {
		if s[i:i+m] == sub {
			return i
		}
	}
	return -1
}

// composerWindow is one settled view of the composer box: the ordinary draft
// rows it currently renders, each tagged with its absolute row index inside the
// draft (1-based), together with the elision markers the CLI drew above and
// below the window ("↑ N more lines" / "↓ N more lines").
//
// The indices are what make several windows composable: the CLI renders the
// same rendered row at the same index regardless of scroll offset, so two
// windows can be merged by row index without guessing at overlap. A row whose
// index is claimed by one window and contradicted by another is a conflict and
// the merge refuses.
type composerWindow struct {
	rows []draftWindowRow // visible ordinary draft rows, in on-screen order
	top  int              // rows elided above this window (the "↑ N" count), 0 if none
	bot  int              // rows elided below this window (the "↓ N" count), 0 if none
}

type draftWindowRow struct {
	index  int    // 1-based rendered row index within the whole draft
	text   string // verbatim rendered row (prompt cell / indent still attached)
	screen int    // screen row this draft row was drawn on in that frame
}

// total is how many ordinary draft rows the CLI says the whole draft occupies.
func (w composerWindow) total() int {
	if len(w.rows) == 0 {
		return 0
	}
	return w.rows[0].index - 1 + len(w.rows) + w.bot
}

// mergeComposerWindows unions settled windows into one row-indexed body,
// refusing on any conflicting row. The result is the full rendered body when
// the windows' coverage is contiguous from row 1 to total().
func mergeComposerWindows(wins []composerWindow) ([]draftWindowRow, bool) {
	byIndex := map[int]string{}
	total := 0
	for _, w := range wins {
		if len(w.rows) == 0 {
			continue
		}
		wt := w.total()
		if wt > total {
			total = wt
		}
		for _, row := range w.rows {
			if prev, seen := byIndex[row.index]; seen {
				if prev != row.text {
					return nil, false
				}
				continue
			}
			byIndex[row.index] = row.text
		}
	}
	if total == 0 || len(byIndex) != total {
		return nil, false
	}
	merged := make([]draftWindowRow, 0, total)
	for i := 1; i <= total; i++ {
		text, ok := byIndex[i]
		if !ok {
			return nil, false
		}
		merged = append(merged, draftWindowRow{index: i, text: text})
	}
	return merged, true
}

// DraftWindow extracts the composer box currently on screen as a window with
// absolute row indices and the elision counts the CLI drew.
//
// The box is anchored on the draft rows themselves, not on a fixed number of
// rows above the hint: the top of a full-height composer sits 20 rows above the
// hint (top border, an "↑ N more lines" marker row, 15 body rows and the bottom
// border), so a fixed band would either clip the leading rows or miss the "↑"
// marker entirely and mis-index every row. Markers and borders terminate the
// walk, so no fixed height is needed at all.
//
// Navigation is not guaranteed to collapse the window to a single row: with the
// cursor parked on a long wrapped row the CLI may still render several rows, so
// a window is defined by what is on screen rather than by an assumed size.
func (t *composerTracker) DraftWindow() (composerWindow, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	_, end, known := t.composerBandLocked()
	if !known || end < 0 {
		return composerWindow{}, false
	}

	kind := func(r int) (k string, n int) {
		if r < 0 || r >= t.rows {
			return "none", 0
		}
		text := t.rowString(r)
		if isEnhancerStatusText(strings.TrimSpace(text)) {
			return "status", 0
		}
		if v, ok := moreLinesCount(text, '↑'); ok {
			return "up", v
		}
		if v, ok := moreLinesCount(text, '↓'); ok {
			return "down", v
		}
		if t.rowIsBorder(r) {
			return "border", 0
		}
		trimmed := strings.TrimRight(text, " ")
		if trimmed == "" || containsMarker(trimmed) {
			return "blank", 0
		}
		return "draft", 0
	}

	// Verified native layout at 120x30, top to bottom:
	//
	//   [ "↑ N more lines" ] | top border run   (one of the two closes the top)
	//   15 ordinary draft rows
	//   [ "↓ N more lines" ]                    (present only when windowed)
	//   bottom border run (several rows)
	//   hint line
	//
	// Walk up from the hint past the bottom border run; the draft rows follow,
	// optionally crowned by the "↑" marker. Marks are read from the rows
	// directly adjacent to the draft, so their counts belong to this window.
	r := end
	for r >= 0 {
		if k, _ := kind(r); k == "border" || k == "status" || k == "blank" {
			r--
			continue
		}
		break
	}
	below, haveBelow := 0, false
	if k, n := kind(r); k == "down" {
		below, haveBelow = n, true
		r--
	}
	if k, _ := kind(r); k != "draft" {
		return composerWindow{}, false
	}
	last := r
	first := r
	for first-1 >= 0 {
		if k, _ := kind(first - 1); k != "draft" {
			break
		}
		first--
	}
	above, haveAbove := 0, false
	if k, n := kind(first - 1); k == "up" {
		above, haveAbove = n, true
	}

	win := composerWindow{top: above, bot: below}
	for i := first; i <= last; i++ {
		win.rows = append(win.rows, draftWindowRow{text: strings.TrimRight(t.rowString(i), " "), screen: i})
	}
	_ = haveAbove
	_ = haveBelow
	for i := range win.rows {
		win.rows[i].index = above + 1 + i
	}
	return win, true
}

// ScreenRowForCursor reports which screen row inside the composer currently holds
// the cursor, expressed as the whole-draft row index of the draft row drawn on
// that screen row. It is how the walk turns "where the caret is" into "which
// draft row the caret is on": the tracker's cursor row is a screen coordinate,
// and the window says which draft row was drawn there.
func (w composerWindow) DraftIndexAtScreenRow(screen int) (int, bool) {
	for _, r := range w.rows {
		if r.screen == screen {
			return r.index, true
		}
	}
	return 0, false
}

// moreLinesCount reports the N of a CLI elision marker "↑ N more lines" /
// "↓ N more lines" (or the Indonesian "lebih baris"), with or without a leading
// prompt cell. It is the same structural shape collapseIndicator already
// recognizes, extended to both arrows so the bottom marker is understood too.
func moreLinesCount(row string, arrow rune) (int, bool) {
	trimmed := strings.TrimSpace(row)
	trimmed = strings.TrimPrefix(trimmed, ">")
	trimmed = strings.TrimSpace(trimmed)
	r := []rune(trimmed)
	if len(r) == 0 || r[0] != arrow {
		return 0, false
	}
	i := 1
	for i < len(r) && r[i] == ' ' {
		i++
	}
	j := i
	for j < len(r) && r[j] >= '0' && r[j] <= '9' {
		j++
	}
	if j == i {
		return 0, false
	}
	n, err := strconv.Atoi(string(r[i:j]))
	if err != nil {
		return 0, false
	}
	rest := strings.TrimSpace(string(r[j:]))
	if strings.HasPrefix(rest, "more line") || strings.HasPrefix(rest, "lebih baris") {
		return n, true
	}
	return 0, false
}

// Resize re-creates the grid at the new console dimensions.
func (t *composerTracker) Resize(cols, rows int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if cols < 20 {
		cols = 20
	}
	if rows < 5 {
		rows = 5
	}
	t.cols, t.rows = cols, rows
	t.clear()
}

// MediaCount returns the attachment count rendered in the composer band, or 0
// when no media marker is present. Used for debug timing only.
func (t *composerTracker) MediaCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	start, end, known := t.composerBandLocked()
	if !known {
		return 0
	}
	for r := start; r <= end; r++ {
		line := t.rowString(r)
		idx := indexOf(line, mediaChipMarker)
		if idx < 0 {
			continue
		}
		n := 0
		j := idx - 1
		for j >= 0 && line[j] == ' ' {
			j--
		}
		pow := 1
		for j >= 0 && line[j] >= '0' && line[j] <= '9' {
			n += int(line[j]-'0') * pow
			pow *= 10
			j--
		}
		if n > 0 {
			return n
		}
	}
	return 0
}

// statusSlot reports the zero-based row immediately below the composer's lower
// border, the only place the wrapper may draw its own status. It returns false
// whenever the rendered frame does not contain an unambiguous composer box, so
// callers never fall back to a terminal-height row or guessed coordinate.
//
// The composer is located from rendered geometry only: a full-width horizontal
// border, a prompt row that starts with '>' directly below the upper border,
// no further border inside the box, and only blank or enhancer-status rows
// below the lower border. The lowest matching pair wins, which keeps quoted
// boxes and horizontal rules in the chat transcript above from being mistaken
// for the live draft box.
func (t *composerTracker) statusSlot() (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.statusSlotLocked()
}

func (t *composerTracker) statusSlotLocked() (int, bool) {
	_, bottom, ok := t.composerBoxLocked()
	if !ok {
		return 0, false
	}
	return bottom + 1, true
}

// composerBoxLocked locates the live draft box from rendered geometry only: a
// full-width horizontal border, a prompt row that starts with '>' directly below
// the upper border, no further border inside the box, and only blank or
// wrapper/native enhancer-status rows below the lower border. The lowest
// matching pair wins, which keeps quoted boxes and horizontal rules in the chat
// transcript above from being mistaken for the live composer. The caller must
// hold t.mu.
func (t *composerTracker) composerBoxLocked() (top, bottom int, ok bool) {
	for bottom = t.rows - 2; bottom >= 2; bottom-- {
		if !t.rowIsBorder(bottom) {
			continue
		}
		for top = bottom - 2; top >= 0; top-- {
			if !t.rowIsBorder(top) {
				continue
			}
			if t.firstVisibleRune(top+1) != '>' {
				continue
			}
			if t.borderBetween(top, bottom) {
				continue
			}
			if tw, bw := t.borderRun(top), t.borderRun(bottom); tw-bw > 2 || bw-tw > 2 {
				continue
			}
			if !t.rowsBelowAreStatus(bottom) {
				continue
			}
			return top, bottom, true
		}
	}
	return 0, 0, false
}

// line returns the rendered text of one zero-based row with trailing blanks
// removed. Used only to prove a previously painted slot is still ours before
// erasing it.
func (t *composerTracker) line(row int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if row < 0 || row >= t.rows {
		return ""
	}
	return string(t.grid[row])
}

func (t *composerTracker) borderRun(row int) int {
	if row < 0 || row >= t.rows {
		return 0
	}
	run := 0
	for _, r := range t.grid[row] {
		switch {
		case isBorderRune(r):
			run++
		case r == ' ':
		default:
			return 0
		}
	}
	return run
}

func (t *composerTracker) rowIsBorder(row int) bool {
	return t.borderRun(row) >= 20
}

func (t *composerTracker) firstVisibleRune(row int) rune {
	if row < 0 || row >= t.rows {
		return 0
	}
	for _, r := range t.grid[row] {
		if r != ' ' {
			return r
		}
	}
	return 0
}

func (t *composerTracker) borderBetween(top, bottom int) bool {
	for row := top + 1; row < bottom; row++ {
		if t.rowIsBorder(row) {
			return true
		}
	}
	return false
}

func (t *composerTracker) rowsBelowAreStatus(bottom int) bool {
	for row := bottom + 1; row < t.rows; row++ {
		text := strings.TrimSpace(string(t.grid[row]))
		if text == "" || isEnhancerStatusText(text) {
			continue
		}
		// The wrapper owns exactly one row below the composer: the slot it paints
		// with the live hint/toast. Identify it by equality with what the wrapper
		// is rendering right now, so a transient toast proves ownership without
		// enumerating every toast string. Any other content still disqualifies the
		// candidate box (transcript text below a quoted box).
		if row == bottom+1 && wrapperOwnsStatusText(text) {
			continue
		}
		if standaloneSettings != nil && row == bottom+1 && isStandaloneModelStatus(text) {
			continue
		}
		return false
	}
	return true
}

// wrapperOwnsStatusText reports whether text is exactly what the wrapper is
// painting in its own composer slot at this instant. It reads only the same
// atomics paintStatus uses, so it adds no lock ordering against the tracker.
func wrapperOwnsStatusText(text string) bool {
	cur := strings.TrimSpace(reAnsi.ReplaceAllString(getCurrentHint(), ""))
	return cur != "" && strings.TrimSpace(text) == cur
}

func isBorderRune(r rune) bool {
	switch r {
	case '─', '━', '═', '╌', '╍':
		return true
	}
	return false
}

// isEnhancerStatusText recognizes only text this wrapper (or the native
// statusLine command it feeds) renders below the composer. Anything else below
// the border means the candidate box is not the live composer.
func isEnhancerStatusText(text string) bool {
	needles := []string{
		"Enhance Prompt",
		"Enhance prompt sedang",
		"Enhanced via editor",
		"Enhancement dibatalkan",
		"Enhancement gagal",
		"Enhancement timeout",
		"Tulis prompt lebih dulu",
		"Draft berisi tempelan",
		"Editor tidak mengembalikan",
	}
	text = strings.TrimLeft(text, " \x00✦⏳|/-\\")
	if standaloneSettings != nil && (strings.HasPrefix(text, "Membatalkan enhance prompt") ||
		strings.HasPrefix(text, "Draft tidak berubah") ||
		strings.HasPrefix(text, "Enhance selesai") ||
		strings.HasPrefix(text, "Enhance dibatalkan")) {
		return true
	}
	for _, n := range needles {
		if strings.HasPrefix(text, n) {
			return true
		}
	}
	return false
}
