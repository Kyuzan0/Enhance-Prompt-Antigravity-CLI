package main

// Live wiring for chip-preserving instruction enhancement.
//
// beginSpanEnhancement is called from the Ctrl+E handler when the composer holds
// a paste chip. It validates a span plan and, if valid, runs the enhancement on a
// background goroutine with the REAL selected session model, then commits it with
// the proven keystroke primitive and resyncs the shadow draft WITHOUT repasting
// (so the atom is never flattened). See span_commit.go for the deterministic,
// unit-tested core and temp/goa-attachment/probe_span/EVIDENCE_MANIFEST.txt for
// the native measurements.
//
// Mutation is NOT assumed atomic. Environment cancellation is honoured only
// before the mutation begins (during the model call), but emission can still
// fail mid-region. Recovery is authorized by actual delivered bytes: zero bytes
// means nothing to roll back; a partial region is containment (input quarantined,
// no recovery keystrokes) because the terminal parser/region state is unknown;
// only an untouched region after fully completed earlier regions may restore
// those earlier regions. There is no promise that a partial instruction is
// repaired automatically.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// Proven key sequences (agy-real 1.5.9). A plain CR/LF is never sent; newlines
// inside a replacement use Shift+Enter.
var (
	ctrlHomeSeq   = []byte("\x1b[1;5H")
	ctrlEndSeq    = []byte("\x1b[1;5F")
	shiftLeftSeq  = []byte("\x1b[1;2D")
	delSeq        = []byte("\x1b[3~")
	backspaceSeq  = []byte{0x7f}
	shiftRightSeq = []byte("\x1b[1;2C")
	shiftEnterSeq = []byte("\x1b[13;2u")
)

// beginSpanEnhancement attempts the chip-preserving path. It returns true when it
// owns the trigger (a job was launched); false means "no valid plan", so the
// caller keeps its existing safe refusal. It never mutates the composer itself.
func beginSpanEnhancement(draft []rune, payload string, model string, outHandle windows.Handle) bool {
	plan := buildSpanPlan(draft, payload)
	if !plan.ok {
		return false
	}
	if isBusyState.Load() {
		return false
	}

	ctx, cancel := context.WithCancel(context.Background())
	enhanceMu.Lock()
	enhanceCancel = cancel
	enhanceMu.Unlock()

	// Ownership snapshot for the WHOLE job: capture the draft epoch together with
	// the original draft BEFORE the model wait, so an edit/caret move/paste during
	// the wait is detected at commit (sampling only at commit would miss it).
	// Draft and geometry are captured together under emitMu with the plan, before
	// the model wait. A resize during the wait must invalidate this snapshot; it
	// must never become the baseline.
	emitMu.Lock()
	ownerEpoch := draftEpoch.Load()
	ownerGeometry := geometryEpoch.Load()
	emitMu.Unlock()

	startRewriteStatus()
	isBusyState.Store(true)
	storeEnhanceKind(enhanceKindSpan)
	toastMessage.Store(nil)
	syncStatusFile(true)
	paintStatus()

	go runSpanEnhancement(ctx, cancel, plan, append([]rune(nil), draft...), ownerEpoch, ownerGeometry, model, outHandle)
	if standaloneSettings != nil {
		owner := rewriteStarted.Load()
		go func() {
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for range ticker.C {
				if !isBusyState.Load() || rewriteStarted.Load() != owner {
					return
				}
				paintStatus()
			}
		}()
	}
	return true
}

