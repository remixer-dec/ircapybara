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

type Channel struct {
	Name        string
	Norm        string
	Topic       string
	Modes       ChannelModes
	Members     map[*Client]*Membership
	Invited     map[*Client]bool
	ChannelBans []Ban
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

	Nick       string
	Username   string
	Realname   string
	Registered bool
	IsOper     bool

	ServerPassOK       bool
	IdentityAuthedNick string
	ExtendedMode       bool
	Captcha            CaptchaState

	LastActivity time.Time
	Tokens       float64
	TokenUpdated time.Time
}
