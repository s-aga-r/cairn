package main

import (
	"os"
	"testing"
)

func TestMainReturnsExitCode(t *testing.T) {
	codes := map[string]int{"--version": 0, "no-such-command": 1}
	for argument, expected := range codes {
		os.Args = []string{"cairn", argument}
		exit = func(code int) {
			if code != expected {
				t.Errorf("cairn %s: exit code %d, want %d", argument, code, expected)
			}
		}
		main()
	}
}