// runSpanEnhancement enhances every ordinary region with the real selected model,
// then commits atomically. Any real-model failure leaves the draft untouched.
func runSpanEnhancement(ctx context.Context, cancel context.CancelFunc, plan spanPlan, original []rune, ownerEpoch, ownerGeometry uint64, model string, outHandle windows.Handle) {
	epoch := spanEpoch.Add(1)
	defer func() {
		if r := recover(); r != nil {
			logDebug("[SPAN] PANIC RECOVERED: %v", r)
		}
		cancel()
		// A superseded job (a newer span job or a cancel+retry started after this
		// one) must NOT clear the newer job's busy state or cancel handle.
		if spanEpoch.Load() != epoch {
			logDebug("[SPAN] stale job %d finishing; leaving newer job state intact", epoch)
			return
		}
		enhanceMu.Lock()
		enhanceCancel = nil
		enhanceMu.Unlock()
		storeEnhanceKind(enhanceKindNone)
		isBusyState.Store(false)
		syncStatusFile(false)
	}()

	if model == "" {
		model = getCurrentModel()
	}

	// Phase 1: real-model rewrite of each ordinary region (no mutation yet).
	replacements := make([]string, len(plan.regions))
	var totalDur float64
	regionDelivered := 0
	for i, rg := range plan.regions {
		lead, core, trail := splitEdgeWS(rg.text)
		if strings.TrimSpace(core) == "" {
			replacements[i] = rg.text
			continue
		}
		res := EnhancePromptWithModelDetailed(ctx, core, model, true)
		logDebug("[SPAN] model io region=%d request_len=%d request_sha=%s response_len=%d response_sha=%s baseline_len=%d baseline_sha=%s", i, len([]rune(core)), sha16(core), len([]rune(res.Text)), sha16(res.Text), len([]rune(rg.text)), sha16(rg.text))
		if ctx.Err() != nil {
			abortSpanNoMutation(outHandle, "\x1b[33m✦ Enhancement dibatalkan\x1b[0m")
			return
		}
		// REAL MODEL ONLY: a deterministic/local fallback is not an enhancement and
		// must never be committed as if it were. Leave the draft untouched.
		// A cache hit is a real prior model result. It may commit only through the
		// same fence/region/chip verification; an empty or invalid entry still
		// fails closed.
		if !spanResultUsable(res) {
			logDebug("[SPAN] real model unavailable (source=%s); draft untouched", res.Source)
			msg := "Model tidak tersedia; draft dipertahankan"
			if strings.TrimSpace(res.Reason) != "" {
				msg = res.Reason
			}
			abortSpanNoMutation(outHandle, "\x1b[31m✦ "+msg+"\x1b[0m")
			return
		}
		replacements[i] = lead + strings.TrimSpace(res.Text) + trail
		totalDur += res.Duration.Seconds()
	}

	// Phase 2: atomic commit. Cancellation was only meaningful during phase 1.
	// A stale job (superseded by a newer span job) must not type into the composer.
	if spanEpoch.Load() != epoch {
		logDebug("[SPAN] stale job %d reached commit; skipping mutation", epoch)
		return
	}
	// Let the composer settle after the model subprocess before the first key, so
	// a navigation key is not lost to a transient repaint (an intermittently
	// dropped Ctrl+Home leaves the instruction in place and types elsewhere).
	waitQuiet(3000 * time.Millisecond)
	time.Sleep(1200 * time.Millisecond)
	changed := false
	for i := range replacements {
		if replacements[i] != plan.regions[i].text {
			changed = true
			break
		}
	}
	if !changed {
		for i := range replacements {
			logDebug("[SPAN] no-change region=%d baseline_len=%d baseline_sha=%s replacement_len=%d replacement_sha=%s equal=%v", i, len([]rune(plan.regions[i].text)), sha16(plan.regions[i].text), len([]rune(replacements[i])), sha16(replacements[i]), replacements[i] == plan.regions[i].text)
		}
		abortSpanNoMutation(outHandle, "\x1b[33m✦ Tidak ada perubahan dari model\x1b[0m")
		return
	}

	w := childWriter
	allOK := true
	failedAt := -1
	chipBefore := chipAtomCount()
	if chipBefore < 0 {
		logDebug("[SPAN] commit deferred: composer chip state unknown; draft untouched")
		abortSpanNoMutation(outHandle, "\x1b[33m✦ Draft tidak berubah; keadaan composer belum terverifikasi\x1b[0m")
		return
	}
	// Event ownership: ownerEpoch was captured with the ORIGINAL draft BEFORE the
	// model wait. The fence requires the draft epoch to STILL equal it, so any edit,
	// caret move or paste during the wait OR during the destructive edits trips it
	// (per key), not just a change sampled at commit.
	spanDeliveredBytes.Store(0)
	fence := func(region int, expected string) bool {
		ctxOK := commitFence(ctx, epoch)
		actual := draftEpoch.Load()
		actualGeom := geometryEpoch.Load()
		ok := ctxOK && ownershipMatches(actual, actualGeom, ownerEpoch, ownerGeometry)
		logDebug("[SPAN] fence region=%d ctx_ok=%v owner_epoch=%d draft_epoch=%d owner_geom=%d geometry_epoch=%d expected_len=%d expected_sha=%s ok=%v", region, ctxOK, ownerEpoch, actual, ownerGeometry, actualGeom, len([]rune(expected)), sha16(expected), ok)
		return ok
	}
	for i, rg := range plan.regions {
		if replacements[i] == rg.text {
			continue
		}
		// Ownership fence: never begin a destructive edit if the job was superseded
		// (newer epoch), cancelled after the model returned, or the user changed the
		// draft (draft revision advanced) since we took ownership. The same fence is
		// re-checked before INSERTION inside commitRegion, so a change landing between
		// the delete and the type also aborts. Earlier committed regions roll back.
		if !fence(i, rg.text) {
			logDebug("[SPAN] commit fence: superseded/cancelled/edited before region %d; aborting", i)
			allOK = false
			failedAt = i
			break
		}
		beforeBytes := spanDeliveredBytes.Load()
		if !commitRegion(w, rg, replacements[i], func() bool { return fence(i, rg.text) }, chipBefore) {
			allOK = false
			failedAt = i
			regionDelivered = int(spanDeliveredBytes.Load() - beforeBytes)
			break
		}
	}

	if !allOK {
		dispatchSpanCommitFailure(classifyCommitFailure(spanDeliveredBytes.Load(), int64(regionDelivered)), ownerGeometry, plan, replacements, original, failedAt, spanDeliveredBytes.Load(), int64(regionDelivered), outHandle)
		return
	}

	// All regions committed and verified. Now (and only now) resync the shadow to
	// the committed text WITHOUT repasting, and deliver held keystrokes in order.
	rebuilt := rebuildDraftWithRegions(original, plan, replacements)
	shadowDraftOverride.Store(&appliedDraft{Text: string(rebuilt)})
	releaseCapturedOps()

	finishSpan(outHandle, "\x1b[32m✦ Enhanced via editor with "+displayModelName(model)+" ("+formatSeconds(totalDur)+"s)!\x1b[0m")
}

// commitFailureAction is the recovery decision for a failed span commit. It is
// derived only from actual child-write delivery counts, never from attempts.
type commitFailureAction int

