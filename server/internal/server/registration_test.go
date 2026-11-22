package server

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
 "unicode/utf8"

	"ircapybara/internal/config"
	"ircapybara/internal/model"
	"ircapybara/internal/protection"
	"ircapybara/internal/protocol"
	"ircapybara/internal/security"
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

func TestCAPAcceptsIRCapybaraHistory(t *testing.T) {
	conn := &memoryConn{}
	c := &model.Client{Conn: conn, RemoteIP: net.ParseIP("127.0.0.1"), ServerPassOK: true}
	s := newRegistrationTestServer(c)
	if disconnect, _ := s.cmdCAP(c, "REQ :server-time ircapybara/history"); disconnect {
		t.Fatal("CAP REQ must not disconnect")
	}
	if !c.Capabilities["server-time"] || !c.Capabilities["ircapybara/history"] {
		t.Fatalf("capabilities not enabled: %#v", c.Capabilities)
	}
	if !strings.Contains(conn.String(), " ACK :server-time ircapybara/history") {
		t.Fatalf("expected ACK, got %q", conn.String())
	}
}

func registeredCredential(t *testing.T, nick, password string) model.Credential {
	t.Helper()
	salt, hash, err := security.NewPasswordHash(password)
	if err != nil {
		t.Fatal(err)
	}
	return model.Credential{NickDisplay: nick, Salt: salt, Hash: hash, CreatedAt: time.Now().Add(-time.Hour)}
}

func TestRegisteredNickCompletesRegistrationReadOnlyUntilIdentify(t *testing.T) {
	conn := &memoryConn{}
	c := &model.Client{Conn: conn, RemoteIP: net.ParseIP("127.0.0.1"), ServerPassOK: true, Capabilities: make(map[string]bool)}
	s := newRegistrationTestServer(c)
	s.identities["registereduser"] = registeredCredential(t, "registereduser", "password1")
	s.channels["#general"] = &model.Channel{
		Name: "#general", Norm: "#general", Topic: "General", HistoryEnabled: true,
		Members: make(map[*model.Client]*model.Membership), Invited: make(map[*model.Client]bool),
		Participants: make(map[string]struct{}), SessionParticipants: make(map[*model.Client]struct{}),
	}

	s.cmdCAP(c, "LS 302")
	s.cmdNICK(c, "registereduser")
	if !c.IdentityReadOnly {
		t.Fatal("registered nick must enter identity read-only mode before IDENTIFY")
	}
	if strings.Contains(conn.String(), " 464 ") {
		t.Fatalf("NICK must not be rejected for an unidentified registered nick: %q", conn.String())
	}
	s.cmdUSER(c, "registereduser 0 * User")
	s.cmdCAP(c, "REQ :server-time")
	s.cmdCAP(c, "END")
	if !c.Registered {
		t.Fatal("registered nickname must still complete IRC registration")
	}
	if !strings.Contains(conn.String(), " 001 registereduser ") {
		t.Fatalf("expected welcome numeric, got %q", conn.String())
	}
	if disconnect, _ := s.cmdJOIN(c, "#general"); disconnect {
		t.Fatal("read-only identity must be able to join channels")
	}
	if _, ok := s.channels["#general"].Members[c]; !ok {
		t.Fatal("read-only identity should be visible as a channel member")
	}
	if s.identityReadOnlyCommandAllowed(protocol.Command{Name: "PRIVMSG", Args: "#general :hello"}) {
		t.Fatal("read-only identity must not be allowed to publish messages")
	}
	if !s.identityReadOnlyCommandAllowed(protocol.Command{Name: "MODE", Args: "#general b"}) {
		t.Fatal("read-only identity must be allowed to query ban lists")
	}

	s.cmdIDENTIFY(c, "password1")
	if c.IdentityReadOnly || c.IdentityAuthedNick != "registereduser" {
		t.Fatal("password-only IDENTIFY should authenticate current nick and leave read-only mode")
	}
}

