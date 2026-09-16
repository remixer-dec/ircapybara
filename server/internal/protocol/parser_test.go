package protocol

import (
	"strings"
	"testing"
)

func TestParseLine(t *testing.T) {
	good := []string{
		"NICK alice",
		"USER alice 0 * :Alice Example",
		"PRIVMSG #general :hello π",
		"JOIN #general",
	}
	for _, s := range good {
		if _, err := ParseLine(s); err != nil {
			t.Fatalf("expected %q to parse: %v", s, err)
		}
	}

	bad := []string{
		"PRIVMSG #general :hello\x00evil",
		" PRIVMSG #general :hello",
		"PRIVMSG  #general :hello",
		string(make([]byte, MaxLineBytes+1)),
	}
	for _, s := range bad {
		if _, err := ParseLine(s); err == nil {
			t.Fatalf("expected %q to fail", s)
		}
	}
}

func TestTextPolicy(t *testing.T) {
	if !ValidText("hello café 👋", 100) {
		t.Fatal("utf-8 text should be accepted")
	}
	if !ValidText("plain ASCII", 100) {
		t.Fatal("ASCII should be accepted")
	}
	if ValidText("bad\x1b[31m", 100) {
		t.Fatal("control sequences must be rejected")
	}
}

func TestLargeMessageLimit(t *testing.T) {
	text := strings.Repeat("é", MaxMessageRunes)
	line := "PRIVMSG #general :" + text
	cmd, err := ParseLine(line)
	if err != nil {
		t.Fatalf("max-sized UTF-8 message should parse: %v", err)
	}
	m := MessageArgsRE.FindStringSubmatch(cmd.Args)
	if m == nil || !ValidText(m[2], MaxMessageRunes) {
		t.Fatal("max-sized message should pass message validation")
	}
	tooLong := text + "x"
	if ValidText(tooLong, MaxMessageRunes) {
		t.Fatal("message over 32768 runes must be rejected")
	}
}
