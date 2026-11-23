package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type wrapperSettings struct {
	Mode           string `json:"mode"`
	Model          string `json:"model"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
	Detail         string `json:"detail"`
	Language       string `json:"language"`
	Shortcut       string `json:"shortcut"`
}

var standaloneSettings *wrapperSettings

func defaultWrapperSettings() wrapperSettings {
	return wrapperSettings{Mode: "native", TimeoutSeconds: 90, Detail: "balanced", Language: "auto", Shortcut: "Ctrl+E"}
}

func settingsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agy-prompt-enhancer", "settings.json"), nil
}

func normalizeShortcut(value string) (string, error) {
	modifier, key, ok := strings.Cut(strings.ToUpper(strings.TrimSpace(value)), "+")
	if !ok || (modifier != "CTRL" && modifier != "ALT") || len(key) != 1 || key[0] < 'A' || key[0] > 'Z' {
		return "", errors.New("shortcut must be Ctrl or Alt and A–Z, for example Ctrl+E")
	}
	if modifier == "CTRL" {
		if strings.Contains("CGHIJM", key) {
			return "", errors.New("Ctrl+C/G/H/I/J/M are reserved for CLI, editor, or terminal input")
		}
		return "Ctrl+" + key, nil
	}
	return "Alt+" + key, nil
}

func (s *wrapperSettings) validate() error {
	for _, field := range []struct {
		name, value string
		choices     []string
	}{
		{"mode", s.Mode, []string{"native", "local", "off"}},
		{"detail", s.Detail, []string{"concise", "balanced", "detailed"}},
		{"language", s.Language, []string{"auto", "id", "en"}},
	} {
		if !slices.Contains(field.choices, field.value) {
			return fmt.Errorf("%s must be one of %s", field.name, strings.Join(field.choices, ", "))
		}
	}
	s.Model = strings.TrimSpace(s.Model)
	if len(s.Model) > 120 || strings.HasPrefix(s.Model, "-") || strings.ContainsFunc(s.Model, unicode.IsControl) {
		return errors.New("model must be an agy model ID or an empty string")
	}
	if s.TimeoutSeconds < 5 || s.TimeoutSeconds > 180 {
		return errors.New("timeoutSeconds must be between 5 and 180")
	}
	var err error
	s.Shortcut, err = normalizeShortcut(s.Shortcut)
	return err
}

func loadWrapperSettings(path string) (wrapperSettings, error) {
	settings := defaultWrapperSettings()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	data = bytes.TrimSpace(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf}))
	if !bytes.HasPrefix(data, []byte("{")) {
		return settings, errors.New("expected a JSON settings object")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return settings, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return settings, errors.New("expected one JSON settings object")
	}
	err = settings.validate()
	return settings, err
}

func saveWrapperSettings(path string, settings wrapperSettings) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".settings-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(settings)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func (s wrapperSettings) updated(key, value string) (wrapperSettings, error) {
	value = strings.TrimSpace(value)
	switch key {
	case "mode":
		s.Mode = value
	case "model":
		s.Model = value
	case "detail":
		s.Detail = value
	case "language":
		s.Language = value
	case "shortcut":
		s.Shortcut = value
	case "timeoutSeconds":
		seconds, err := strconv.Atoi(value)
		if err != nil {
			return s, errors.New("timeoutSeconds must be an integer")
		}
		s.TimeoutSeconds = seconds
	default:
		return s, fmt.Errorf("unknown setting %q", key)
	}
	err := s.validate()
	return s, err
}

func manageWrapperSettings(path string, args []string, input io.Reader, output io.Writer) error {
	if slices.Equal(args, []string{"--help"}) {
		fmt.Fprintln(output, "agy-enhance settings [menu | reset | KEY VALUE]")
		fmt.Fprintln(output, "Keys: mode, model, detail, language, shortcut, timeoutSeconds")
		return nil
	}
	if slices.Equal(args, []string{"reset"}) {
		return saveWrapperSettings(path, defaultWrapperSettings())
	}
	settings, err := loadWrapperSettings(path)
	if err != nil {
		return err
	}
	if slices.Equal(args, []string{"menu"}) {
		return wrapperSettingsMenu(path, settings, input, output)
	}
	if len(args) == 2 {
		settings, err = settings.updated(args[0], args[1])
		if err != nil {
			return err
		}
		if err := saveWrapperSettings(path, settings); err != nil {
			return err
		}
	} else if len(args) != 0 {
		return errors.New("use agy-enhance settings --help")
	}
	fmt.Fprintln(output, path)
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(settings)
}

func wrapperSettingsMenu(path string, settings wrapperSettings, input io.Reader, output io.Writer) error {
	scanner := bufio.NewScanner(input)
	keys := []string{"detail", "language", "shortcut", "mode", "model", "timeoutSeconds"}
	for {
		fmt.Fprintf(output, "\nPengaturan agy-enhance\n  1. Detail: %s (concise / balanced / detailed)\n"+
			"  2. Bahasa: %s (auto / id / en)\n  3. Shortcut: %s\n"+
			"  4. Mode: %s (native / local / off)\n  5. Model: %s (kosong untuk default agy)\n"+
			"  6. Timeout: %d detik\n  0. Keluar\nPilih nomor: ",
			settings.Detail, settings.Language, settings.Shortcut, settings.Mode, settings.Model, settings.TimeoutSeconds)
		if !scanner.Scan() {
			return scanner.Err()
		}
		selection := strings.TrimSpace(scanner.Text())
		if selection == "" || selection == "0" {
			return nil
		}
		index, err := strconv.Atoi(selection)
		if err != nil || index < 1 || index > len(keys) {
			fmt.Fprintln(output, "Pilih nomor yang tertera.")
			continue
		}
		key := keys[index-1]
		fmt.Fprintf(output, "%s: ", key)
		if !scanner.Scan() {
			return scanner.Err()
		}
		value := strings.TrimSpace(scanner.Text())
		if value == "" && key != "model" {
			continue
		}
		updated, err := settings.updated(key, value)
		if err != nil {
			fmt.Fprintln(output, err)
			continue
		}
		if err := saveWrapperSettings(path, updated); err != nil {
			return err
		}
		settings = updated
		fmt.Fprintln(output, "Tersimpan. Berlaku di sesi baru.")
	}
}

type rewriteSettingsKey struct{}

func rewriteSettingsFor(ctx context.Context) *wrapperSettings {
	if settings, ok := ctx.Value(rewriteSettingsKey{}).(*wrapperSettings); ok {
		return settings
	}
	return standaloneSettings
}

func (s *wrapperSettings) timeout(fallback time.Duration) time.Duration {
	if s == nil {
		return fallback
	}
	return time.Duration(s.TimeoutSeconds) * time.Second
}

func shortcutLabel() string {
	if standaloneSettings != nil {
		return standaloneSettings.Shortcut
	}
	return "Ctrl+E"
}

func standaloneName(name string) bool {
	return strings.EqualFold(filepath.Base(name), "agy-enhance.exe")
}

func modelFromArguments(args []string) string {
	for i, arg := range args {
		if id, ok := strings.CutPrefix(arg, "--model="); ok {
			return id
		}
		if arg == "--model" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