func TestIdentifyTrustIPIsRuntimeAndIdentitySpecific(t *testing.T) {
	first := &model.Client{Conn: &memoryConn{}, RemoteIP: net.ParseIP("192.0.2.10"), ServerPassOK: true}
	s := newRegistrationTestServer(first)
	s.cfg.IdentifyTrustIP = true
	s.identities["alice"] = registeredCredential(t, "alice", "password1")
	s.identities["bob"] = registeredCredential(t, "bob", "password2")

	first.Nick = "alice"
	first.IdentityReadOnly = true
	s.cmdIDENTIFY(first, "password1")
	if !s.identityIPTrusted(first.RemoteIP, "alice") {
		t.Fatal("successful IDENTIFY should remember the IP/identity pair")
	}

	second := &model.Client{Conn: &memoryConn{}, RemoteIP: net.ParseIP("192.0.2.10"), ServerPassOK: true}
	s.clients[second] = struct{}{}
	s.cmdNICK(second, "alice")
	if second.IdentityReadOnly || second.IdentityAuthedNick != "alice" {
		t.Fatal("same identity from remembered IP should be restored automatically")
	}

	third := &model.Client{Conn: &memoryConn{}, RemoteIP: net.ParseIP("192.0.2.10"), ServerPassOK: true}
	s.clients[third] = struct{}{}
	s.cmdNICK(third, "bob")
	if !third.IdentityReadOnly || third.IdentityAuthedNick != "" {
		t.Fatal("remembering alice must not authorize bob from the same IP")
	}
}

func TestModeBareBQueriesBanList(t *testing.T) {
	conn := &memoryConn{}
	c := &model.Client{Conn: conn, RemoteIP: net.ParseIP("127.0.0.1"), Nick: "alice", Username: "alice", Registered: true, ServerPassOK: true}
	s := newRegistrationTestServer(c)
	s.channels["#general"] = &model.Channel{
		Name: "#general", Norm: "#general", Members: map[*model.Client]*model.Membership{c: {}},
		Invited: make(map[*model.Client]bool), Participants: make(map[string]struct{}), SessionParticipants: make(map[*model.Client]struct{}),
	}
	if disconnect, _ := s.cmdMODE(c, "#general b"); disconnect {
		t.Fatal("MODE #channel b must not disconnect")
	}
	if !strings.Contains(conn.String(), " 368 alice #general :End of channel ban list") {
		t.Fatalf("expected ban-list terminator, got %q", conn.String())
	}
}

func TestWHOISAcceptsClientDuplicateNickForm(t *testing.T) {
	conn := &memoryConn{}
	requester := &model.Client{Conn: conn, RemoteIP: net.ParseIP("127.0.0.1"), Nick: "alice", Username: "alice", Registered: true}
	target := &model.Client{Conn: &memoryConn{}, RemoteIP: net.ParseIP("192.0.2.9"), Nick: "bob", Username: "bob", Realname: "Bob", Registered: true}
	s := newRegistrationTestServer(requester)
	s.clients[target] = struct{}{}
	if disconnect, _ := s.cmdWHOIS(requester, "bob bob"); disconnect {
		t.Fatal("WHOIS nick nick must not disconnect")
	}
	if !strings.Contains(conn.String(), " 311 alice bob bob 192.0.2.9 * :Bob") {
		t.Fatalf("unexpected WHOIS response: %q", conn.String())
	}
}

func TestJOINAcceptsCommaSeparatedChannels(t *testing.T) {
	conn := &memoryConn{}
	c := &model.Client{Conn: conn, RemoteIP: net.ParseIP("127.0.0.1"), Nick: "alice", Username: "alice", Registered: true}
	s := newRegistrationTestServer(c)
	for _, name := range []string{"#general", "#ai", "#rss-news"} {
		s.channels[name] = &model.Channel{
			Name: name, Norm: name, Members: make(map[*model.Client]*model.Membership), Invited: make(map[*model.Client]bool),
			Participants: make(map[string]struct{}), SessionParticipants: make(map[*model.Client]struct{}),
		}
	}
	if disconnect, _ := s.cmdJOIN(c, "#rss-news,#general,#ai"); disconnect {
		t.Fatal("multi JOIN must not disconnect")
	}
	for _, name := range []string{"#general", "#ai", "#rss-news"} {
		if _, ok := s.channels[name].Members[c]; !ok {
			t.Fatalf("client was not joined to %s", name)
		}
	}
}

