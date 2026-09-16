package protocol

import (
	"strings"
	"testing"
)

func TestParseTaggedMessage(t *testing.T) {
	got, err := Parse("@time=2026-09-12T12:00:00Z;ircapybara-history=1 :source!plugin@server PRIVMSG #general :hello world")
	if err != nil {
		t.Fatal(err)
	}
	if got.Command != "PRIVMSG" || got.Prefix == nil || got.Prefix.Name != "source" || got.Prefix.User != "plugin" {
		t.Fatalf("unexpected parse: %#v", got)
	}
	if got.Tags["ircapybara-history"] != "1" || got.Trailing() != "hello world" {
		t.Fatalf("unexpected tags/trailing: %#v", got)
	}
}

func TestParseUnknownCommand(t *testing.T) {
	got, err := Parse(":server FUTURECMD a b :c")
	if err != nil {
		t.Fatal(err)
	}
	if got.Command != "FUTURECMD" || len(got.Params) != 3 {
		t.Fatalf("bad parse: %#v", got)
	}
}

func TestLargeUnicodeMessage(t *testing.T) {
	text := strings.Repeat("🦫", 32768)
	got, err := Parse(":a!u@h PRIVMSG #general :" + text)
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(got.Trailing())) != 32768 {
		t.Fatalf("wrong rune count")
	}
}
