package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net"
	"strings"
	"testing"
	"time"

	"ircapybara/internal/config"
	"ircapybara/internal/model"
)

func transferTestClient(id, nick string) (*model.Client, *memoryConn) {
	conn := &memoryConn{}
	c := &model.Client{
		ID: id, Conn: conn, RemoteIP: net.ParseIP("127.0.0.1"), Nick: nick, Username: strings.ToLower(nick),
		Registered: true, ExtendedMode: true, ServerPassOK: true, Capabilities: make(map[string]bool),
		Connected: time.Now(), LastActivity: time.Now(),
	}
	return c, conn
}

func TestAttachmentUploadAndReceiveRoundTrip(t *testing.T) {
	sender, senderConn := transferTestClient("c1", "alice")
	receiver, receiverConn := transferTestClient("c2", "bob")
	ch := &model.Channel{
		Name: "#files", Norm: "#files", Members: map[*model.Client]*model.Membership{sender: {}, receiver: {}},
		Invited: make(map[*model.Client]bool), Participants: make(map[string]struct{}), SessionParticipants: make(map[*model.Client]struct{}),
	}
	s := &Server{
		cfg:     config.Config{ServerName: "irc.test", TransferMaxBytes: 15 * 1024 * 1024},
		clients: map[*model.Client]struct{}{sender: {}, receiver: {}}, channels: map[string]*model.Channel{"#files": ch},
		transfers: make(map[string]*attachmentRecord),
	}
	if disconnect, _ := s.cmdXFER(sender, "START #files 3 text/plain :a.txt"); disconnect {
		t.Fatal("START disconnected")
	}
	var id string
	for k := range s.transfers {
		id = k
	}
	if id == "" {
		t.Fatal("START did not reserve transfer")
	}
	if !strings.Contains(senderConn.String(), " XFER READY alice "+id+" 240 1") {
		t.Fatalf("missing READY: %q", senderConn.String())
	}
	payload := []byte("abc")
	chunk := base64.StdEncoding.EncodeToString(payload)
	s.cmdXFER(sender, "CHUNK "+id+" 0 "+chunk)
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	s.cmdXFER(sender, "END "+id+" "+hash)
	if !strings.Contains(receiverConn.String(), " XATTACH #files "+id+" 3 text/plain :a.txt") {
		t.Fatalf("missing attachment offer: %q", receiverConn.String())
	}

	receiverConn.Reset()
	if disconnect, why := s.cmdRECV(receiver, id); disconnect {
		t.Fatalf("RECV disconnected: %s", why)
	}
	got := receiverConn.String()
	if !strings.Contains(got, " XFER START bob "+id+" 3 240 1 text/plain :a.txt") ||
		!strings.Contains(got, " XFER CHUNK "+id+" 0 :"+chunk) ||
		!strings.Contains(got, " XFER END "+id+" "+hash) {
		t.Fatalf("unexpected transfer replay: %q", got)
	}
	if s.transfers[id].DownloadedAt.IsZero() {
		t.Fatal("successful RECV should mark attachment downloaded")
	}
}

func TestChannelAttachmentACLIsSnapshot(t *testing.T) {
	sender, _ := transferTestClient("c1", "alice")
	receiver, _ := transferTestClient("c2", "bob")
	late, lateConn := transferTestClient("c3", "carol")
	ch := &model.Channel{
		Name: "#files", Norm: "#files", Members: map[*model.Client]*model.Membership{sender: {}, receiver: {}},
		Invited: make(map[*model.Client]bool), Participants: make(map[string]struct{}), SessionParticipants: make(map[*model.Client]struct{}),
	}
	s := &Server{
		cfg:     config.Config{ServerName: "irc.test", TransferMaxBytes: 1024},
		clients: map[*model.Client]struct{}{sender: {}, receiver: {}, late: {}}, channels: map[string]*model.Channel{"#files": ch},
		transfers: make(map[string]*attachmentRecord),
	}
	s.cmdXFER(sender, "START #files 1 text/plain :x.txt")
	var id string
	for k := range s.transfers {
		id = k
	}
	chunk := base64.StdEncoding.EncodeToString([]byte("x"))
	s.cmdXFER(sender, "CHUNK "+id+" 0 "+chunk)
	sum := sha256.Sum256([]byte("x"))
	s.cmdXFER(sender, "END "+id+" "+hex.EncodeToString(sum[:]))

	ch.Members[late] = &model.Membership{}
	s.cmdRECV(late, id)
	if !strings.Contains(lateConn.String(), "Attachment is not available in this conversation") {
		t.Fatalf("late joiner must not receive old attachment: %q", lateConn.String())
	}
}

func TestTransferOutboundChunkFitsIRCLimit(t *testing.T) {
	serverName := strings.Repeat("s", 127)
	id := strings.Repeat("a", 24)
	encoded := base64.StdEncoding.EncodeToString(make([]byte, transferChunkBytes))
	line := ":" + serverName + " XFER CHUNK " + id + " 999999 :" + encoded
	if len(line) > 510 {
		t.Fatalf("chunk line is %d bytes, exceeds IRC payload limit", len(line))
	}
}

func TestTransferWindowIsBounded(t *testing.T) {
	s := &Server{cfg: config.Config{ServerName: "irc.test", TransferMaxBytes: 1024}, transfers: make(map[string]*attachmentRecord)}
	for i := 0; i < transferWindow; i++ {
		id := strings.Repeat(string(rune('a'+i)), 24)
		s.transfers[id] = &attachmentRecord{ID: id, Complete: true, CreatedAt: time.Now(), Data: []byte{1}, Size: 1}
	}
	c, conn := transferTestClient("c1", "alice")
	s.clients = map[*model.Client]struct{}{c: {}}
	target, _ := transferTestClient("c2", "bob")
	s.clients[target] = struct{}{}
	s.cmdXFER(c, "START bob 1 text/plain :x.txt")
	if !strings.Contains(conn.String(), "Attachment window is full") {
		t.Fatalf("expected bounded-window rejection, got %q", conn.String())
	}
}

func TestTransferFastPathRequiresOwnedUpload(t *testing.T) {
	c, _ := transferTestClient("c1", "alice")
	s := &Server{transfers: make(map[string]*attachmentRecord)}
	id := strings.Repeat("a", 24)
	args := "CHUNK " + id + " 0 YQ=="
	if s.transferChunkOwnedBy(c, args) {
		t.Fatal("random transfer id must not get transfer fast path")
	}
	s.transfers[id] = &attachmentRecord{ID: id, SenderID: c.ID, Size: 1, Parts: 1, CreatedAt: time.Now()}
	if !s.transferChunkOwnedBy(c, args) {
		t.Fatal("owned incomplete upload should get transfer fast path")
	}
	other, _ := transferTestClient("c2", "bob")
	if s.transferChunkOwnedBy(other, args) {
		t.Fatal("other client must not get fast path for someone else's upload")
	}
	s.transfers[id].Complete = true
	if s.transferChunkOwnedBy(c, args) {
		t.Fatal("completed upload must not get transfer fast path")
	}
}
