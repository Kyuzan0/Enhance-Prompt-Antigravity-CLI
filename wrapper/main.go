package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"github.com/UserExistsError/conpty"
	"golang.org/x/sys/windows"
)

var (
	isBusyState                 atomic.Bool
	outRenderer                 *outputRenderer
	bridgeEpoch                 atomic.Uint64
	toastMessage                atomic.Pointer[string]
	childSupportsBracketedPaste atomic.Bool
	composerChip                atomic.Bool
	composerBandKnown           atomic.Bool
	chipObservedFrame           atomic.Uint64
	pasteGuard                  atomic.Bool
	outputFrames                atomic.Uint64
	modelPickerOpen             atomic.Bool
	lastOutputNano              atomic.Int64
	currentSessionModel         atomic.Pointer[string]
	appliedDraftMirror          atomic.Pointer[appliedDraft]
	// shadowDraftOverride carries the committed chip-preserving span result to the
	// input loop. Unlike appliedDraftMirror it is text-only: the composer already
	// holds the new text (typed by keystrokes), so the shadow is updated WITHOUT a
	// clear-and-repaste, which would flatten the paste atom.
	shadowDraftOverride atomic.Pointer[appliedDraft]
	// enhanceKind says which flow owns the busy state, so an abort during a span
	// commit cancels only the model phase and never the atomic keystroke mutation.
	enhanceKind atomic.Int32
	spanEpoch   atomic.Uint64
	// draftEpoch advances on EVERY user keystroke event (printable/backspace/
	// delete/paste/navigation). The span job samples it at ownership; a change
	// before commit means the caret or content may have moved, so the plan is no
	// longer provably ours and the commit aborts (content fence still re-checks the
	// region text and chip count at the point of mutation).
	draftEpoch         atomic.Uint64
	shadowResetPending atomic.Bool
	triggerHoldActive  atomic.Bool
	enhanceCancel      context.CancelFunc
	enhanceMu          sync.Mutex
	logFileMu          sync.Mutex
	// childWriter is the ConPTY the wrapper forwards user input to. It is set once
	// when the child starts. childMu serializes writes to it, because the input loop
	// and the editor-bridge watcher both deliver keystrokes.
	childWriter byteWriter
	childMu     sync.Mutex
	// emitMu linearizes the two operations whose interleaving would let a
	// destructive commit key land AFTER a user event: (a) the input loop's
	// draftEpoch increment (user-event epoch bump) and (b) the commit's
	// check-then-emit of each destructive key. Both run under emitMu, so a
	// keystroke cannot bump the epoch in the gap between the commit's fence
	// check and its byte emission: the check and the write are atomic with
	// respect to user input. emitMu is a LEAF taken after capturedMu and before
	// childMu, matching the existing capturedMu->childMu order, so it adds no
	// new lock cycle.
	emitMu sync.Mutex
	// capturedOps holds keystrokes the wrapper held back while it was replacing
	// the composer, in the order they arrived. See capturedOp.
	capturedMu  sync.Mutex
	capturedOps []capturedOp
	// releasing is true while a handback is writing held keystrokes to the child.
	// A keystroke offered during that window is held instead of forwarded, so the
	// older bytes always reach the composer first: otherwise the input loop could
	// forward the user's newest keystroke immediately (the busy flag is already
	// clear at that point) and the handback would then type older text after it.
	releasing bool
	// pendingDraftApplies carries the draft effects of released keystrokes to the
	// input loop, which is the only owner of the shadow draft. It is used on the
	// paths where the watcher, not the loop, hands the composer back — a timeout
	// or a failure — because those paths have no resync to apply the effects to.
	// It is a list, not a single slot: two rewrites can end before the loop takes
	// its next event, and the second must not overwrite the first's effects.
	pendingDraftApplies []func([]rune) []rune
	// quarantinedOps holds keystrokes whose composer state became UNKNOWN after a
	// failed commit (a compromised/unrecoverable rollback). They are NOT replayed
	// onto the unknown draft and NOT dropped: they are kept aside so the state is
	// recoverable. While quarantineNonEmpty is set, forwardChild holds further
	// keystrokes (so nothing is delivered against an unknown draft); the batch is
	// released only by an explicit recover path (dropCapturedInput or a resync).
	quarantinedOps     []capturedOp
	quarantineNonEmpty bool
)

var (
	reAnsi         = regexp.MustCompile(`(?:\x1b\[[0-9;?]*[a-zA-Z~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?|\x1b[()][0-9A-Za-z]|\x1b[=><]|\x1b[78]|\x1bN|\x1bO)`)
	reStatusCol    = regexp.MustCompile(`(?i)(?:(?:auto|plan|accept-edits)\s*·\s*)?([a-zA-Z0-9.\-_ ()]+?)\s*·\s*(xhigh|max|high|medium|low)`)
	reStatusNoTier = regexp.MustCompile(`(?i)(?:(?:auto|plan|accept-edits)\s*·\s*)(Orchestrator\s*\([a-zA-Z0-9.\-_ ]+\))`)
)

func logDebug(format string, args ...any) {
	logFileMu.Lock()
	defer logFileMu.Unlock()
	logPath := filepath.Join(os.TempDir(), "agy-wrapper-debug.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	timestamp := time.Now().Format("2006-01-02 15:04:05.000")
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(f, "[%s][PID %d] %s\n", timestamp, os.Getpid(), msg)
}

// logSafeArgs returns argv with prompt payloads redacted, so a launch such as
// `agy -p "<prompt>"` never writes the prompt into the debug log.
func logSafeArgs(args []string) []string {
	out := make([]string, len(args))
	redactNext := false
	for i, a := range args {
		if redactNext {
			out[i] = "<redacted>"
			redactNext = false
			continue
		}
		switch {
		case a == "-p" || a == "--prompt":
			out[i] = a
			redactNext = true
		case strings.HasPrefix(a, "--prompt="):
			out[i] = "--prompt=<redacted>"
		case strings.HasPrefix(a, "-p="):
			out[i] = "-p=<redacted>"
		default:
			out[i] = a
		}
	}
	return out
}

const (
	busyHintText = "⏳ Enhance prompt sedang berlangsung... (Tekan Ctrl+E untuk berhenti)"
	busyHintStr  = "\x1b[33m" + busyHintText + "\x1b[0m"

	idleHintText = "✦ Enhance Prompt: Ctrl+E"
	// Visual width: 24 chars + 46 spaces = 70 cols to cleanly cover busyHintText
	idleHintStr = "\x1b[90m✦ Enhance Prompt: \x1b[36mCtrl+E\x1b[0m                                              "

	// The statusLine command's output is decoded by the native CLI as ANSI
	// (Windows-1252), so the status file must stay ASCII to avoid mojibake.
	idleStatusText = "Enhance Prompt: Ctrl+E"
	busyStatusText = "Enhance prompt sedang berlangsung... (Tekan Ctrl+E untuk berhenti)"
)

var statusFilePath = filepath.Join(os.TempDir(), "agy-statusline.txt")

// syncStatusFile keeps the native statusLine row blank for the whole wrapper
// session. During an external-editor wait the native CLI's statusLine refresh
// is not provable from an isolated invocation (unsigned HOME stops at
// onboarding), so the wrapper owns exactly one status row in the composer slot
// and must not let a second, possibly stale, copy render below it. The file
// remains readable ASCII; the idle text is restored on exit for plain runs.
func syncStatusFile(_ bool) {
	_ = os.WriteFile(statusFilePath, []byte(" "), 0644)
}

func restoreNativeStatusLine() {
	_ = os.WriteFile(statusFilePath, []byte(idleStatusText), 0644)
}

func getCurrentModel() string {
	// Observed per-session selection (status bar or picker) outranks env and
	// runtime defaults: settings and env are only the starting seed.
	if m := currentSessionModel.Load(); m != nil && *m != "" {
		return *m
	}
	if ctx := GetActiveRuntimeContext(); ctx != nil && ctx.ActiveModelID != "" {
		return ctx.ActiveModelID
	}
	if envModel := os.Getenv("ACTIVE_MODEL"); envModel != "" {
		return envModel
	}
	return ""
}

func getBusyHint() string {
	return busyHintStr
}

func getCurrentHint() string {
	if tm := toastMessage.Load(); tm != nil {
		return *tm
	}
	if isBusyState.Load() {
		return getBusyHint()
	}
	return idleHintStr
}

func initSessionModel() {
	var initialModel string
	var source ModelContextSource = SourceSettingsConfig

	// 1. Read default model from ~/.gemini/antigravity-cli/settings.json
	home, err := os.UserHomeDir()
	if err == nil {
		settingsPath := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
		if data, err := os.ReadFile(settingsPath); err == nil {
			var s struct {
				Model string `json:"model"`
			}
			if json.Unmarshal(data, &s) == nil && s.Model != "" {
				initialModel = s.Model
			}
		}
	}

	// 2. Check environment variable override
	if envModel := os.Getenv("ACTIVE_MODEL"); envModel != "" {
		initialModel = envModel
		source = SourceHTTPHeader
	}

	// 3. Check explicit CLI flags e.g. agy --model "Claude Sonnet 4.6"
	for i, arg := range os.Args[1:] {
		if arg == "--model" && i+1 < len(os.Args[1:]) {
			initialModel = os.Args[i+2]
			source = SourcePayloadOverride
		} else if strings.HasPrefix(arg, "--model=") {
			initialModel = strings.TrimPrefix(arg, "--model=")
			source = SourcePayloadOverride
		}
	}

	if initialModel != "" {
		family := DetectFamily(initialModel)
		_, effort := ResolveCanonicalModelID(initialModel)
		SetActiveRuntimeContext(&ModelRuntimeContext{
			ActiveModelID: initialModel,
			Family:        family,
			Effort:        ReasoningEffort(effort),
			Source:        source,
		})
		logDebug("[MODEL] Initialized session model from %s: %s (family=%s, effort=%s)", source, initialModel, family, effort)
	}
}

// updateSessionModelFromPickerMessage freezes the model the user explicitly
// selected in this session's picker. It is process-scoped authority: settings.json
// is only the initial seed, never re-read while the session runs.
func updateSessionModelFromPickerMessage(data []byte) {
	plain := string(reAnsi.ReplaceAll(data, nil))
	idx := strings.LastIndex(plain, "Model set to ")
	if idx < 0 {
		return
	}
	rest := plain[idx+len("Model set to "):]
	if nl := strings.IndexAny(rest, "\r\n"); nl >= 0 {
		rest = rest[:nl]
	}
	model := strings.TrimSpace(rest)
	if model == "" || strings.Contains(strings.ToLower(model), "no models") {
		return
	}
	if ctx := GetActiveRuntimeContext(); ctx != nil && ctx.ActiveModelID == model {
		return
	}
	_, effort := ResolveCanonicalModelID(model)
	SetActiveRuntimeContext(&ModelRuntimeContext{
		ActiveModelID: model,
		Family:        DetectFamily(model),
		Effort:        ReasoningEffort(effort),
		Source:        SourceActiveSession,
	})
	logDebug("[MODEL] Frozen session selection from picker: %s (effort=%s)", model, effort)
}

func formatEffortTitle(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "xhigh":
		return "XHigh"
	case "":
		return ""
	default:
		e := strings.ToLower(strings.TrimSpace(effort))
		return strings.ToUpper(e[:1]) + e[1:]
	}
}

func updateModelFromChunk(data []byte) {
	if !bytes.Contains(data, []byte("\xc2\xb7")) {
		return
	}
	plain := string(reAnsi.ReplaceAll(data, nil))
	var model, modelBase, effort string
	if matches := reStatusCol.FindStringSubmatch(plain); len(matches) >= 3 {
		modelBase = strings.TrimSpace(matches[1])
		effort = strings.ToLower(strings.TrimSpace(matches[2]))
		if effort != "" {
			model = fmt.Sprintf("%s (%s)", modelBase, formatEffortTitle(effort))
		} else {
			model = modelBase
		}
	} else if matches := reStatusNoTier.FindStringSubmatch(plain); len(matches) >= 2 {
		modelBase = strings.TrimSpace(matches[1])
		model = modelBase
	}
	if model != "" {
		curr := GetActiveRuntimeContext()
		if curr == nil || curr.ActiveModelID != model {
			family := DetectFamily(modelBase)
			SetActiveRuntimeContext(&ModelRuntimeContext{
				ActiveModelID: model,
				Family:        family,
				Effort:        ReasoningEffort(effort),
				Source:        SourcePTYStatusBar,
			})
			logDebug("[MODEL] Detected active session model from status bar: %s (family=%s, effort=%s)", model, family, effort)
		}
	}
}

func getConsoleSize(handle windows.Handle) (int, int) {
	var csbi windows.ConsoleScreenBufferInfo
	err := windows.GetConsoleScreenBufferInfo(handle, &csbi)
	if err != nil {
		return 120, 30
	}
	w := int(csbi.Window.Right - csbi.Window.Left + 1)
	h := int(csbi.Window.Bottom - csbi.Window.Top + 1)
	if w <= 0 {
		w = 120
	}
	if h <= 0 {
		h = 30
	}
	return w, h
}

// deleteWordBackwards deletes backwards to the previous word boundary or path delimiter.
// isEnhanceBangStart reports whether the buffered leading "!" text can still
// become one of the wrapper-owned enhance aliases. It deliberately does not
// claim "! " because that is ambiguous with ordinary native shell mode.
func isEnhanceBangStart(text string) bool {
	lower := strings.ToLower(text)
	for _, alias := range []string{"!/enhance", "!enhance"} {
		if strings.HasPrefix(alias, lower) || strings.HasPrefix(lower, alias) {
			return true
		}
	}
	return false
}

func deleteWordBackwards(runes []rune) []rune {
	if len(runes) == 0 {
		return nil
	}
	i := len(runes) - 1
	// 1. Skip trailing spaces or tabs
	for i >= 0 && (runes[i] == ' ' || runes[i] == '\t') {
		i--
	}
	if i < 0 {
		return nil
	}
	// If at a newline, delete just the newline
	if runes[i] == '\n' {
		return runes[:i]
	}

	isDelim := func(r rune) bool {
		return r == '/' || r == '\\' || r == '.' || r == ':' || r == '-' || r == '_' || r == ',' || r == ';'
	}

	// If trailing character is a delimiter, consume consecutive delimiters
	if isDelim(runes[i]) {
		for i >= 0 && isDelim(runes[i]) {
			i--
		}
		return runes[:i+1]
	}

	// Otherwise, delete word characters up to previous delimiter or whitespace
	for i >= 0 {
		r := runes[i]
		if r == ' ' || r == '\t' || r == '\n' || isDelim(r) {
			break
		}
		i--
	}
	return runes[:i+1]
}

// appendPastePayload records the runes a paste just contributed to the shadow
// draft. It is fed the draft's own appended slice so the payload and the draft
// can never disagree about how a character decoded, and it honours the payload
// cap by dropping from the front, which is the same window the gate compares.
func appendPastePayload(dst []rune, added []rune) []rune {
	if len(added) == 0 {
		return dst
	}
	dst = append(dst, added...)
	if len(dst) > pastePayloadLimit {
		dst = dst[len(dst)-pastePayloadLimit:]
	}
	return dst
}

// abortHelper publishes the cancel signal and clears the busy state.
//
// The enhancement helper runs as a *separate process*; the only thing it
// observes is the cancel marker file in the bridge directory. Every abort path
// must therefore publish that marker, and must do it before the busy flag is
// cleared: a path that only clears the flag leaves the helper running to
// success, so an aborted enhancement can still write its draft back and report
// completion. Ctrl+E and Ctrl+C both call this.
// capturedOp is one keystroke the wrapper held back while it was replacing the
// composer, together with the effect that keystroke has on the shadow draft.
//
// Both parts are needed. The bytes must reach the composer once the replacement
// has landed, in the order they were typed; the effect must be applied to the
// draft at that same moment, and in the same order, because the replacement
// starts the draft over from the enhanced text. A keystroke whose effect were
// applied early would be wiped by the replacement, and one whose effect were
// never applied would leave the draft describing a composer the user no longer
// has — the draft is what the chip guard and the next gate decision read.
type capturedOp struct {
	raw   []byte
	apply func([]rune) []rune
}

// forwardOutcome makes delivery explicit so no caller treats a failed/partial
// child write as delivered. fcDelivered: bytes fully written now (apply shadow).
// fcHeld: queued for the rewrite window (capture owns bytes+effect). fcFailed: not
// (fully) delivered now; the unsent byte suffix was quarantined (containment, sent
// prefix preserved, no whole-event retry) — callers must NOT apply the shadow.
type forwardOutcome int

const (
	fcDelivered forwardOutcome = iota
	fcHeld
	fcFailed
)

// applyIfDelivered applies a draft effect ONLY when the byte was fully delivered
// now. Held (capture owns the effect) and failed (not delivered; suffix
// quarantined) leave the shadow unchanged.
func applyIfDelivered(out forwardOutcome, d []rune, apply func([]rune) []rune) []rune {
	if out == fcDelivered {
		return apply(d)
	}
	return d
}

// keepPrefixIfNotDelivered truncates the shadow back to before the event unless it
// was fully delivered now (held -> capture owns; failed -> match native).
func keepPrefixIfNotDelivered(out forwardOutcome, d []rune, before int) []rune {
	if out == fcDelivered {
		return d
	}
	if before < 0 {
		before = 0
	}
	if before > len(d) {
		before = len(d)
	}
	return d[:before]
}

// captureOp records one held keystroke. apply may be nil for a key that only
// moves the caret or that the wrapper tracks without changing the draft.
func captureOp(raw []byte, apply func([]rune) []rune) {
	capturedMu.Lock()
	defer capturedMu.Unlock()
	capturedOps = append(capturedOps, capturedOp{raw: raw, apply: apply})
}

