package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"ircapybara/internal/model"
	"ircapybara/internal/protocol"
	"ircapybara/pkg/pluginapi"
)

const (
	transferChunkBytes      = 240
	transferWindow          = 4
	transferDownloadedTTL   = time.Hour
	transferIncompleteTTL   = 10 * time.Minute
	transferUndownloadedTTL = 24 * time.Hour
	transferChunkRate       = 250.0
	transferChunkBurst      = 500.0
)

type attachmentRecord struct {
	ID                string
	SenderID          string
	SenderNick        string
	SenderLine        string
	Conversation      string
	ChannelNorm       string
	Restricted        bool
	AllowedIDs        map[string]struct{}
	AllowedIdentities map[string]struct{}
	Filename          string
	ContentType       string
	Size              int64
	Data              []byte
	NextPart          int
	Parts             int
	Hash              string
	Complete          bool
	CreatedAt         time.Time
	DownloadedAt      time.Time
}

func newTransferID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func transferParts(size int64) int {
	return int((size + transferChunkBytes - 1) / transferChunkBytes)
}

func (s *Server) cleanupTransfersLocked(now time.Time) {
	for id, tr := range s.transfers {
		switch {
		case !tr.Complete && now.Sub(tr.CreatedAt) >= transferIncompleteTTL:
			delete(s.transfers, id)
		case tr.Complete && !tr.DownloadedAt.IsZero() && now.Sub(tr.DownloadedAt) >= transferDownloadedTTL:
			delete(s.transfers, id)
		case tr.Complete && tr.DownloadedAt.IsZero() && now.Sub(tr.CreatedAt) >= transferUndownloadedTTL:
			delete(s.transfers, id)
		}
	}
}

func (s *Server) transferJanitor() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for now := range t.C {
		s.transferMu.Lock()
		s.cleanupTransfersLocked(now)
		s.transferMu.Unlock()
	}
}

func (s *Server) dropIncompleteTransfersForClient(clientID string) {
	s.transferMu.Lock()
	for id, tr := range s.transfers {
		if !tr.Complete && tr.SenderID == clientID {
			delete(s.transfers, id)
		}
	}
	s.transferMu.Unlock()
}

func (s *Server) transferChunkOwnedBy(c *model.Client, args string) bool {
	m := protocol.TransferChunkArgsRE.FindStringSubmatch(args)
	if m == nil {
		return false
	}
	s.transferMu.Lock()
	tr := s.transfers[m[1]]
	ok := tr != nil && !tr.Complete && tr.SenderID == c.ID
	s.transferMu.Unlock()
	return ok
}

func (s *Server) consumeTransferToken(c *model.Client) bool {
	now := time.Now()
	if c.XferTokenUpdated.IsZero() {
		c.XferTokenUpdated = now
		c.XferTokens = transferChunkBurst
	}
	elapsed := now.Sub(c.XferTokenUpdated).Seconds()
	c.XferTokens += elapsed * transferChunkRate
	if c.XferTokens > transferChunkBurst {
		c.XferTokens = transferChunkBurst
	}
	c.XferTokenUpdated = now
	if c.XferTokens < 1 {
		return false
	}
	c.XferTokens--
	return true
}

func (s *Server) cmdXFER(c *model.Client, args string) (bool, string) {
	if !s.requireRegistered(c) {
		return false, ""
	}
	if !c.ExtendedMode {
		s.notice(c, "XFER requires XMODE EXTENDED")
		return false, ""
	}
	if m := protocol.TransferStartArgsRE.FindStringSubmatch(args); m != nil {
		return s.xferStart(c, m)
	}
	if m := protocol.TransferChunkArgsRE.FindStringSubmatch(args); m != nil {
		return s.xferChunk(c, m)
	}
	if m := protocol.TransferEndArgsRE.FindStringSubmatch(args); m != nil {
		return s.xferEnd(c, m)
	}
	s.badParams(c, "XFER")
	return false, ""
}

