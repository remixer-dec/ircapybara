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
}

type CaptchaState struct {
	Required bool
	Solved   bool
	ID       string
	Answer   int
}

type Client struct {
	Conn      net.Conn
	RemoteIP  net.IP
	Connected time.Time
	WriteMu   sync.Mutex

	Nick           string
	Username       string
	Realname       string
	Registered     bool
	CapNegotiating bool
	IsOper         bool

	ServerPassOK       bool
	IdentityAuthedNick string
	ExtendedMode       bool
	Captcha            CaptchaState

	LastActivity time.Time
	Tokens       float64
	TokenUpdated time.Time
}