const (
	commitFailureNoMutation commitFailureAction = iota
	commitFailureContainment
	commitFailurePriorRegionsOnly
)

// classifyCommitFailure is the single production decision. total>0 and a
// non-zero CURRENT-region delta means a partial/unknown terminal state: recovery
// keystrokes are forbidden. Zero total means no byte reached native.
func classifyCommitFailure(total, regionDelivered int64) commitFailureAction {
	if total <= 0 {
		return commitFailureNoMutation
	}
	if regionDelivered > 0 {
		return commitFailureContainment
	}
	return commitFailurePriorRegionsOnly
}

// rollbackOutcome classifies what a failed-commit rollback actually achieved. The
// chip-intact guard cannot RECREATE an already-lost chip, so that case must be
// reported as unrecoverable, never as atomic/full-preserved.
type rollbackOutcome int

const (
	// rollbackRestored: chip atom count was positive and unchanged, and region text
	// was restored to the original. The draft is believed recovered.
	rollbackRestored rollbackOutcome = iota
	// rollbackChipCompromised: chip atom count changed during or after attempt, or
	// the final chip is absent. The composer is NOT known to be intact.
	rollbackChipCompromised
	// rollbackChipLostUnrecoverable: the chip was already absent BEFORE rollback.
	// Keystrokes cannot recreate the atom, so nothing was mutated. Data may be lost;
	// the caller must surface this, not claim recovery.
	rollbackChipLostUnrecoverable
)

// rollbackComposerToOriginal restores the composer to the ORIGINAL draft after a
// failed commit, WITHOUT ever flattening/recreating the chip atom and without any
// submit. It uses ONLY caret moves (Ctrl+Home / Ctrl+End), the exact-count
// Delete/Backspace primitive, and plain typing with Shift+Enter newlines — never
// Ctrl+A, never bracketed paste, never Enter. It restores only the regions that
// could have been mutated (index <= failedAt); untouched regions are left alone.
// If a region is currently empty (a prior delete succeeded but the type did not),
// it retypes the original instead of erroring. A chip that is already gone cannot
// be recreated by keystrokes, so in that case it mutates nothing and the caller
// reports the failure honestly.
func rollbackComposerToOriginal(plan spanPlan, replacements []string, failedAt int) rollbackOutcome {
	chipBefore := chipAtomCount()
	if chipBefore < 0 {
		logDebug("[SPAN] rollback deferred: composer chip state unknown")
		return rollbackChipCompromised
	}
	if chipBefore == 0 {
		// No chip remains; keystrokes cannot recreate its atom. Mutate nothing and
		// report loss as unrecoverable rather than claiming rollback success.
		logDebug("[SPAN] rollback: chip ALREADY ABSENT before rollback (atoms=%d); mutating nothing; CHIP_LOST_UNRECOVERABLE", chipBefore)
		return rollbackChipLostUnrecoverable
	}
	// Restore only regions at index <= failedAt: earlier ones committed (must be
	// reverted to original) and failedAt may be partially mutated. Regions AFTER
	// failedAt were never touched, so we must NOT run a delete+retype on them.
	last := failedAt
	if last < 0 || last >= len(plan.regions) {
		last = len(plan.regions) - 1
	}
	for i := 0; i <= last && i < len(plan.regions); i++ {
		rg := plan.regions[i]
		if replacements[i] == rg.text {
			continue // unchanged region was never mutated
		}
		atStart := rg.prim == primReplaceFromStart
		// Navigate to the region edge. NEVER Ctrl+A / never bracketed paste: only
		// Ctrl+Home / Ctrl+End, which move the caret to the draft edge.
		ok := false
		for attempt := 0; attempt < 5 && !ok; attempt++ {
			if atStart {
				_, _ = writeChild(ctrlHomeSeq)
				ok = waitCaretAtDraftStart(700 * time.Millisecond)
			} else {
				_, _ = writeChild(ctrlEndSeq)
				ok = waitCaretAtDraftEnd(700 * time.Millisecond)
			}
		}
		if !ok {
			logDebug("[SPAN] rollback: could not reach region %d edge; stopping", i)
			return rollbackChipCompromised
		}
		// Delete whatever currently sits at the region edge. If it is already empty
		// (a prior delete succeeded but the type did not), do NOT error out: skip the
		// delete and retype the original. deleteRegionExact measures the CURRENT
		// rendered region and never crosses the chip.
		if cur := measureInstructionRunes(atStart); cur > 0 {
			if err := deleteRegionExact(atStart, nil); err != nil {
				logDebug("[SPAN] rollback: delete region %d failed (%v); stopping", i, err)
				return rollbackChipCompromised
			}
		}
		// Retype the ORIGINAL region text (newlines are Shift+Enter, never a submit).
		// Honor the exact write outcome: a partial/failed retype must NOT be treated
		// as restored. Stop and report compromised so the caller quarantines/does not
		// publish a shadow for a write that did not fully land.
		if !typeSpanText(childWriter, rg.text, nil) {
			logDebug("[SPAN] rollback: retype region %d write failed; stopping", i)
			return rollbackChipCompromised
		}
		time.Sleep(300 * time.Millisecond)
		// VERIFY the region now equals the ORIGINAL EXACTLY (character for
		// character, order preserved; only composer cells/soft-wrap/tab tolerated).
		// A row-count match is NOT enough: the caller publishes `original` as the
		// shadow, so a different-but-same-row text would be a false recovery. On a
		// mismatch we have an UNKNOWN draft -> stop and report compromised.
		if !draftRowsMatchPayload(instructionRegionRows(atStart), expectedRenderedRegion(rg.text, atStart)) {
			logDebug("[SPAN] rollback: region %d did NOT restore to the exact original; stopping as COMPROMISED", i)
			return rollbackChipCompromised
		}
	}
	chipAfter := chipAtomCount()
	logDebug("[SPAN] rollback: done chip atoms before=%d after=%d", chipBefore, chipAfter)
	if chipAfter < 0 || chipAfter == 0 || chipAfter != chipBefore {
		return rollbackChipCompromised
	}
	return rollbackRestored
}

