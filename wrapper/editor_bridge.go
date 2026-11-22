package main

// Native editor bridge: enhancement through the CLI's own Ctrl+G external-editor
// roundtrip. The wrapper points the child's EDITOR at itself in --wrap-editor-helper
// mode. A normal Ctrl+G (no pending request) is delegated to the user's editor;
// a Ctrl+E enhancement request is served in-process and written back as UTF-8
// text only, so the TUI keeps its own attachment chips untouched.

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"
)

const (
	editorHelperFlag = "--wrap-editor-helper"

	envEditorToken    = "AGY_WRAP_EDITOR_TOKEN"
	envEditorDir      = "AGY_WRAP_EDITOR_DIR"
	envEditorOriginal = "AGY_WRAP_EDITOR_ORIGINAL"

	editorRequestName = "request.json"
	editorClaimName   = "request.claimed.json"
	editorDoneName    = "done.json"
	editorCancelName  = "cancel"
	editorAppliedName = "applied.txt"
)

type editorRequest struct {
	Token        string `json:"token"`
	ID           string `json:"id"`
	Path         string `json:"path"`
	Model        string `json:"model,omitempty"`
	StripTrigger bool   `json:"strip_trigger,omitempty"`
	// Expect is the immutable composer snapshot the wrapper observed for this
	// request. When set, the decoded native export must equal it EXACTLY before
	// any model call; a mismatch is refused with zero provider calls.
	Expect   string `json:"expect,omitempty"`
	Revision uint64 `json:"rev,omitempty"`
}

type editorDone struct {
	Token    string  `json:"token"`
	ID       string  `json:"id"`
	Revision uint64  `json:"rev,omitempty"`
	Status   string  `json:"status"`
	Model    string  `json:"model,omitempty"`
	Source   string  `json:"source,omitempty"`
	Duration float64 `json:"duration,omitempty"`
	Detail   string  `json:"detail,omitempty"`
	Path     string  `json:"path,omitempty"`
}

// editorBridge is the wrapper-side handle for one session.
type editorBridge struct {
	dir   string
	token string
	reqID string
	// owner is the snapshot published by the most recent request. The caller
	// must match a result against it BEFORE applying anything.
	owner atomic.Pointer[bridgeOwner]
}

// bridgeOwner binds one published request to its revision.
type bridgeOwner struct {
	id  string
	rev uint64
}

// ownsDone reports whether a helper result belongs to the currently published
// snapshot. A stale, superseded or foreign result fails this check.
func (b *editorBridge) ownsDone(done editorDone) bool {
	if b == nil {
		return false
	}
	own := b.owner.Load()
	if own == nil || own.id == "" {
		return false
	}
	return done.ID == own.id && done.Revision == own.rev && done.Token == b.token
}

func setupEditorBridge() (*editorBridge, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(raw)
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "agy-wrap", "editor", token)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	hardenEditorDir(dir)

	self, err := os.Executable()
	if err != nil {
		return nil, err
	}

	original := os.Getenv("EDITOR")
	_ = os.Setenv(envEditorToken, token)
	_ = os.Setenv(envEditorDir, dir)
	_ = os.Setenv(envEditorOriginal, original)

	helper := syscall.EscapeArg(self) + " " + editorHelperFlag
	_ = os.Setenv("EDITOR", helper)
	return &editorBridge{dir: dir, token: token}, nil
}