// forwardChild writes one input event's bytes to the child — unless the wrapper
// is about to replace the whole composer, in which case the bytes are held and
// replayed once the replacement has landed.
//
// The gate is the rewrite window itself: while the helper is running, and while
// an applied draft is still waiting to be written into the composer, a byte sent
// to the child now would be erased by the clear-and-paste that ends that window.
//
// The decision and the action must not be split. Read separately — is the
// wrapper busy? is a handback draining? — the answers can go stale between the
// read and the write: a handback that starts in that gap delivers OLDER held
// bytes after this newer keystroke, and the composer then shows the user's text
// out of order. Holding capturedMu across both the test and the write closes the
// gap, because a handback takes the same lock before it extracts anything. The
// queue depth is part of the test for the same reason: bytes already waiting are
// older than this keystroke, so forwarding past them is the same inversion.
//
// writeChild is called while capturedMu is held; it takes only childMu, which no
// path holds while waiting for capturedMu, so the two locks are never acquired in
// opposite orders and no deadlock is possible.
//
// It returns true when the keystroke was held. The caller must then leave its
// own draft untouched: apply owns that effect and will run it at playback.
func forwardChild(raw []byte, apply func([]rune) []rune) forwardOutcome {
	capturedMu.Lock()
	defer capturedMu.Unlock()
	if isBusyState.Load() || appliedDraftMirror.Load() != nil || releasing || len(capturedOps) > 0 || quarantineNonEmpty {
		capturedOps = append(capturedOps, capturedOp{raw: raw, apply: apply})
		if len(raw) <= 24 {
			logDebug("[CAPTURE] held %d byte(s) hex=%x while busy=%v mirror=%v releasing=%v queue=%d", len(raw), raw, isBusyState.Load(), appliedDraftMirror.Load() != nil, releasing, len(capturedOps))
		}
		return fcHeld
	}
	n, err := writeChild(raw)
	if err == nil && n == len(raw) {
		return fcDelivered
	}
	// Not fully delivered: the delivered prefix (if any) reached native; the unsent
	// suffix is quarantined as containment (unknown native state) preserving it and
	// ordering WITHOUT retrying the whole event. A failed delivery arms the hold so
	// later keystrokes are held rather than typed into an unknown composer. We are
	// already under capturedMu, so append directly (no re-lock, no export here).
	suffix := raw
	if n > 0 && n <= len(raw) {
		suffix = raw[n:]
	}
	if len(suffix) > 0 {
		quarantinedOps = append(quarantinedOps, capturedOp{raw: append([]byte(nil), suffix...)})
		quarantineNonEmpty = true
	}
	logDebug("[CAPTURE] forward write FAILED (n=%d/%d err=%v); %d byte suffix quarantined, not retried", n, len(raw), err, len(suffix))
	return fcFailed
}

// writeChild delivers bytes to the child under the writer lock: the input loop
// and the editor-bridge watcher can both be delivering keystrokes at once.
// writeChild delivers bytes to the child under the writer lock and returns the
// EXACT outcome: a nil writer is errNoChildWriter, a short write with no error is
// io.ErrShortWrite, and any childWriter error is returned verbatim. Callers that
// publish shadow/UI state (commit/rollback/queue drain) MUST honor it so a
// partial/failed child write never masquerades as success.
func writeChild(raw []byte) (int, error) {
	if childWriter == nil {
		return 0, errNoChildWriter
	}
	if p := os.Getenv("AGY_INDUMP"); p != "" {
		if f, e := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); e == nil {
			fmt.Fprintf(f, "W %s %d %x\n", time.Now().Format("15:04:05.000"), len(raw), raw)
			_ = f.Close()
		}
	}
	childMu.Lock()
	n, err := childWriter.Write(raw)
	childMu.Unlock()
	if err == nil && n < len(raw) {
		err = io.ErrShortWrite
	}
	return n, err
}

// errNoChildWriter means the child is not attached yet; a write that reports this
// must not be treated as delivered.
var errNoChildWriter = fmt.Errorf("child writer not attached")

// activeCommitFence, when non-nil, is the ownership predicate for the region
// commit currently mutating the composer. spanEmit re-evaluates it under emitMu
// immediately before each destructive byte, so "check ownership then write" is
// atomic with respect to the input loop's draftEpoch bump (also under emitMu):
// a user event can no longer slip between the fence check and the write.
var activeCommitFence atomic.Pointer[func() bool]

// spanEmit emits one destructive byte for a span mutation, atomically re-checking
// the active commit fence under emitMu. With no active commit it is exactly
// writeChild. Returns errOwnershipLost if the fence trips at emit time; the caller
// aborts (and the commit rolls back).
// spanDeliveredBytes counts destructive bytes that actually reached the child
// during this commit attempt. It is the only rollback authorization: a region
// attempt or a tripped fence does not increment it, while a partial write with
// n>0 and err!=nil does.
var spanDeliveredBytes atomic.Int64

// geometryEpoch advances only on an OBSERVED real console dimension change,
// under emitMu. Detection is the 250ms watcher (or synthetic redraw); a resize
// not yet observed can still be stale for up to that interval.
var geometryEpoch atomic.Uint64

func spanEmit(raw []byte) error {
	emitMu.Lock()
	defer emitMu.Unlock()
	if fn := activeCommitFence.Load(); fn != nil && !(*fn)() {
		return errOwnershipLost
	}
	n, err := writeChild(raw)
	if n > 0 {
		spanDeliveredBytes.Add(int64(n))
	}
	if n > 0 && err != nil {
		logDebug("[SPAN] partial emission: %d/%d byte(s) delivered (err=%v)", n, len(raw), err)
	}
	return err
}

// errOwnershipLost is returned by spanEmit when the ownership fence trips at the
// moment of emission; callers treat it as an aborted commit.
var errOwnershipLost = fmt.Errorf("span ownership lost at emit")