func TestGuardedJOINAllowsOnlyTwoTargetsPerMinute(t *testing.T) {
	conn := &memoryConn{}
	c := &model.Client{Conn: conn, RemoteIP: net.ParseIP("127.0.0.1"), Nick: "alice", Username: "alice", Registered: true}
	s := newRegistrationTestServer(c)
	s.protection.SetGuard(2, protection.GuardSnapshot{})
	for _, name := range []string{"#one", "#two", "#three"} {
		s.channels[name] = &model.Channel{
			Name: name, Norm: name, Members: make(map[*model.Client]*model.Membership), Invited: make(map[*model.Client]bool),
			Participants: make(map[string]struct{}), SessionParticipants: make(map[*model.Client]struct{}),
		}
	}
	if disconnect, _ := s.cmdJOIN(c, "#one,#two"); disconnect {
		t.Fatal("guarded JOIN must not disconnect")
	}
	if disconnect, _ := s.cmdJOIN(c, "#three"); disconnect {
		t.Fatal("rate-limited JOIN must not disconnect")
	}
	if _, ok := s.channels["#three"].Members[c]; ok {
		t.Fatal("guard level 2 must stop a user after two channel joins per minute")
	}
	if !strings.Contains(conn.String(), "2 channel joins per minute") {
		t.Fatalf("expected guarded JOIN notice, got %q", conn.String())
	}
}

func TestLongPRIVMSGUpTo32768Runes(t *testing.T) {
	senderConn := &memoryConn{}
	receiverConn := &memoryConn{}
	sender := &model.Client{Conn: senderConn, RemoteIP: net.ParseIP("127.0.0.1"), Nick: "alice", Username: "alice", Registered: true}
	receiver := &model.Client{Conn: receiverConn, RemoteIP: net.ParseIP("127.0.0.2"), Nick: "bob", Username: "bob", Registered: true}
	s := newRegistrationTestServer(sender)
	s.clients[receiver] = struct{}{}
	s.channels["#general"] = &model.Channel{
		Name: "#general", Norm: "#general", HistoryEnabled: true,
		Members: map[*model.Client]*model.Membership{sender: {}, receiver: {}}, Invited: make(map[*model.Client]bool),
		Participants: make(map[string]struct{}), SessionParticipants: make(map[*model.Client]struct{}),
	}
	text := strings.Repeat("é", protocol.MaxMessageRunes)
	if disconnect, _ := s.cmdMessage(sender, "#general :"+text, false); disconnect {
		t.Fatal("long PRIVMSG must not disconnect")
	}
	var received strings.Builder
	for _,line := range strings.Split(strings.TrimSuffix(receiverConn.String(),"\r\n"),"\r\n") {
		_,body,ok := strings.Cut(line," PRIVMSG #general :")
		if !ok || len(line)>510 || !utf8.ValidString(line) { t.Fatalf("invalid fallback line: %q",line) }
		received.WriteString(body)
	}
	if received.String()!=text {
		t.Fatal("receiver did not get full 32768-rune message")
	}
}

func TestGuardZeroIgnoresUnsupportedEnvelope(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	now := time.Now()
	c := &model.Client{
		Conn: serverConn, RemoteIP: net.ParseIP("127.0.0.1"), Connected: now, LastActivity: now,
		Tokens: 20, TokenUpdated: now, ServerPassOK: true,
	}
	s := newRegistrationTestServer(c)
	s.cfg.RatePerSecond = 5
	s.cfg.RateBurst = 20

	done := make(chan struct{})
	go func() { s.serveClient(c); close(done) }()
	// CTCP framing is intentionally unsupported by IRCapybara, but permissive
	// guard level 0 must ignore it rather than terminate an otherwise healthy client.
	if _, err := clientConn.Write([]byte("PRIVMSG alice :\x01VERSION\x01\r\nPING :still-here\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = clientConn.SetReadDeadline(time.Now().Add(time.Second))
	line, err := bufio.NewReader(clientConn).ReadString('\n')
	if err != nil {
		t.Fatalf("expected connection to survive unsupported envelope: %v", err)
	}
	if !strings.Contains(line, " PONG ") {
		t.Fatalf("expected PONG after ignored unsupported input, got %q", line)
	}
	_ = clientConn.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("serveClient did not stop")
	}
}