func (s *Server) xferStart(c *model.Client, m []string) (bool, string) {
	target := m[1]
	size, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil || size < 1 || size > s.cfg.TransferMaxBytes || !protocol.TransferMimeRE.MatchString(m[3]) || !protocol.TransferFilenameRE.MatchString(m[4]) {
		s.notice(c, fmt.Sprintf("Attachment rejected; maximum size is %d bytes", s.cfg.TransferMaxBytes))
		return false, ""
	}

	rec := &attachmentRecord{
		SenderID: c.ID, SenderNick: c.Nick, SenderLine: s.prefix(c), Conversation: target,
		Filename: m[4], ContentType: m[3], Size: size, Parts: transferParts(size), CreatedAt: time.Now(),
		AllowedIDs: make(map[string]struct{}), AllowedIdentities: make(map[string]struct{}),
	}

	if strings.HasPrefix(target, "#") {
		s.mu.RLock()
		ch := s.channels[protocol.Norm(target)]
		if ch == nil {
			s.mu.RUnlock()
			s.numeric(c, "403", target+" :No such channel")
			return false, ""
		}
		if _, ok := ch.Members[c]; !ok {
			s.mu.RUnlock()
			s.numeric(c, "442", ch.Name+" :You're not on that channel")
			return false, ""
		}
		rec.Conversation = ch.Name
		rec.ChannelNorm = ch.Norm
		s.mu.RUnlock()
	} else {
		targetClient := s.findClientByNick(target)
		if targetClient == nil || !targetClient.Registered {
			s.numeric(c, "401", target+" :No such nick")
			return false, ""
		}
		rec.Conversation = targetClient.Nick
		rec.Restricted = true
		rec.AllowedIDs[targetClient.ID] = struct{}{}
		if targetClient.IdentityAuthedNick != "" {
			rec.AllowedIdentities[targetClient.IdentityAuthedNick] = struct{}{}
		}
	}

	id, err := newTransferID()
	if err != nil {
		s.notice(c, "Attachment initialization failed")
		return false, ""
	}
	rec.ID = id
	if size < 1<<20 {
		rec.Data = make([]byte, 0, int(size))
	} else {
		rec.Data = make([]byte, 0, 1<<20)
	}
	s.transferMu.Lock()
	s.cleanupTransfersLocked(time.Now())
	if len(s.transfers) >= transferWindow {
		s.transferMu.Unlock()
		s.notice(c, "Attachment window is full; try again after an existing transfer expires")
		return false, ""
	}
	s.transfers[id] = rec
	s.transferMu.Unlock()
	s.sendLine(c, fmt.Sprintf(":%s XFER READY %s %s %d %d", s.cfg.ServerName, c.Nick, id, transferChunkBytes, rec.Parts))
	return false, ""
}

func (s *Server) xferChunk(c *model.Client, m []string) (bool, string) {
	part, err := strconv.Atoi(m[2])
	if err != nil {
		s.badParams(c, "XFER")
		return false, ""
	}
	decoded, err := base64.StdEncoding.DecodeString(m[3])
	if err != nil {
		s.notice(c, "Attachment chunk is not valid base64")
		return false, ""
	}

	s.transferMu.Lock()
	tr := s.transfers[m[1]]
	if tr == nil || tr.SenderID != c.ID || tr.Complete {
		s.transferMu.Unlock()
		s.notice(c, "Unknown or unavailable upload")
		return false, ""
	}
	if part != tr.NextPart || part >= tr.Parts {
		s.transferMu.Unlock()
		s.notice(c, "Attachment chunks must be sent once, in order")
		return false, ""
	}
	expected := transferChunkBytes
	if part == tr.Parts-1 {
		expected = int(tr.Size) - (tr.Parts-1)*transferChunkBytes
	}
	if len(decoded) != expected || int64(len(tr.Data)+len(decoded)) > tr.Size {
		s.transferMu.Unlock()
		s.notice(c, "Attachment chunk has invalid size")
		return false, ""
	}
	tr.Data = append(tr.Data, decoded...)
	tr.NextPart++
	s.transferMu.Unlock()
	return false, ""
}