// sendSettled writes one navigation key and waits for the CLI to repaint and then
// go quiet before returning, so the next action (typing) acts on the settled
// composer, not the frame before the key. This is the same discipline the native
// window walk uses; without it, a fast Ctrl+Home followed immediately by typing
// can interleave when the model's replacement is longer/multi-line.
func sendSettled(raw []byte) {
	before := outputFrames.Load()
	if _, err := writeChild(raw); err != nil {
		logDebug("[CAPTURE] sendSettled write failed (%v); %d byte(s) not fully delivered", err, len(raw))
	}
	deadline := time.Now().Add(1800 * time.Millisecond)
	for time.Now().Before(deadline) {
		advanced := outputFrames.Load() > before
		quiet := time.Since(time.Unix(0, lastOutputNano.Load())) >= composerQuietWindow
		if advanced && quiet {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
}

// flushCapturedOps delivers every held keystroke to the composer, in order, and
// applies each one's effect to the draft in the same order, returning the updated
// draft. It is called at the moment the composer is handed back (after a resync,
// or when a rewrite ends without one), and takes only what is waiting then, so a
// keystroke arriving later is forwarded normally instead of being queued behind a
// delivery that already happened.
func flushCapturedOps(draft []rune, w byteWriter) (int, []rune) {
	capturedMu.Lock()
	ops := capturedOps
	capturedOps = nil
	capturedMu.Unlock()

	if len(ops) == 0 {
		return 0, draft
	}
	// The bytes go out as one write — a burst of single-byte writes to the
	// ConPTY would be delivered as a burst of tiny reads — while the effects are
	// applied one at a time, in the order the user typed them.
	payload := make([]byte, 0, 16)
	for _, op := range ops {
		payload = append(payload, op.raw...)
	}
	if w != nil && len(payload) > 0 {
		_, _ = w.Write(payload)
	}
	for _, op := range ops {
		if op.apply != nil {
			draft = op.apply(draft)
		}
	}
	logDebug("[CAPTURE] delivered %d keystroke(s) held during the composer replacement, %d byte(s)", len(ops), len(payload))
	return len(ops), draft
}

// releaseCapturedOps is flushCapturedOps for the paths where the watcher, not
// the input loop, hands the composer back: a timeout or a failure, where no
// replacement erased the held keystrokes, so they are typed into the composer
// now instead of being silently dropped. The bytes are written here; the effects
// travel to the input loop through pendingDraftApplies, because the loop is the
// only owner of the shadow draft.
//
// A cancel does not call this: it clears the composer back to the pre-rewrite
// draft, so bytes typed for the abandoned run belong to a draft the user has
// already left. See dropCapturedInput.
func releaseCapturedOps() (released int) {
	for {
		capturedMu.Lock()
		ops := capturedOps
		capturedOps = nil
		if len(ops) == 0 {
			releasing = false
			capturedMu.Unlock()
			return released
		}
		// The drain gate is taken together with the queue, before any byte is
		// written, so a keystroke offered from here on is held rather than
		// forwarded ahead of these older bytes.
		releasing = true
		payload := make([]byte, 0, 16)
		for _, op := range ops {
			payload = append(payload, op.raw...)
		}
		capturedMu.Unlock()
		// The child write happens outside the mutex: it can block, and holding
		// capturedMu would stop the input loop from recording anything. Honor the
		// EXACT (n,err): only bytes that actually reached native may have their draft
		// effects applied. Account op-by-op so a partial write is never replayed as a
		// whole (that duplicated the delivered prefix); a straddling op / unsent
		// suffix (e.g. an interrupted escape sequence) is CONTAINMENT (quarantined,
		// its native state unknown) — never blind-replayed, never silently dropped.
		var n int
		var werr error
		if len(payload) > 0 {
			n, werr = writeChild(payload)
		}
		if werr == nil || n >= len(payload) {
			// All bytes reached native: apply every op's draft effect (reflects reality).
			capturedMu.Lock()
			for _, op := range ops {
				if op.apply != nil {
					pendingDraftApplies = append(pendingDraftApplies, op.apply)
				}
			}
			capturedMu.Unlock()
			released += len(ops)
			logDebug("[CAPTURE] released %d keystroke(s) held during a rewrite that never resynced, %d byte(s) hex=%x", len(ops), len(payload), payload)
			if werr != nil {
				capturedMu.Lock()
				releasing = false
				capturedMu.Unlock()
				logDebug("[CAPTURE] release: all bytes observed but child reported %v; stopping drain", werr)
				return
			}
		} else {
			if n < 0 {
				n = 0
			}
			var landed []capturedOp
			off := 0
			splitIdx := len(ops)
			var straddle *capturedOp
			for i := range ops {
				L := len(ops[i].raw)
				if off+L <= n {
					landed = append(landed, ops[i])
					off += L
					continue
				}
				if off < n && n < off+L {
					c := ops[i]
					straddle = &c
				}
				splitIdx = i
				break
			}
			if len(landed) > 0 {
				capturedMu.Lock()
				for _, op := range landed {
					if op.apply != nil {
						pendingDraftApplies = append(pendingDraftApplies, op.apply)
					}
				}
				capturedMu.Unlock()
			}
			var unsent []capturedOp
			if straddle != nil {
				unsent = append(unsent, *straddle)
				unsent = append(unsent, ops[splitIdx+1:]...)
			} else if splitIdx < len(ops) {
				unsent = append(unsent, ops[splitIdx:]...)
			}
			capturedMu.Lock()
			if len(unsent) > 0 {
				quarantinedOps = append(quarantinedOps, unsent...)
				quarantineNonEmpty = true
			}
			releasing = false
			capturedMu.Unlock()
			released += len(landed)
			logDebug("[CAPTURE] release PARTIAL: n=%d err=%v; %d op(s) landed (draft-effects applied), %d op(s) quarantined (unknown native state, NOT replayed)", n, werr, len(landed), len(unsent))
			exportQuarantinedInput("release-partial")
			return released
		}
		// Loop: keystrokes that arrived while those bytes were being written are
		// the user's typing too, and nothing else will deliver them on a path
		// with no resync. The queue is re-checked under the gate, so the next
		// batch is taken only once the previous one has landed.
	}
}

// adoptPendingDraftOps runs the draft effects of keystrokes released by the
// watcher, in the order they were typed. It is called by the input loop, which
// owns the draft.
func adoptPendingDraftOps(draft []rune) []rune {
	capturedMu.Lock()
	applies := pendingDraftApplies
	pendingDraftApplies = nil
	capturedMu.Unlock()
	for _, apply := range applies {
		draft = apply(draft)
	}
	return draft
}

// dropCapturedInput discards keystrokes held for a rewrite the user abandoned.
// The abort is the newer intent, and the shadow draft is being reset with it.
func dropCapturedInput() {
	capturedMu.Lock()
	capturedOps = nil
	pendingDraftApplies = nil
	releasing = false
	quarantinedOps = nil
	quarantineNonEmpty = false
	capturedMu.Unlock()
}

// quarantineCapturedInput is the handback for a failed commit whose composer state
// is UNKNOWN (a compromised or unrecoverable rollback). Replaying held keystrokes
// there could type the user's text into the wrong position, and dropping them
// would lose the user's typing; both are unsafe when we cannot see the draft. So
// the held bytes are moved aside intact (quarantinedOps) and further keystrokes
// are held, so the buffer stays a complete, recoverable batch. Nothing is
// delivered or discarded; recovery is an explicit later step.
func quarantineCapturedInput() {
	capturedMu.Lock()
	quarantinedOps = append(quarantinedOps, capturedOps...)
	capturedOps = nil
	quarantineNonEmpty = true
	n := len(quarantinedOps)
	capturedMu.Unlock()
	logDebug("[CAPTURE] quarantined %d keystroke(s): composer state unknown after failed commit; not replayed, not dropped", n)
}

// openExclusivePrivate opens base for a durable recovery export without ever
// overwriting an existing file: O_CREATE|O_EXCL|O_WRONLY, mode 0600. A directory
// (or a non-EEXIST open error) is a hard failure. A name already taken by a file
// yields a unique ".N" sibling, itself O_EXCL, so two exports can never collide.
func openExclusivePrivate(base string) (*os.File, string, error) {
	if st, err := os.Stat(base); err == nil && st.IsDir() {
		return nil, "", fmt.Errorf("export path is a directory: %s", base)
	}
	for i := 0; i < 100; i++ {
		p := base
		if i > 0 {
			p = fmt.Sprintf("%s.%d", base, i)
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			return f, p, nil
		}
		if os.IsExist(err) {
			continue // name taken by a file -> try a unique sibling, never overwrite
		}
		return nil, "", err
	}
	return nil, "", fmt.Errorf("no free export name for %s", base)
}

// exportQuarantinedInput is the LOSSLESS recovery of quarantined keystrokes: it
// writes the exact held bytes to a file and returns (path, byteCount). It never
// consumes the internal buffer, so it is safe to call on Ctrl+C and at process
// exit — the user's input is preserved to disk instead of being dropped when the
// process dies. Returns ("",0) when nothing is quarantined. Path comes from
// AGY_WRAP_QUARANTINE_EXPORT, else %TEMP%\agy_wrapper_quarantine_<pid>.bin.
func exportQuarantinedInput(reason string) (string, int) {
	capturedMu.Lock()
	defer capturedMu.Unlock()
	if len(quarantinedOps) == 0 {
		return "", 0
	}
	base := strings.TrimSpace(os.Getenv("AGY_WRAP_QUARANTINE_EXPORT"))
	if base == "" {
		// Unique default: PID alone collides on PID reuse and concurrent exports.
		base = filepath.Join(os.TempDir(), fmt.Sprintf("agy_wrapper_quarantine_%d_%d.bin", os.Getpid(), time.Now().UnixNano()))
	}
	var payload []byte
	for _, op := range quarantinedOps {
		payload = append(payload, op.raw...)
	}
	// Durable write with exclusive, no-overwrite, private semantics: O_EXCL (never
	// O_TRUNC) so an existing file is never clobbered; 0600 so the recovery file is
	// private. If the configured name is already taken by a FILE we pick a unique
	// .N sibling (still exclusive); if the path is a directory (or any non-EEXIST
	// error) we fail and keep the bytes in memory. Then fsync + readback confirm the
	// exact bytes landed. The in-memory buffer is retained regardless.
	f, path, err := openExclusivePrivate(base)
	if err != nil {
		logDebug("[CAPTURE] quarantine export open %q FAILED (%v); bytes still held in memory", base, err)
		return "", 0
	}
	if _, err = f.Write(payload); err == nil {
		err = f.Sync()
	}
	cerr := f.Close()
	if err != nil || cerr != nil {
		logDebug("[CAPTURE] quarantine export write/sync %q FAILED (%v/%v); bytes still held in memory", path, err, cerr)
		return "", 0
	}
	if back, rerr := os.ReadFile(path); rerr != nil || !bytes.Equal(back, payload) {
		logDebug("[CAPTURE] quarantine export readback %q MISMATCH (%v); not confirmed; buffer retained", path, rerr)
		return "", 0
	}
	logDebug("[CAPTURE] quarantine EXPORTED+FSYNCED+VERIFIED (%s): %d keystroke(s), %d byte(s) -> %s (buffer retained)", reason, len(quarantinedOps), len(payload), path)
	return path, len(payload)
}

// endRewriteWithoutResync is the handback for a rewrite that produced no draft
// to apply: a timeout, a failure, or a cancel. The composer still holds the
// draft the user had, so keystrokes the wrapper held are typed into it here
// rather than dropped — they were never delivered, and losing them would lose
// the user's own typing. A cancel is no exception: the wrapper consumes the
// abort key itself and the CLI's draft is left intact, so text the user typed
// while the rewrite owned the composer belongs in that draft.
//
// A rewrite whose draft the wrapper is discarding — the withheld-trigger case,
// where the trigger text never reached the CLI — resets the shadow so the next
// keystrokes start clean, but still delivers the held bytes: they are the user's
// typing either way, and a composer that shows them is strictly safer than one
// that silently swallowed them.
func endRewriteWithoutResync() {
	if triggerHoldActive.Swap(false) {
		shadowResetPending.Store(true)
	}
	releaseCapturedOps()
}

// abortHelper publishes the cancel signal and clears the busy state.
//
// It deliberately does not decide the fate of keystrokes held for the abandoned
// rewrite. The helper is a separate process and may still be running when this
// returns, so delivering those bytes here would race the CLI's own return from
// the editor; instead they stay held until the watcher observes the rewrite's
// terminal status, which is the single place that decides (see
// endRewriteWithoutResync). Delivering them there also guarantees the bytes land
// in a composer the CLI has already handed back.
func abortHelper(handle *editorBridge) {
	if handle != nil {
		handle.cancel()
	}
	isBusyState.Store(false)
	syncStatusFile(false)
}

// caretAnchor is where the caret was before the walk moved the box: which draft
// row it was on, how far into that row, and which window was on screen. All three
// are needed to put the view back. Ctrl+End is not a restore of this: it always
// parks the caret at the end of the draft, so a user editing in the middle would
// be silently moved, and a draft taller than its box would also come back on a
// different window than the one the user was reading.
type caretAnchor struct {
	draftRow int // 1-based rendered row index within the whole draft
	col      int // 0-based column within that rendered row
	top      int // rows elided above the window at capture time
	bot      int // rows elided below the window at capture time
	total    int // rendered height of the whole draft at capture time
	valid    bool
}

// captureCaretAnchor turns the tracker's cursor screen position into a position
// in the draft itself, using the window that is on screen right now. Without a
// window there is no mapping, so no anchor is claimed.
//
// The membership test is the window's own mapping, not the tracker's composer
// band: the band is deliberately narrow (it also bounds the chip scan), so a
// tall draft's rows sit outside it and a band test would refuse an anchor for
// exactly the offscreen drafts the walk exists for.
func captureCaretAnchor(tracker *composerTracker, win composerWindow) caretAnchor {
	if tracker == nil {
		return caretAnchor{}
	}
	row, col, _ := tracker.CursorPos()
	idx, ok := win.DraftIndexAtScreenRow(row)
	if !ok {
		return caretAnchor{}
	}
	return caretAnchor{draftRow: idx, col: col, top: win.top, bot: win.bot, total: win.total(), valid: true}
}

// restoreCaretAnchor puts the caret back on the draft row it started on, in the
// viewport it was found in.
//
// The row alone is not the caret: a restore that landed one column to the left
// would report success while leaving the user somewhere they were not, so the
// column is checked too. And the row alone is not the view: the box shows only a
// window of a tall draft, and the same draft row can be visible in more than one
// window — on a 24-row draft in a 15-row box, row 15 is in both the head window
// (rows 1..15) and the tail window (rows 10..24). Restoring a tail view by
// walking down from Ctrl+Home therefore lands the caret on the right row inside
// the wrong window, which is exactly the surprise this restore exists to prevent.
// The approach is chosen from the anchor's own scrolled state — the rows the CLI
// elided above it — so the walk arrives from the same side the user left.
//
// The restored window must then match the captured one exactly, bound for bound.
// A caret-driven viewport is a minimal-scroll view, so an anchor that was itself
// a middle window (rows elided both above and below) is not reproducible with
// caret keys at all: the walk cannot create that view, and it says so rather than
// reporting a window that differs as if it were the same. That outcome is a
// withheld allow — the caller turns it into a defer — not a claimed restore.
//
// The column is corrected rather than only judged: Ctrl+Home parks the caret at
// the start of the draft and the vertical walks leave it at column 0, so the
// horizontal position the user had is re-walked with the non-destructive arrows.
// A correction that cannot reach the captured column, or a window that cannot be
// brought back to the captured scroll state, leaves the restore failed, so
// certify withholds the allow instead of handing the user a moved caret or a
// different view.
func restoreCaretAnchor(anchor caretAnchor, send func([]byte), tracker *composerTracker) bool {
	if !anchor.valid || send == nil || tracker == nil {
		return false
	}
	// Choose the end to start from: the anchor had rows elided above it, so the
	// user was reading the lower part of the draft and must be brought back from
	// the tail; with rows elided below (or neither) the head is the shorter way
	// in and reaches the top of the draft directly.
	if anchor.top > 0 {
		send([]byte("\x1b[1;5F")) // Ctrl+End, then Up to the anchored row
	} else {
		send([]byte("\x1b[1;5H")) // Ctrl+Home, then Down to the anchored row
	}
	if anchor.top > 0 {
		// Upwards from the last visible row; the row count reached is checked
		// below, and an overshoot is corrected there.
		back := anchor.total - anchor.draftRow
		for i := 0; i < back; i++ {
			send([]byte("\x1b[A"))
		}
	} else {
		for i := 1; i < anchor.draftRow; i++ {
			send([]byte("\x1b[B"))
		}
	}
	// Re-walk horizontally: Right is CSI C, Left is CSI D. Both move the caret
	// without changing the draft, so a correction can never alter the text.
	if _, col, _ := tracker.CursorPos(); col != anchor.col {
		switch {
		case col < anchor.col:
			for i := col; i < anchor.col; i++ {
				send([]byte("\x1b[C"))
			}
		default:
			for i := anchor.col; i < col; i++ {
				send([]byte("\x1b[D"))
			}
		}
	}
	deadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		if win, ok := tracker.DraftWindow(); ok {
			row, col, _ := tracker.CursorPos()
			if idx, found := win.DraftIndexAtScreenRow(row); found && idx == anchor.draftRow {
				if col != anchor.col {
					logDebug("[WALK] caret restore landed on row %d but column %d (want %d)", anchor.draftRow, col, anchor.col)
					return false
				}
				// The viewport must match exactly, not merely agree on whether it
				// is scrolled: the box shows a window of a tall draft, and two
				// different windows can both be scrolled while showing the user a
				// different part of the draft. The captured bounds are the view
				// the user was reading, so an identical top and bot is the only
				// observation that proves the same window came back. Nothing
				// weaker is accepted: a restore that lands the caret correctly on
				// a different window has moved what the user was looking at.
				if win.top != anchor.top || win.bot != anchor.bot {
					logDebug("[WALK] caret restore landed on row %d but the window is %d/%d (want %d/%d)",
						anchor.draftRow, win.top, win.bot, anchor.top, anchor.bot)
					return false
				}
				logDebug("[WALK] caret restored to draftRow=%d col=%d; window top=%d bot=%d (captured top=%d bot=%d)",
					idx, col, win.top, win.bot, anchor.top, anchor.bot)
				return true
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	logDebug("[WALK] caret restore did not land on draft row %d", anchor.draftRow)
	return false
}

// trackPasteEnter records one Enter event that arrived inside a bracketed paste.
// Windows terminals send CRLF as two separate Enter events (CR then LF); without
// coalescing each half would append a newline and turn every line break inside a
// pasted block into a blank line. A CR is therefore appended immediately and
// remembered, and the LF that pairs with it is skipped. Because pendingCR is owned
// by the caller, coalescing also works when the CR and LF land in different stdin
// reads. Standalone CR, standalone LF and consecutive blank lines are preserved.
func trackPasteEnter(draft []rune, raw []byte, pendingCR *bool) []rune {
	switch string(raw) {
	case "\r":
		*pendingCR = true
		return append(draft, '\n')
	case "\n":
		if *pendingCR {
			*pendingCR = false
			return draft
		}
		return append(draft, '\n')
	}
	*pendingCR = false
	return append(draft, '\n')
}

const minNativeAgyBytes int64 = 50 * 1024 * 1024

func isNativeBinaryCandidate(path, selfClean string, requireNativeSize bool) (string, bool) {
	clean := filepath.Clean(path)
	if selfClean != "" && strings.EqualFold(clean, selfClean) {
		return "", false
	}
	fi, err := os.Stat(clean)
	if err != nil || fi.IsDir() {
		return "", false
	}
	if requireNativeSize && fi.Size() < minNativeAgyBytes {
		return "", false
	}
	return clean, true
}

// findAgyBinary resolves the real Antigravity binary (agy-real.exe).
// It strictly prevents recursive fork bombs by ensuring it never returns a
// wrapper binary (even across agy-wrap.exe -> agy.exe) and tolerates the brief
// rename window during background self-updates.
func findAgyBinary() (string, error) {
	// 1. Explicit override via environment variable
	if custom := os.Getenv("AGY_REAL_PATH"); custom != "" {
		if p, ok := isNativeBinaryCandidate(custom, "", false); ok {
			return p, nil
		}
	}

	selfExe, err := os.Executable()
	var selfDir, selfClean string
	if err == nil {
		selfClean = filepath.Clean(selfExe)
		selfDir = filepath.Dir(selfClean)
	}

	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		if home := os.Getenv("USERPROFILE"); home != "" {
			localAppData = filepath.Join(home, "AppData", "Local")
		}
	}

	// Self-heal if web installer (install.ps1) overwrote agy.exe with a native
	// >50MB binary while we were launched via agy-wrap.exe.
	if selfClean != "" && !strings.EqualFold(filepath.Base(selfClean), "agy.exe") {
		siblingAgy := filepath.Join(selfDir, "agy.exe")
		if p, ok := isNativeBinaryCandidate(siblingAgy, selfClean, true); ok {
			siblingReal := filepath.Join(selfDir, "agy-real.exe")
			if err := os.Rename(p, siblingReal); err == nil {
				logDebug("[SELF-HEAL] Promoted native %s to %s", p, siblingReal)
				return siblingReal, nil
			}
			return p, nil
		}
	}

	// Retry briefly to bridge the Swap Phase 1 -> Phase 2 window of --bg-updater.
	for attempt := 0; attempt < 6; attempt++ {
		if selfDir != "" {
			if p, ok := isNativeBinaryCandidate(filepath.Join(selfDir, "agy-real.exe"), selfClean, false); ok {
				return p, nil
			}
			if p, ok := isNativeBinaryCandidate(filepath.Join(selfDir, "agy-core.exe"), selfClean, false); ok {
				return p, nil
			}
		}
		if localAppData != "" {
			if p, ok := isNativeBinaryCandidate(filepath.Join(localAppData, "agy", "bin", "agy-real.exe"), selfClean, false); ok {
				return p, nil
			}
		}
		if attempt < 5 {
			time.Sleep(150 * time.Millisecond)
		}
	}

	// 4. Look in PATH for agy-real.exe
	if p, err := exec.LookPath("agy-real.exe"); err == nil {
		if cand, ok := isNativeBinaryCandidate(p, selfClean, false); ok {
			return cand, nil
		}
	}
	if p, err := exec.LookPath("agy-real"); err == nil {
		if cand, ok := isNativeBinaryCandidate(p, selfClean, false); ok {
			return cand, nil
		}
	}

	// 5. Fallback to Antigravity IDE's managed native CLI (~/.gemini/bin/agy.exe)
	if home := os.Getenv("USERPROFILE"); home != "" {
		if cand, ok := isNativeBinaryCandidate(filepath.Join(home, ".gemini", "bin", "agy.exe"), selfClean, true); ok {
			return cand, nil
		}
	}

	// 6. Fallback: native agy.exe (>50MB only, never another wrapper)
	if localAppData != "" {
		if cand, ok := isNativeBinaryCandidate(filepath.Join(localAppData, "agy", "bin", "agy.exe"), selfClean, true); ok {
			return cand, nil
		}
	}
	if p, err := exec.LookPath("agy.exe"); err == nil {
		if cand, ok := isNativeBinaryCandidate(p, selfClean, true); ok {
			return cand, nil
		}
	}

	return "", fmt.Errorf("could not find real Antigravity binary ('agy-real.exe'). Ensure original agy.exe was renamed to agy-real.exe in %s", selfDir)
}

type ActionType int

const (
	ActionUnknown ActionType = iota
	ActionPrintable
	ActionTab
	ActionCtrlE
	ActionEnter
	ActionCtrlC
	ActionCtrlU
	ActionCtrlW
	ActionBackspace
	ActionKeyUpIgnore
	ActionBracketedPasteStart
	ActionBracketedPasteEnd
	// ActionPointerHover is pointer motion with no button: it cannot move the
	// caret/selection, so it may skip the draft-epoch bump.
	ActionPointerHover
	// ActionPointerInput is click/drag/wheel: it can move the caret or scroll the
	// composer, so it retains ownership invalidation. Neither ever appends text.
	ActionPointerInput
	// ActionNonEditKeyUp is a Win32 key-up release other than Ctrl+E: it is
	// forwarded to the child so modifier state stays synced, without bumping
	// draftEpoch.
	ActionNonEditKeyUp
)

type ParsedEvent struct {
	Action   ActionType
	Char     rune
	RawBytes []byte
}

// appendInputRune merges Win32 UTF-16 surrogate pairs into one astral rune.
// A lone high surrogate is held until the next printable input; a non-low
// rune flushes it as U+FFFD instead of silently dropping user bytes.
func appendInputRune(dst []rune, r rune, pendingHigh *rune) []rune {
	if r >= 0xD800 && r <= 0xDBFF {
		*pendingHigh = r
		return dst
	}
	if r >= 0xDC00 && r <= 0xDFFF {
		if *pendingHigh != 0 {
			dst = append(dst, utf16.DecodeRune(*pendingHigh, r))
			*pendingHigh = 0
			return dst
		}
		return dst
	}
	if *pendingHigh != 0 {
		dst = append(dst, utf8.RuneError)
		*pendingHigh = 0
	}
	return append(dst, r)
}

// composerGateResult is the pre-editor safety decision for a draft that may
// contain a native paste chip. Any paste makes the draft unverified; the gate
// refuses a rendered chip/collapse, allows only a positively confirmed
// ordinary-text composer, and otherwise defers without opening the editor.
type composerGateResult int

const (
	composerGateReady composerGateResult = iota
	composerGateChip
	composerGatePending
)

// pasteGate correlates one bracketed paste with the composer frames that follow
// it. The decision is never taken from elapsed time: a paste whose rendered
// representation is still unknown stays pending until the composer itself shows
// either an opaque paste representation or the pasted text as ordinary draft
// content.
//
// Two frame counters are kept deliberately separate:
//   - gen: the generation baseline captured when the paste was armed. It is the
//     authority for "this observation belongs to this paste generation".
//   - redraw: bumped before the settlement repaint. It only proves a fresh frame
//     was produced; it must never erase the generation authority.
type pasteGate struct {
	pending bool
	gen     uint64 // output frame counter at paste arm time (generation authority)
	redraw  uint64 // output frame counter before the settlement repaint
	payload string // normalized pasted text, bounded; never logged
	// spanPayload is the ORIGINAL pasted body captured at arm time. It is the
	// chip's identity for the chip-preserving span plan and must survive later
	// edits: rearmAfterEdit rebases payload to the whole current draft for gate
	// classification, and using that value for planning made buildSpanPlan see
	// the entire draft as the chip body (0 outside regions), so a chip followed
	// by a typed instruction could never be enhanced.
	spanPayload string
	// spanGen is the arm generation that bound spanPayload to the paste that
	// created the chip. Later arms are only allowed to rebind when no chip is
	// rendered or observed for this binding any more.
	spanGen uint64
	// chipSeen is sticky for the current generation. It is set the first time a
	// chip marker is observed while this generation is armed, so a later repaint
	// that clears the glyph cannot launder a chip that was really there.
	chipSeen bool
}

const pastePayloadLimit = 16384

// invalidateGeneration drops the paste generation and rebases it onto the live
// composer generation. The caller passes the current output-frame counter so a
// released gate can never re-match an older chip observation: any observation at
// or below this generation belongs to a draft the user has already left behind.
func (g *pasteGate) invalidateGeneration(frame uint64) {
	g.pending = false
	g.gen = frame
	g.redraw = frame
	g.payload = ""
	g.spanPayload = ""
	g.spanGen = 0
	g.chipSeen = false
}

// observeChip records a chip marker observed while this generation is armed.
// The flag is sticky for the generation: a marker that is painted and then
// repainted away (flicker, scroll, or a torn frame) must still refuse.
func (g *pasteGate) observeChip(present bool) {
	if present && g.pending {
		g.chipSeen = true
	}
}

// stripPromptCell removes the single prompt glyph the CLI draws at the start of
// the first draft row. It is applied to row 0 only, so a '>' the user typed at
// the start of their own text is preserved (row 0 would render as "> > text").
func stripPromptCell(row string) string {
	if strings.HasPrefix(row, ">") {
		row = row[1:]
		if strings.HasPrefix(row, " ") {
			row = row[1:]
		}
	}
	return row
}

// rendererIndent is the fixed indentation the CLI draws on every composer row
// after the first. Observed as exactly two spaces in every archived native
// capture (wrapped rows and hard-newline rows alike).
const rendererIndent = "  "

// tabRenderWidth is how many spaces the composer substitutes for one tab. Unlike
// a terminal tab stop it does not depend on the column: measured on native
// captures with tabs at columns 0, 1, 2, 3, 4 and 8, every tab rendered as
// exactly four spaces.
const tabRenderWidth = 4

// stripRendererIndent removes at most the renderer's own row indentation. It
// removes only what the CLI drew, never more, so leading spaces the user typed
// inside a payload line survive the comparison.
func stripRendererIndent(row string) string {
	if strings.HasPrefix(row, rendererIndent) {
		return row[len(rendererIndent):]
	}
	return row
}

// draftBodyFromRows renders the extracted composer rows into one comparison
// string, one row per line, with only the composer's own cells removed: the
// prompt glyph on row 0 and the fixed indent on the rest.
func draftBodyFromRows(rows []string) string {
	if len(rows) == 0 {
		return ""
	}
	out := make([]string, 0, len(rows))
	for i, row := range rows {
		row = strings.TrimRight(row, " ")
		if i == 0 {
			row = stripPromptCell(row)
		} else {
			row = stripRendererIndent(row)
		}
		out = append(out, row)
	}
	return strings.Join(out, "\n")
}

// draftRowsMatchPayload reports whether the rendered composer body reproduces
// the pasted payload character for character.
//
// Justification comes from the archived native captures rather than from
// assumptions. Observed geometry for a wrapped paste at 120 columns:
//
//	row[0]="> analisis struktur … alur"
//	row[1]="  data pada … alur data"
//
// Two structural facts follow, and only these two are compensated for:
//
//  1. Row 0 carries the CLI's own prompt cell ("> ") and later rows a two-space
//     indent. Those cells belong to the composer, not to the draft.
//  2. Every row break is either a newline present in the payload or a soft wrap,
//     and a soft wrap consumes exactly one space at the break. The payload reads
//     "… alur data …" while the rows read "… alur" + "  data …".
//
// Nothing else is ever dropped. An interior space the user typed, a '>', a
// box-drawing rune, a missing fragment, a chip marker that swallowed the draft —
// each is a mismatch and therefore defers. That closes the earlier defect where
// both sides lost spaces, '>' and border runes, so "a b", "a>b" and "a─b" all
// collapsed to one key and a mismatched body could certify a payload.
//
// Stated limits, not hidden ones: a wrapped line whose continuation begins with
// the user's own spaces is ambiguous with the renderer indent and defers when
// the indent is not exactly the renderer's, and any draft the composer cannot
// render in full defers. Both directions are safe — the editor is never opened
// on unproven content — and both are properties of the native composer contract,
// not claims of proof.
func draftRowsMatchPayload(rows []string, payload string) bool {
	if len(rows) == 0 || payload == "" {
		return false
	}
	body := draftBodyFromRows(rows)
	if body == "" {
		return false
	}
	payload = strings.ReplaceAll(payload, "\r\n", "\n")

	i, j := 0, 0
	for i < len(body) {
		ch := body[i]
		if ch == '\n' {
			// A row break renders as no character, so whatever the payload
			// holds here has to be something the composer printed as nothing:
			// a run of blanks its row trim removed (a whitespace-only pasted
			// line, or a wrap at a space), and then optionally the pasted
			// newline itself. A wrap mid-word consumes neither, which is the
			// existing soft-wrap case. Every character is still accounted for
			// in order and the payload must still be consumed whole, so this
			// cannot let a chip through: a chip row holds marker text, not
			// blanks.
			for j < len(payload) && (payload[j] == ' ' || payload[j] == '\t') {
				j++
			}
			if j < len(payload) && payload[j] == '\n' {
				j++
			}
			i++
			continue
		}
		if j >= len(payload) {
			return false
		}
		// A tab is expanded by the composer to exactly four spaces. Measured
		// on native captures at columns 0, 1, 2, 3, 4 and 8: the expansion is
		// always four spaces, not a tab stop, and it is the only transform the
		// CLI applies to user text.
		if payload[j] == '\t' {
			for k := 0; k < tabRenderWidth; k++ {
				if i+k >= len(body) || body[i+k] != ' ' {
					return false
				}
			}
			i += tabRenderWidth
			j++
			continue
		}
		if payload[j] != ch {
			return false
		}
		i++
		j++
	}
	// The composer trims trailing whitespace per row, so a payload's trailing
	// blanks produce no rendered characters. Nothing else may remain unmatched:
	// a body that stops short of the payload must defer.
	return strings.TrimRight(payload[j:], " \t\r\n") == ""
}

func (g *pasteGate) arm(frame uint64, pasted string) {
	wasPending := g.pending
	g.pending = true
	g.gen = frame
	g.redraw = frame
	g.chipSeen = false
	g.payload = pasted
	if len(g.payload) > pastePayloadLimit {
		g.payload = g.payload[len(g.payload)-pastePayloadLimit:]
	}
	// A later paste (e.g. the instruction pasted after the chip) must NOT
	// redefine the chip identity while that chip is still live. Bind only when
	// no chip is currently rendered or was observed for the existing binding;
	// otherwise the plan would treat the newer text as the chip body and could
	// rewrite the chip region.
	// Bind only when this arm starts a new gate sequence. While a gate is already
	// pending, the FIRST payload is the one that created the chip: a later paste
	// can arrive before the chip frame is observed (live 16:55:22, 2.7s after the
	// 18-line paste), so rendered/observed state is not a safe key here.
	if g.spanPayload == "" || (!composerChip.Load() && composerBandKnown.Load() && !wasPending) {
		g.spanPayload = g.payload
		g.spanGen = frame
		logDebug("[STDIN] paste arm gen=%d binds chip identity payloadRunes=%d", g.gen, len([]rune(g.spanPayload)))
	} else {
		logDebug("[STDIN] paste arm gen=%d keeps chip identity from gen=%d payloadRunes=%d",
			g.gen, g.spanGen, len([]rune(g.spanPayload)))
	}
}

// chipBody returns the original pasted body that anchors the chip-preserving
// span plan. It deliberately does not follow rearmAfterEdit: payload may be
// rebased to the current draft for gate classification, but the chip identity
// does not change under plain text edits outside the label.
func (g *pasteGate) chipBody() string {
	if g.spanPayload != "" {
		return g.spanPayload
	}
	return g.payload
}

// invalidateForPasteStart rebases the gate for a new bracketed paste but keeps
// the chip identity when a chip is already rendered. Starting a second paste
// (the instruction) must not erase the identity of the chip that paste is
// editing; a genuine rebind requires a chip-free draft.
func (g *pasteGate) invalidateForPasteStart(frame uint64) {
	keptPayload, keptGen := g.spanPayload, g.spanGen
	hadChip := composerChip.Load()
	g.invalidateGeneration(frame)
	if hadChip && keptPayload != "" {
		g.spanPayload, g.spanGen = keptPayload, keptGen
		logDebug("[STDIN] paste start preserved chip identity from gen=%d payloadRunes=%d", keptGen, len([]rune(keptPayload)))
	}
}

// invalidate releases the gate when no live frame counter is at hand; it
// advances the generation so older observations stay behind it.
func (g *pasteGate) invalidate() {
	g.invalidateGeneration(g.gen + 1)
}

// rearmAfterEdit starts a new paste generation after the user changed the
// draft. It is what makes "delete the chip, then type a normal draft" work: the
// old generation's chip latch can no longer poison the decision, while the new
// draft still has to be positively rendered as plain text before the editor may
// open. An unconfirmed new draft defers, which is safe.
func (g *pasteGate) rearmAfterEdit(frame uint64, draft string) {
	if !g.pending {
		return
	}
	normalized := draft
	if normalized == "" {
		// Nothing left to verify: an emptied draft has no chip to preserve.
		g.invalidateGeneration(frame)
		return
	}
	g.gen = frame
	g.redraw = frame
	g.payload = normalized
	if len(g.payload) > pastePayloadLimit {
		g.payload = g.payload[len(g.payload)-pastePayloadLimit:]
	}
	// The user changed the draft, so a marker seen for the previous content must
	// not keep refusing; the new content is verified on its own merits.
	g.chipSeen = false
}

// textMatchesDraft is the positive-evidence test: the rendered composer text
// must reproduce the pasted payload in full.
//
// The rule is deliberately all-or-nothing rather than a coverage percentage or
// fragment overlap. Justification from the archived native replay: in the real
// chip path the composer body never contains the payload at all (only the prompt
// glyph and the Auto-mode hint, then the marker), while in the ordinary path the
// body renders the payload verbatim (bodyRunes == payloadRunes for the short,
// long-line and multiline fixtures). Any threshold below "full" would let a
// composer that shows a chip alongside some payload text satisfy the gate, which
// is exactly the failure being closed.
//
// Consequence, stated rather than hidden: a draft too long for the composer to
// render in full defers instead of being allowed. That is the conservative
// direction — the gate never opens the editor on unproven content — and it is a
// limitation of the native composer contract, not a claim of proof.

// The evidence is passed as the tracker's extracted rows rather than as a
// pre-rendered string: draftRowsMatchPayload needs the row boundaries to account
// for the composer's own prompt cell and its soft wraps, and taking them here
// keeps a single extraction path from frame to decision.
func (g pasteGate) textMatchesDraft(rows []string) bool {
	if g.payload == "" {
		return false
	}
	return draftRowsMatchPayload(rows, g.payload)
}

// windowedBody reports whether the extracted composer rows carry the CLI's own
// elision marker ("↑/↓ N more lines", or the Indonesian "lebih baris"). It means
// the box is showing only a window of the draft, which is the signal to walk the
// box rather than to refuse it: the CLI draws this marker for ordinary offscreen
// drafts. It is deliberately structural (the arrow plus a digit count plus the
// phrase), so a prompt that merely mentions "more lines" is not mistaken for a
// window.
func windowedBody(rows []string) bool {
	body := strings.Join(rows, "\n")
	for _, arrow := range []string{"↑", "↓"} {
		if _, ok := moreLinesCount(body, []rune(arrow)[0]); ok {
			return true
		}
		if idx := strings.Index(body, arrow+" "); idx >= 0 {
			rest := body[idx+len(arrow)+1:]
			if strings.HasPrefix(rest, "more line") || strings.HasPrefix(rest, "lebih baris") {
				return true
			}
		}
	}
	return false
}

const (
	composerQuietWindow = 140 * time.Millisecond
	composerGateBudget  = 2000 * time.Millisecond
)

// classify is the pure state transition shared by the wait loop and unit tests.
//
// Ordering is itself an invariant: the gate's own sticky observation is checked
// only while the generation is armed, so a released gate can never be poisoned
// by an older chip. The live marker is always authoritative.
//
//	chip: a chip marker is rendered in the composer band right now.
//	rows: rendered composer draft rows as reported by the tracker.
//	frame: composer frames observed so far.
func (g pasteGate) classify(chip bool, rows []string, frame uint64, lastOutput, now time.Time) composerGateResult {
	// A marker rendered right now refuses regardless of generation: it is direct
	// evidence that the composer currently holds an opaque paste.
	if chip {
		return composerGateChip
	}
	// A marker observed AFTER this gate's baseline belongs to the draft that is
	// in the composer now, so it refuses even when nothing is being verified.
	//
	// The marker is not a stable state: the recorded runs paint it and repaint
	// it away 17-23 ms later (see the archived logs), so a decision that reads
	// only the live flag is a decision taken against a flicker. The sticky
	// latch is what survives the flicker, and it must not be scoped to the
	// armed case: releasing a generation on a ready decision, or a paste that
	// never armed one, would otherwise let the very next Ctrl+E allow on a
	// draft the composer really collapsed into a chip. The latch is compared
	// against the baseline rather than trusted outright, so a marker observed
	// before the current draft is still ignored (a user edit advances the
	// baseline, which is what keeps "delete the chip, then type a fresh draft"
	// enhancable).
	if chipObservedFrame.Load() > g.gen {
		return composerGateChip
	}
	if !g.pending {
		// Nothing is being verified and no marker postdates this draft: the
		// draft can be the ordinary kind.
		return composerGateReady
	}
	// Sticky observation for THIS generation only.
	if g.chipSeen {
		return composerGateChip
	}
	// A windowed box is NOT a refusal. The CLI draws its "↑/↓ N more lines"
	// marker for an ordinary offscreen draft, so a marker here would refuse
	// ordinary drafts. The window shows only part of the draft, so the full
	// comparison below cannot consume the payload and the gate defers; the
	// settle loop then walks the box's window to prove the draft in full.
	// The composer must have repainted after the paste before anything it shows
	// can be trusted as this draft's representation.
	if frame <= g.redraw || lastOutput.IsZero() {
		return composerGatePending
	}
	// A torn frame can momentarily show a slice of the payload; require the frame
	// to be quiet before treating its content as settled.
	if now.Sub(lastOutput) < composerQuietWindow {
		return composerGatePending
	}
	// Positive evidence only: the rendered body must reproduce the pasted draft
	// in full. A partial window, a shared fragment, or a percentage heuristic
	// cannot certify, because a chip can be present while most text is visible.
	if g.textMatchesDraft(rows) {
		return composerGateReady
	}
	return composerGatePending
}

// composerNav is how the settle loop walks the composer's window. ctrlHome and
// ctrlEnd are the CLI's own window-navigation keys; both are optional so the
// gate still compiles and runs with pure observation.
//
// restore is how the view and the caret are put back when the walk is over. It
// is deliberately separate from end: Ctrl+End reveals the tail, which is a walk
// step, but it always parks the caret at the very end of the draft and so cannot
// restore a caret the user left in the middle. When restore is nil the caller
// has no anchor and the walk falls back to end, which is the old, conservative
// behavior rather than a claim of restoration.
type composerNav struct {
	home    func()
	end     func()
	restore func() bool
	quiet   func()
	// inputArrived reports whether a keystroke reached the console while the
	// walk was driving the composer. The walk runs on the input loop's own
	// goroutine, so nothing reads stdin while it navigates: a record appearing in
	// the console input buffer during that window is the user's own typing, and a
	// draft proven while the user was editing it is a draft the composer no
	// longer holds. certify withholds rather than allow.
	inputArrived func() bool
}

// consoleKey records denote input the user actually produced. The console also
// queues records that are not keystrokes at all — window and focus events the
// host emits on its own — and GetNumberOfConsoleInputEvents counts those too.
// A count taken that way moves whenever the terminal is resized or repainted,
// which would report "the user typed" for an idle window and withhold a verdict
// the wrapper had already proven: on a real ConPTY that produced a deferred
// offscreen rewrite with zero provider calls while the harness typed nothing.
// Only key records are counted here, so the signal means what the walk needs it
// to mean.
const (
	consoleKeyEvent = 0x0001
	// sizeof(INPUT_RECORD) on every supported Windows version: WORD EventType,
	// WORD padding for alignment, then the union (the widest member is
	// KEY_EVENT_RECORD at 16 bytes). PeekConsoleInputW writes records at this
	// stride, so a different size would read the fields at the wrong offsets.
	inputRecordSize = 20
)

// inputRecord is the fixed prefix of INPUT_RECORD this probe reads: only
// EventType is inspected, and the union is skipped rather than interpreted.
type inputRecord struct {
	EventType uint16
	_         uint16
	_         [inputRecordSize - 4]byte
}

var (
	kernel32              = windows.NewLazySystemDLL("kernel32.dll")
	procPeekConsoleInputW = kernel32.NewProc("PeekConsoleInputW")
)

// classifyConsoleRecords turns what a bounded peek returned into the pending
// count the walk uses, and it is separate from the syscall so its boundaries can
// be exercised without a console: the interesting cases are the total that just
// fits and the total that overflows the peek buffer.
//
// total is what GetNumberOfConsoleInputEvents reported, got is how many records
// the peek actually wrote, and the slice holds those records in console order.
//
// Truncation is the withheld case, never "no input": when the buffer is larger
// than the peek window and no key record was seen inside that window, the tail
// that was not examined may still hold one, so the count is reported as
// unavailable. Reporting zero there would let the walk certify over a draft the
// user was typing into.
func classifyConsoleRecords(total, got uint32, recs []inputRecord) (uint32, bool) {
	var keys uint32
	for i := uint32(0); i < got && i < uint32(len(recs)); i++ {
		if recs[i].EventType&consoleKeyEvent != 0 {
			keys++
		}
	}
	if total > maxPeek && keys == 0 {
		return 0, false
	}
	return keys, true
}

// maxPeek bounds one peek. The caller only needs to know whether any key record
// is waiting, and an unbounded allocation on a hostile console count is exactly
// what this probe must not do.
const maxPeek = 512

// consoleInputPending reports how many KEY records are waiting in the console
// input buffer. It fails closed: a handle that cannot be queried reports no
// count, so the caller keeps whatever it decided without this signal.
func consoleInputPending(h windows.Handle) (uint32, bool) {
	if h == 0 {
		return 0, false
	}
	var total uint32
	if err := windows.GetNumberOfConsoleInputEvents(h, &total); err != nil {
		return 0, false
	}
	if total == 0 {
		return 0, true
	}
	// Peek (never Read) so the wrapper's own reader still sees every record.
	n := total
	if n > maxPeek {
		n = maxPeek
	}
	recs := make([]inputRecord, n)
	var got uint32
	r1, _, _ := procPeekConsoleInputW.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&recs[0])),
		uintptr(n),
		uintptr(unsafe.Pointer(&got)),
	)
	if r1 == 0 {
		return 0, false
	}
	return classifyConsoleRecords(total, got, recs)
}