// commitFence is the commit-ownership fence. The model phase is cancellable; the
// commit must not race a cancel or a superseding edit that arrives after the model
// returns. Immediately before each destructive edit the committer checks that
// (a) it still owns the job — its epoch is current, so no newer job/trigger
// superseded it — and (b) no cancel has fired (ctx not cancelled, wrapper still
// busy for span). Either failing means the composer is no longer provably ours to
// mutate, so the commit aborts (which triggers rollback of any earlier region).
func commitFence(ctx context.Context, epoch uint64) bool {
	if ctx.Err() != nil || spanEpoch.Load() != epoch || !isBusyState.Load() {
		return false
	}
	return true
}

// waitQuiet blocks until the composer has been quiet for the quiet window (bounded).
func waitQuiet(max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if time.Since(time.Unix(0, lastOutputNano.Load())) >= composerQuietWindow {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
}

// commitRegion replaces one ordinary region by first REMOVING it (delete), then
// typing the replacement into the gap beside the chip. Deleting first shrinks the
// buffer, so typing a long multi-line replacement cannot reflow over text that has
// not yet been removed (the race that made type-first intermittent). The removal
// and insertion happen entirely on the ordinary text beside the chip; the atom is
// never selected or deleted.
func commitRegion(w byteWriter, rg spanRegion, replacement string, fence func() bool, chipBefore int) bool {
	if w == nil {
		return false
	}
	// Arm the atomic ownership gate for every destructive byte this commit emits:
	// spanEmit re-checks this fence under emitMu right before each write, closing the
	// check->emit window against the input loop's draftEpoch bump. Cleared on return
	// so the failure rollback (which must run even after the fence trips) emits freely.
	activeCommitFence.Store(&fence)
	defer activeCommitFence.Store(nil)
	// Content draft-fence: before the FIRST destructive key of this region, prove the
	// draft is still the one the plan was computed from — the region text equals the
	// planned original EXACTLY and the chip row count is unchanged. Captured user input
	// typed during the model wait is only ever APPENDED after the instruction, so it
	// does not change the leading region text; a real edit that alters the instruction
	// or damages the chip does, and aborts here. (A monotonic draft counter cannot
	// serve: it would trip on that legitimate captured append.)
	atStart := rg.prim == primReplaceFromStart
	chipNow := chipAtomCount()
	if chipBefore < 0 || chipNow < 0 || chipNow != chipBefore {
		logDebug("[SPAN] commit: chip state unknown or changed (was %d now %d); refusing to mutate", chipBefore, chipNow)
		return false
	}
	if !draftRowsMatchPayload(instructionRegionRows(atStart), expectedRenderedRegion(rg.text, atStart)) {
		logDebug("[SPAN] commit: draft region changed since planning; refusing to mutate")
		return false
	}
	logDebug("[SPAN] commit prim=%v regionRunes=%d replRunes=%d", rg.prim, len([]rune(rg.text)), len([]rune(replacement)))
	switch rg.prim {
	case primReplaceFromStart:
		// Instruction before the chip: Ctrl+Home, forward-Delete EXACTLY the
		// measured rendered instruction length, type in place. No +slack, no
		// retry loop: an overshoot is what ate the chip; a retry loop is what
		// duplicated text. Exact count is measured from the settled composer.
		reached := false
		for attempt := 0; attempt < 5 && !reached; attempt++ {
			_, _ = writeChild(ctrlHomeSeq)
			reached = waitCaretAtDraftStart(700 * time.Millisecond)
		}
		if !reached {
			logDebug("[SPAN] region start: caret not verified at draft start; abort")
			return false
		}
		if err := deleteRegionExact(true, fence); err != nil {
			logDebug("[SPAN] region start: delete failed (%v); abort", err)
			return false
		}
		if fence != nil && !fence() {
			logDebug("[SPAN] region start: fence refused before INSERTION; abort")
			return false
		}
		if !typeRegionExact(replacement, true, fence) {
			logDebug("[SPAN] region start: type not verified; abort")
			return false
		}
		return true
	case primReplaceFromEnd:
		// Instruction after the chip: Ctrl+End, Backspace EXACTLY the measured
		// rendered instruction length, type in place. Same exactness discipline.
		reached := false
		for attempt := 0; attempt < 5 && !reached; attempt++ {
			_, _ = writeChild(ctrlEndSeq)
			reached = waitCaretAtDraftEnd(700 * time.Millisecond)
		}
		if !reached {
			logDebug("[SPAN] region end: caret not verified at draft end; abort")
			return false
		}
		if err := deleteRegionExact(false, fence); err != nil {
			logDebug("[SPAN] region end: delete failed (%v); abort", err)
			return false
		}
		if fence != nil && !fence() {
			logDebug("[SPAN] region end: fence refused before INSERTION; abort")
			return false
		}
		if !typeRegionExact(replacement, false, fence) {
			logDebug("[SPAN] region end: type not verified; abort")
			return false
		}
		return true
	}
	return false
}

// measureInstructionRunes returns the exact rendered rune count of the ordinary
// instruction at the caret's draft edge (before the chip for start, after it for
// end). It strips only the "> " prompt cell and stops at the chip marker WITHOUT
// trimming the boundary space, so the count matches what the native deletes with
// the proven primitive (e.g. the 77-rune fixture). Returns 0 if it cannot read a
// sane region; the caller then aborts instead of guessing.
func measureInstructionRunes(atStart bool) int {
	r := outRenderer
	if r == nil || r.t == nil {
		return 0
	}
	t := r.t
	top, bot, ok := draftBorders(t)
	if !ok {
		return 0
	}
	spans, known := t.chipMarkerSpans()
	if !known {
		return 0
	}
	if atStart {
		var b strings.Builder
		for row := top + 1; row <= bot-1; row++ {
			line := strings.TrimRight(t.rowString(row), " ")
			if c, startHit, contHit := chipRowCut(t, row, spans); startHit {
				if c >= 0 && c <= len(line) {
					line = line[:c]
				}
				line = strings.TrimPrefix(line, "> ")
				b.WriteString(strings.TrimPrefix(line, ">"))
				return len([]rune(b.String()))
			} else if contHit {
				return len([]rune(b.String()))
			}
			line = strings.TrimPrefix(line, "> ")
			line = strings.TrimPrefix(line, ">")
			b.WriteString(line)
		}
		return 0
	}
	// End: text after the chip to the bottom border.
	var b strings.Builder
	seenChip := false
	var chip *chipSpan
	for k := range spans {
		if spans[k].StartRow >= top+1 {
			chip = &spans[k]
			break
		}
	}
	for row := top + 1; row <= bot-1; row++ {
		line := strings.TrimRight(t.rowString(row), " ")
		if !seenChip {
			if chip == nil {
				continue
			}
			if row < chip.EndRow {
				continue
			}
			if row == chip.EndRow {
				seenChip = true
				ec := gridColToRowIndex(t, row, chip.EndCol)
				rest := ""
				if ec >= 0 && ec <= len(line) {
					if j := strings.Index(line[ec:], "]"); j >= 0 {
						rest = strings.TrimSpace(line[ec+j+1:])
					}
				}
				b.WriteString(rest)
			}
			continue
		}
		b.WriteString(line)
	}
	n := len([]rune(strings.TrimSpace(b.String())))
	// Include any trailing spaces between the last non-blank rune and the caret
	// (e.g. when the user typed a space at the end before Ctrl+E), because
	// Ctrl+End places the caret after them and Backspace deletes them first.
	if row, col, inComposer := t.CursorPos(); inComposer && row >= top+1 && row <= bot-1 && col > 0 {
		rawRow := t.rowString(row)
		if idx := gridColToRowIndex(t, row, col); idx >= 0 && idx <= len(rawRow) {
			caretRunes := len([]rune(rawRow[:idx]))
			trimmedRunes := len([]rune(strings.TrimRight(rawRow, " ")))
			if caretRunes > trimmedRunes {
				n += caretRunes - trimmedRunes
			}
		}
	}
	return n
}

// deleteRegionExact removes the instruction at the caret's draft edge with exactly
// measureInstructionRunes() keystrokes: forward-Delete from the start edge (caret
// at offset 0 via Ctrl+Home) or Backspace from the end edge (caret after the
// region via Ctrl+End). No slack, no retry. Aborts (error) if the count cannot be
// measured sanely, rather than deleting an unknown number of runes.
func deleteRegionExact(atStart bool, guard func() bool) error {
	count := measureInstructionRunes(atStart)
	if count <= 0 {
		return fmt.Errorf("cannot measure instruction (count=%d)", count)
	}
	if count > 500 {
		return fmt.Errorf("implausible instruction count %d", count)
	}
	key := delSeq
	if !atStart {
		key = backspaceSeq
	}
	for i := 0; i < count; i++ {
		if guard != nil && !guard() {
			logDebug("[SPAN] deleteRegionExact: fence tripped at key %d/%d", i, count)
			return fmt.Errorf("delete fence tripped at key %d", i)
		}
		if err := spanEmit(key); err != nil {
			return fmt.Errorf("delete emission aborted: %w", err)
		}
		time.Sleep(22 * time.Millisecond)
	}
	time.Sleep(350 * time.Millisecond)
	logDebug("[SPAN] deleteRegionExact atStart=%v deleted=%d", atStart, count)
	return nil
}

// typeRegionExact types the replacement once and verifies the whole instruction
// region equals it EXACTLY (character for character, order preserved) — not
// whitespace-flattened. The comparison reads the region's rendered rows and uses
// draftRowsMatchPayload, which models ONLY the composer's own cells (prompt cell,
// two-space row indent, tab=4, and a soft wrap consuming no character) and refuses
// any other difference. So a spacing or newline difference the user or model
// introduced is a MISMATCH (the earlier flatten let "a   b" certify as "a b"). It
// NEVER retypes a tail: a mismatch returns false so the caller rolls back.
func typeRegionExact(replacement string, atStart bool, fence func() bool) bool {
	if fence != nil && !fence() {
		return false
	}
	target := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(replacement)
	if !typeSpanText(childWriter, target, fence) {
		// Fence tripped mid-insertion: do not verify as success; the caller rolls back.
		return false
	}
	time.Sleep(400 * time.Millisecond)
	return draftRowsMatchPayload(instructionRegionRows(atStart), expectedRenderedRegion(target, atStart))
}

// expectedRenderedRegion normalizes a region string to match instructionRegionRows,
// which trims right-side terminal grid padding on every row and trims leading
// spaces after the chip's closing ']' when !atStart.
func expectedRenderedRegion(text string, atStart bool) string {
	s := strings.TrimRight(text, " ")
	if !atStart {
		s = strings.TrimLeft(s, " ")
	}
	return s
}

// instructionRegionRows returns the rendered composer rows for the ordinary region
// at the caret's draft edge (before the chip for start, after it for end), with the
// composer's own cells (prompt glyph on the composer's first row, two-space indent
// on later rows) removed and the chip excluded. Trailing blank filler rows are
// dropped. The caller compares these rows to the expected text with
// draftRowsMatchPayload, which tolerates only renderer cells/soft-wrap/tab.
func instructionRegionRows(atStart bool) []string {
	r := outRenderer
	if r == nil || r.t == nil {
		return nil
	}
	t := r.t
	top, bot, ok := draftBorders(t)
	if !ok {
		return nil
	}
	var rows []string
	spans, known := t.chipMarkerSpans()
	if !known {
		return nil
	}
	if atStart {
		for row := top + 1; row <= bot-1; row++ {
			line := strings.TrimRight(t.rowString(row), " ")
			cut := false
			if c, startHit, contHit := chipRowCut(t, row, spans); startHit {
				if c >= 0 && c <= len(line) {
					line = line[:c]
				}
				cut = true
			} else if contHit {
				cut = true
			}
			if row == top+1 {
				line = stripPromptCell(line)
			} else {
				line = stripRendererIndent(line)
			}
			rows = append(rows, line)
			if cut {
				break
			}
		}
	} else {
		seen := false
		var chip *chipSpan
		for k := range spans {
			if spans[k].StartRow >= top+1 {
				chip = &spans[k]
				break
			}
		}
		for row := top + 1; row <= bot-1; row++ {
			line := strings.TrimRight(t.rowString(row), " ")
			if !seen {
				if chip == nil {
					continue
				}
				if row < chip.EndRow {
					continue
				}
				if row == chip.EndRow {
					seen = true
					ec := gridColToRowIndex(t, row, chip.EndCol)
					rest := ""
					if ec >= 0 && ec <= len(line) {
						if j := strings.Index(line[ec:], "]"); j >= 0 {
							rest = line[ec+j+1:]
						}
					}
					rest = strings.TrimLeft(rest, " ")
					if rest != "" {
						rows = append(rows, rest)
					}
					continue
				}
				continue
			}
			rows = append(rows, stripRendererIndent(line))
		}
	}
	for len(rows) > 0 && strings.TrimRight(rows[len(rows)-1], " \t") == "" {
		rows = rows[:len(rows)-1]
	}
	return rows
}

// chipAtomCount returns the number of detected chip atoms, or -1 when composer
// membership is unknown. Mutating callers must defer on -1.
func chipAtomCount() int {
	r := outRenderer
	if r == nil || r.t == nil {
		return -1
	}
	count, known := r.t.chipMarkerCount()
	if !known {
		return -1
	}
	return count
}

// chipRowCut consults the canonical chip spans and reports, for composer grid
// row r: whether a marker STARTS on this row (cut = index into rowString(r) where
// the marker begins) and whether this row is a CONTINUATION of a marker that
// began earlier (the whole row belongs to the chip). Wrap-aware: a marker split
// across soft-wrapped rows is handled by coordinate, never by collapsing text.
func chipRowCut(t *composerTracker, r int, spans []chipSpan) (cut int, startHit, contHit bool) {
	line := t.rowString(r)
	for _, sp := range spans {
		if r == sp.StartRow {
			c := gridColToRowIndex(t, r, sp.StartCol)
			if c < 0 || c > len(line) {
				return 0, true, false
			}
			return c, true, false
		}
		if r > sp.StartRow && r <= sp.EndRow {
			return 0, false, true
		}
	}
	return -1, false, false
}

// gridColToRowIndex maps a grid column on row r to its index in rowString(r),
// accounting for wide-rune filler cells (NUL) that rowString drops.
func gridColToRowIndex(t *composerTracker, r, col int) int {
	if r < 0 || r >= t.rows {
		return -1
	}
	row := t.grid[r]
	n := 0
	for i := 0; i < col && i < len(row); i++ {
		if row[i] != 0 {
			n++
		}
	}
	return n
}

// waitCaretAtDraftStart polls the frame tracker until the CLI's caret sits on the
// first ordinary draft row at column 0 (Ctrl+Home landed), or the deadline. It
// only trusts a frame that actually arrived after the key: a stale frame would
// read the pre-navigation caret and could pass or fail wrongly.
func waitCaretAtDraftStart(max time.Duration) bool {
	return waitCaretOnDraftEdge(max, true)
}

// waitCaretAtDraftEnd polls the frame tracker until the caret sits on the last
// ordinary draft row (Ctrl+End landed), or the deadline.
func waitCaretAtDraftEnd(max time.Duration) bool {
	return waitCaretOnDraftEdge(max, false)
}

func waitCaretOnDraftEdge(max time.Duration, wantStart bool) bool {
	deadline := time.Now().Add(max)
	var lastRow, lastCol int
	var lastIn bool
	var lastTop, lastBot int
	for time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
		r := outRenderer
		if r == nil || r.t == nil {
			return false
		}
		t := r.t
		top, bot, ok := draftBorders(t)
		if !ok {
			continue
		}
		row, col, inComposer := t.CursorPos()
		lastRow, lastCol, lastIn, lastTop, lastBot = row, col, inComposer, top, bot
		if !inComposer {
			continue
		}
		if wantStart {
			if row == top+1 && col <= 3 {
				logDebug("[SPAN] verify OK start: cursor row=%d col=%d top=%d bot=%d body=%q", row, col, top, bot, t.rowString(row))
				return true
			}
		} else {
			if row == bot-1 && col >= 0 {
				return true
			}
		}
	}
	logDebug("[SPAN] caret verify failed: wantStart=%v row=%d col=%d inComposer=%v topBody=%d botBody=%d", wantStart, lastRow, lastCol, lastIn, lastTop+1, lastBot-1)
	return false
}