// requestEnhancement publishes a one-shot request; the next helper invocation
// consumes it. Returns false when the bridge is unavailable.
func (b *editorBridge) requestEnhancement(stripTrigger bool, expect string, rev uint64) bool {
	if b == nil {
		return false
	}
	_ = os.Remove(filepath.Join(b.dir, editorCancelName))
	_ = os.Remove(filepath.Join(b.dir, editorDoneName))
	_ = os.Remove(filepath.Join(b.dir, editorClaimName))
	idRaw := make([]byte, 8)
	if _, err := rand.Read(idRaw); err != nil {
		return false
	}
	b.reqID = hex.EncodeToString(idRaw)
	req := editorRequest{Token: b.token, ID: b.reqID, Path: "", Model: getCurrentModel(), StripTrigger: stripTrigger, Expect: expect, Revision: rev}
	buf, err := json.Marshal(req)
	if err != nil {
		return false
	}
	tmp := filepath.Join(b.dir, editorRequestName+".tmp")
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return false
	}
	if err := os.Rename(tmp, filepath.Join(b.dir, editorRequestName)); err != nil {
		return false
	}
	b.owner.Store(&bridgeOwner{id: b.reqID, rev: rev})
	return true
}

func (b *editorBridge) cancel() {
	if b == nil {
		return
	}
	_ = os.WriteFile(filepath.Join(b.dir, editorCancelName), []byte("1"), 0o600)
}

// beginVerified is the plain Ctrl+E path: it carries the immutable composer
// snapshot as an integrity expectation, so a native export that lost or rewrote
// characters is refused before any provider call. Every enhancement caller must
// publish that snapshot; there is no unverified entry point.
func (b *editorBridge) beginVerified(w interface{ Write([]byte) (int, error) }, expect string, rev uint64) bool {
	return b.beginWithTrigger(w, false, expect, rev)
}

// beginWithTrigger opens the native editor. expect is the RAW text the wrapper
// expects the CLI to export; when non-empty the decoded export must equal it
// exactly before the model runs. stripTrigger then removes at most one leading
// approved trigger from the verified text, never an ordinary word.
func (b *editorBridge) beginWithTrigger(w interface{ Write([]byte) (int, error) }, stripTrigger bool, expect string, rev uint64) bool {
	if b == nil || expect == "" || !b.requestEnhancement(stripTrigger, expect, rev) {
		return false
	}
	_, err := w.Write([]byte{0x07})
	if err == nil {
		logDebug("[EDITOR-BRIDGE] editor request published; Ctrl+G forwarded")
	}
	return err == nil
}

// bridgeWaitTimeout bounds the editor bridge wait. AGY_WRAP_BRIDGE_TIMEOUT_MS
// exists for owned fixtures that must exercise the timeout path without a slow
// upstream; unset, production keeps the 130 s window.
func bridgeWaitTimeout() time.Duration {
	const def = 130 * time.Second
	raw := strings.TrimSpace(os.Getenv("AGY_WRAP_BRIDGE_TIMEOUT_MS"))
	if raw == "" {
		return def
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms < 500 || ms > 600000 {
		return def
	}
	return time.Duration(ms) * time.Millisecond
}

// wait blocks until the helper reports completion or the deadline expires.
func (b *editorBridge) wait(timeout time.Duration) (editorDone, bool) {
	if b == nil {
		return editorDone{}, false
	}
	deadline := time.Now().Add(timeout)
	path := filepath.Join(b.dir, editorDoneName)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			var done editorDone
			if json.Unmarshal(raw, &done) == nil && done.Token == b.token && (b.reqID == "" || done.ID == b.reqID) {
				return done, true
			}
		}
		time.Sleep(120 * time.Millisecond)
	}
	return editorDone{}, false
}

// runEditorHelper is the entrypoint invoked by the CLI as the external editor.
func runEditorHelper(args []string) int {
	path := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		path = args[0]
	}

	token := os.Getenv(envEditorToken)
	dir := os.Getenv(envEditorDir)
	req, haveReq := claimRequest(dir, token)

	if !haveReq || path == "" {
		return delegateToOriginalEditor(path)
	}
	return serveEnhancement(dir, req, path)
}

