package bridge

import (
	"encoding/json"
	"testing"
)

func TestTelegramMessageTextMediaFallbacks(t *testing.T) {
	if got := telegramMessageText(&tgMessage{Text: "hello\nworld"}); got != "hello world" {
		t.Fatalf("text=%q", got)
	}
	if got := telegramMessageText(&tgMessage{Photo: []json.RawMessage{json.RawMessage(`{}`)}}); got != "[image]" {
		t.Fatalf("photo=%q", got)
	}
	if got := telegramMessageText(&tgMessage{Animation: json.RawMessage(`{}`)}); got != "[gif]" {
		t.Fatalf("animation=%q", got)
	}
}

func TestDeriveNameIsIRCCompatible(t *testing.T) {
	got := deriveName(tgChat{ID: -100123, Title: "Some Telegram Chat!!!"})
	if got != "Some-Telegram-Chat" {
		t.Fatalf("got %q", got)
	}
}

func TestChatIDCannotBeClaimedByTwoIdentities(t *testing.T) {
	p := &Plugin{mode: "dm", byChat: make(map[int64]*binding), byTarget: make(map[string]*binding)}
	first, err := p.bind(12345, "alice", "family")
	if err != nil {
		t.Fatal(err)
	}
	if first.Identity != "alice" {
		t.Fatalf("identity=%q", first.Identity)
	}
	if _, err := p.bind(12345, "bob", "other"); err == nil {
		t.Fatal("expected second identity to be rejected")
	}
	if again, err := p.bind(12345, "alice", "ignored"); err != nil || again != first {
		t.Fatalf("same identity should receive existing binding: binding=%#v err=%v", again, err)
	}
}
