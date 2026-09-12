package server

import (
	"testing"
	"time"

	"ircapybara/internal/model"
	"ircapybara/internal/protocol"
)

func TestReadOnlyCommandPolicy(t *testing.T) {
	s := &Server{}
	allowed := []protocol.Command{
		{Name: "PING", Args: "x"},
		{Name: "JOIN", Args: "#general"},
		{Name: "HISTORY", Args: "#general 10"},
		{Name: "TOPIC", Args: "#general"},
		{Name: "MODE", Args: "#general"},
		{Name: "MODE", Args: "#general +b"},
		{Name: "GUARD", Args: "0"},
	}
	for _, cmd := range allowed {
		if !s.readOnlyCommandAllowed(cmd) {
			t.Fatalf("expected %s %q to be allowed in read-only mode", cmd.Name, cmd.Args)
		}
	}
	blocked := []protocol.Command{
		{Name: "PRIVMSG", Args: "#general :hello"},
		{Name: "NOTICE", Args: "#general :hello"},
		{Name: "TOPIC", Args: "#general :new"},
		{Name: "MODE", Args: "#general +m"},
		{Name: "REGISTER", Args: "password1"},
		{Name: "CHANADD", Args: "#x :x"},
	}
	for _, cmd := range blocked {
		if s.readOnlyCommandAllowed(cmd) {
			t.Fatalf("expected %s %q to be blocked in read-only mode", cmd.Name, cmd.Args)
		}
	}
}

func TestPrivateChannelVisibility(t *testing.T) {
	client := &model.Client{IdentityAuthedNick: "alice"}
	ch := &model.Channel{
		Private:             true,
		Participants:        map[string]struct{}{},
		SessionParticipants: map[*model.Client]struct{}{},
		Members:             map[*model.Client]*model.Membership{},
	}
	s := &Server{}
	if s.canSeeChannelLocked(client, ch) {
		t.Fatal("unrelated client should not see private channel")
	}
	ch.Participants["alice"] = struct{}{}
	if !s.canSeeChannelLocked(client, ch) {
		t.Fatal("registered prior participant should see private channel")
	}
}

func TestGuard3Retention(t *testing.T) {
	now := time.Now()
	cutoff := now.Add(-30 * time.Minute)
	s := &Server{identities: map[string]model.Credential{
		"established": {CreatedAt: now.Add(-2 * time.Hour)},
		"fresh":       {CreatedAt: now.Add(-5 * time.Minute)},
	}}
	tests := []struct {
		name string
		c    *model.Client
		want bool
	}{
		{"operator", &model.Client{IsOper: true, Connected: now}, true},
		{"old anonymous session", &model.Client{Connected: now.Add(-2 * time.Hour)}, true},
		{"recent established identity", &model.Client{Connected: now, Nick: "Established", IdentityAuthedNick: "established"}, true},
		{"recent fresh identity", &model.Client{Connected: now, Nick: "Fresh", IdentityAuthedNick: "fresh"}, false},
		{"recent anonymous", &model.Client{Connected: now, Nick: "guest"}, false},
	}
	for _, tc := range tests {
		if got := s.guard3RetainsLocked(tc.c, cutoff); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}
