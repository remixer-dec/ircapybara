package protocol

import "testing"

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