// restoreView puts the view and the caret back the way the walk found them and
// reports whether the caret really landed where it started.
//
// The verified restore is the only path that reports true. With no restore the
// walk still returns the box to the draft's tail — leaving the screen parked on
// the head would be a visible side effect of a decision the user never asked for
// — but it moves the caret to the very end to do it and therefore reports false,
// which is why certify refuses that outcome whenever a restore was available.
func (n composerNav) restoreView() bool {
	if n.restore != nil {
		if !n.restore() {
			return false
		}
		if n.quiet != nil {
			n.quiet()
		}
		return true
	}
	if n.end != nil {
		n.end()
	}
	if n.quiet != nil {
		n.quiet()
	}
	return false
}

// multiWindowBudget bounds how many navigation steps a settle may take. The
// window logic needs at most 1 (head reveals the exact total) but a second pass
// is allowed for an unusual renderer; past that the draft defers.
const multiWindowAttempts = 3

// resolveDraftWindow walks the composer's window and reports whether the whole
// draft is now provable from one or more settled windows.
//
// Order of observation matters and mirrors how a user would inspect the draft:
//
//  1. the window as it stands may already hold the whole draft (small drafts
//     need no navigation at all);
//  2. otherwise Ctrl+Home reveals the head, whose "↓ N more lines" count states
//     the draft's exact rendered height, and whose rows 1..k are the true start
//     of the body;
//  3. if the head still does not cover the whole body, Ctrl+End reveals the tail
//     and the two windows are merged by absolute row index;
//  4. finally the view is restored with Ctrl+End so the user's own view is
//     exactly what it was before the gate looked.
//
// Every window is taken only from a settled frame (the caller re-reads the
// tracker after the navigation has been rendered), and rows are merged only
// when their absolute indices agree, so a torn or contradictory frame can never
// fabricate coverage. Incomplete coverage is reported as "not yet", which the
// caller turns into defer — never into a refusal, because a large but ordinary
// draft is not a chip.
func resolveDraftWindow(g *pasteGate, window func() (composerWindow, bool), nav composerNav) ([]draftWindowRow, bool) {
	initial, ok := window()
	if !ok {
		logDebug("[WALK] no composer window on screen")
		return nil, false
	}
	// The composer is about to be driven with the wrapper's own navigation
	// keys. If the terminal is already holding input for the wrapper, the user
	// is typing right now, so whatever the walk measures is a draft in motion
	// and its verdict is withheld (never a refusal: the caller defers).

	logDebug("[WALK] initial top=%d bot=%d rows=%d total=%d", initial.top, initial.bot, len(initial.rows), initial.total())
	merged, complete := mergeComposerWindows([]composerWindow{initial})
	if complete && nav.certify(g, merged, false, "initial window") {
		return merged, true
	}
	logDebug("[WALK] initial not complete (complete=%v)", complete)

	// Head: exact total and the first rows.
	if nav.home == nil {
		nav.restoreView()
		return nil, false
	}
	nav.home()
	if nav.quiet != nil {
		nav.quiet()
	}
	head, hok := window()
	if !hok {
		nav.restoreView()
		return nil, false
	}
	logDebug("[WALK] head top=%d bot=%d rows=%d total=%d", head.top, head.bot, len(head.rows), head.total())
	wins := []composerWindow{initial, head}
	if m, ok := mergeComposerWindows(wins); ok && nav.certify(g, m, true, "head merge") {
		return m, true
	}
	// Tail: merge head + tail by absolute row index.
	if nav.end == nil {
		nav.restoreView()
		return nil, false
	}
	nav.end()
	if nav.quiet != nil {
		nav.quiet()
	}
	tail, tok := window()
	if !tok {
		nav.restoreView()
		return nil, false
	}
	logDebug("[WALK] tail top=%d bot=%d rows=%d total=%d", tail.top, tail.bot, len(tail.rows), tail.total())
	wins = append(wins, tail)
	if m, ok := mergeComposerWindows(wins); ok && nav.certify(g, m, true, "tail merge") {
		return m, true
	}
	// The draft could not be proven: a defer must still leave the caret exactly
	// where the walk found it, and the caller is told whether it did.
	restored := nav.restoreView()
	logDebug("[WALK] not proven; caret restored=%v", restored)
	return nil, false
}