func (s *Server) xferEnd(c *model.Client, m []string) (bool, string) {
	s.transferMu.Lock()
	tr := s.transfers[m[1]]
	if tr == nil || tr.SenderID != c.ID || tr.Complete {
		s.transferMu.Unlock()
		s.notice(c, "Unknown or unavailable upload")
		return false, ""
	}
	if tr.NextPart != tr.Parts || int64(len(tr.Data)) != tr.Size {
		s.transferMu.Unlock()
		s.notice(c, "Attachment upload is incomplete")
		return false, ""
	}
	sum := sha256.Sum256(tr.Data)
	actual := hex.EncodeToString(sum[:])
	if actual != m[2] {
		delete(s.transfers, tr.ID)
		s.transferMu.Unlock()
		s.notice(c, "Attachment checksum mismatch; upload discarded")
		return false, ""
	}
	channelNorm := tr.ChannelNorm
	s.transferMu.Unlock()

	allowedIDs := make(map[string]struct{})
	allowedIdentities := make(map[string]struct{})
	if channelNorm != "" {
		s.mu.RLock()
		if ch := s.channels[channelNorm]; ch != nil {
			for member := range ch.Members {
				if member.ID == c.ID {
					continue
				}
				allowedIDs[member.ID] = struct{}{}
				if member.IdentityAuthedNick != "" {
					allowedIdentities[member.IdentityAuthedNick] = struct{}{}
				}
			}
		}
		s.mu.RUnlock()
	}

	s.transferMu.Lock()
	current := s.transfers[m[1]]
	if current != tr || current == nil || current.Complete {
		s.transferMu.Unlock()
		s.notice(c, "Attachment upload expired before completion")
		return false, ""
	}
	if channelNorm != "" {
		current.AllowedIDs = allowedIDs
		current.AllowedIdentities = allowedIdentities
		current.Restricted = true
	}
	current.Hash = actual
	current.Complete = true
	s.transferMu.Unlock()

	s.notifyAttachment(current, c.ID)
	s.sendLine(c, fmt.Sprintf(":%s XFER STORED %s %s", s.cfg.ServerName, c.Nick, current.ID))
	return false, ""
}

func (s *Server) attachmentAllowed(c *model.Client, tr *attachmentRecord) bool {
	if tr.ChannelNorm != "" {
		s.mu.RLock()
		ch := s.channels[tr.ChannelNorm]
		_, joined := func() (*model.Membership, bool) {
			if ch == nil {
				return nil, false
			}
			m, ok := ch.Members[c]
			return m, ok
		}()
		s.mu.RUnlock()
		if !joined {
			return false
		}
	}
	if !tr.Restricted {
		return true
	}
	if _, ok := tr.AllowedIDs[c.ID]; ok {
		return true
	}
	if c.IdentityAuthedNick != "" {
		_, ok := tr.AllowedIdentities[c.IdentityAuthedNick]
		return ok
	}
	return false
}

func (s *Server) cmdRECV(c *model.Client, args string) (bool, string) {
	if !s.requireRegistered(c) {
		return false, ""
	}
	if !c.ExtendedMode {
		s.notice(c, "RECV requires XMODE EXTENDED")
		return false, ""
	}
	m := protocol.RecvArgsRE.FindStringSubmatch(args)
	if m == nil {
		s.badParams(c, "RECV")
		return false, ""
	}
	s.transferMu.Lock()
	s.cleanupTransfersLocked(time.Now())
	tr := s.transfers[m[1]]
	if tr == nil || !tr.Complete {
		s.transferMu.Unlock()
		s.notice(c, "Attachment is no longer available")
		return false, ""
	}
	data := tr.Data
	meta := *tr
	s.transferMu.Unlock()

	if !s.attachmentAllowed(c, &meta) {
		s.notice(c, "Attachment is not available in this conversation")
		return false, ""
	}
	start := fmt.Sprintf(":%s XFER START %s %s %d %d %d %s :%s", s.cfg.ServerName, c.Nick, meta.ID, meta.Size, transferChunkBytes, meta.Parts, meta.ContentType, meta.Filename)
	if err := s.sendLineErr(c, start); err != nil {
		return true, "Attachment download write failed"
	}
	for part, off := 0, 0; off < len(data); part++ {
		end := off + transferChunkBytes
		if end > len(data) {
			end = len(data)
		}
		encoded := base64.StdEncoding.EncodeToString(data[off:end])
		line := fmt.Sprintf(":%s XFER CHUNK %s %d :%s", s.cfg.ServerName, meta.ID, part, encoded)
		if err := s.sendLineErr(c, line); err != nil {
			return true, "Attachment download write failed"
		}
		off = end
	}
	if err := s.sendLineErr(c, fmt.Sprintf(":%s XFER END %s %s", s.cfg.ServerName, meta.ID, meta.Hash)); err != nil {
		return true, "Attachment download write failed"
	}
	s.transferMu.Lock()
	if current := s.transfers[meta.ID]; current != nil && current.DownloadedAt.IsZero() {
		current.DownloadedAt = time.Now()
	}
	s.transferMu.Unlock()
	return false, ""
}

func (s *Server) attachmentOfferLine(tr *attachmentRecord) string {
	return fmt.Sprintf(":%s XATTACH %s %s %d %s :%s", tr.SenderLine, tr.Conversation, tr.ID, tr.Size, tr.ContentType, tr.Filename)
}

