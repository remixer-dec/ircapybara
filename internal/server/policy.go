package server

import (
	"time"

	"ircapybara/internal/model"
	"ircapybara/internal/protocol"
)

// readOnlyCommandAllowed defines guard level 3's emergency read-only boundary.
// Connection lifecycle, identity authentication, read/query commands, and
// security/operator controls remain available. User-generated content and
// channel-state mutations are blocked.
func (s *Server) readOnlyCommandAllowed(cmd protocol.Command) bool {
	switch cmd.Name {
	case "CAP", "PASS", "IDENTIFY", "NICK", "USER", "PING", "PONG", "QUIT", "CAPTCHA",
		"JOIN", "PART", "NAMES", "WHO", "WHOIS", "LIST", "MOTD", "HISTORY", "TIME", "UPTIME",
		"OPER", "KILL", "GUARD", "BANADD", "BANDEL", "BANLIST", "XMODE":
		return true
	case "TOPIC":
		return protocol.SingleChannelRE.MatchString(cmd.Args)
	case "MODE":
		m := protocol.ModeArgsRE.FindStringSubmatch(cmd.Args)
		return m != nil && (m[2] == "" || (m[2] == "+b" && m[3] == ""))
	default:
		return false
	}
}

func (s *Server) guard3RetainsLocked(c *model.Client, cutoff time.Time) bool {
	if c.IsOper || !c.Connected.After(cutoff) {
		return true
	}
	if c.IdentityAuthedNick == "" || c.IdentityAuthedNick != protocol.Norm(c.Nick) {
		return false
	}
	cred, ok := s.identities[c.IdentityAuthedNick]
	return ok && !cred.CreatedAt.After(cutoff)
}

func (s *Server) canSeeChannelLocked(c *model.Client, ch *model.Channel) bool {
	if !ch.Private || c.IsOper {
		return true
	}
	if _, ok := ch.Members[c]; ok {
		return true
	}
	if _, ok := ch.SessionParticipants[c]; ok {
		return true
	}
	if c.IdentityAuthedNick != "" {
		if c.IdentityAuthedNick == ch.CreatorNorm {
			return true
		}
		if _, ok := ch.Participants[c.IdentityAuthedNick]; ok {
			return true
		}
	}
	return false
}

func (s *Server) canInspectChannelLocked(c *model.Client, ch *model.Channel) bool {
	if !ch.Private || c.IsOper {
		return true
	}
	_, ok := ch.Members[c]
	return ok
}

func (s *Server) canInspectChannel(c *model.Client, ch *model.Channel) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.canInspectChannelLocked(c, ch)
}

func (s *Server) canSeeChannel(c *model.Client, ch *model.Channel) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.canSeeChannelLocked(c, ch)
}