// claimRequest atomically consumes request.json into the claim record. The
// rename is the ownership handover: the record survives the claim, so the
// helper can still prove which snapshot it serves, while a request published
// after the claim (a newer file under editorRequestName) is detectable as
// supersession rather than as a missing owner.
func claimRequest(dir, token string) (editorRequest, bool) {
	reqPath := filepath.Join(dir, editorRequestName)
	claimPath := filepath.Join(dir, editorClaimName)
	raw, err := os.ReadFile(reqPath)
	if err != nil || token == "" || dir == "" {
		return editorRequest{}, false
	}
	var req editorRequest
	if json.Unmarshal(raw, &req) != nil || req.Token != token {
		return editorRequest{}, false
	}
	if err := os.Rename(reqPath, claimPath); err != nil {
		return editorRequest{}, false
	}
	return req, true
}

func isEditorCanceled(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, editorCancelName))
	return err == nil
}

// stripEnhanceTrigger removes exactly one leading prompt-enhancer trigger.
// The returned draft is trimmed of the separator whitespace that followed the
// trigger; the original text is returned unchanged when no trigger matches.
func stripEnhanceTrigger(text string) (string, string, bool) {
	trimmed := strings.TrimSpace(text)
	lower := strings.ToLower(trimmed)
	prefixes := []string{"!/enhance:", "!/enhance ", "!enhance:", "!enhance ", "/enhance:", "/enhance ", "! "}
	for _, prefix := range prefixes {
		if strings.HasPrefix(lower, prefix) {
			return strings.TrimSpace(trimmed[len(prefix):]), prefix, true
		}
	}
	if lower == "!/enhance" || lower == "!enhance" || lower == "/enhance" {
		return "", trimmed, true
	}
	return text, "", false
}

func writeEvidence(path, text string) {
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		logDebug("[EDITOR-BRIDGE] evidence write failed: %v", err)
	}
}

// requestStillOwned reports whether the surviving claim record still carries
// this invocation's token/id/revision AND no newer request has been published
// since. The claim record is what proves ownership after consumption; a newer
// request.json means this invocation was superseded and must not commit.
func requestStillOwned(dir string, req editorRequest) bool {
	raw, err := os.ReadFile(filepath.Join(dir, editorClaimName))
	if err != nil {
		return false
	}
	var claimed editorRequest
	if err := json.Unmarshal(raw, &claimed); err != nil {
		return false
	}
	if claimed.Token != req.Token || claimed.ID != req.ID || claimed.Revision != req.Revision {
		return false
	}
	newerRaw, err := os.ReadFile(filepath.Join(dir, editorRequestName))
	if err != nil {
		return true // consumed and not replaced: still the owner
	}
	var newer editorRequest
	if json.Unmarshal(newerRaw, &newer) != nil {
		return false
	}
	return newer.ID == req.ID && newer.Revision == req.Revision
}