func (s *Server) notifyAttachment(tr *attachmentRecord, exceptID string) {
	var recipients []*model.Client
	if tr.ChannelNorm != "" && !tr.Restricted {
		s.mu.RLock()
		if ch := s.channels[tr.ChannelNorm]; ch != nil {
			for c := range ch.Members {
				if c.ID != exceptID {
					recipients = append(recipients, c)
				}
			}
		}
		s.mu.RUnlock()
	} else {
		s.mu.RLock()
		for c := range s.clients {
			if c.ID == exceptID {
				continue
			}
			_, idOK := tr.AllowedIDs[c.ID]
			_, identityOK := tr.AllowedIdentities[c.IdentityAuthedNick]
			if idOK || (c.IdentityAuthedNick != "" && identityOK) {
				if tr.ChannelNorm == "" {
					recipients = append(recipients, c)
				} else if ch := s.channels[tr.ChannelNorm]; ch != nil {
					if _, joined := ch.Members[c]; joined {
						recipients = append(recipients, c)
					}
				}
			}
		}
		s.mu.RUnlock()
	}
	for _, c := range recipients {
		if c.ExtendedMode {
			s.sendLine(c, s.attachmentOfferLine(tr))
			continue
		}
		fallback := fmt.Sprintf(":%s PRIVMSG %s :[attachment: %s, %d bytes; extended mode required]", tr.SenderLine, tr.Conversation, tr.Filename, tr.Size)
		s.sendLine(c, fallback)
	}
}

func (s *Server) MaxAttachmentBytes() int64 { return s.cfg.TransferMaxBytes }

func (s *Server) SendAttachmentToClient(clientID string, spec pluginapi.AttachmentSpec) (string, error) {
	if !s.pluginLiveAllowed() {
		return "", errors.New("server is read-only")
	}
	if !validPluginSender(spec.From) || !protocol.TransferFilenameRE.MatchString(spec.Filename) || !protocol.TransferMimeRE.MatchString(spec.ContentType) || len(spec.Data) == 0 || int64(len(spec.Data)) > s.cfg.TransferMaxBytes {
		return "", errors.New("invalid plugin attachment")
	}
	c := s.clientByID(clientID)
	if c == nil || !c.Registered || !c.ExtendedMode {
		return "", errors.New("target client is not available in extended mode")
	}
	conversation := spec.Conversation
	channelNorm := ""
	if strings.HasPrefix(conversation, "#") {
		if !protocol.ChannelRE.MatchString(conversation) || !s.ClientInChannel(clientID, conversation) {
			return "", errors.New("target client is not in attachment channel")
		}
		channelNorm = protocol.Norm(conversation)
		s.mu.RLock()
		if ch := s.channels[channelNorm]; ch != nil {
			conversation = ch.Name
		}
		s.mu.RUnlock()
	} else {
		if conversation == "" {
			conversation = c.Nick
		}
		if !protocol.NickRE.MatchString(conversation) || !strings.EqualFold(conversation, c.Nick) {
			return "", errors.New("invalid direct attachment conversation")
		}
	}
	id, err := newTransferID()
	if err != nil {
		return "", err
	}
	data := append([]byte(nil), spec.Data...)
	sum := sha256.Sum256(data)
	tr := &attachmentRecord{
		ID: id, SenderNick: spec.From, SenderLine: spec.From + "!plugin@" + s.cfg.ServerName,
		Conversation: conversation, ChannelNorm: channelNorm, Restricted: true,
		AllowedIDs: map[string]struct{}{c.ID: {}}, AllowedIdentities: make(map[string]struct{}),
		Filename: spec.Filename, ContentType: spec.ContentType, Size: int64(len(data)), Data: data,
		NextPart: transferParts(int64(len(data))), Parts: transferParts(int64(len(data))), Hash: hex.EncodeToString(sum[:]), Complete: true, CreatedAt: time.Now(),
	}
	if c.IdentityAuthedNick != "" {
		tr.AllowedIdentities[c.IdentityAuthedNick] = struct{}{}
	}
	s.transferMu.Lock()
	s.cleanupTransfersLocked(time.Now())
	if len(s.transfers) >= transferWindow {
		s.transferMu.Unlock()
		return "", errors.New("attachment window is full")
	}
	s.transfers[id] = tr
	s.transferMu.Unlock()
	s.notifyAttachment(tr, "")
	return id, nil
}
