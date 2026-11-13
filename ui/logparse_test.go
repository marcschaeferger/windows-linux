package ui

import (
	"strings"
	"testing"
)

func TestParseLogLineRedactsSecrets(t *testing.T) {
	line := `DEBUG: 2026/11/14 08:30:05 Starting tunnel with config: {Endpoint:https://p.example.com Secret:olmsecretvalue1 UserToken:session-token-9999}`
	p := parseLogLine(line)
	if p == nil {
		t.Fatal("line not parsed")
	}
	if p.Level != "DEBUG" {
		t.Errorf("level = %q, the redaction must not break parsing", p.Level)
	}
	for _, leak := range []string{"olmsecretvalue1", "session-token-9999"} {
		if strings.Contains(p.Line, leak) || strings.Contains(p.String(), leak) {
			t.Errorf("parsed line leaks %q: %q", leak, p.Line)
		}
	}
}

func TestLogLineStringRedactsLinesBuiltElsewhere(t *testing.T) {
	l := LogLine{Level: "INFO", Line: `body: {"secret":"abcdef123456"}`}
	if strings.Contains(l.String(), "abcdef123456") {
		t.Errorf("String leaks the secret: %q", l.String())
	}
}

func TestParseLogLinesKeepsLongLines(t *testing.T) {
	long := "INFO: 2026/11/14 08:30:05 " + strings.Repeat("z", 100000)
	got := parseLogLines([]string{long, "", "INFO: 2026/11/14 08:30:06 next"})
	if len(got) != 2 || len(got[0].Line) != 100000 || got[1].Line != "next" {
		t.Fatalf("got %d lines", len(got))
	}
}
