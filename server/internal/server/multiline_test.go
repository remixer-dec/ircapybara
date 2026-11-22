package server

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	"ircapybara/internal/model"
	"ircapybara/internal/protocol"
)

func TestBatchThroughServer(t *testing.T) {
	serverConn, peer := net.Pipe()
	defer peer.Close()
	alice := &model.Client{Conn: serverConn, Nick: "alice", Username: "alice", RemoteIP: net.IPv4(127, 0, 0, 1), Registered: true, Capabilities: map[string]bool{"batch": true, "draft/multiline": true}, Tokens: 2, TokenUpdated: time.Now()}
	s := newRegistrationTestServer(alice)
	// One token for the batch and one for the PING that follows it.
	s.cfg.RateBurst = 2
	modernWire, oldWire := &memoryConn{}, &memoryConn{}
	modern := &model.Client{Conn: modernWire, Nick: "bob", Registered: true, Capabilities: map[string]bool{"batch": true, "draft/multiline": true, "ircapybara/history": true, "server-time": true}}
	old := &model.Client{Conn: oldWire, Nick: "carol", Registered: true}
	ch := &model.Channel{Name: "#chat", Norm: "#chat", HistoryEnabled: true, Members: map[*model.Client]*model.Membership{alice: {}, modern: {}, old: {}}}
	s.channels[ch.Norm] = ch
	go s.serveClient(alice)
	peer.SetDeadline(time.Now().Add(3 * time.Second))
	reader := bufio.NewReader(peer)
	source := "BATCH +a draft/multiline #chat\r\n@batch=a PRIVMSG #chat :hello\r\n@batch=a PRIVMSG #chat :\r\n@batch=a PRIVMSG #chat :\tπ\r\n@batch=a;draft/multiline-concat PRIVMSG #chat :!\r\nBATCH -a\r\nPING :done\r\n"
	if _, err := peer.Write([]byte(source)); err != nil {
		t.Fatal(err)
	}
	if line, err := reader.ReadString('\n'); err != nil || !strings.Contains(line, " PONG ") {
		t.Fatalf("PONG: %q, %v", line, err)
	}
	if len(ch.History) != 1 || ch.History[0].Text != "hello\n\n\tπ!" {
		t.Fatalf("history: %#v", ch.History)
	}
	if got := modernWire.String(); strings.Count(got, " BATCH ") != 2 || strings.Count(got, " PRIVMSG ") != 3 {
		t.Fatalf("live batch: %q", got)
	}
	if got := oldWire.String(); strings.Count(got, " PRIVMSG ") != 2 || !strings.Contains(got, " :\tπ!\r\n") {
		t.Fatalf("fallback: %q", got)
	}
	s.cmdHISTORY(modern, "#chat 1")
	if got := modernWire.String(); !strings.Contains(got, "@ircapybara-history=1;time=") || strings.Count(got, " BATCH ") != 4 {
		t.Fatalf("replay: %q", got)
	}
}

func TestInvalidMultilineFragments(t *testing.T) {
	for _, tc := range []struct {
		lines []string
		code  string
	}{
		{[]string{"@batch=a PRIVMSG #other :x"}, "MULTILINE_INVALID_TARGET #chat #other"},
		{[]string{"@batch=a PRIVMSG #chat :", "BATCH -a"}, "MULTILINE_INVALID"},
		{[]string{"@batch=a PRIVMSG #chat :a", "@batch=a NOTICE #chat :b"}, "MULTILINE_INVALID"},
		{[]string{"@batch=a;draft/multiline-concat PRIVMSG #chat :"}, "MULTILINE_INVALID"},
		{[]string{"@batch=a;time=forged PRIVMSG #chat :x"}, "MULTILINE_INVALID"},
		{[]string{"PING :interleaved"}, "MULTILINE_INVALID"},
	} {
		b := incomingBatch{id: "a", target: "#chat"}
		var code string
		for _, line := range tc.lines {
			cmd, err := protocol.ParseLine(line)
			if err != nil {
				t.Fatal(err)
			}
			_, code = b.accept(cmd)
		}
		if code != tc.code {
			t.Errorf("%v: got %q want %q", tc.lines, code, tc.code)
		}
	}
}
