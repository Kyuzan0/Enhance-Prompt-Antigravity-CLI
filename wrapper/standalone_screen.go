package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	enterAlternate = "\x1b[?1049h"
	exitAlternate  = "\x1b[?1049l"
	eraseScreen    = "\x1b[2J"
)

var screenTransitions = regexp.MustCompile(`\x1b\[\?1049[hl]|\x1b\[2J`)
var standaloneModelStatus = regexp.MustCompile(`^(?:(?:auto|plan|accept-edits) · )?(?:Orchestrator · )?[A-Za-z0-9 ._()\-]+ · (?:low|medium|high|xhigh|max)$|^Orchestrator \([A-Za-z0-9 ._\-]+\)$`)

func isStandaloneModelStatus(text string) bool {
	return standaloneModelStatus.MatchString(text)
}

// editorScreen keeps the frame visible through the one editor invocation the
// wrapper owns. Ordinary Ctrl+G and native full-screen views pass through.
type editorScreen struct {
	alternate, armed, held, awaitingRedraw bool
}

func (s *editorScreen) filter(output []byte) []byte {
	return screenTransitions.ReplaceAllFunc(output, func(sequence []byte) []byte {
		switch string(sequence) {
		case enterAlternate:
			s.alternate = true
			if s.held {
				s.held = false
				s.awaitingRedraw = true
				return nil
			}
		case exitAlternate:
			if s.held {
				return nil
			}
			if s.armed && s.alternate {
				s.held, s.armed = true, false
				return []byte("\x1b[?25l")
			}
			s.alternate = false
		case eraseScreen:
			s.awaitingRedraw = false
		}
		return sequence
	})
}

func (s *editorScreen) pending() bool {
	return s.armed || s.held || s.awaitingRedraw
}

func (s *editorScreen) release() string {
	held := s.held
	s.armed, s.held, s.awaitingRedraw = false, false, false
	if held {
		return "\x1b[?25h"
	}
	return ""
}

// standaloneSessionArgs adds a per-session default and forwards explicit user
// overrides. A private temporary settings file keeps values off the command line.
func standaloneSessionArgs(args []string, directory string) ([]string, error) {
	first, end := 0, 0
	var supplied string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if arg == "--settings" && i+1 < len(args) {
			first, end, supplied = i, i+2, args[i+1]
			i++
		} else if value, ok := strings.CutPrefix(arg, "--settings="); ok {
			first, end, supplied = i, i+1, value
		}
	}
	config := map[string]json.RawMessage{}
	if supplied != "" {
		data := []byte(supplied)
		if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
			var err error
			data, err = os.ReadFile(supplied)
			if err != nil {
				return args, nil
			}
		}
		if err := json.Unmarshal(data, &config); err != nil || config == nil {
			return args, nil
		}
	}
	_, screenConfigured := config["altScreenMode"]
	_, editorConfigured := config["editor"]
	if screenConfigured && editorConfigured {
		return args, nil
	}
	if !screenConfigured {
		config["altScreenMode"] = json.RawMessage(`"always"`)
	}
	if !editorConfigured {
		config["editor"] = json.RawMessage(`"auto"`)
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(directory, "native-settings.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return nil, err
	}
	result := append([]string{}, args[:first]...)
	result = append(result, "--settings", path)
	return append(result, args[end:]...), nil
}