// measureInstructionBeforeChip returns the rune count of the ordinary instruction
// that precedes the chip, as rendered right now (after Ctrl+Home, so the caret is
// at the draft start). It flattens soft wraps by concatenating every draft row up
// to the chip: the instruction may wrap a long row even though logical lines do
// not, and rg.text is the logical slice which can differ by soft-wrap whitespace.
// Deleting exactly this count means no forward-Delete ever touches the chip atom.
func measureInstructionBeforeChip() int {
	r := outRenderer
	if r == nil || r.t == nil {
		return 0
	}
	t := r.t
	top, bot, ok := draftBorders(t)
	if !ok {
		return 0
	}
	var b strings.Builder
	for row := top + 1; row <= bot-1; row++ {
		line := strings.TrimRight(t.rowString(row), " ")
		if i := strings.Index(line, pastedChipMarker); i >= 0 {
			line = line[:i]
			line = strings.TrimPrefix(line, ">")
			b.WriteString(line)
			return len([]rune(strings.TrimRight(b.String(), " ")))
		}
		line = strings.TrimPrefix(line, ">")
		b.WriteString(line)
	}
	// No chip found in the body: fall back to whatever ordinary text is present.
	return len([]rune(strings.TrimRight(b.String(), " ")))
}

// logFullComposer dumps the entire tracked screen plus a chip count, for judging
// the fully-settled state after a commit.
func logFullComposer(tag string) {
	r := outRenderer
	if r == nil || r.t == nil {
		logDebug("[SPAN] %s: no tracker", tag)
		return
	}
	t := r.t
	chips := 0
	for rr := 0; rr < 40; rr++ {
		l := strings.TrimRight(t.rowString(rr), " ")
		if strings.Contains(l, pastedChipMarker) {
			chips++
		}
		if l != "" {
			logDebug("[SPAN] %s row%02d=%q", tag, rr, l)
		}
	}
	logDebug("[SPAN] %s CHIPROWS=%d", tag, chips)
}