func serveEnhancement(dir string, req editorRequest, path string) int {
	finish := func(status, detail string, res *EnhanceResult) int {
		if isEditorCanceled(dir) {
			status = "canceled"
			detail = "canceled"
		}
		done := editorDone{Token: req.Token, ID: req.ID, Revision: req.Revision, Status: status, Detail: detail, Path: path}
		if res != nil {
			done.Model = res.ModelName
			done.Source = string(res.Source)
			done.Duration = res.Duration.Seconds()
		}
		raw, _ := json.Marshal(done)
		tmp := filepath.Join(dir, editorDoneName+".tmp")
		if os.WriteFile(tmp, raw, 0o600) == nil {
			_ = os.Rename(tmp, filepath.Join(dir, editorDoneName))
		}
		return 0
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return finish("error", "read: "+err.Error(), nil)
	}
	evidencePath := strings.TrimSpace(os.Getenv("AGY_WRAP_EVIDENCE_PATH"))
	draft, enc, ok := decodePromptFile(data)
	if !ok {
		logDebug("[EDITOR-BRIDGE] refused draft: bytes=%d nul=%d mixed-or-binary", len(data), len(data))
		// Our own claimed request must surface an explicit error instead of
		// handing the draft to the user's editor.
		return finish("error", "unsupported prompt encoding", nil)
	}
	nativeActual := draft
	// Integrity contract: the ORIGINAL decoded native export is compared with the
	// immutable snapshot the caller published, before any trigger strip or
	// sanitisation could make both sides the same value by construction. A
	// mismatch refuses with ZERO model calls and no file mutation. A request that
	// carries no snapshot is refused too: the only unverified route is the user's
	// own editor, which never enters this helper.
	if req.Expect == "" {
		logDebug("[EDITOR-BRIDGE] integrity REFUSED: no snapshot published rev=%d", req.Revision)
		return finish("refused-integrity", "permintaan tanpa snapshot composer ditolak", nil)
	}
	if nativeActual != req.Expect {
		logDebug("[EDITOR-BRIDGE] integrity REFUSED rev=%d expectedChars=%d nativeChars=%d utf16=%v be=%v bom=%v",
			req.Revision, len([]rune(req.Expect)), len([]rune(nativeActual)), enc.utf16, enc.bigEndian, enc.bom)
		writeEvidence(strings.TrimSpace(os.Getenv("AGY_WRAP_EVIDENCE_NATIVE_PATH")), nativeActual)
		writeEvidence(strings.TrimSpace(os.Getenv("AGY_WRAP_EVIDENCE_EXPECT_PATH")), req.Expect)
		return finish("refused-integrity", "draf native tidak sama dengan draf composer; tidak ada yang di-enhance", nil)
	}
	logDebug("[EDITOR-BRIDGE] integrity OK rev=%d chars=%d exact", req.Revision, len([]rune(nativeActual)))
	fullDraft := nativeActual
	writeEvidence(strings.TrimSpace(os.Getenv("AGY_WRAP_EVIDENCE_EDITOR_INPUT_PATH")), fullDraft)
	logDebug("[EDITOR-BRIDGE] draft bytes=%d utf16=%v be=%v bom=%v decodedChars=%d", len(data), enc.utf16, enc.bigEndian, enc.bom, len([]rune(draft)))
	if req.StripTrigger {
		stripped, prefix, ok := stripEnhanceTrigger(draft)
		if !ok {
			writeEvidence(evidencePath, fullDraft)
			return finish("error", "enhance trigger missing from editor draft", nil)
		}
		logDebug("[EDITOR-BRIDGE] stripped trigger %q; draftChars=%d", prefix, len([]rune(stripped)))
		draft = stripped
	}
	draft, err = SanitizeDraftInput(draft)
	if err != nil {
		return finish("error", err.Error(), nil)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if isEditorCanceled(dir) {
					cancel()
					return
				}
			}
		}
	}()

	model := req.Model
	if model == "" {
		model = getCurrentModel()
	}
	writeEvidence(strings.TrimSpace(os.Getenv("AGY_WRAP_EVIDENCE_MODEL_PATH")), model)
	writeEvidence(strings.TrimSpace(os.Getenv("AGY_WRAP_EVIDENCE_REQUEST_ID_PATH")), req.ID)
	// Ownership gate: a newer request (or a removed one) supersedes this helper,
	// so nothing is enhanced and nothing is committed for it.
	if !requestStillOwned(dir, req) {
		logDebug("[EDITOR-BRIDGE] request superseded before model rev=%d", req.Revision)
		return finish("superseded", "permintaan ini sudah digantikan", nil)
	}
	res := enhanceForBridge(ctx, draft, model)
	if ctx.Err() != nil {
		writeEvidence(evidencePath, fullDraft)
		return finish("canceled", "timeout", &res)
	}
	if res.Source == SourceUnresolvedModel {
		writeEvidence(evidencePath, fullDraft)
		return finish("error", "unresolved session model", &res)
	}
	if res.Source == SourceContainmentBlocked || res.Source == SourceUntrustedWorkdir {
		writeEvidence(evidencePath, fullDraft)
		reason := strings.TrimSpace(res.Reason)
		if reason == "" {
			reason = string(res.Source)
		}
		return finish("error", reason, &res)
	}
	if strings.TrimSpace(res.Text) == "" {
		writeEvidence(evidencePath, fullDraft)
		return finish("error", "empty enhancement", &res)
	}

	text := finalizeEditedText(draft, res.Text)
	payload := []byte(text)
	if enc.utf16 {
		payload = encodeUTF16(text, enc.bigEndian, enc.bom, strings.Contains(draft, "\r\n"))
	}
	err = commitEditedDraft(dir, path, req.ID, data, payload, func() bool {
		return isEditorCanceled(dir) || !requestStillOwned(dir, req)
	})
	switch {
	case errors.Is(err, errEditorCanceled):
		writeEvidence(evidencePath, fullDraft)
		return finish("canceled", "canceled", &res)
	case err != nil:
		writeEvidence(evidencePath, fullDraft)
		return finish("error", "write: "+err.Error(), &res)
	}
	writeEvidence(evidencePath, text)
	if err := os.WriteFile(filepath.Join(dir, editorAppliedName), []byte(text), 0o600); err != nil {
		logDebug("[EDITOR-BRIDGE] applied draft marker failed: %v", err)
	}
	return finish("ok", "", &res)
}

