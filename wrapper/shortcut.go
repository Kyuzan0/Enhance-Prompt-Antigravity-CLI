package main

import (
	"strconv"
	"strings"
)

// shortcutEvent recognizes the selected chord in legacy VT, Windows Input
// Mode, Kitty, and xterm modifyOtherKeys. A repeat/release is consumed without
// activating the shortcut again.
func shortcutEvent(raw []byte, shortcut string) (matched, press bool) {
	modifier, letter, ok := strings.Cut(shortcut, "+")
	if !ok || len(letter) != 1 {
		return false, false
	}
	key := int(letter[0])
	ctrl := modifier == "Ctrl"
	text := string(raw)
	if ctrl && len(raw) == 1 && int(raw[0]) == key-'A'+1 {
		return true, true
	}
	if !ctrl && len(raw) == 2 && raw[0] == 27 && (int(raw[1]) == key || int(raw[1]) == key+32) {
		return true, true
	}
	if !strings.HasPrefix(text, "\x1b[") || len(text) < 4 {
		return false, false
	}
	fields := strings.Split(text[2:len(text)-1], ";")
	switch text[len(text)-1] {
	case '_':
		if len(fields) != 6 {
			return false, false
		}
		vk, _ := strconv.Atoi(fields[0])
		char, _ := strconv.Atoi(fields[2])
		state, _ := strconv.Atoi(fields[4])
		repeat, _ := strconv.Atoi(fields[5])
		actualCtrl, actualAlt := state&12 != 0, state&3 != 0
		if vk == 0 && ctrl && state&31 == 0 && char == key-'A'+1 {
			return true, fields[3] == "1" && repeat <= 1
		}
		return vk == key && actualCtrl == ctrl && actualAlt == !ctrl && state&16 == 0,
			fields[3] == "1" && repeat <= 1
	case 'u':
		if len(fields) < 2 {
			return false, false
		}
		code, _, _ := strings.Cut(fields[0], ":")
		mod, event, _ := strings.Cut(fields[1], ":")
		return matchesVTChord(code, mod, key, ctrl), event == "" || event == "1"
	case '~':
		if len(fields) == 3 && fields[0] == "27" {
			return matchesVTChord(fields[2], fields[1], key, ctrl), true
		}
	}
	return false, false
}

func matchesVTChord(code, modifier string, key int, ctrl bool) bool {
	char, _ := strconv.Atoi(code)
	mod, err := strconv.Atoi(modifier)
	expected := 2
	if ctrl {
		expected = 4
	}
	return err == nil && mod >= 1 && (mod-1)&15 == expected && (char == key || char == key+32)
}