// logCaretState dumps the tracked composer body and caret for post-mortem of the
// commit sequence (drafts are normally never logged; this is diagnostic only).
func logCaretState(tag string) {
	r := outRenderer
	if r == nil || r.t == nil {
		logDebug("[SPAN] %s: no tracker", tag)
		return
	}
	t := r.t
	row, col, inC := t.CursorPos()
	logDebug("[SPAN] %s: caret row=%d col=%d inC=%v", tag, row, col, inC)
	for rr := 8; rr <= 24; rr++ {
		l := strings.TrimRight(t.rowString(rr), " ")
		if l != "" {
			logDebug("[SPAN] %s row%02d=%q", tag, rr, l)
		}
	}
}

// draftBorders finds the composer's top and bottom border rows by scanning for
// the hint line, walking up to the bottom border, then up again over the body to
// the top border. It does not depend on a fixed body height, so it also works for
// a tall multi-line instruction (unlike the narrow composerBand).
func draftBorders(t *composerTracker) (top, bot int, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	hint := -1
	for r := t.rows - 1; r >= 0; r-- {
		line := t.rowString(r)
		if indexOf(line, "Enhance Prompt") >= 0 || indexOf(line, "Enhance prompt sedang") >= 0 {
			hint = r
			break
		}
	}
	if hint < 1 {
		return 0, 0, false
	}
	// Walk up past any status row to the bottom border.
	r := hint - 1
	bottom := -1
	for ; r >= 0; r-- {
		if isEnhancerStatusText(strings.TrimSpace(t.rowString(r))) {
			continue
		}
		if t.rowIsBorder(r) {
			bottom = r
			break
		}
		return 0, 0, false // first non-status row above the hint must be the border
	}
	if bottom < 1 {
		return 0, 0, false
	}
	// Walk up over the body rows to the top border (bounded).
	r = bottom - 1
	for steps := 0; r >= 0 && steps < 40; steps, r = steps+1, r-1 {
		if t.rowIsBorder(r) {
			return r, bottom, true
		}
	}
	return 0, 0, false
}

