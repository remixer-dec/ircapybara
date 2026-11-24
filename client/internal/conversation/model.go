package conversation

import (
	"sort"
	"strings"
	"sync"
	"time"
)

type Kind string

const (
	PublicChannel  Kind = "channel"
	PrivateChannel Kind = "private-channel"
	DM             Kind = "dm"
	VirtualDM      Kind = "virtual-dm"
	PluginChannel  Kind = "plugin-channel"
	Status         Kind = "status"
)

type Attachment struct {
	FileID   string `json:"fileId"`
	Filename string `json:"filename"`
	MIME     string `json:"mime"`
	Size     int64  `json:"size"`
	Status   string `json:"status,omitempty"`
}

type Message struct {
	ID         string      `json:"id"`
	Target     string      `json:"target"`
	Sender     string      `json:"sender"`
	Text       string      `json:"text"`
	Time       time.Time   `json:"time"`
	Historical bool        `json:"historical,omitempty"`
	DedupeKey  string      `json:"-"`
	Synthetic  bool        `json:"synthetic,omitempty"`
	Attachment *Attachment `json:"attachment,omitempty"`
	System     bool        `json:"system,omitempty"`
}

type Member struct {
	Nick     string `json:"nick"`
	Operator bool   `json:"operator,omitempty"`
	Online   bool   `json:"online"`
}

type Conversation struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Kind        Kind      `json:"kind"`
	Topic       string    `json:"topic,omitempty"`
	Unread      int       `json:"unread"`
	MemberCount int       `json:"memberCount,omitempty"`
	OnlineCount int       `json:"onlineCount,omitempty"`
	Members     []Member  `json:"members,omitempty"`
	Joined      bool      `json:"joined,omitempty"`
	Catalogued  bool      `json:"catalogued,omitempty"`
	Pinned      bool      `json:"pinned,omitempty"`
	Messages    []Message `json:"messages"`
}

type Store struct {
	mu          sync.RWMutex
	items       []Conversation
	maxMessages int
}

func NewStore(maxMessages int) *Store {
	if maxMessages <= 0 {
		maxMessages = 300
	}
	s := &Store{maxMessages: maxMessages}
	s.Reset("")
	return s
}

// Reset keeps Home as a real conversation, but does not manufacture a chat
// message for onboarding copy. Empty-state UI is the right place for that.
func (s *Store) Reset(_ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = nil
	s.ensureLocked("status", "Home", Status)
}

func (s *Store) Snapshot() []Conversation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]Conversation(nil), s.items...)
	for i := range out {
		out[i].Messages = append([]Message(nil), s.items[i].Messages...)
		out[i].Members = append([]Member(nil), s.items[i].Members...)
	}
	return out
}

func (s *Store) Get(id string) (Conversation, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.items {
		if strings.EqualFold(s.items[i].ID, id) {
			out := s.items[i]
			out.Messages = append([]Message(nil), s.items[i].Messages...)
			out.Members = append([]Member(nil), s.items[i].Members...)
			return out, true
		}
	}
	return Conversation{}, false
}

func (s *Store) Ensure(id, name string, kind Kind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLocked(id, name, kind)
}

func (s *Store) EnsureCatalogued(id, name string, kind Kind, members int, topic string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.ensureLocked(id, name, kind)
	c.Catalogued = true
	if members >= 0 {
		c.MemberCount = members
	}
	if topic != "" {
		c.Topic = topic
	}
}

func (s *Store) ensureLocked(id, name string, kind Kind) *Conversation {
	for i := range s.items {
		if strings.EqualFold(s.items[i].ID, id) {
			if name != "" {
				s.items[i].Name = name
			}
			return &s.items[i]
		}
	}
	s.items = append(s.items, Conversation{ID: id, Name: name, Kind: kind, Messages: []Message{}})
	return &s.items[len(s.items)-1]
}

func (s *Store) AddMessage(convID, name string, kind Kind, m Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.ensureLocked(convID, name, kind)
	appendBounded(c, m, s.maxMessages)
}

// AddHistoricalMessage merges a replay into the existing conversation. History
// is usually older than the live messages already displayed, so it must be
// inserted into the timeline rather than appended to it. Replays have no
// stable IDs in the core protocol, therefore the message identity fallback is
// deliberately used while importing history, but never when adding live
// traffic.
func (s *Store) AddHistoricalMessage(convID, name string, kind Kind, m Message) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.ensureLocked(convID, name, kind)
	for i := range c.Messages {
		if c.Messages[i].System {
			continue
		}
		if sameHistoricalMessage(c.Messages[i], m) {
			return false
		}
	}
	m.Historical = true
	insertHistorical(c, m, s.maxMessages)
	return true
}

func sameHistoricalMessage(a, b Message) bool {
	if a.DedupeKey != "" && b.DedupeKey != "" {
		return a.DedupeKey == b.DedupeKey
	}
	if !strings.EqualFold(a.Target, b.Target) || !strings.EqualFold(a.Sender, b.Sender) || a.Text != b.Text {
		return false
	}
	if (a.Attachment == nil) != (b.Attachment == nil) {
		return false
	}
	if a.Attachment != nil && b.Attachment != nil {
		return a.Attachment.FileID == b.Attachment.FileID && a.Attachment.Filename == b.Attachment.Filename
	}
	return true
}