var errEditorCanceled = errors.New("editor request canceled")

// commitEditedDraft stages the payload and renames it over the editor draft.
// When a cancel wins the race, the original draft bytes are restored so a late
// helper never leaves previously canceled text behind.
func commitEditedDraft(dir, path, id string, original, payload []byte, canceled func() bool) error {
	tmp := filepath.Join(dir, "write."+id+".tmp")
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return err
	}
	if canceled() {
		_ = os.Remove(tmp)
		return errEditorCanceled
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if canceled() {
		_ = os.WriteFile(path, original, 0o600)
		return errEditorCanceled
	}
	return nil
}

// decodePromptFile accepts UTF-8 prompts and the UTF-16LE form the CLI writes
// for larger drafts. Unsupported encodings are reported so the caller delegates.
// promptEncoding records how the CLI wrote the editor draft so the rewrite can
// be staged back in the same encoding.
type promptEncoding struct {
	utf16     bool
	bigEndian bool
	bom       bool
}

// decodePromptFile accepts UTF-8 prompts and the UTF-16 form the CLI writes for
// larger drafts. Ambiguous or human-unreadable payloads are rejected instead of
// being guessed, so a binary blob is never rewritten.
func decodePromptFile(data []byte) (string, promptEncoding, bool) {
	var enc promptEncoding
	if len(data) >= 2 && data[0] == 0xFF && data[1] == 0xFE {
		enc = promptEncoding{utf16: true, bom: true}
		text := decodeUTF16(data[2:], false)
		if !plausiblePromptText(text) {
			return "", promptEncoding{}, false
		}
		return text, enc, true
	}
	if len(data) >= 2 && data[0] == 0xFE && data[1] == 0xFF {
		enc = promptEncoding{utf16: true, bigEndian: true, bom: true}
		text := decodeUTF16(data[2:], true)
		if !plausiblePromptText(text) {
			return "", promptEncoding{}, false
		}
		return text, enc, true
	}
	if containsNUL(data) {
		if len(data) < 8 || len(data)%2 != 0 {
			return "", promptEncoding{}, false
		}
		even, odd := 0, 0
		for i, c := range data {
			if c != 0 {
				continue
			}
			if i%2 == 0 {
				even++
			} else {
				odd++
			}
		}
		if even == 0 && odd == 0 {
			return "", promptEncoding{}, false
		}
		if even > 0 && odd > 0 {
			// Both byte positions carry NULs: not a distinguishable UTF-16 draft.
			return "", promptEncoding{}, false
		}
		// A genuine ASCII-heavy UTF-16 draft is roughly half NUL bytes. Sparse
		// NULs mean a mixed/partial encoding the CLI produced for large pasted
		// text; guessing here would rewrite mojibake, so the draft is refused.
		if float64(even+odd)/float64(len(data)) < 0.40 {
			return "", promptEncoding{}, false
		}
		bigEndian := even > odd
		text := decodeUTF16(data, bigEndian)
		if !plausiblePromptText(text) {
			return "", promptEncoding{}, false
		}
		return text, promptEncoding{utf16: true, bigEndian: bigEndian}, true
	}
	return string(data), promptEncoding{}, true
}

