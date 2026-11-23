package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStandaloneSettingsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("\xef\xbb\xbf"+`{"shortcut":"alt+k"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := loadWrapperSettings(path)
	if err != nil || settings.Shortcut != "Alt+K" || settings.timeout(time.Second) != 90*time.Second {
		t.Fatalf("settings = %+v, err = %v", settings, err)
	}
	var output bytes.Buffer
	if err := manageWrapperSettings(path, []string{"menu"}, strings.NewReader("2\nen\n1\ndetailed\n0\n"), &output); err != nil {
		t.Fatal(err)
	}
	settings, err = loadWrapperSettings(path)
	if err != nil || settings.Language != "en" || settings.Detail != "detailed" {
		t.Fatalf("settings = %+v, err = %v", settings, err)
	}
	if _, err := settings.updated("shortcut", "Ctrl+C"); err == nil {
		t.Fatal("reserved Ctrl+C accepted")
	}
}

func TestStandaloneShortcutProtocols(t *testing.T) {
	for _, test := range []struct {
		raw, shortcut  string
		matches, press bool
	}{
		{"\x05", "Ctrl+E", true, true},
		{"\x1b[0;0;5;1;0;1_", "Ctrl+E", true, true},
		{"\x1bk", "Alt+K", true, true},
		{"\x1b[75;37;107;1;2;1_", "Alt+K", true, true},
		{"\x1b[75;37;107;0;2;1_", "Alt+K", true, false},
		{"\x1b[75;37;107;1;2;3_", "Alt+K", true, false},
		{"\x1b[75;37;107;1;9;1_", "Alt+K", false, false},
		{"\x1b[107;3u", "Alt+K", true, true},
		{"\x1b[27;3;107~", "Alt+K", true, true},
		{"\x1b[107;3:2u", "Alt+K", true, false},
		{"\x05", "Alt+K", false, false},
	} {
		match, press := shortcutEvent([]byte(test.raw), test.shortcut)
		if match != test.matches || match && press != test.press {
			t.Errorf("%q / %s: match=%t, press=%t", test.raw, test.shortcut, match, press)
		}
	}
}

func TestStandaloneEditorKeepsFrameOnlyForOwnedRoundTrip(t *testing.T) {
	var screen editorScreen
	if got := string(screen.filter([]byte(enterAlternate + "draft"))); got != enterAlternate+"draft" {
		t.Fatal(got)
	}
	screen.armed = true
	if got := string(screen.filter([]byte(exitAlternate))); got != "\x1b[?25l" || !screen.pending() {
		t.Fatal(got)
	}
	if got := string(screen.filter([]byte(enterAlternate))); got != "" || !screen.pending() {
		t.Fatalf("input released before repaint: %q", got)
	}
	if got := string(screen.filter([]byte(eraseScreen + "rewritten"))); got != eraseScreen+"rewritten" || screen.pending() {
		t.Fatal(got)
	}
	if got := string(screen.filter([]byte(exitAlternate + enterAlternate))); got != exitAlternate+enterAlternate {
		t.Fatalf("normal editor transitions changed: %q", got)
	}
}

func TestStandaloneSessionSettingsPreserveUserOverrides(t *testing.T) {
	dir := t.TempDir()
	args, err := standaloneSessionArgs([]string{"--settings", `{"editor":"vim","mode":"plan"}`, "--model", "fixture"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(args[2:], []string{"--model", "fixture"}) {
		t.Fatal(args)
	}
	raw, err := os.ReadFile(args[1])
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]string
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if data["editor"] != "vim" || data["altScreenMode"] != "always" || data["mode"] != "plan" {
		t.Fatal(data)
	}
	if modelFromArguments([]string{"--model=fixture"}) != "fixture" {
		t.Fatal("wrong model")
	}
	if !isStandaloneModelStatus("Orchestrator · Gemini 4 Neon · high") || isStandaloneModelStatus("native log") {
		t.Fatal("model status is not properly anchored")
	}
}