// certify is the single exit for a positive verdict. The merged window must
// cover the payload, and no keystroke may have arrived while the composer was
// being driven. When the walk moved the box, the caret must also be back where
// it was found; when the first window already held the whole draft nothing was
// moved, so there is nothing to restore and no caret to protect.
func (nav composerNav) certify(g *pasteGate, merged []draftWindowRow, moved bool, where string) bool {
	if !g.textMatchesWindow(merged) {
		return false
	}
	// The walk drives the composer with real navigation keys, so it must prove
	// what the user's draft is, not what it became while they were typing into
	// it. A keystroke that reached the console during the walk means the draft on
	// screen may already be stale, and the paste gate's own comparison cannot see
	// that: it compares the payload against the rows the walk collected, both of
	// which predate the keystroke.
	if nav.inputArrived != nil && nav.inputArrived() {
		logDebug("[WALK] input arrived while the composer was being driven; allow withheld (%s)", where)
		nav.restoreView()
		return false
	}
	if moved {
		restored := nav.restoreView()
		if nav.restore != nil && !restored {
			logDebug("[WALK] caret not restored after %s; allow withheld", where)
			return false
		}
	}
	logDebug("[WALK] certified from %s: rows=%d moved=%v", where, len(merged), moved)
	return true
}

// textMatchesWindow certifies a merged window body against the payload.
func (g pasteGate) textMatchesWindow(rows []draftWindowRow) bool {
	if g.payload == "" || len(rows) == 0 {
		return false
	}
	texts := make([]string, 0, len(rows))
	for _, r := range rows {
		texts = append(texts, r.text)
	}
	return draftRowsMatchPayload(texts, g.payload)
}

// settleEnhancementGate resolves a just-pasted draft before any editor write.
// One prompt repaint is requested (a resize, never Ctrl+G) so the CLI renders
// the paste's settled representation immediately instead of at its next idle
// tick; the decision is then taken from composer content, not from elapsed time.
// An unresolved draft is deferred, never opened speculatively.
func settleEnhancementGate(g *pasteGate, draftRows func() ([]string, bool), window func() (composerWindow, bool), redraw func(), nav composerNav) composerGateResult {
	if !composerBandKnown.Load() {
		return composerGatePending
	}
	// A marker visible right now is conclusive even before any repaint.
	if composerChip.Load() {
		return composerGateChip
	}
	// A marker observed after this draft's baseline is equally conclusive, and
	// it has to be read HERE as well as in classify: classify is only reached
	// once the settle loop sees either an armed generation or a resolved draft,
	// so an unarmed gate would otherwise return Ready below without the latch
	// ever being consulted. The marker is painted and repainted away within
	// 17-23 ms in the recorded runs, so "not visible right now" is not
	// evidence that no chip was rendered.
	if chipObservedFrame.Load() > g.gen {
		return composerGateChip
	}
	if !g.pending {
		return composerGateReady
	}
	// The generation baseline captured at arm time stays authoritative; the
	// redraw baseline only gates "a fresh frame arrived".
	g.redraw = outputFrames.Load()
	redrawCalls := 0
	if redraw != nil {
		redraw()
		redrawCalls++
	}
	// The walk runs at most once per settle, after the composer has settled. Once
	// it has been attempted, a failure is final for this settle: the walk is
	// expensive (it repaints the composer twice), so it is never retried inside
	// the wait loop.
	walkTried := false
	deadline := time.Now().Add(composerGateBudget)
	for {
		last := time.Unix(0, lastOutputNano.Load())
		rows, rowsOK := draftRows()
		// Fold the global marker observation into this generation's sticky flag.
		// The counter is compared against this gate's generation, so an
		// observation belonging to a draft the user already replaced cannot arm
		// this gate; a marker seen while armed stays seen for this generation.
		g.observeChip(chipObservedFrame.Load() > g.gen)
		if !composerBandKnown.Load() {
			return composerGatePending
		}
		if !rowsOK {
			rows = nil
		}
		switch g.classify(composerChip.Load(), rows, outputFrames.Load(), last, time.Now()) {
		case composerGateChip:
			logDebug("[STDIN] gate refused: gen=%d sticky=%v chipObserved=%d frames=%d",
				g.gen, g.chipSeen, chipObservedFrame.Load(), outputFrames.Load())
			return composerGateChip
		case composerGateReady:
			logDebug("[STDIN] gate ready: bodyRunes=%d payloadRunes=%d frames=%d",
				len([]rune(draftBodyFromRows(rows))), len(g.payload), outputFrames.Load())
			return composerGateReady
		}
		// The composer is showing only a window of the draft (it draws its own
		// "↑/↓ N more lines" marker). No single frame can certify it, so the
		// walk runs as soon as the frames are quiet rather than waiting out the
		// budget; the marker is the CLI's own statement that the draft is
		// offscreen, so this is not a speculative path.
		if !walkTried && window != nil && windowedBody(rows) &&
			!last.IsZero() && time.Since(last) >= composerQuietWindow {
			walkTried = true
			if merged, ok := resolveDraftWindow(g, window, nav); ok {
				logDebug("[STDIN] gate ready (multi-window): rows=%d payloadRunes=%d frames=%d",
					len(merged), len(g.payload), outputFrames.Load())
				return composerGateReady
			}
		}
		if time.Now().After(deadline) {
			// One bounded second repaint covers a renderer that coalesced the
			// first one; the latch is re-read each iteration, so a chip that
			// appears during this window is still attributed to this paste.
			if redrawCalls == 1 && redraw != nil {
				redraw()
				redrawCalls++
				deadline = time.Now().Add(composerGateBudget)
				continue
			}
			// Final chance: the composer may be showing a window even when the
			// extracted rows did not reveal a marker (for example the box was
			// still repainting when the rows were read). It may still be an
			// ordinary offscreen draft whose tail the box is showing, so walk the
			// composer's window (head/tail) and try to prove full coverage from
			// the merged rows. A chip would have been seen before this point: a
			// chip is rendered as a marker, and a marker either arms the sticky
			// latch or is caught by DraftWindow as its own first row. Incomplete
			// coverage stays a defer, never a refusal.
			if !walkTried && window != nil {
				walkTried = true
				if merged, ok := resolveDraftWindow(g, window, nav); ok {
					logDebug("[STDIN] gate ready (multi-window): rows=%d payloadRunes=%d frames=%d",
						len(merged), len(g.payload), outputFrames.Load())
					return composerGateReady
				}
			}
			logDebug("[STDIN] gate deferring: gen=%d redraw=%d bodyRunes=%d payloadRunes=%d frames=%d chipObserved=%d",
				g.gen, g.redraw, len([]rune(draftBodyFromRows(rows))), len(g.payload),
				outputFrames.Load(), chipObservedFrame.Load())
			return composerGatePending
		}
		time.Sleep(15 * time.Millisecond)
	}
}

// isNativeNewlineSequence reports editor-newline keys observed against the
// installed CLI: Shift+Enter (CSI-u), Alt+Enter and Ctrl+J all insert a line
// instead of submitting. Plain Enter is a bare CR and still submits.
func isNativeNewlineSequence(raw []byte) bool {
	switch string(raw) {
	case "\x1b[13;2u", "\x1b\r", "\n":
		return true
	}
	return false
}

func classifyEvent(raw []byte) (ActionType, rune) {
	n := len(raw)
	if n == 0 {
		return ActionUnknown, 0
	}
	// SGR mouse reports and focus reports cannot edit the draft. Classifying them
	// here keeps them out of the epoch counter and the shadow append path.
	if n >= 6 && raw[0] == 0x1b && raw[1] == '[' && raw[2] == '<' && (raw[n-1] == 'M' || raw[n-1] == 'm') {
		// SGR button byte: bit 5 = motion; low two bits 3 = no button held.
		if semi := bytes.IndexByte(raw[3:n-1], ';'); semi > 0 {
			if b, err := strconv.Atoi(string(raw[3 : 3+semi])); err == nil {
				if b&32 != 0 && b&3 == 3 && b&64 == 0 {
					return ActionPointerHover, 0
				}
			}
		}
		return ActionPointerInput, 0
	}
	if n == 3 && raw[0] == 0x1b && raw[1] == '[' && (raw[2] == 'I' || raw[2] == 'O') {
		return ActionPointerHover, 0
	}

	// 1. Bracketed Paste Boundary: \x1b[200~ (Start) and \x1b[201~ (End)
	if n >= 6 && raw[0] == 0x1b && raw[1] == '[' && raw[n-1] == '~' {
		s := string(raw)
		if s == "\x1b[200~" {
			return ActionBracketedPasteStart, 0
		}
		if s == "\x1b[201~" {
			return ActionBracketedPasteEnd, 0
		}
	}

	// 1. Raw ASCII / Single Byte
	if n == 1 {
		b := raw[0]
		switch b {
		case 0x05:
			return ActionCtrlE, 0
		case '\r', '\n':
			return ActionEnter, 0
		case 0x03:
			return ActionCtrlC, 0
		case 0x15:
			return ActionCtrlU, 0
		case 0x17:
			return ActionCtrlW, 0
		case 0x08, 0x7f:
			return ActionBackspace, 0
		case '\t':
			// A literal tab. Outside a paste it is passthrough; inside a paste it is
			// payload content, and the composer renders it as four spaces.
			return ActionTab, '\t'
		default:
			if b >= 32 {
				return ActionPrintable, rune(b)
			}
		}
		return ActionUnknown, 0
	}

	// 2. Win32 Input Mode: \x1b[<Vk>;<Scan>;<Char>;<KeyDown>;<State>;<Repeat>_
	if len(raw) > 3 && raw[0] == 0x1b && raw[1] == '[' && raw[n-1] == '_' {
		payload := string(raw[2 : n-1])
		parts := strings.Split(payload, ";")
		if len(parts) >= 6 {
			vk, _ := strconv.Atoi(parts[0])
			charVal, _ := strconv.Atoi(parts[2])
			keyDown := parts[3] == "1"
			state, _ := strconv.Atoi(parts[4])

			if !keyDown {
				if vk == 0x45 || charVal == 5 {
					return ActionKeyUpIgnore, 0
				}
				return ActionNonEditKeyUp, 0
			}

			// Bare modifier key-downs (Shift, Ctrl, Alt, Win) never edit the
			// draft or move the caret (e.g. Win+Shift+S screenshot or Alt+Tab
			// while waiting for the model). Forward without bumping draftEpoch.
			if charVal == 0 && (vk == 0x10 || vk == 0x11 || vk == 0x12 || vk == 0x5B || vk == 0x5C || (vk >= 0xA0 && vk <= 0xA5)) {
				return ActionNonEditKeyUp, 0
			}

			isCtrl := (state & 0x08) != 0 // RIGHT_CTRL_PRESSED
			if (state & 0x04) != 0 {      // LEFT_CTRL_PRESSED
				isCtrl = true
			}
			hasAlt := (state & 0x03) != 0 // RIGHT_ALT_PRESSED (0x01) or LEFT_ALT_PRESSED (0x02)
			if hasAlt {
				isCtrl = false // Prevent AltGr+E from triggering Ctrl+E on international layouts
			}

			if (vk == 0x45 || charVal == 5) && isCtrl {
				return ActionCtrlE, 0
			}
			if vk == 0x0D || charVal == 13 {
				return ActionEnter, 0
			}
			if (vk == 0x43 || charVal == 3) && isCtrl {
				return ActionCtrlC, 0
			}
			if (vk == 0x55 || charVal == 21) && isCtrl {
				return ActionCtrlU, 0
			}
			if (vk == 0x57 || charVal == 23) && isCtrl {
				return ActionCtrlW, 0
			}
			if vk == 0x08 || charVal == 8 {
				return ActionBackspace, 0
			}
			if vk == 0x09 || charVal == 9 {
				// A tab in Win32 Input Mode is vk=9 with charVal=9, which is
				// below the printable range. Inside a paste it is payload
				// content the composer renders as four spaces, so it has to be
				// classified rather than dropped.
				return ActionTab, '\t'
			}
			if charVal >= 32 && charVal < 0x110000 {
				return ActionPrintable, rune(charVal)
			}
		}
		return ActionUnknown, 0
	}

	// 3. Kitty Keyboard Protocol: \x1b[<codepoint>;<modifiers>u or \x1b[<codepoint>u
	if len(raw) > 2 && raw[0] == 0x1b && raw[1] == '[' && raw[n-1] == 'u' {
		payload := string(raw[2 : n-1])
		parts := strings.Split(payload, ";")
		keyPart := parts[0]
		if colonIdx := strings.Index(keyPart, ":"); colonIdx != -1 {
			keyPart = keyPart[:colonIdx]
		}
		code, err := strconv.Atoi(keyPart)
		if err == nil {
			mod := 1
			evType := 1 // 1: press, 2: repeat, 3: release
			if len(parts) > 1 {
				modSub := strings.Split(parts[1], ":")
				mod, _ = strconv.Atoi(modSub[0])
				if len(modSub) > 1 {
					evType, _ = strconv.Atoi(modSub[1])
				}
			}

			if evType == 3 {
				if (code == 101 || code == 69) && ((mod-1)&4 != 0) {
					return ActionKeyUpIgnore, 0
				}
				return ActionUnknown, 0
			}

			isCtrl := ((mod - 1) & 4) != 0
			hasAlt := ((mod - 1) & 2) != 0
			if hasAlt {
				isCtrl = false
			}

			if (code == 101 || code == 69) && isCtrl {
				return ActionCtrlE, 0
			}
			if code == 13 {
				return ActionEnter, 0
			}
			if (code == 99 || code == 67) && isCtrl {
				return ActionCtrlC, 0
			}
			if (code == 117 || code == 85) && isCtrl {
				return ActionCtrlU, 0
			}
			if (code == 119 || code == 87) && isCtrl {
				return ActionCtrlW, 0
			}
			if code == 127 || code == 8 {
				return ActionBackspace, 0
			}
			if !isCtrl && code >= 32 && code < 0x110000 {
				charRune := rune(code)
				if (mod-1)&1 != 0 && code >= 'a' && code <= 'z' {
					charRune = rune(code - 32)
				}
				return ActionPrintable, charRune
			}
		}
		return ActionUnknown, 0
	}

	// 4. xterm modifyOtherKeys: \x1b[27;<mod>;<codepoint>~
	if len(raw) > 6 && strings.HasPrefix(string(raw), "\x1b[27;") && raw[n-1] == '~' {
		payload := string(raw[5 : n-1])
		parts := strings.Split(payload, ";")
		if len(parts) >= 2 {
			mod, _ := strconv.Atoi(parts[0])
			code, _ := strconv.Atoi(parts[1])
			isCtrl := ((mod - 1) & 4) != 0
			hasAlt := ((mod - 1) & 2) != 0
			if hasAlt {
				isCtrl = false
			}

			if (code == 101 || code == 69) && isCtrl {
				return ActionCtrlE, 0
			}
			if code == 13 {
				return ActionEnter, 0
			}
			if (code == 99 || code == 67) && isCtrl {
				return ActionCtrlC, 0
			}
			if (code == 117 || code == 85) && isCtrl {
				return ActionCtrlU, 0
			}
			if (code == 119 || code == 87) && isCtrl {
				return ActionCtrlW, 0
			}
			if code == 127 || code == 8 {
				return ActionBackspace, 0
			}
			if !isCtrl && code >= 32 && code < 0x110000 {
				charRune := rune(code)
				if (mod-1)&1 != 0 && code >= 'a' && code <= 'z' {
					charRune = rune(code - 32)
				}
				return ActionPrintable, charRune
			}
		}
		return ActionUnknown, 0
	}

	// 5. UTF-8 multi-byte rune (non-escape)
	if raw[0] != 0x1b {
		r, _ := utf8.DecodeRune(raw)
		if r != utf8.RuneError && r >= 32 {
			return ActionPrintable, r
		}
	}

	return ActionUnknown, 0
}

