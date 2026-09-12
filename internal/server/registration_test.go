package server

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"ircapybara/internal/config"
	"ircapybara/internal/model"
	"ircapybara/internal/protection"
)

type memoryConn struct{ bytes.Buffer }

func (c *memoryConn) Read([]byte) (int, error)         { return 0, errors.New("read unsupported") }
func (c *memoryConn) Close() error                     { return nil }
func (c *memoryConn) LocalAddr() net.Addr              { return testAddr("local") }
func (c *memoryConn) RemoteAddr() net.Addr             { return testAddr("remote") }
func (c *memoryConn) SetDeadline(time.Time) error      { return nil }
func (c *memoryConn) SetReadDeadline(time.Time) error  { return nil }
func (c *memoryConn) SetWriteDeadline(time.Time) error { return nil }

type testAddr string

func (a testAddr) Network() string { return "test" }
func (a testAddr) String() string  { return string(a) }

func newRegistrationTestServer(c *model.Client) *Server {
	return &Server{
		cfg:        config.Config{ServerName: "irc.test"},
		clients:    map[*model.Client]struct{}{c: {}},
		channels:   make(map[string]*model.Channel),
		identities: make(map[string]model.Credential),
		protection: protection.New(nil, 0, nil, nil),
	}
}

func TestCAPSuspendsRegistrationAndUSERAllowsSingleWordWithoutColon(t *testing.T) {
	conn := &memoryConn{}
	c := &model.Client{
		Conn:         conn,
		RemoteIP:     net.ParseIP("127.0.0.1"),
		ServerPassOK: true,
	}
	s := newRegistrationTestServer(c)

	if disconnect, _ := s.cmdCAP(c, "LS 302"); disconnect {
		t.Fatal("CAP LS must not disconnect")
	}
	if !c.CapNegotiating {
		t.Fatal("CAP LS must suspend IRC registration until CAP END")
	}
	if disconnect, _ := s.cmdNICK(c, "newuser"); disconnect {
		t.Fatal("NICK must not disconnect")
	}
	if disconnect, _ := s.cmdUSER(c, "newuser 0 * NewUser"); disconnect {
		t.Fatal("USER must not disconnect")
	}
	if c.Username != "newuser" || c.Realname != "NewUser" {
		t.Fatalf("unexpected USER state: username=%q realname=%q", c.Username, c.Realname)
	}
	if c.Registered {
		t.Fatal("client must not receive registration before CAP END")
	}
	if strings.Contains(conn.String(), " 001 ") {
		t.Fatal("001 must not be sent while CAP negotiation is active")
	}

	if disconnect, _ := s.cmdCAP(c, "END"); disconnect {
		t.Fatal("CAP END must not disconnect")
	}
	if !c.Registered {
		t.Fatal("CAP END should complete registration once NICK and USER exist")
	}
	if !strings.Contains(conn.String(), " 001 newuser ") {
		t.Fatalf("expected welcome numeric after CAP END, got %q", conn.String())
	}
}

func TestUSERColonFormStillAllowsSpaces(t *testing.T) {
	conn := &memoryConn{}
	c := &model.Client{Conn: conn, RemoteIP: net.ParseIP("127.0.0.1"), ServerPassOK: true}
	s := newRegistrationTestServer(c)
	c.Nick = "alice"

	if disconnect, _ := s.cmdUSER(c, "alice 0 * :Alice Example"); disconnect {
		t.Fatal("USER must not disconnect")
	}
	if c.Realname != "Alice Example" {
		t.Fatalf("unexpected realname %q", c.Realname)
	}
}

func TestServeClientExactModernRegistrationSequence(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	now := time.Now()
	c := &model.Client{
		Conn:         serverConn,
		RemoteIP:     net.ParseIP("127.0.0.1"),
		Connected:    now,
		LastActivity: now,
		Tokens:       20,
		TokenUpdated: now,
		ServerPassOK: true,
	}
	s := newRegistrationTestServer(c)
	s.cfg.RatePerSecond = 5
	s.cfg.RateBurst = 20

	done := make(chan struct{})
	go func() {
		s.serveClient(c)
		close(done)
	}()

	reader := bufio.NewReader(clientConn)
	if _, err := clientConn.Write([]byte("CAP LS 302\r\n")); err != nil {
		t.Fatal(err)
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading CAP reply: %v", err)
	}
	if !strings.Contains(line, " CAP * LS :") {
		t.Fatalf("unexpected CAP reply %q", line)
	}

	if _, err := clientConn.Write([]byte("NICK newuser\r\nUSER newuser 0 * NewUser\r\nCAP END\r\n")); err != nil {
		t.Fatal(err)
	}
	gotWelcome := false
	for i := 0; i < 5; i++ {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading registration reply: %v", err)
		}
		if strings.Contains(line, " 001 newuser ") {
			gotWelcome = true
		}
		if strings.Contains(line, " 376 newuser ") {
			break
		}
	}
	if !gotWelcome {
		t.Fatal("did not receive 001 welcome")
	}

	_ = clientConn.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("serveClient did not stop after peer close")
	}
}