// plausiblePromptText rejects decodes that are not human-readable prompt text.
func plausiblePromptText(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		if r == 0xFFFD || r < 0x20 || r == 0x7F {
			return false
		}
	}
	return true
}

func decodeUTF16(b []byte, bigEndian bool) string {
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if bigEndian {
			units = append(units, binary.BigEndian.Uint16(b[i:i+2]))
		} else {
			units = append(units, binary.LittleEndian.Uint16(b[i:i+2]))
		}
	}
	return string(utf16.Decode(units))
}

func encodeUTF16(s string, bigEndian, bom, crlf bool) []byte {
	if crlf {
		s = strings.ReplaceAll(s, "\n", "\r\n")
	}
	units := utf16.Encode([]rune(s))
	size := len(units) * 2
	out := make([]byte, 0, size+2)
	if bom {
		if bigEndian {
			out = append(out, 0xFE, 0xFF)
		} else {
			out = append(out, 0xFF, 0xFE)
		}
	}
	for _, u := range units {
		if bigEndian {
			out = binary.BigEndian.AppendUint16(out, u)
		} else {
			out = binary.LittleEndian.AppendUint16(out, u)
		}
	}
	return out
}

// finalizeEditedText keeps the prompt file free of injected newlines: the model
// output is normalized to LF and only mirrors a trailing newline the user already had.
func finalizeEditedText(original, enhanced string) string {
	normalized := strings.ReplaceAll(enhanced, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	if strings.HasSuffix(original, "\n") {
		normalized = strings.TrimRight(normalized, "\n") + "\n"
	} else {
		normalized = strings.TrimRight(normalized, "\n")
	}
	return normalized
}

func containsNUL(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}

func delegateToOriginalEditor(path string) int {
	parts := splitCommandLine(os.Getenv(envEditorOriginal))
	if len(parts) == 0 {
		parts = []string{defaultEditor()}
	}
	args := append(parts[1:], path)
	cmd := exec.Command(parts[0], args...)
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

func defaultEditor() string {
	for _, candidate := range []string{"vim", "notepad"} {
		if p, err := exec.LookPath(candidate); err == nil {
			return p
		}
	}
	return "notepad"
}

// splitCommandLine splits an editor command on whitespace, honouring double quotes.
// It never invokes a shell, so arguments cannot be reinterpreted.
func splitCommandLine(s string) []string {
	var out []string
	var cur strings.Builder
	inQuotes := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
		case (r == ' ' || r == '\t') && !inQuotes:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// hardenEditorDir removes inherited ACLs and grants only the owner rights SID.
// Windows does not honour POSIX modes, so the token directory must be locked down
// explicitly before it holds request/result metadata.
func hardenEditorDir(dir string) {
	cmd := exec.Command("icacls", dir, "/inheritance:r", "/grant:r", "*S-1-3-4:F")
	_ = cmd.Run()
}

func (b *editorBridge) cleanup() {
	if b != nil {
		_ = os.RemoveAll(b.dir)
	}
}

// enhanceForBridge is the single model entry used by the editor helper. It is a
// variable so a test can prove the integrity contract refuses a mismatched
// export WITHOUT any provider call.
var enhanceForBridge = EnhancePromptWithModelDetailed
