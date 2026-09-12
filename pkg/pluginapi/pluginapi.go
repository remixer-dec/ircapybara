package pluginapi

import "errors"

var ErrDenied = errors.New("plugin denied operation")

type ClientView struct {
	Nick     string
	Username string
	IP       string
	IsOper   bool
	Extended bool
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

type Plugin interface {
	Name() string
	OnConnect(ClientView)
	OnDisconnect(ClientView)
	BeforeCommand(CommandEvent) error
	BeforeMessage(MessageEvent) (MessageEvent, error)
	OnJoin(JoinEvent)
}
