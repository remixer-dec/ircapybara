package server

import (
	"net"
	"strings"
	"testing"
	"time"

	"ircapybara/internal/config"
	"ircapybara/internal/model"
	"ircapybara/pkg/pluginapi"
)

func TestDeliverSyntheticHistoryUsesNegotiatedTags(t *testing.T) {
	conn := &memoryConn{}
	c := &model.Client{
		ID: "c1", Conn: conn, Nick: "alice", Registered: true,
		RemoteIP:     net.ParseIP("127.0.0.1"),
		Capabilities: map[string]bool{"ircapybara/history": true, "server-time": true},
	}
	s := &Server{
		cfg:     config.Config{ServerName: "irc.test"},
		clients: map[*model.Client]struct{}{c: {}},
	}
	at := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	err := s.DeliverSyntheticHistory("c1", "#rss-news", []pluginapi.SyntheticHistoryItem{{
		At: at, From: "rss-mirror", Text: "headline",
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := conn.String()
	if !strings.Contains(got, "@ircapybara-history=1;time=2026-09-12T12:00:00Z") {
		t.Fatalf("missing history/time tags: %q", got)
	}
	if !strings.Contains(got, " PRIVMSG #rss-news :headline") {
		t.Fatalf("missing replay message: %q", got)
	}
}

func TestDeliverSyntheticHistoryRequiresCapability(t *testing.T) {
	c := &model.Client{ID: "c1", Conn: &memoryConn{}, Nick: "alice", Registered: true, Capabilities: map[string]bool{}}
	s := &Server{cfg: config.Config{ServerName: "irc.test"}, clients: map[*model.Client]struct{}{c: {}}}
	if err := s.DeliverSyntheticHistory("c1", "#rss-news", nil); err == nil {
		t.Fatal("expected history capability requirement")
	}
}
