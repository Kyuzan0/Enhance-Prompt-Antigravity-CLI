package main

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// outputRenderer is the single serialized writer for both child output and the
// wrapper's own status line. Every byte that reaches the terminal passes
// through it, so child repaints and overlay paints can never interleave inside
// one escape sequence. Cursor state is preserved with DEC save/restore (ESC 7 /
// ESC 8); the draft, prompt and any native content outside the owned slot are
// never touched.
type outputRenderer struct {
	mu        sync.Mutex
	w         io.Writer
	t         *composerTracker
	lastRow   int // zero-based; -1 when nothing is painted
	lastPlain string
}

func newOutputRenderer(w io.Writer, t *composerTracker) *outputRenderer {
	return &outputRenderer{w: w, t: t, lastRow: -1}
}

// writeChild forwards native output unchanged and keeps the frame model in
// sync under the same lock, so the anchor is always derived from the exact
// bytes the user saw.
func (r *outputRenderer) writeChild(b []byte) {
	if r == nil || len(b) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.t.Feed(b)
	_, _ = r.w.Write(b)
}

// paint redraws the current status in the composer-owned slot. With no
// unambiguous anchor it paints nothing and only removes a previous overlay
// when that row is still provably the wrapper's own text.
func (r *outputRenderer) paint() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paintLocked()
}

func (r *outputRenderer) paintLocked() {
	ansi := getCurrentHint()
	plain := strings.TrimSpace(reAnsi.ReplaceAllString(ansi, ""))
	row, ok := r.t.statusSlot()
	if !ok {
		r.clearOwnedLocked()
		return
	}
	if r.lastRow >= 0 && r.lastRow != row {
		r.clearOwnedLocked()
	}
	seq := fmt.Sprintf("\x1b7\x1b[%d;1H\x1b[2K%s\x1b8", row+1, ansi)
	r.t.Feed([]byte(seq))
	_, _ = io.WriteString(r.w, seq)
	r.lastRow, r.lastPlain = row, plain
}

// clearOwnedLocked erases the previous overlay only when the rendered frame
// still shows the text this renderer painted there. If native content replaced
// it, the slot is no longer ours and nothing is deleted.
func (r *outputRenderer) clearOwnedLocked() {
	if r.lastRow < 0 {
		return
	}
	if r.lastPlain != "" && strings.TrimSpace(r.t.line(r.lastRow)) == r.lastPlain {
		seq := fmt.Sprintf("\x1b7\x1b[%d;1H\x1b[2K\x1b8", r.lastRow+1)
		r.t.Feed([]byte(seq))
		_, _ = io.WriteString(r.w, seq)
	}
	r.lastRow, r.lastPlain = -1, ""
}

// paintStatus is the package-level entry point used by async timers and
// watchers. It is a no-op before the renderer is wired up (unit tests).
func paintStatus() {
	if outRenderer != nil {
		outRenderer.paint()
	}
}