// typeSpanText writes text as keystrokes; newlines are Shift+Enter, never a raw
// CR/LF (which would submit the prompt).
func typeSpanText(w byteWriter, s string, guard func() bool) bool {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if guard != nil && !guard() {
			logDebug("[SPAN] typeSpanText: fence tripped before chunk %d/%d", i, len(lines))
			return false
		}
		if i > 0 {
			if err := spanEmit(shiftEnterSeq); err != nil {
				logDebug("[SPAN] typeSpanText: ownership lost at newline %d/%d", i, len(lines))
				return false
			}
			time.Sleep(12 * time.Millisecond)
		}
		if line != "" {
			if guard != nil && !guard() {
				logDebug("[SPAN] typeSpanText: fence tripped before writing line %d", i)
				return false
			}
			if err := spanEmit([]byte(line)); err != nil {
				logDebug("[SPAN] typeSpanText: ownership lost writing line %d", i)
				return false
			}
		}
	}
	return true
}

func finishSpan(outHandle windows.Handle, msg string) {
	toastMessage.Store(&msg)
	paintStatus()
	go keepToastVisible(&msg, outHandle)
}

// abortSpanNoMutation ends a span attempt that never mutated the composer. Input
// captured while busy must not be silently stranded there: replay it (no
// mutation happened, so the draft still matches it) and log exactly how many ops
// were released. A partial write is already quarantined by releaseCapturedOps.
func abortSpanNoMutation(outHandle windows.Handle, msg string) {
	finishSpan(outHandle, msg)
	if n := releaseCapturedOps(); n > 0 {
		logDebug("[SPAN] abort released %d held input op(s); draft untouched", n)
	} else {
		logDebug("[SPAN] abort: no held input to release")
	}
}