func insertHistorical(c *Conversation, m Message, max int) {
	index := len(c.Messages)
	if m.DedupeKey != "" && !m.Time.IsZero() {
		// Tagged history has a server-provided timestamp. Use it to merge into
		// the complete timeline, including any previously imported history.
		for i, existing := range c.Messages {
			if !existing.Time.IsZero() && existing.Time.After(m.Time) {
				index = i
				break
			}
		}
	} else {
		// Legacy core HISTORY has no timestamp. Its replay is ordered oldest
		// first, so keep it ahead of live messages already in the store.
		for i, existing := range c.Messages {
			if !existing.Historical {
				index = i
				break
			}
		}
	}

	c.Messages = append(c.Messages, Message{})
	copy(c.Messages[index+1:], c.Messages[index:])
	c.Messages[index] = m
	if len(c.Messages) > max {
		c.Messages = append([]Message(nil), c.Messages[len(c.Messages)-max:]...)
	}
}

func appendBounded(c *Conversation, m Message, max int) {
	c.Messages = append(c.Messages, m)
	if len(c.Messages) > max {
		c.Messages = append([]Message(nil), c.Messages[len(c.Messages)-max:]...)
	}
}

func (s *Store) SetKind(id string, kind Kind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.ensureLocked(id, strings.TrimPrefix(id, "#"), kind)
	c.Kind = kind
}

func (s *Store) SetTopic(id, topic string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.ensureLocked(id, strings.TrimPrefix(id, "#"), PublicChannel)
	c.Topic = topic
}

func (s *Store) SetMembers(id string, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.ensureLocked(id, strings.TrimPrefix(id, "#"), PublicChannel)
	c.MemberCount = total
}

func (s *Store) SetOnlineMembers(id string, members []Member) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.ensureLocked(id, strings.TrimPrefix(id, "#"), PublicChannel)
	seen := make(map[string]struct{}, len(members))
	normalized := make([]Member, 0, len(members))
	for _, member := range members {
		member.Nick = strings.TrimSpace(member.Nick)
		if member.Nick == "" {
			continue
		}
		key := strings.ToLower(member.Nick)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		member.Online = true
		normalized = append(normalized, member)
	}
	sort.SliceStable(normalized, func(i, j int) bool {
		if normalized[i].Operator != normalized[j].Operator {
			return normalized[i].Operator
		}
		return strings.ToLower(normalized[i].Nick) < strings.ToLower(normalized[j].Nick)
	})
	c.Members = normalized
	c.OnlineCount = len(normalized)
	if c.MemberCount < c.OnlineCount {
		c.MemberCount = c.OnlineCount
	}
}

func (s *Store) UpdateMemberPresence(id, nick string, online bool, operator bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.ensureLocked(id, strings.TrimPrefix(id, "#"), PublicChannel)
	key := strings.ToLower(strings.TrimSpace(nick))
	if key == "" {
		return
	}
	for i := range c.Members {
		if strings.ToLower(c.Members[i].Nick) == key {
			if !online {
				c.Members = append(c.Members[:i], c.Members[i+1:]...)
				c.OnlineCount = len(c.Members)
				return
			}
			c.Members[i].Online = true
			c.Members[i].Operator = c.Members[i].Operator || operator
			c.OnlineCount = len(c.Members)
			return
		}
	}
	if online {
		c.Members = append(c.Members, Member{Nick: nick, Online: true, Operator: operator})
		c.OnlineCount = len(c.Members)
		if c.MemberCount < c.OnlineCount {
			c.MemberCount = c.OnlineCount
		}
	}
}

func (s *Store) RemoveMemberEverywhere(nick string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(strings.TrimSpace(nick))
	if key == "" {
		return
	}
	for ci := range s.items {
		c := &s.items[ci]
		for i := 0; i < len(c.Members); i++ {
			if strings.ToLower(c.Members[i].Nick) == key {
				c.Members = append(c.Members[:i], c.Members[i+1:]...)
				i--
			}
		}
		c.OnlineCount = len(c.Members)
	}
}

func (s *Store) RenameMember(oldNick, newNick string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	oldKey := strings.ToLower(strings.TrimSpace(oldNick))
	newNick = strings.TrimSpace(newNick)
	if oldKey == "" || newNick == "" {
		return
	}
	for ci := range s.items {
		for i := range s.items[ci].Members {
			if strings.ToLower(s.items[ci].Members[i].Nick) == oldKey {
				s.items[ci].Members[i].Nick = newNick
			}
		}
	}
}

func (s *Store) MarkDisconnected() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		s.items[i].Joined = false
		s.items[i].Members = nil
		s.items[i].OnlineCount = 0
	}
}

func (s *Store) SetPinned(id string, pinned bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if strings.EqualFold(s.items[i].ID, id) {
			s.items[i].Pinned = pinned
		}
	}
}

func (s *Store) SetJoined(id string, joined bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.ensureLocked(id, strings.TrimPrefix(id, "#"), PublicChannel)
	c.Joined = joined
}

func (s *Store) Remove(id string) bool {
	if strings.EqualFold(id, "status") {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if strings.EqualFold(s.items[i].ID, id) {
			s.items = append(s.items[:i], s.items[i+1:]...)
			return true
		}
	}
	return false
}
