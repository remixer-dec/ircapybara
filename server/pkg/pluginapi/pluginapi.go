package pluginapi

import (
	"errors"
	"time"
)

var (
	ErrDenied  = errors.New("plugin denied operation")
	ErrHandled = errors.New("plugin handled operation")
)

type ClientView struct {
	ID         string
	Nick       string
	Username   string
	Identity   string
	IP         string
	Registered bool
	IsOper     bool
	Extended   bool
	HistoryCap bool
}

type CommandEvent struct {
	Client  ClientView
	Command string
	Args    string
}

type MessageEvent struct {
	Client ClientView
	Target string
	Text   string
	Notice bool
}

type JoinEvent struct {
	Client  ClientView
	Channel string
}

type SyntheticHistoryItem struct {
	At   time.Time
	From string
	Text string
}

type AttachmentSpec struct {
	Conversation string
	From         string
	Filename     string
	ContentType  string
	Data         []byte
}

type ChannelSpec struct {
	Name           string
	Topic          string
	Private        bool
	InviteOnly     bool
	HistoryEnabled bool
	OwnerIdentity  string
}

// Host is deliberately narrow. Plugins can create RAM-only plugin channels and
// deliver validated synthetic IRC messages, but they do not get direct access
// to sockets, the database, bans, or server internals.
type Host interface {
	EnsurePluginChannel(ChannelSpec) error
	EnsureIdentityInChannel(identity, channel string) error
	ClientInChannel(clientID, channel string) bool
	SendNotice(clientID, text string) error
	SendNoticeToIdentity(identity, text string) error
	SendChannelMessage(channel, from, text string) error
	SendChannelMessageToClient(clientID, channel, from, text string) error
	SendChannelMessageToIdentity(channel, identity, from, text string) error
	SendDirectMessageToClient(clientID, from, text string) error
	SendDirectMessageToIdentity(identity, from, text string) error
	DeliverSyntheticHistory(clientID, channel string, items []SyntheticHistoryItem) error
	MaxAttachmentBytes() int64
	SendAttachmentToClient(clientID string, spec AttachmentSpec) (string, error)
	Clients() []ClientView
	UserCount() int
}

type Plugin interface {
	Name() string
	Init(Host) error
	OnConnect(ClientView)
	OnDisconnect(ClientView)
	BeforeCommand(CommandEvent) error
	BeforeMessage(MessageEvent) (MessageEvent, error)
	OnJoin(JoinEvent)
}

// HelpProvider is optional. Modules return short syntax/example lines for HELP <module>.
// Module names are matched case-insensitively by the server.
type HelpProvider interface {
	HelpModules() map[string][]string
}

// CommandHandler is optional. It exists for small plugin-owned commands such as
// BRIDGE without making the core dispatcher aware of provider-specific syntax.
type CommandHandler interface {
	HandleCommand(CommandEvent) bool
}