func displayModelName(m string) string {
	if mm := strings.TrimSpace(strings.Split(m, "(")[0]); mm != "" {
		return mm
	}
	return "AI Model"
}

func formatSeconds(s float64) string {
	d := time.Duration(s * float64(time.Second)).Round(100 * time.Millisecond)
	return strings.TrimSuffix(d.String(), "s")
}

// sha16 is the first 8 bytes of SHA-256, hex. Used only to compare model I/O
// without ever logging draft, prompt, or response text.
func sha16(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

// spanResultUsable accepts a genuine CLI result and a cache hit (a real prior
// model result); both still pass the commit fence and region/chip checks.
func spanResultUsable(res EnhanceResult) bool {
	if strings.TrimSpace(res.Text) == "" {
		return false
	}
	if res.Source == SourceCliSubprocess {
		return true
	}
	if res.Source == SourceCache {
		return res.CacheOrigin == SourceCliSubprocess
	}
	return false
}

// ownershipMatches is the production fence predicate for draft and geometry.
// A dimension change bumps geometryEpoch under emitMu, so a stale plan cannot
// emit destructive keys after a resize.
func ownershipMatches(actualDraft, actualGeometry, ownerDraft, ownerGeometry uint64) bool {
	return actualDraft == ownerDraft && actualGeometry == ownerGeometry
}

// priorRollbackLimit returns the last region a prior-only recovery may restore.
// It is always before the untouched failed region, which must never be deleted.
func priorRollbackLimit(failedAt int) int { return failedAt - 1 }

// dispatchSpanCommitFailure performs the production recovery side effects for a
// failed commit. Recovery keystrokes are forbidden after any delivered bytes if
// the geometry changed: stale coordinates are not authorization.
func dispatchSpanCommitFailure(action commitFailureAction, ownerGeometry uint64, plan spanPlan, replacements []string, original []rune, failedAt int, delivered, regionDelivered int64, outHandle windows.Handle) {
	if action == commitFailurePriorRegionsOnly && geometryEpoch.Load() != ownerGeometry {
		logDebug("[SPAN] geometry lost after %d delivered byte(s); prior rollback NOT authorized, containing", delivered)
		action = commitFailureContainment
	}
	switch action {
	case commitFailureNoMutation:
		logDebug("[SPAN] abort before mutation region=%d; delivered_bytes=0; nothing to roll back", failedAt)
		abortSpanNoMutation(outHandle, "\x1b[33m✦ Enhancement dibatalkan sebelum mutasi; draft tidak berubah\x1b[0m")
	case commitFailureContainment:
		logDebug("[SPAN] commit region=%d delivered_bytes=%d partial=%d; containment: no recovery writes", failedAt, delivered, regionDelivered)
		quarantineCapturedInput()
		finishSpan(outHandle, "\x1b[31m✦ Enhancement gagal; state region tidak terverifikasi - input ditahan, JANGAN edit dulu\x1b[0m")
	default:
		logDebug("[SPAN] commit fence at untouched region=%d; delivered_bytes=%d; restoring prior regions only", failedAt, delivered)
		outcome := rollbackComposerToOriginal(plan, replacements, priorRollbackLimit(failedAt))
		switch outcome {
		case rollbackRestored:
			shadowDraftOverride.Store(&appliedDraft{Text: string(original)})
			releaseCapturedOps()
			finishSpan(outHandle, "\x1b[31m✦ Enhancement gagal; draft dikembalikan (terverifikasi)\x1b[0m")
		case rollbackChipCompromised:
			quarantineCapturedInput()
			finishSpan(outHandle, "\x1b[31m✦ Enhancement gagal; draft TIDAK terverifikasi - input ditahan utk dipulihkan, PERIKSA manual\x1b[0m")
		default:
			quarantineCapturedInput()
			finishSpan(outHandle, "\x1b[31m✦ Enhancement gagal; CHIP HILANG tak bisa dipulihkan - input ditahan utk dipulihkan, PERIKSA manual\x1b[0m")
		}
	}
}