// tryParseConPTYWrappedVT checks if the input stream at startIdx begins with a ConPTY
// Win32 Input Mode sequence wrapping an ANSI/VT escape sequence (e.g. Kitty \x1b[101;5u
// wrapped into multiple \x1b[0;0;<char>;1;0;1_ tokens by Windows ConPTY).
// If matched, it reassembles the underlying VT sequence and returns it along with the
// total consumed bytes in buf.
func tryParseConPTYWrappedVT(buf []byte, startIdx int) ([]byte, int, bool) {
	n := len(buf)
	if startIdx >= n || buf[startIdx] != 0x1b {
		return nil, 0, false
	}

	parseToken := func(pos int) (charVal int, keyDown bool, tokenLen int, ok bool) {
		if pos+3 >= n || buf[pos] != 0x1b || buf[pos+1] != '[' {
			return 0, false, 0, false
		}
		end := -1
		for j := pos + 2; j < n && j-pos < 48; j++ {
			if buf[j] == '_' {
				end = j
				break
			}
			if buf[j] == 0x1b {
				break
			}
		}
		if end == -1 {
			return 0, false, 0, false
		}

		payload := string(buf[pos+2 : end])
		parts := strings.Split(payload, ";")
		if len(parts) < 6 {
			return 0, false, 0, false
		}

		vk, err1 := strconv.Atoi(parts[0])
		scan, err2 := strconv.Atoi(parts[1])
		ch, err3 := strconv.Atoi(parts[2])
		if err1 != nil || err2 != nil || err3 != nil {
			return 0, false, 0, false
		}

		// ConPTY assigns vk == 0 and scan == 0 to synthesized character tokens
		// from VT escape sequences received on its input pipe.
		if vk != 0 || scan != 0 {
			return 0, false, 0, false
		}

		kd := parts[3] == "1"
		return ch, kd, (end - pos + 1), true
	}

	// First token MUST be ESC (char 27)
	firstChar, _, firstLen, ok := parseToken(startIdx)
	if !ok || firstChar != 27 {
		return nil, 0, false
	}

	var vtBytes []byte
	vtBytes = append(vtBytes, 0x1b)
	curr := startIdx + firstLen

	for curr < n {
		ch, _, tLen, match := parseToken(curr)
		if !match {
			break
		}
		vtBytes = append(vtBytes, byte(ch))
		curr += tLen

		// If this is a CSI sequence (\x1b[...), it terminates on a character in 0x40-0x7E (@ through ~)
		// e.g. 'u', '~', 'A', 'H', etc.
		if len(vtBytes) >= 3 && vtBytes[1] == '[' {
			lastByte := vtBytes[len(vtBytes)-1]
			if lastByte >= 0x40 && lastByte <= 0x7E {
				return vtBytes, curr - startIdx, true
			}
		}
	}

	if len(vtBytes) >= 3 && vtBytes[1] == '[' {
		lastByte := vtBytes[len(vtBytes)-1]
		if lastByte >= 0x40 && lastByte <= 0x7E {
			return vtBytes, curr - startIdx, true
		}
	}

	return nil, 0, false
}

func parseInputEvents(buf []byte) ([]ParsedEvent, int) {
	var events []ParsedEvent
	n := len(buf)
	i := 0

	for i < n {
		// 1. Check for ConPTY-wrapped VT sequence first (e.g. Ghostty/Kitty in enowX)
		if vtSeq, consumed, ok := tryParseConPTYWrappedVT(buf, i); ok {
			action, ch := classifyEvent(vtSeq)
			events = append(events, ParsedEvent{
				Action:   action,
				Char:     ch,
				RawBytes: buf[i : i+consumed],
			})
			i += consumed
			continue
		}

		if buf[i] == 0x1b {
			if i+1 >= n {
				break
			}

			if buf[i+1] == '[' {
				endIdx := -1
				for j := i + 2; j < n; j++ {
					if buf[j] == '_' || (buf[j] >= 'A' && buf[j] <= 'Z') || (buf[j] >= 'a' && buf[j] <= 'z') || buf[j] == '~' {
						endIdx = j
						break
					}
					if buf[j] == 0x1b || j-i > 48 {
						break
					}
				}

				if endIdx == -1 {
					if n-i < 32 {
						break
					}
					raw := buf[i : i+1]
					action, ch := classifyEvent(raw)
					events = append(events, ParsedEvent{
						Action:   action,
						Char:     ch,
						RawBytes: raw,
					})
					i++
					continue
				}

				raw := buf[i : endIdx+1]
				action, ch := classifyEvent(raw)
				events = append(events, ParsedEvent{
					Action:   action,
					Char:     ch,
					RawBytes: raw,
				})
				i = endIdx + 1
				continue
			}
		}

		r, size := utf8.DecodeRune(buf[i:])
		if r == utf8.RuneError && size <= 1 && n-i < 4 {
			break
		}

		raw := buf[i : i+size]
		action, ch := classifyEvent(raw)
		events = append(events, ParsedEvent{
			Action:   action,
			Char:     ch,
			RawBytes: raw,
		})
		i += size
	}

	return events, i
}

func printWrapHelp() {
	fmt.Printf(`Antigravity ConPTY Interceptor Wrapper (agy) v%s — Created by %s
Usage: agy [wrapper-options] [agy options...]

Wrapper Options:
  --wrap-concise       Use concise prompt enhancement mode (1-2 sentences)
  --wrap-architect     Use full Senior Software Architect prompt enhancement mode (default)
  --wrap-version       Show wrapper version and author info
  --wrap-help          Show this wrapper help message
  --wrap-editor-helper (internal) Serve one owned Ctrl+E editor request; delegate otherwise

Keystrokes:
  Ctrl + E (0x05)      Enhance prompt draft on the active input line in real-time
  Ctrl + U (0x15)      Erase line
  Ctrl + C (0x03)      Abort prompt / cancel turn
  Enter (\r, \n)       Submit prompt

All other flags are forwarded transparently to agy-real.exe.
`, WrapperVersion, WrapperAuthor)
}

type appliedDraft struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

const (
	enhanceKindNone   int32 = 0
	enhanceKindEditor int32 = 1
	enhanceKindSpan   int32 = 2
)

func storeEnhanceKind(k int32) { enhanceKind.Store(k) }

// applyPendingShadowOverride updates the shadow draft from a completed span commit
// without repasting. The composer already shows the committed text (typed as
// keystrokes), so only the shadow is reconciled here.
func applyPendingShadowOverride(draft []rune) []rune {
	if ov := shadowDraftOverride.Load(); ov != nil {
		draft = []rune(ov.Text)
		shadowDraftOverride.Store(nil)
		logDebug("[SPAN] shadow draft resynced from span commit (%d runes)", len(draft))
	}
	return draft
}

type byteWriter interface {
	Write([]byte) (int, error)
}

func applyPendingDraftMirror(draft []rune, w byteWriter) []rune {
	if applied := appliedDraftMirror.Load(); applied != nil {
		// Whole-value rewrite guard: the sequence below erases the composer with
		// Ctrl+U/Backspace and re-pastes the entire value. The native CLI clears
		// its paste-chip slice on every programmatic whole-value write, so this
		// path must never fire while a chip is present, or while the composer
		// band / chip state is unknown (fail closed). The mirror is kept pending
		// so the rewrite can still be applied later instead of being dropped.
		atoms := chipAtomCount()
		if !composerBandKnown.Load() || atoms != 0 {
			logDebug("[MIRROR] guard: deferring whole-value rewrite (bandKnown=%v chipAtoms=%d)",
				composerBandKnown.Load(), atoms)
			return draft
		}
		old := string(draft)
		draft = []rune(applied.Text)
		appliedDraftMirror.Store(nil)
		if w != nil && old != applied.Text {
			lineCount := strings.Count(old, "\n")
			for i := 0; i <= lineCount; i++ {
				_, _ = w.Write([]byte{0x15, 0x08})
			}
			if len(old) > 0 {
				_, _ = w.Write([]byte(strings.Repeat("\b", len(old))))
			}
			_, _ = w.Write([]byte("\x1b[200~" + applied.Text + "\x1b[201~"))
		}
		if p := strings.TrimSpace(os.Getenv("AGY_WRAP_EVIDENCE_APPLIED_PATH")); p != "" {
			if raw, err := json.Marshal(applied); err == nil {
				_ = os.WriteFile(p, raw, 0o600)
			}
		}
		// The replacement has landed. Anything the user typed while it was
		// being assembled would have been erased by it, so those keystrokes are
		// typed into the composer now, after it, which is the only ordering that
		// neither drops nor duplicates them. Each one's effect is applied to the
		// shadow draft at the same moment and in the same order, so the draft
		// keeps describing what the composer holds.
		// applyPendingDraftMirror runs on the input loop, which owns the draft,
		// so the delivery and the draft update are safe here and need no hand-off.
		if n, updated := flushCapturedOps(draft, w); n > 0 {
			draft = updated
		}
		logDebug("[STDIN] draft mirror resynced from editor applied marker (%d runes)", len(draft))
	}
	return draft
}

// keepToastVisible repaints one toast across child screen repaints until its
// visibility window ends. It only clears the pointer it owns, so a newer
// enhancement's toast is never removed by an older timer.
func keepToastVisible(tm *string, outHandle windows.Handle) {
	deadline := time.Now().Add(5800 * time.Millisecond)
	for time.Now().Before(deadline) {
		if toastMessage.Load() != tm {
			return
		}
		paintStatus()
		time.Sleep(100 * time.Millisecond)
	}
	if toastMessage.CompareAndSwap(tm, nil) {
		paintStatus()
	}
}

func watchEditorBridge(bridge *editorBridge, outHandle windows.Handle) {
	epoch := bridgeEpoch.Add(1)
	isBusyState.Store(true)
	toastMessage.Store(nil)
	syncStatusFile(true)
	paintStatus()

	go func() {
		stopRepaint := make(chan struct{})
		var repaintWG sync.WaitGroup
		repaintWG.Add(1)
		go func() {
			defer repaintWG.Done()
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stopRepaint:
					return
				case <-ticker.C:
					if toastMessage.Load() != nil {
						continue
					}
					paintStatus()
				}
			}
		}()

		defer func() {
			if r := recover(); r != nil {
				logDebug("[PANIC RECOVERED] in editor bridge watcher: %v", r)
			}
			if bridgeEpoch.Load() != epoch {
				return
			}
			isBusyState.Store(false)
			writeEvidence(strings.TrimSpace(os.Getenv("AGY_WRAP_EVIDENCE_IDLE_PATH")), "idle")
			syncStatusFile(false)
			if tm := toastMessage.Load(); tm != nil {
				go keepToastVisible(tm, outHandle)
				return
			}
			paintStatus()
		}()

		defer func() {
			close(stopRepaint)
			repaintWG.Wait()
		}()

		done, ok := bridge.wait(bridgeWaitTimeout())
		if bridgeEpoch.Load() != epoch {
			return
		}
		if ok && !bridge.ownsDone(done) {
			logDebug("[BRIDGE] stale result ignored: id=%s rev=%d", done.ID, done.Revision)
			endRewriteWithoutResync()
			return
		}
		if !ok {
			bridge.cancel()
			endRewriteWithoutResync()
			warn := "\x1b[33m✦ Enhancement timeout (editor tidak kembali)\x1b[0m" + strings.Repeat(" ", 12)
			toastMessage.Store(&warn)
			paintStatus()
			return
		}

		var msg string
		switch done.Status {
		case "ok":
			triggerHoldActive.Store(false)
			if raw, err := os.ReadFile(filepath.Join(bridge.dir, editorAppliedName)); err == nil {
				text := string(raw)
				appliedDraftMirror.Store(&appliedDraft{ID: done.ID, Text: text})
				if p := strings.TrimSpace(os.Getenv("AGY_WRAP_EVIDENCE_APPLIED_PATH")); p != "" {
					if ev, err := json.Marshal(appliedDraft{ID: done.ID, Text: text}); err == nil {
						_ = os.WriteFile(p, ev, 0o600)
					}
				}
				_ = os.Remove(filepath.Join(bridge.dir, editorAppliedName))
			}
			model := strings.TrimSpace(strings.Split(done.Model, "(")[0])
			if model == "" {
				model = "AI Model"
			}
			msg = fmt.Sprintf("\x1b[32m✦ Enhanced via editor with %s (%.1fs)!\x1b[0m", model, done.Duration)
		case "canceled":
			endRewriteWithoutResync()
			msg = "\x1b[33m✦ Enhancement dibatalkan\x1b[0m" + strings.Repeat(" ", 26)
		default:
			endRewriteWithoutResync()
			msg = "\x1b[31m✦ Enhancement gagal: " + done.Detail + "\x1b[0m"
		}
		toastMessage.Store(&msg)
		paintStatus()
	}()
}

func isNonInteractiveInvocation(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--wrap-concise", "--wrap-detailed":
			continue
		case "-p", "--print", "--prompt", "--version", "-v", "--help", "-h", "--bg-updater":
			return true
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		switch a {
		case "update", "install", "models", "mcp", "plugin", "plugins",
			"agent", "agents", "changelog", "help", "acp", "mic-serve", "remote-control":
			return true
		default:
			return false
		}
	}
	return false
}

func runDirectPassthrough(args []string) int {
	agyBin, err := findAgyBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] %v\n", err)
		return 1
	}
	var forwarded []string
	for _, a := range args {
		if a == "--wrap-concise" || a == "--wrap-detailed" {
			continue
		}
		forwarded = append(forwarded, a)
	}
	cmd := exec.Command(agyBin, forwarded...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		return 1
	}
	return 0
}

