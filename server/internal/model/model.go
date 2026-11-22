package model

import (
	"net"
	"sync"
	"time"
)

type Credential struct {
	NickDisplay string
	Salt        []byte
	Hash        []byte
	CreatedAt   time.Time
}

type Ban struct {
	ID          int64
	Scope       string
	ChannelNorm string
	Kind        string
	Value       string
}

type ChannelModes struct {
	InviteOnly bool
	Moderated  bool
	TopicOps   bool
	Key        string
	Limit      int
}

type Membership struct {
	Op bool
}

type HistoryMessage struct {
	At       time.Time
	Nick     string
	Username string
	Host     string
	Text     string
	Notice   bool
}

type Channel struct {
	Name           string
	Norm           string
	Topic          string
	Private        bool
	HistoryEnabled bool
	CreatorNorm    string
	Modes          ChannelModes
	Members        map[*Client]*Membership
	Invited        map[*Client]bool
	ChannelBans    []Ban

	// Participants stores durable registered identities that have previously
	// joined this channel. SessionParticipants provides the same visibility for
	// unregistered users only for the lifetime of their current connection.
	Participants        map[string]struct{}
	SessionParticipants map[*Client]struct{}
	History             []HistoryMessage

	// PluginOwned channels exist only in RAM. OwnerIdentity, when set, may join
	// an invite-only plugin channel without an explicit INVITE.
	PluginOwned   bool
	OwnerIdentity string
}

type CaptchaState struct {
	Required bool
	Solved   bool
	ID       string
	Answer   int
}

type Client struct {
	ID        string
	Conn      net.Conn
	RemoteIP  net.IP
	Connected time.Time
	WriteMu   sync.Mutex

	Nick           string
	Username       string
	Realname       string
	Registered     bool
	CapNegotiating bool
	CapMu          sync.RWMutex
	Capabilities   map[string]bool
	IsOper         bool

	ServerPassOK       bool
	IdentityAuthedNick string
	IdentityReadOnly   bool
	ExtendedMode       bool
	Captcha            CaptchaState

	LastActivity     time.Time
	Tokens           float64
	TokenUpdated     time.Time
	JoinTimes        []time.Time
	XferTokens       float64
	XferTokenUpdated time.Time
}

func (c *Client) HasCaps(names ...string) bool {
	c.CapMu.RLock()
	defer c.CapMu.RUnlock()
	for _, name := range names {
		if !c.Capabilities[name] {
			return false
		}
	}
	return true
}
