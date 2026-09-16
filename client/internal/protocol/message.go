package protocol

import "strings"

type Prefix struct {
	Raw  string `json:"raw"`
	Name string `json:"name"`
	User string `json:"user,omitempty"`
	Host string `json:"host,omitempty"`
}

type Message struct {
	Raw     string            `json:"raw"`
	Tags    map[string]string `json:"tags,omitempty"`
	Prefix  *Prefix           `json:"prefix,omitempty"`
	Command string            `json:"command"`
	Params  []string          `json:"params,omitempty"`
}

func (m Message) Trailing() string {
	if len(m.Params) == 0 {
		return ""
	}
	return m.Params[len(m.Params)-1]
}

func (m Message) IsNumeric() bool {
	if len(m.Command) != 3 {
		return false
	}
	for _, r := range m.Command {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func ParsePrefix(raw string) Prefix {
	p := Prefix{Raw: raw, Name: raw}
	nickUser := raw
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		nickUser, p.Host = raw[:at], raw[at+1:]
	}
	if bang := strings.Index(nickUser, "!"); bang >= 0 {
		p.Name, p.User = nickUser[:bang], nickUser[bang+1:]
	} else {
		p.Name = nickUser
	}
	return p
}
