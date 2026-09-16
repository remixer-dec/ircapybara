package conversation

import (
	"testing"
	"time"
)

func TestResetClearsOldConversations(t *testing.T) {
	s := NewStore(10)
	s.Ensure("#old", "old", PublicChannel)
	s.Reset("Connecting…")
	snap := s.Snapshot()
	if len(snap) != 1 || snap[0].ID != "status" {
		t.Fatalf("old conversations survived reset: %#v", snap)
	}
}

func TestSetKindMarksPrivateChannel(t *testing.T) {
	s := NewStore(10)
	s.Ensure("#private", "private", PublicChannel)
	s.SetKind("#private", PrivateChannel)
	for _, c := range s.Snapshot() {
		if c.ID == "#private" && c.Kind != PrivateChannel {
			t.Fatalf("kind = %q", c.Kind)
		}
	}
}

func TestMessageHistoryIsBounded(t *testing.T) {
	s := NewStore(2)
	s.AddMessage("#a", "a", PublicChannel, Message{ID: "1"})
	s.AddMessage("#a", "a", PublicChannel, Message{ID: "2"})
	s.AddMessage("#a", "a", PublicChannel, Message{ID: "3"})
	for _, c := range s.Snapshot() {
		if c.ID == "#a" {
			if len(c.Messages) != 2 || c.Messages[0].ID != "2" || c.Messages[1].ID != "3" {
				t.Fatalf("unexpected messages: %#v", c.Messages)
			}
			return
		}
	}
	t.Fatal("conversation not found")
}

func TestHistoricalReplayIsDeduplicatedWithoutAffectingLiveMessages(t *testing.T) {
	s := NewStore(10)
	m := Message{ID: "hist-1", Target: "#general", Sender: "alice", Text: "same text", Historical: true}
	if !s.AddHistoricalMessage("#general", "general", PublicChannel, m) {
		t.Fatal("first historical message was unexpectedly rejected")
	}
	m.ID = "hist-2"
	if s.AddHistoricalMessage("#general", "general", PublicChannel, m) {
		t.Fatal("duplicate historical replay was appended")
	}
	s.AddMessage("#general", "general", PublicChannel, Message{ID: "live", Target: "#general", Sender: "alice", Text: "same text"})
	conv, ok := s.Get("#general")
	if !ok || len(conv.Messages) != 2 {
		t.Fatalf("expected one historical and one live message, got %#v", conv.Messages)
	}
}

func TestCataloguedChannelCanBeJoinedAndRemoved(t *testing.T) {
	s := NewStore(10)
	s.EnsureCatalogued("#dev", "dev", PublicChannel, 12, "Development")
	s.SetJoined("#dev", true)
	conv, ok := s.Get("#dev")
	if !ok || !conv.Catalogued || !conv.Joined || conv.MemberCount != 12 || conv.Topic != "Development" {
		t.Fatalf("unexpected catalog channel: %#v", conv)
	}
	if !s.Remove("#dev") {
		t.Fatal("expected channel removal")
	}
	if _, ok := s.Get("#dev"); ok {
		t.Fatal("removed channel still exists")
	}
}

func TestTaggedHistoryDedupeKeyPreservesIdenticalTextAtDifferentTimes(t *testing.T) {
	s := NewStore(20)
	m1 := Message{ID: "a", Target: "#general", Sender: "feed", Text: "same", Time: time.Now(), Historical: true, DedupeKey: "feed\x00#general\x002026-09-12T12:00:00Z\x00same"}
	m2 := Message{ID: "b", Target: "#general", Sender: "feed", Text: "same", Time: time.Now(), Historical: true, DedupeKey: "feed\x00#general\x002026-09-12T13:00:00Z\x00same"}
	if !s.AddHistoricalMessage("#general", "general", PublicChannel, m1) || !s.AddHistoricalMessage("#general", "general", PublicChannel, m2) {
		t.Fatal("distinct tagged history messages were collapsed")
	}
	conv, _ := s.Get("#general")
	if len(conv.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(conv.Messages))
	}
}

func TestOnlineMembersAreTrackedSeparatelyFromCatalogueTotal(t *testing.T) {
	s := NewStore(10)
	s.EnsureCatalogued("#general", "general", PublicChannel, 12, "General")
	s.SetOnlineMembers("#general", []Member{{Nick: "@bad"}, {Nick: "alice", Operator: true}, {Nick: "bob"}})
	conv, _ := s.Get("#general")
	if conv.MemberCount != 12 || conv.OnlineCount != 3 || len(conv.Members) != 3 {
		t.Fatalf("unexpected presence: %#v", conv)
	}
	foundOperator := false
	for _, member := range conv.Members {
		if member.Nick == "alice" && member.Operator {
			foundOperator = true
			break
		}
	}
	if !foundOperator {
		t.Fatal("operator metadata lost")
	}
}

func TestMarkDisconnectedClearsLiveChannelState(t *testing.T) {
	s := NewStore(20)
	s.EnsureCatalogued("#general", "general", PublicChannel, 5, "topic")
	s.SetJoined("#general", true)
	s.SetOnlineMembers("#general", []Member{{Nick: "alice", Online: true}})
	s.MarkDisconnected()
	c, ok := s.Get("#general")
	if !ok {
		t.Fatal("channel disappeared")
	}
	if c.Joined || c.OnlineCount != 0 || len(c.Members) != 0 {
		t.Fatalf("live state survived disconnect: %#v", c)
	}
	if !c.Catalogued || c.MemberCount != 5 {
		t.Fatalf("catalogue metadata should be preserved: %#v", c)
	}
}
