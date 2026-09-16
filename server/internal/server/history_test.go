package server

import (
	"fmt"
	"net"
	"testing"

	"ircapybara/internal/model"
)

func TestChannelHistoryBound(t *testing.T) {
	ch := &model.Channel{HistoryEnabled: true}
	c := &model.Client{Nick: "alice", Username: "alice", RemoteIP: net.ParseIP("192.0.2.1")}
	for i := 0; i < 105; i++ {
		appendChannelHistoryLocked(ch, c, fmt.Sprintf("message-%d", i), false)
	}
	if len(ch.History) != maxChannelHistory {
		t.Fatalf("history len=%d, want %d", len(ch.History), maxChannelHistory)
	}
	if ch.History[0].Text != "message-5" || ch.History[99].Text != "message-104" {
		t.Fatalf("unexpected retained window: first=%q last=%q", ch.History[0].Text, ch.History[99].Text)
	}
	ch.HistoryEnabled = false
	appendChannelHistoryLocked(ch, c, "ignored", false)
	if len(ch.History) != maxChannelHistory {
		t.Fatal("history-free channel should not append")
	}
}