func main() {
	for _, arg := range os.Args[1:] {
		if arg == "--wrap-version" {
			fmt.Printf("agy-wrapper v%s — Created by %s\n", WrapperVersion, WrapperAuthor)
			return
		}
		if arg == "--wrap-help" {
			printWrapHelp()
			return
		}
		if arg == editorHelperFlag {
			os.Exit(runEditorHelper(os.Args[2:]))
		}
	}

	// Non-TUI subcommands/flags (update, models, --version, -p, etc.) bypass
	// ConPTY and must never touch %TEMP%\agy-statusline.txt.
	if isNonInteractiveInvocation(os.Args[1:]) {
		os.Exit(runDirectPassthrough(os.Args[1:]))
	}

	syncStatusFile(false)
	defer restoreNativeStatusLine()

	agyBin, err := findAgyBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] %v\n", err)
		restoreNativeStatusLine()
		os.Exit(1)
	}

	initSessionModel()
	childSupportsBracketedPaste.Store(true)

	editorBridgeHandle, bridgeErr := setupEditorBridge()
	if bridgeErr != nil {
		logDebug("[EDITOR-BRIDGE] setup failed: %v", bridgeErr)
		editorBridgeHandle = nil
	}
	defer editorBridgeHandle.cleanup()

	logDebug("=== agy-wrapper started === agyBin=%s, args=%v, activeModel=%s", agyBin, logSafeArgs(os.Args[1:]), getCurrentModel())

	var passthroughArgs []string
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--wrap-concise":
			_ = os.Setenv("ENHANCE_STYLE", "concise")
		case "--wrap-architect":
			_ = os.Setenv("ENHANCE_STYLE", "architect")
		case "--wrap-help":
			printWrapHelp()
			return
		default:
			passthroughArgs = append(passthroughArgs, arg)
		}
	}

	cmdParts := []string{syscall.EscapeArg(agyBin)}
	for _, arg := range passthroughArgs {
		cmdParts = append(cmdParts, syscall.EscapeArg(arg))
	}
	cmdLine := strings.Join(cmdParts, " ")

	inHandle := windows.Handle(os.Stdin.Fd())
	outHandle := windows.Handle(os.Stdout.Fd())

	var origInMode, origOutMode uint32
	hasInConsole := windows.GetConsoleMode(inHandle, &origInMode) == nil
	hasOutConsole := windows.GetConsoleMode(outHandle, &origOutMode) == nil

	defer func() {
		if hasInConsole {
			_ = windows.SetConsoleMode(inHandle, origInMode)
		}
		if hasOutConsole {
			_ = windows.SetConsoleMode(outHandle, origOutMode)
		}
	}()

	if hasInConsole {
		rawInMode := origInMode
		rawInMode &^= (windows.ENABLE_LINE_INPUT | windows.ENABLE_ECHO_INPUT | windows.ENABLE_PROCESSED_INPUT)
		rawInMode |= (windows.ENABLE_VIRTUAL_TERMINAL_INPUT | windows.ENABLE_WINDOW_INPUT)
		_ = windows.SetConsoleMode(inHandle, rawInMode)
	}
	if hasOutConsole {
		vtOutMode := origOutMode | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING
		_ = windows.SetConsoleMode(outHandle, vtOutMode)
	}

	w, h := getConsoleSize(outHandle)

	cpty, err := conpty.Start(cmdLine, conpty.ConPtyDimensions(w, h))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start ConPTY for %s: %v\n", agyBin, err)
		logDebug("Failed to start ConPTY for %s: %v", agyBin, err)
		restoreNativeStatusLine()
		os.Exit(1)
	}
	childWriter = cpty
	defer cpty.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	composer := newComposerTracker(w, h)
	outRenderer = newOutputRenderer(os.Stdout, composer)
	defer func() { outRenderer = nil }()

	// Window resize dynamic tracker
	go func() {
		lastW, lastH := w, h
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				curW, curH := getConsoleSize(outHandle)
				if geometryChanged(curW, curH, lastW, lastH) {
					lastW, lastH = curW, curH
					// Linearize the geometry invalidation with destructive emission:
					// a span key cannot pass the fence between bump and tracker rebuild.
					emitMu.Lock()
					geometryEpoch.Add(1)
					_ = cpty.Resize(curW, curH)
					composer.Resize(curW, curH)
					emitMu.Unlock()
				}
			}
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancel()
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine 1: PTY Output -> os.Stdout
	go func() {
		defer wg.Done()
		var leftover []byte
		readBuf := make([]byte, 8192)
		sawChip := false
		lastMedia := 0
		observe := func(data []byte) {
			spans, known := composer.chipMarkerSpans()
			prevChip := composerChip.Load()
			nowChip := known && len(spans) > 0
			composerBandKnown.Store(known)
			composerChip.Store(nowChip)
			if known {
				if nowChip {
					sawChip = true
				} else if sawChip {
					// Confirmed disappearance: input-side net has handed over to the
					// rendered marker and can release.
					pasteGuard.Store(false)
					sawChip = false
				}
			}
			if prevChip != nowChip {
				logDebug("[COMPOSER] chip state %v -> %v known=%v", prevChip, nowChip, known)
			}
			if known && nowChip {
				chipObservedFrame.Store(outputFrames.Load() + 1)
			}
			if known {
				if mc := composer.MediaCount(); mc != lastMedia {
					logDebug("[COMPOSER] media count %d -> %d", lastMedia, mc)
					lastMedia = mc
				}
			}
			outputFrames.Add(1)
		}
		for {
			n, err := cpty.Read(readBuf)
			if n > 0 {
				chunk := append(leftover, readBuf[:n]...)
				lastOutputNano.Store(time.Now().UnixNano())
				if p := os.Getenv("AGY_RAWDUMP"); p != "" {
					if f, e := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); e == nil {
						_, _ = f.Write(readBuf[:n])
						_ = f.Close()
					}
				}

				// Detect bracketed paste capability advertised by child (e.g. Bubbletea in agy-real)
				if bytes.Contains(chunk, []byte("\x1b[?2004h")) {
					childSupportsBracketedPaste.Store(true)
					logDebug("[CHILD PTY] Bracketed paste enabled (\\x1b[?2004h)")
				}
				if bytes.Contains(chunk, []byte("\x1b[?2004l")) {
					childSupportsBracketedPaste.Store(false)
					logDebug("[CHILD PTY] Bracketed paste disabled (\\x1b[?2004l)")
				}

				// Dynamically track active model from Bubbletea status bar stream
				updateModelFromChunk(chunk)
				updateSessionModelFromPickerMessage(chunk)
				if bytes.Contains(chunk, []byte("enter Select")) {
					modelPickerOpen.Store(true)
				}

				// Keep up to 32 trailing bytes if chunk ends with incomplete escape sequence
				emit := chunk
				if lastEsc := bytes.LastIndexByte(chunk, 0x1b); lastEsc != -1 && len(chunk)-lastEsc < 32 {
					hasTerm := false
					for _, b := range chunk[lastEsc:] {
						if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || b == '~' {
							hasTerm = true
							break
						}
					}
					if !hasTerm {
						leftover = append([]byte(nil), chunk[lastEsc:]...)
						emit = chunk[:lastEsc]
						// Feed the frame into the tracker BEFORE observing it. The
						// inverse order judged the PREVIOUS, possibly mid-repaint
						// state; if that state was band-unknown and the chunk was the
						// last one, composerBandKnown latched false forever while the
						// screen showed a valid hint+chip (pasteGate then stayed
						// pending). Observing post-feed re-establishes band knowledge
						// from the frame the user actually sees.
						outRenderer.writeChild(emit)
						observe(emit)
						continue
					}
				}
				leftover = nil
				// See the ordering note above: observe the POST-feed frame so a band
				// that was transiently unknown cannot latch.
				outRenderer.writeChild(emit)
				observe(emit)
			}
			if err != nil {
				cancel()
				return
			}
		}
	}()

	// Goroutine 2: os.Stdin -> PTY Input with Ctrl+E Interception
	go func() {
		defer wg.Done()
		var draftRunes []rune
		var leftover []byte
		var inBracketedPaste bool
		var pendingPasteCR bool
		var pendingHighSurrogate rune
		var pasteBytes int
		var pasteHasNewline bool
		var pendingBangRaw []byte
		var pendingBangDraft []rune
		var heldTriggerDraft bool
		pendingBang := false
		readBuf := make([]byte, 2048)
		var pendingPaste pasteGate
		// pendingPastePayload accumulates the decoded characters of the paste being
		// tracked. It is filled at the decode layer, from each event's character, and
		// not from raw bytes: this terminal delivers keystrokes as Win32 Input Mode
		// records, so a literal "\x1b[200~" boundary never appears in the byte stream
		// and a tab is a charVal rather than a 0x09 byte. The payload therefore has to
		// be built from what the parser decoded, and both the printable path and the
		// paste-newline path contribute to it, so no content the composer renders is
		// missing from what the gate compares against.
		var pendingPastePayload []rune
		// redrawComposer asks the CLI to repaint the composer so a settled paste
		// representation is available immediately. It is a resize, never a
		// Ctrl+G editor request, and the original dimensions are restored.
		redrawComposer := func() {
			cw, ch := getConsoleSize(outHandle)
			if cw < 40 || ch < 10 {
				return
			}
			// Synthetic repaint is a real geometry change: cover it with the same
			// ordering as the watcher so a live span cannot emit from stale rows.
			emitMu.Lock()
			geometryEpoch.Add(1)
			err := cpty.Resize(cw, ch+1)
			if err == nil {
				composer.Resize(cw, ch+1)
			}
			emitMu.Unlock()
			if err != nil {
				return
			}
			time.Sleep(40 * time.Millisecond)
			emitMu.Lock()
			geometryEpoch.Add(1)
			_ = cpty.Resize(cw, ch)
			composer.Resize(cw, ch)
			emitMu.Unlock()
		}

		// ctrlHome/ctrlEnd are the composer's own window-navigation keys (CSI
		// 1;5H / 1;5F), the same ones a user presses to walk an offscreen draft.
		// They are sent through the same key path the user's own keys take, so the
		// CLI treats them exactly as typed navigation. Issued only while the gate is
		// settling an armed paste, and the view is walked back at the end.
		// navSend writes a navigation key and waits for the repaint it triggers: it
		// requires the output-frame counter to advance and then the composer to go
		// quiet, so the window read next is the settled result of the navigation
		// rather than the frame that was on screen before it. Waiting only for
		// "quiet" is not enough, because the composer is already quiet at the moment
		// the key is written and the CLI may not have started repainting yet.
		navSend := func(seq []byte) {
			before := outputFrames.Load()
			if p := os.Getenv("AGY_INDUMP"); p != "" {
				if f, e := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); e == nil {
					fmt.Fprintf(f, "N %s %d %x\n", time.Now().Format("15:04:05.000"), len(seq), seq)
					_ = f.Close()
				}
			}
			_, _ = cpty.Write(seq)
			deadline := time.Now().Add(1500 * time.Millisecond)
			for time.Now().Before(deadline) {
				advanced := outputFrames.Load() > before
				quiet := time.Since(time.Unix(0, lastOutputNano.Load())) >= composerQuietWindow
				if advanced && quiet {
					return
				}
				time.Sleep(15 * time.Millisecond)
			}
		}
		// walkAnchor is captured from the tracker the instant the walk is about
		// to move the box, so it describes the view the walk found. It is only
		// touched by this goroutine: the walk runs synchronously inside the
		// Ctrl+E settle, before any further stdin read.
		var walkAnchor caretAnchor
		ctrlHome := func() {
			walkAnchor = caretAnchor{}
			if win, ok := composer.DraftWindow(); ok {
				walkAnchor = captureCaretAnchor(composer, win)
			}
			if walkAnchor.valid {
				logDebug("[WALK] caret anchor captured: draftRow=%d col=%d", walkAnchor.draftRow, walkAnchor.col)
			} else {
				logDebug("[WALK] caret anchor not capturable; the walk will not claim a restore")
			}
			navSend([]byte("\x1b[1;5H"))
		}
		ctrlEnd := func() { navSend([]byte("\x1b[1;5F")) }
		// nav.restore is the verified restore: Ctrl+Home to the head, then Down to
		// the anchored draft row, confirmed against the tracker. restoreView
		// reports its result, and the walk withholds the allow when the caret did
		// not land where it started rather than handing back a moved caret.
		// The console input buffer is the only place a mid-walk keystroke can be seen:
		// nothing reads stdin while the walk runs, so records that appear in it are
		// the user's own typing. The baseline is the queue depth the instant this
		// Ctrl+E is handled — after the Ctrl+E record itself was consumed — so a
		// record that arrives later is input the wrapper has not read yet. A queue
		// that cannot be queried leaves the signal unavailable rather than claiming a
		// clean walk.
		inHandle := windows.Handle(os.Stdin.Fd())
		baseline, haveBaseline := consoleInputPending(inHandle)
		if !haveBaseline {
			logDebug("[WALK] console input queue not observable; mid-walk input cannot be detected")
		} else {
			logDebug("[WALK] console key records pending at walk start: %d", baseline)
		}
		nav := composerNav{
			home: ctrlHome,
			end:  ctrlEnd,
			restore: func() bool {
				if !walkAnchor.valid {
					return false
				}
				return restoreCaretAnchor(walkAnchor, navSend, composer)
			},
			quiet: func() {},
			inputArrived: func() bool {
				if !haveBaseline {
					// The queue is the only place a mid-walk keystroke can be seen, so
					// an unobservable queue means the walk cannot exclude one. Reporting
					// "no input" here would be a false claim that the draft was quiet
					// while the composer was driven; the walk defers instead, which the
					// caller already treats as "not proven" rather than as a refusal.
					return true
				}
				now, ok := consoleInputPending(inHandle)
				if !ok {
					return true
				}
				if now > baseline {
					logDebug("[WALK] console key records pending now: %d (baseline %d)", now, baseline)
				}
				// Strictly deeper than the baseline: the records already waiting when
				// the walk began belong to the draft the walk is measuring, and the
				// payload comparison is what judges those.
				return now > baseline
			},
		}

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			n, err := os.Stdin.Read(readBuf)
			if err != nil {
				return
			}
			if n == 0 {
				continue
			}

			chunk := append(leftover, readBuf[:n]...)
			// Structural only: drafts are never written to the debug log.
			logDebug("[STDIN RAW] read %d bytes", n)

			events, consumed := parseInputEvents(chunk)
			leftover = append([]byte(nil), chunk[consumed:]...)

			for _, ev := range events {
				heldTriggerDraft = false
				draftRunes = applyPendingDraftMirror(draftRunes, cpty)
				draftRunes = applyPendingShadowOverride(draftRunes)
				if shadowResetPending.Swap(false) {
					// The cancelled trigger draft was withheld from native: its
					// bytes never entered the composer. Drop the shadow so the
					// next keystrokes start clean and are forwarded in order.
					draftRunes = nil
					pendingBang = false
					pendingBangRaw = nil
					pendingBangDraft = nil
					pendingPasteCR = false
					pendingHighSurrogate = 0
					pasteGuard.Store(false)
					pasteBytes = 0
					pasteHasNewline = false
					pendingPaste.invalidateGeneration(outputFrames.Load())
					logDebug("[STDIN] cancelled held-trigger shadow reset")
				}
				// A rewrite that ended without a resync (a timeout or a failure)
				// hands its held keystrokes back on one path: the watcher typed the
				// bytes into the composer, and their draft effects are applied here,
				// in the order the user typed them. A reset above belongs to a
				// rewrite that discarded its keystrokes, so it left none to apply.
				draftRunes = adoptPendingDraftOps(draftRunes)
				// 1. Suppress KeyUp event
				if ev.Action == ActionKeyUpIgnore {
					continue
				}
				// Event ownership: any real user keystroke (printable, delete, paste,
				// navigation, caret move) advances the draft epoch, so a span commit that
				// sampled an older epoch knows the caret/content may have moved. The bump
				// takes emitMu so it cannot interleave between a commit's per-key fence
				// check and that key's writeChild (see emitMu).
				if !eventEditsDraft(ev.Action) {
					logDebug("[STDIN] non-text input class=nontext rawlen=%d; epoch unchanged", len(ev.RawBytes))
				} else {
					emitMu.Lock()
					draftEpoch.Add(1)
					emitMu.Unlock()
				}

				// Any non-Enter input breaks a CR/LF pairing, so a CR that is not
				// followed by LF must not swallow a later, unrelated LF.
				if ev.Action != ActionEnter {
					pendingPasteCR = false
				}

				flushBang := func() {
					if !pendingBang {
						return
					}
					// If the rewrite window holds the byte, the capture owns both
					// the bytes and the draft edit, so they are applied together at
					// playback; otherwise the edit lands here, now.
					draftRunes = applyIfDelivered(forwardChild(pendingBangRaw, func(d []rune) []rune {
						return append(d, pendingBangDraft...)
					}), draftRunes, func(d []rune) []rune {
						return append(d, pendingBangDraft...)
					})
					pendingBangRaw = nil
					pendingBangDraft = nil
					pendingBang = false
				}

				if pendingBang && modelPickerOpen.Load() {
					flushBang()
				}

				if !pendingBang && ev.Action == ActionPrintable && !inBracketedPaste && ev.Char == '!' && len(draftRunes) == 0 && !modelPickerOpen.Load() {
					pendingBang = true
					pendingBangRaw = append([]byte(nil), ev.RawBytes...)
					pendingBangDraft = []rune{'!'}
					continue
				}

				if pendingBang {
					switch ev.Action {
					case ActionKeyUpIgnore:
						continue
					case ActionPrintable:
						if inBracketedPaste {
							flushBang()
							break
						}
						pendingBangRaw = append(pendingBangRaw, ev.RawBytes...)
						pendingBangDraft = append(pendingBangDraft, ev.Char)
						if !isEnhanceBangStart(string(pendingBangDraft)) {
							flushBang()
						}
						continue
					case ActionBackspace:
						if len(pendingBangDraft) > 0 {
							pendingBangDraft = pendingBangDraft[:len(pendingBangDraft)-1]
							if len(pendingBangRaw) > 0 {
								pendingBangRaw = pendingBangRaw[:len(pendingBangRaw)-1]
							}
						}
						if len(pendingBangDraft) == 0 {
							pendingBangRaw = nil
							pendingBang = false
						}
						continue
					case ActionEnter:
						text := string(pendingBangDraft)
						if isEnhanceBangStart(text) {
							if _, _, ok := stripEnhanceTrigger(text); ok {
								draftRunes = []rune(text)
								heldTriggerDraft = true
								pendingBangRaw = nil
								pendingBang = false
							} else {
								flushBang()
							}
						} else {
							flushBang()
						}
					default:
						flushBang()
					}
				}

				// 2. Handle Bracketed Paste boundaries
				if ev.Action == ActionBracketedPasteStart {
					inBracketedPaste = true
					pendingPaste.invalidateForPasteStart(outputFrames.Load())
					pendingPasteCR = false
					pasteBytes = 0
					pasteHasNewline = false
					pendingPastePayload = pendingPastePayload[:0]
					_ = forwardChild(ev.RawBytes, nil)
					continue
				}
				if ev.Action == ActionBracketedPasteEnd {
					inBracketedPaste = false
					pendingPasteCR = false
					if pasteBytes > 0 || pasteHasNewline {
						// The payload is the decoded characters of the paste.
						// Reconstruction happens at the decode layer because that
						// is the only layer that sees the characters on every
						// terminal: this one delivers keystrokes as Win32 Input
						// Mode records, so a literal paste boundary never appears
						// in the raw byte stream at all.
						pendingPaste.arm(outputFrames.Load(), string(pendingPastePayload))
						logDebug("[STDIN] paste gate armed gen=%d payloadRunes=%d",
							pendingPaste.gen, len([]rune(pendingPaste.payload)))
					}
					if isProtectedPasteInput(pasteHasNewline, pasteBytes) {
						pasteGuard.Store(true)
						logDebug("[STDIN] paste guard armed: %d bytes, newline=%v", pasteBytes, pasteHasNewline)
					}
					_ = forwardChild(ev.RawBytes, nil)
					continue
				}

				// 3. Detect Ctrl+E
				if ev.Action == ActionCtrlE && modelPickerOpen.Load() {
					// The CLI is showing its model picker; Ctrl+E belongs to the picker,
					// not to a prompt draft (the typed text is the picker filter).
					logDebug("[STDIN] Ctrl+E inside model picker: forwarding without shadow draft")
					_ = forwardChild(ev.RawBytes, nil)
					continue
				}

				if ev.Action == ActionCtrlE {
					// Busy: Ctrl+E is a cancel toggle for EITHER flow. Handled BEFORE
					// the gate so a span-run paste generation cannot misroute it. The
					// span job honours cancellation only during its model phase; the
					// atomic keystroke commit ignores it, so this never mutates.
					if isBusyState.Load() {
						enhanceMu.Lock()
						if enhanceCancel != nil {
							enhanceCancel()
							enhanceCancel = nil
						}
						enhanceMu.Unlock()
						kind := enhanceKind.Load()
						if kind == enhanceKindEditor {
							abortHelper(editorBridgeHandle)
						}
						pendingBang = false
						pendingBangRaw = nil
						pendingBangDraft = nil
						pendingPasteCR = false
						var warn string
						if kind == enhanceKindSpan {
							warn = "\x1b[33m✦ Enhancement dibatalkan (Ctrl+E)\x1b[0m" + strings.Repeat(" ", 20)
						} else {
							warn = "\x1b[33m✦ Enhancement dibatalkan (Ctrl+E)\x1b[0m" + strings.Repeat(" ", 35)
						}
						toastMessage.Store(&warn)
						paintStatus()
						go func() {
							tm := &warn
							deadline := time.Now().Add(2600 * time.Millisecond)
							for time.Now().Before(deadline) {
								if toastMessage.Load() == tm {
									paintStatus()
								}
								time.Sleep(100 * time.Millisecond)
							}
							if toastMessage.CompareAndSwap(tm, nil) {
								paintStatus()
							}
						}()
						continue
					}
					switch settleEnhancementGate(&pendingPaste, composer.DraftBodyRows, composer.DraftWindow, redrawComposer, nav) {
					case composerGateChip:
						// A paste chip is present. Try the chip-preserving span path:
						// it rewrites ONLY the ordinary text beside the atom and never
						// touches the atom itself, so the native paste object survives.
						if beginSpanEnhancement(draftRunes, pendingPaste.chipBody(), getCurrentModel(), outHandle) {
							logDebug("[STDIN] Ctrl+E chip: span enhancement started (chip preserved)")
							continue
						}
						// No safe span plan (chip-only, body unlocatable): keep the
						// safe refusal. The paste generation stays armed on purpose:
						// the chip is
						// still in the composer, so a second Ctrl+E must be refused
						// again rather than fall through to the editor. The gate is
						// cleared only by a clear/submit/new paste, or by a user edit
						// that re-arms it with the new draft as positive evidence.
						logDebug("[STDIN] Ctrl+E refused: chip present; paste stays armed gen=%d guard=%v",
							pendingPaste.gen, pasteGuard.Load())
						warn := "\x1b[33m✦ Draft berisi tempelan/lampiran; enhancer tidak bisa menjaga chip. Kirim apa adanya.\x1b[0m"
						toastMessage.Store(&warn)
						paintStatus()
						go func() {
							tm := &warn
							time.Sleep(2200 * time.Millisecond)
							if toastMessage.CompareAndSwap(tm, nil) {
								paintStatus()
							}
						}()
						continue
					case composerGatePending:
						logDebug("[STDIN] Ctrl+E deferred: paste classification pending")
						warn := "\x1b[33m✦ Tempelan masih diproses; tekan Ctrl+E lagi sebentar lagi.\x1b[0m"
						toastMessage.Store(&warn)
						paintStatus()
						go func() {
							tm := &warn
							time.Sleep(2200 * time.Millisecond)
							if toastMessage.CompareAndSwap(tm, nil) {
								paintStatus()
							}
						}()
						continue
					}
					pendingPaste.invalidateGeneration(outputFrames.Load())
					logDebug("[STDIN] Ctrl+E triggered! current draftRunes length=%d", len(draftRunes))

					// Cancellation support: if already enhancing, pressing Ctrl+E toggles abort
					if isBusyState.Load() {
						enhanceMu.Lock()
						if enhanceCancel != nil {
							enhanceCancel()
							enhanceCancel = nil
						}
						enhanceMu.Unlock()
						abortHelper(editorBridgeHandle)
						// Disarm any trigger hold captured before the abort so a
						// buffered "!" cannot be flushed later as a stale byte.
						// draftRunes is deliberately preserved: Ctrl+E abort must
						// restore the original draft exactly.
						pendingBang = false
						pendingBangRaw = nil
						pendingBangDraft = nil
						pendingPasteCR = false
						if triggerHoldActive.Swap(false) {
							shadowResetPending.Store(true)
						}

						warn := "\x1b[33m✦ Enhancement dibatalkan (Ctrl+E)\x1b[0m" + strings.Repeat(" ", 35)
						toastMessage.Store(&warn)
						paintStatus()
						go func() {
							tm := &warn
							deadline := time.Now().Add(2600 * time.Millisecond)
							for time.Now().Before(deadline) {
								if toastMessage.Load() == tm {
									paintStatus()
								}
								time.Sleep(100 * time.Millisecond)
							}
							if toastMessage.CompareAndSwap(tm, nil) {
								paintStatus()
							}
						}()
						continue
					}

					_, err := SanitizeDraftInput(string(draftRunes))
					if err != nil {
						if errors.Is(err, ErrEmptyDraft) {
							logDebug("[STDIN] Ctrl+E ignored because draft is empty")
							_, _ = os.Stdout.Write([]byte("\a"))
							warn := "\x1b[31m✦ Tulis prompt lebih dulu sebelum Ctrl+E\x1b[0m" + strings.Repeat(" ", 29)
							toastMessage.Store(&warn)
							paintStatus()
							go func() {
								tm := &warn
								time.Sleep(1500 * time.Millisecond)
								if toastMessage.CompareAndSwap(tm, nil) {
									paintStatus()
								}
							}()
							continue
						}
						if errors.Is(err, ErrNonSemanticDraft) {
							logDebug("[STDIN] Ctrl+E rejected non-semantic input (length=%d)", len(draftRunes))
							warn := "\x1b[33m✦ Tulis instruksi tugas yang jelas sebelum Ctrl+E\x1b[0m" + strings.Repeat(" ", 22)
							toastMessage.Store(&warn)
							paintStatus()
							go func() {
								tm := &warn
								time.Sleep(1800 * time.Millisecond)
								if toastMessage.CompareAndSwap(tm, nil) {
									paintStatus()
								}
							}()
							continue
						}
						if errors.Is(err, ErrInvalidUTF8) || errors.Is(err, ErrDraftTooLarge) {
							logDebug("[STDIN] Ctrl+E rejected invalid draft: %v", err)
							warn := "\x1b[31m✦ Draft tidak valid (karakter atau ukuran abnormal)\x1b[0m" + strings.Repeat(" ", 15)
							toastMessage.Store(&warn)
							paintStatus()
							go func() {
								tm := &warn
								time.Sleep(1800 * time.Millisecond)
								if toastMessage.CompareAndSwap(tm, nil) {
									paintStatus()
								}
							}()
							continue
						}
					}

					// Native path: drive the CLI's own Ctrl+G editor roundtrip. The TUI
					// rewrites only prompt text and keeps attachment chips in its state.
					// Plain Ctrl+E: carry the immutable shadow snapshot so a native export
					// that lost or rewrote characters is refused before any model call.
					if editorBridgeHandle != nil && editorBridgeHandle.beginVerified(cpty, string(draftRunes), draftEpoch.Load()) {
						watchEditorBridge(editorBridgeHandle, outHandle)
						continue
					}

					// The editor bridge is the only path that can rewrite native prompt
					// text without corrupting attachment/chip state. If it is unavailable,
					// fail before the model with a truthful message and preserve the draft.
					warn := "\x1b[31m✦ Editor bridge tidak tersedia; draft dipertahankan\x1b[0m" + strings.Repeat(" ", 10)
					toastMessage.Store(&warn)
					paintStatus()
					go func() {
						tm := &warn
						time.Sleep(2400 * time.Millisecond)
						if toastMessage.CompareAndSwap(tm, nil) {
							paintStatus()
						}
					}()
					continue
				}

				// 4. Track Enter (\r or \n) -> prompt submitted
				if ev.Action == ActionEnter {
					if modelPickerOpen.Load() {
						modelPickerOpen.Store(false)
						_ = forwardChild(ev.RawBytes, nil)
						continue
					}
					if inBracketedPaste {
						// A newline inside a paste is payload content; it is
						// recorded in the payload from the same runes the shadow
						// draft gained, so the two can never disagree.
						pasteHasNewline = true
						before := len(draftRunes)
						draftRunes = trackPasteEnter(draftRunes, ev.RawBytes, &pendingPasteCR)
						pendingPastePayload = appendPastePayload(pendingPastePayload, draftRunes[before:])
						// A copy, not a slice of the live draft: the effect runs
						// later, and an append to the draft before then could
						// reuse the same backing array and overwrite these runes.
						added := append([]rune(nil), draftRunes[before:]...)
						// The payload is the record of what was pasted, so it keeps
						// the runes regardless of whether the rewrite window held the
						// byte; only the draft waits for playback.
						draftRunes = keepPrefixIfNotDelivered(forwardChild(ev.RawBytes, func(d []rune) []rune {
							return append(d, added...)
						}), draftRunes, before)
						continue
					}
					pendingPasteCR = false
					if isNativeNewlineSequence(ev.RawBytes) {
						pendingHighSurrogate = 0
						_ = forwardChild(ev.RawBytes, func(d []rune) []rune { return append(d, '\n') })
						continue
					}
					if !isBusyState.Load() {
						if stripped, prefix, ok := stripEnhanceTrigger(string(draftRunes)); ok {
							if strings.TrimSpace(stripped) == "" {
								warn := "\x1b[31m✦ Tulis prompt setelah !/enhance lebih dulu\x1b[0m"
								toastMessage.Store(&warn)
								paintStatus()
								go func() {
									tm := &warn
									time.Sleep(1800 * time.Millisecond)
									if toastMessage.CompareAndSwap(tm, nil) {
										paintStatus()
									}
								}()
								continue
							}
							switch settleEnhancementGate(&pendingPaste, composer.DraftBodyRows, composer.DraftWindow, redrawComposer, nav) {
							case composerGateChip:
								logDebug("[STDIN] local trigger refused: composer marker=true input paste guard=%v", pasteGuard.Load())
								warn := "\x1b[33m✦ Draft berisi tempelan/lampiran; enhancer tidak bisa menjaga chip. Tidak dikirim.\x1b[0m"
								toastMessage.Store(&warn)
								paintStatus()
								go func() {
									tm := &warn
									time.Sleep(2400 * time.Millisecond)
									if toastMessage.CompareAndSwap(tm, nil) {
										paintStatus()
									}
								}()
								continue
							case composerGatePending:
								logDebug("[STDIN] local trigger deferred: paste classification pending")
								warn := "\x1b[33m✦ Tempelan masih diproses; coba lagi sebentar lagi.\x1b[0m"
								toastMessage.Store(&warn)
								paintStatus()
								go func() {
									tm := &warn
									time.Sleep(2400 * time.Millisecond)
									if toastMessage.CompareAndSwap(tm, nil) {
										paintStatus()
									}
								}()
								continue
							}
							pendingPaste.invalidateGeneration(outputFrames.Load())
							if editorBridgeHandle != nil && editorBridgeHandle.beginWithTrigger(cpty, true, string(draftRunes), draftEpoch.Load()) {
								logDebug("[STDIN] local trigger %q intercepted before submit; native editor opened", prefix)
								triggerHoldActive.Store(heldTriggerDraft)
								watchEditorBridge(editorBridgeHandle, outHandle)
								continue
							}
							warn := "\x1b[31m✦ Local enhancer tidak tersedia; draft dipertahankan dan tidak dikirim\x1b[0m"
							toastMessage.Store(&warn)
							paintStatus()
							go func() {
								tm := &warn
								time.Sleep(2400 * time.Millisecond)
								if toastMessage.CompareAndSwap(tm, nil) {
									paintStatus()
								}
							}()
							continue
						}
					}
					if isBusyState.Load() {
						logDebug("[STDIN] Enter blocked while enhancement busy; draft preserved")
						continue
					}
					writeEvidence(strings.TrimSpace(os.Getenv("AGY_WRAP_EVIDENCE_SUBMIT_PATH")), string(draftRunes))
					logDebug("[STDIN] Enter pressed; resetting draftRunes (was %d runes)", len(draftRunes))
					pendingPaste.invalidateGeneration(outputFrames.Load())
					draftRunes = nil
					pendingHighSurrogate = 0
					pasteGuard.Store(false)
					pasteBytes = 0
					pasteHasNewline = false
					_ = forwardChild(ev.RawBytes, nil)
					continue
				}

				// 5. Track Ctrl+C (\x03) -> abort
				if ev.Action == ActionCtrlC {
					logDebug("[STDIN] Ctrl+C observed; busy=%v", isBusyState.Load())
					// Quarantine escape: a deliberate Ctrl+C while input is held for a
					// failed commit returns the held keystrokes to the composer and
					// clears the hold (lossless, not dropped), so the user is never
					// locked out. An export copy is written first for safety.
					if quarantineNonEmpty {
						// The composer state is UNKNOWN here (that is why input was
						// quarantined). Never replay onto an unknown draft and never
						// clear the held queue on a hope. Durable-export the exact bytes;
						// only when the export is CONFIRMED (write+fsync+readback equal)
						// do we surface the path so the user can recover their text.
						// The in-memory buffer is RETAINED regardless (nothing lost, queue
						// not cleared until an explicitly safe consumption is possible).
						path, n := exportQuarantinedInput("ctrl+c")
						if path == "" || n == 0 {
							logDebug("[CAPTURE] Ctrl+C: quarantine export NOT confirmed; held input retained, no replay")
							warn := "\x1b[31m✦ Ekspor gagal; input ditahan (tidak diputar ulang) agar tidak hilang\x1b[0m"
							toastMessage.Store(&warn)
							paintStatus()
							continue
						}
						warn := "\x1b[33m✦ Input ditahan disimpan ke: " + path + " (draft tidak diputar ulang)\x1b[0m"
						toastMessage.Store(&warn)
						paintStatus()
						continue
					}
					if isBusyState.Load() {
						enhanceMu.Lock()
						if enhanceCancel != nil {
							enhanceCancel()
							enhanceCancel = nil
						}
						enhanceMu.Unlock()
						// The helper is a separate process: the only signal it
						// observes is the cancel marker. Without this write Ctrl+C
						// cleared the wrapper's own busy flag while the helper ran
						// on to success, so the aborted enhancement could still
						// write its draft back and report completion.
						abortHelper(editorBridgeHandle)
						// Disarm the trigger machine so a buffered "!" cannot be
						// flushed later as a stale byte, but keep draftRunes: the
						// abort leaves the composer on the draft the user had, so the
						// shadow must keep describing it. The reset decision belongs to
						// the watcher's single handback (endRewriteWithoutResync),
						// which resets only when the trigger draft was withheld.
						pendingBang = false
						pendingBangRaw = nil
						pendingBangDraft = nil
						pendingPasteCR = false
						pendingHighSurrogate = 0
						// Keystrokes typed while the rewrite owned the composer are
						// still held here. The watcher delivers them once it observes
						// the cancelled status, which is after the CLI has handed the
						// composer back, so nothing is typed into a draft that is
						// about to be replaced.
						pendingPaste.invalidateGeneration(outputFrames.Load())
						warn := "\x1b[33m✦ Enhancement dibatalkan (Ctrl+C)\x1b[0m" + strings.Repeat(" ", 35)
						toastMessage.Store(&warn)
						paintStatus()
						go func() {
							tm := &warn
							time.Sleep(1500 * time.Millisecond)
							if toastMessage.CompareAndSwap(tm, nil) {
								paintStatus()
							}
						}()
						continue
					}

					draftRunes = nil
					pendingHighSurrogate = 0
					pendingPaste.invalidateGeneration(outputFrames.Load())
					_ = forwardChild(ev.RawBytes, nil)
					continue
				}

				// 6. Track Ctrl+U (\x15) -> clear line
				if ev.Action == ActionCtrlU {
					// The wipe belongs to whichever draft the composer ends up
					// holding, so it travels with the byte: applied at playback
					// when the byte is held, applied now when it is forwarded.
					// Applying it early would let the replacement erase the
					// effect along with the draft it was meant for.
					clearDraft := func([]rune) []rune { return nil }
					pendingHighSurrogate = 0
					pendingPaste.invalidateGeneration(outputFrames.Load())
					pasteGuard.Store(false)
					pasteBytes = 0
					pasteHasNewline = false
					draftRunes = applyIfDelivered(forwardChild(ev.RawBytes, clearDraft), draftRunes, clearDraft)
					continue
				}

				// 7. Track Backspace (\x08 or \x7f)
				if ev.Action == ActionBackspace {
					backspace := func(d []rune) []rune {
						if len(d) > 0 {
							return d[:len(d)-1]
						}
						return d
					}
					pendingHighSurrogate = 0
					// Deleting re-bases the generation; the remaining draft must be
					// re-verified instead of trusting the stale chip latch.
					pendingPaste.rearmAfterEdit(outputFrames.Load(), string(backspace(draftRunes)))
					draftRunes = applyIfDelivered(forwardChild(ev.RawBytes, backspace), draftRunes, backspace)
					continue
				}

				// 8. Track Ctrl+W (\x17) -> delete word
				if ev.Action == ActionCtrlW {
					pendingPaste.rearmAfterEdit(outputFrames.Load(), string(deleteWordBackwards(draftRunes)))
					draftRunes = applyIfDelivered(forwardChild(ev.RawBytes, deleteWordBackwards), draftRunes, deleteWordBackwards)
					continue
				}

				// 9. Track Printable Characters
				if ev.Action == ActionPrintable || ev.Action == ActionTab {
					before := len(draftRunes)
					payloadBefore := len(pendingPastePayload)
					if inBracketedPaste {
						// The payload is the draft's own contribution from this
						// paste, recorded after the same surrogate pairing the
						// draft went through. Taking it from the draft keeps one
						// decoding path for both: a second one that appended the
						// event's code unit directly would put two lone halves of
						// an astral character in the payload while the draft held
						// one rune, so an emoji in a paste would desync them.
						pasteBytes += len(ev.RawBytes)
						draftRunes = appendInputRune(draftRunes, ev.Char, &pendingHighSurrogate)
						pendingPastePayload = appendPastePayload(pendingPastePayload, draftRunes[before:])
					} else {
						draftRunes = appendInputRune(draftRunes, ev.Char, &pendingHighSurrogate)
					}
					if !inBracketedPaste {
						// A user edit re-bases the generation: the chip latch for
						// the old paste can no longer apply, and the new draft has
						// to render as plain text before the editor may open.
						pendingPaste.rearmAfterEdit(outputFrames.Load(), string(draftRunes))
					}
					// If the rewrite window held the byte, the capture owns both the
					// bytes and the edit: the runes this keystroke contributed are
					// applied at playback, in the order the composer sees them.
					added := append([]rune(nil), draftRunes[before:]...)
					fout := forwardChild(ev.RawBytes, func(d []rune) []rune {
						return append(d, added...)
					})
					draftRunes = keepPrefixIfNotDelivered(fout, draftRunes, before)
					if fout != fcDelivered {
						pendingPastePayload = pendingPastePayload[:payloadBefore]
					}
					continue
				}

				// 10. All other inputs passthrough unmodified
				_ = forwardChild(ev.RawBytes, nil)
			}
		}
	}()

	exitCode, _ := cpty.Wait(ctx)
	cancel()
	cpty.Close()

	// Never lose held input on exit: if keystrokes are still quarantined, write the
	// exact bytes to disk (lossless) and log the path before the process dies.
	if quarantineNonEmpty {
		if p, n := exportQuarantinedInput("process-exit"); p != "" {
			logDebug("=== agy-wrapper exiting with %d quarantined keystroke(s) EXPORTED to %s ===", n, p)
		} else {
			logDebug("=== agy-wrapper exiting with quarantined keystrokes and EXPORT FAILED; data unrecovered ===")
		}
	}

	logDebug("=== agy-wrapper exited with code %d ===", exitCode)
	restoreNativeStatusLine()
	os.Exit(int(exitCode))
}

// eventEditsDraft reports whether an input event may change draft text/caret.
// Non-text terminal reports (mouse/focus/resize) must not invalidate a span
// commit's ownership epoch.
func eventEditsDraft(a ActionType) bool {
	// Hover/focus and key-up releases cannot move the caret or edit the draft;
	// click/drag/wheel can, so they keep the ownership bump even though they
	// never append text.
	return a != ActionPointerHover && a != ActionKeyUpIgnore && a != ActionNonEditKeyUp
}

// geometryChanged is the watcher's sole resize predicate: unchanged dimensions
// must not bump geometryEpoch and invalidate a healthy span.
func geometryChanged(curW, curH, lastW, lastH int) bool { return curW != lastW || curH != lastH }
