package core

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"ircapybara/client/internal/conversation"
	"ircapybara/client/internal/session"
)

// fakeServer is a scripted single-connection IRC server behind TLS.
type fakeServer struct {
	t        *testing.T
	listener net.Listener

	mu       sync.Mutex
	conn     net.Conn
	received []string
	handler  func(s *fakeServer, line string)
	ready    chan struct{}
}

func newFakeServer(t *testing.T, handler func(s *fakeServer, line string)) *fakeServer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "ircapybara-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{t: t, listener: listener, handler: handler, ready: make(chan struct{})}
	t.Cleanup(func() { _ = listener.Close(); s.closeConn() })
	go s.serve()
	return s
}

func (s *fakeServer) port() int { return s.listener.Addr().(*net.TCPAddr).Port }

func (s *fakeServer) serve() {
	conn, err := s.listener.Accept()
	if err != nil {
		return
	}
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	close(s.ready)
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Text()
		s.mu.Lock()
		s.received = append(s.received, line)
		s.mu.Unlock()
		s.handler(s, line)
	}
}

func (s *fakeServer) send(lines ...string) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return
	}
	for _, line := range lines {
		_, _ = conn.Write([]byte(line + "\r\n"))
	}
}

func (s *fakeServer) closeConn() {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (s *fakeServer) saw(line string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, got := range s.received {
		if got == line {
			return true
		}
	}
	return false
}

// registration answers the connection handshake the way an IRCapybara server
// does and returns true when it consumed the line.
func registration(s *fakeServer, line string) bool {
	switch {
	case line == "CAP LS 302":
		s.send(":srv CAP * LS :server-time ircapybara/history")
	case strings.HasPrefix(line, "CAP REQ "):
		s.send(":srv CAP alice ACK " + strings.TrimPrefix(line, "CAP REQ "))
	case line == "CAP END":
		s.send(":srv 001 alice :Welcome", ":srv 005 alice IRCAPYBARA=1 IRCAPYBARA-XFER=2 :are supported by this server")
	case line == "LIST":
		s.send(":srv 322 alice #general 3 :Main room", ":srv 323 alice :End of LIST")
	case line == "JOIN #general":
		s.send(":alice!alice@host JOIN #general")
	case line == "NAMES #general":
		s.send(":srv 353 alice = #general :@bob alice", ":srv 366 alice #general :End of NAMES")
	case strings.HasPrefix(line, "NICK "), strings.HasPrefix(line, "USER "), line == "XMODE EXTENDED":
	default:
		return false
	}
	return true
}

// recordingHost captures everything the core asks of its platform.
type recordingHost struct {
	NopHost
	downloadDir string

	mu        sync.Mutex
	state     ClientSnapshot
	transfers map[string]TransferUpdate
	published []string
	changed   chan struct{}
}

func newRecordingHost(t *testing.T) *recordingHost {
	return &recordingHost{downloadDir: t.TempDir(), transfers: map[string]TransferUpdate{}, changed: make(chan struct{}, 1)}
}

func (h *recordingHost) Emit(event string, payload any) {
	h.mu.Lock()
	switch v := payload.(type) {
	case ClientSnapshot:
		h.state = v
	case TransferUpdate:
		h.transfers[v.FileID] = v
	}
	h.mu.Unlock()
	select {
	case h.changed <- struct{}{}:
	default:
	}
}

func (h *recordingHost) ChooseDownloadPath(_, name, _ string) (string, error) {
	return filepath.Join(h.downloadDir, name), nil
}

func (h *recordingHost) PublishDownload(path, name, _ string) (string, error) {
	h.mu.Lock()
	h.published = append(h.published, path)
	h.mu.Unlock()
	return "published://" + name, nil
}

func (h *recordingHost) FileExists(location string) bool {
	return strings.HasPrefix(location, "published://")
}

func (h *recordingHost) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		h.mu.Lock()
		ok := cond()
		h.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-h.changed:
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func (h *recordingHost) conversation(id string) (conversation.Conversation, bool) {
	for _, conv := range h.state.Conversations {
		if conv.ID == id {
			return conv, true
		}
	}
	return conversation.Conversation{}, false
}

func connectTestApp(t *testing.T, host Host, server *fakeServer) *App {
	t.Helper()
	app := New(Options{Host: host, Platform: PlatformAndroid, ConfigDir: t.TempDir()})
	t.Cleanup(app.Shutdown)
	err := app.Connect(ConnectionConfig{Host: "127.0.0.1", Port: server.port(), Nick: "alice", AllowSelfSigned: true})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return app
}

func TestSessionLifecycleThroughHost(t *testing.T) {
	server := newFakeServer(t, func(s *fakeServer, line string) {
		if registration(s, line) && line == "JOIN #general" {
			s.send("@time=2026-01-02T03:04:05Z :bob!bob@host PRIVMSG #general :hello alice\u2028second line")
		}
	})
	host := newRecordingHost(t)
	app := connectTestApp(t, host, server)

	host.waitFor(t, "ready session with a joined #general and bob's message", func() bool {
		conv, ok := host.conversation("#general")
		if host.state.State != session.Ready || !host.state.Extended || !ok || !conv.Joined || len(conv.Messages) != 2 {
			return false
		}
		join, message := conv.Messages[0], conv.Messages[1]
		return join.System && join.Sender == "alice" && join.Text == "joined the channel" &&
			message.Sender == "bob" && message.Text == "hello alice\nsecond line"
	})

	boot := app.Bootstrap()
	if boot.Platform != PlatformAndroid {
		t.Fatalf("platform = %q", boot.Platform)
	}
	if boot.Settings.Host != "127.0.0.1" || boot.Settings.Nick != "alice" {
		t.Fatalf("settings were not persisted to the configured directory: %+v", boot.Settings)
	}

	if err := app.SendMessage("#general", "hi bob\nsecond line"); err != nil {
		t.Fatalf("send: %v", err)
	}
	host.waitFor(t, "the server to receive the multiline message", func() bool {
		return server.saw("PRIVMSG #general :hi bob\u2028second line")
	})

	server.closeConn()
	host.waitFor(t, "the remote close to surface as a disconnect", func() bool {
		return host.state.State == session.Disconnected && host.state.StateDetail == "Connection closed"
	})
	if err := app.SendMessage("#general", "late"); err == nil {
		t.Fatal("sending after a disconnect should fail")
	}
}

func TestNegotiatedMultilineSendAndReceive(t *testing.T) {
	const caps = "server-time ircapybara/history batch draft/multiline"
	const reply = "First paragraph\n\n```go\n\tfmt.Println(\"hello\")\n```"
	server := newFakeServer(t, func(s *fakeServer, line string) {
		switch line {
		case "CAP LS 302":
			s.send(":srv CAP * LS :" + caps + "=max-bytes=131072,max-lines=512")
		case "JOIN #general":
			registration(s, line)
			s.send(
				":srv NOTICE alice :IRCapybara extended mode active; rendering format v0 is passthrough",
				"@ircapybara-history=1;time=2026-01-02T03:04:05Z :srv BATCH +reply draft/multiline #general",
				"@batch=reply :bot!plugin@srv PRIVMSG #general :First paragraph",
				"@batch=reply :bot!plugin@srv PRIVMSG #general :",
				"@batch=reply :bot!plugin@srv PRIVMSG #general :```go",
				"@batch=reply :bot!plugin@srv PRIVMSG #general :\tfmt.Print",
				"@batch=reply;draft/multiline-concat :bot!plugin@srv PRIVMSG #general :ln(\"hello\")",
				"@batch=reply :bot!plugin@srv PRIVMSG #general :```",
				":srv BATCH -reply",
			)
		default:
			registration(s, line)
		}
	})
	host := newRecordingHost(t)
	app := connectTestApp(t, host, server)
	host.waitFor(t, "one reassembled bot reply", func() bool {
		conv, ok := host.conversation("#general")
		if !ok || !conv.Joined || len(conv.Messages) != 2 {
			return false
		}
		for _, message := range conv.Messages {
			if message.Sender == "bot" && message.Text == reply && message.Synthetic && message.Historical {
				return true
			}
		}
		return false
	})
	if !server.saw("CAP REQ :" + caps) {
		t.Fatal("client did not request multiline capabilities")
	}
	if status, ok := app.convs.Get("status"); ok {
		for _, message := range status.Messages {
			if strings.Contains(message.Text, "rendering format v0") {
				t.Fatalf("obsolete join notice displayed: %q", message.Text)
			}
		}
	}
	if err := app.SendMessage("#general", "one\n\n\ttwo"); err != nil {
		t.Fatal(err)
	}
	host.waitFor(t, "one complete outgoing batch", func() bool {
		server.mu.Lock()
		defer server.mu.Unlock()
		for i, line := range server.received {
			if rest, ok := strings.CutPrefix(line, "BATCH +"); ok {
				ref, target, _ := strings.Cut(rest, " ")
				want := []string{
					"@batch=" + ref + " PRIVMSG #general :one",
					"@batch=" + ref + " PRIVMSG #general :",
					"@batch=" + ref + " PRIVMSG #general :\ttwo",
					"BATCH -" + ref,
				}
				return target == "draft/multiline #general" && len(server.received) >= i+5 &&
					slices.Equal(server.received[i+1:i+5], want)
			}
		}
		return false
	})
}

func TestUserDisconnectStopsConsumer(t *testing.T) {
	server := newFakeServer(t, func(s *fakeServer, line string) { registration(s, line) })
	host := newRecordingHost(t)
	app := connectTestApp(t, host, server)
	host.waitFor(t, "ready", func() bool { return host.state.State == session.Ready })

	if err := app.Disconnect(); err != nil {
		t.Fatal(err)
	}
	host.waitFor(t, "disconnected", func() bool { return host.state.State == session.Disconnected })
	// A user-initiated disconnect has no detail text; the consumer must not
	// overwrite it with a late "Connection closed".
	time.Sleep(100 * time.Millisecond)
	host.mu.Lock()
	detail := host.state.StateDetail
	host.mu.Unlock()
	if detail != "" {
		t.Fatalf("state detail after user disconnect = %q", detail)
	}
}

func TestCheckConnectionDropsSilentServer(t *testing.T) {
	previous := probeTimeout
	probeTimeout = 150 * time.Millisecond
	t.Cleanup(func() { probeTimeout = previous })

	answer := true
	var mu sync.Mutex
	server := newFakeServer(t, func(s *fakeServer, line string) {
		if registration(s, line) {
			return
		}
		mu.Lock()
		ok := answer
		mu.Unlock()
		if strings.HasPrefix(line, "PING ") && ok {
			s.send(":srv PONG srv " + strings.TrimPrefix(line, "PING "))
		}
	})
	host := newRecordingHost(t)
	app := connectTestApp(t, host, server)
	host.waitFor(t, "ready", func() bool { return host.state.State == session.Ready })

	if err := app.CheckConnection(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * probeTimeout)
	host.mu.Lock()
	state := host.state.State
	host.mu.Unlock()
	if state != session.Ready {
		t.Fatalf("an answered probe must keep the session, got %q", state)
	}

	mu.Lock()
	answer = false
	mu.Unlock()
	if err := app.CheckConnection(); err != nil {
		t.Fatal(err)
	}
	host.waitFor(t, "the unanswered probe to drop the session", func() bool {
		return host.state.State == session.Disconnected && strings.Contains(host.state.StateDetail, "stopped responding")
	})
}

func TestDownloadIsPublishedThroughHost(t *testing.T) {
	content := []byte("capybaras are the largest living rodents")
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])
	const chunk = 16
	parts := (len(content) + chunk - 1) / chunk

	server := newFakeServer(t, func(s *fakeServer, line string) {
		if registration(s, line) {
			if line == "JOIN #general" {
				s.send(fmt.Sprintf(":bob!bob@host XATTACH #general f1 %d text/plain :notes.txt", len(content)))
			}
			return
		}
		if line == "RECV f1" {
			s.send(fmt.Sprintf("XFER START alice f1 %d %d %d text/plain :notes.txt", len(content), chunk, parts))
			for i := 0; i < parts; i++ {
				end := min((i+1)*chunk, len(content))
				s.send(fmt.Sprintf("XFER CHUNK f1 %d %s", i, base64.StdEncoding.EncodeToString(content[i*chunk:end])))
			}
			s.send("XFER END f1 " + sha)
		}
		if line == "RECV ACK f1 "+sha {
			s.send("XFER ACK alice f1")
		}
	})
	host := newRecordingHost(t)
	app := connectTestApp(t, host, server)
	host.waitFor(t, "the attachment message", func() bool {
		conv, ok := host.conversation("#general")
		if host.state.State != session.Ready || !host.state.Extended || !ok {
			return false
		}
		for _, message := range conv.Messages {
			if message.Attachment != nil && message.Attachment.FileID == "f1" {
				return true
			}
		}
		return false
	})

	if err := app.DownloadAttachment("f1"); err != nil {
		t.Fatalf("download: %v", err)
	}
	host.waitFor(t, "the acknowledged download", func() bool {
		return host.transfers["f1"].Status == "complete" && server.saw("RECV ACK f1 "+sha)
	})
	// XFER ACK re-emits the transfer from the manager's snapshot, whose path is
	// the private one; the published location must survive that.
	time.Sleep(100 * time.Millisecond)

	host.mu.Lock()
	update := host.transfers["f1"]
	published := append([]string(nil), host.published...)
	host.mu.Unlock()
	if update.Path != "published://notes.txt" {
		t.Fatalf("frontend path = %q, want the published location", update.Path)
	}
	if len(published) != 1 {
		t.Fatalf("PublishDownload calls = %d, want exactly 1", len(published))
	}
	saved, err := os.ReadFile(published[0])
	if err != nil || string(saved) != string(content) {
		t.Fatalf("verified file not at the chosen path: %v", err)
	}
	if !app.DownloadedFileExists("f1") {
		t.Fatal("published download should be reported as present")
	}
	boot := app.Bootstrap()
	if len(boot.Transfers) != 1 || boot.Transfers[0].Path != "published://notes.txt" {
		t.Fatalf("bootstrap transfers = %+v", boot.Transfers)
	}
}
