package server

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"ircapybara/internal/model"
	"ircapybara/internal/protocol"
	"ircapybara/pkg/pluginapi"
)

func validPluginSender(from string) bool {
	return protocol.NickRE.MatchString(from)
}

func (s *Server) EnsurePluginChannel(spec pluginapi.ChannelSpec) error {
	if !protocol.ChannelRE.MatchString(spec.Name) || !protocol.ValidText(spec.Topic, 180) {
		return errors.New("invalid plugin channel")
	}
	if spec.OwnerIdentity != "" && !protocol.NickRE.MatchString(spec.OwnerIdentity) {
		return errors.New("invalid plugin channel owner")
	}
	norm := protocol.Norm(spec.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch := s.channels[norm]; ch != nil {
		if spec.OwnerIdentity != "" && (!ch.PluginOwned || ch.OwnerIdentity != protocol.Norm(spec.OwnerIdentity)) {
			return errors.New("channel already exists with a different owner")
		}
		// A plugin may attach to an administrator-defined channel. It may disable
		// in-memory history, but it never rewrites that channel in SQLite.
		if !spec.HistoryEnabled {
			ch.HistoryEnabled = false
			ch.History = nil
		}
		return nil
	}
	s.channels[norm] = &model.Channel{
		Name: spec.Name, Norm: norm, Topic: spec.Topic, Private: spec.Private,
		HistoryEnabled: spec.HistoryEnabled, CreatorNorm: protocol.Norm(spec.OwnerIdentity),
		Modes:   model.ChannelModes{TopicOps: true, InviteOnly: spec.InviteOnly},
		Members: make(map[*model.Client]*model.Membership), Invited: make(map[*model.Client]bool),
		Participants: make(map[string]struct{}), SessionParticipants: make(map[*model.Client]struct{}),
		PluginOwned: true, OwnerIdentity: protocol.Norm(spec.OwnerIdentity),
	}
	return nil
}

func (s *Server) EnsureIdentityInChannel(identity, channel string) error {
	if !protocol.NickRE.MatchString(identity) || !protocol.ChannelRE.MatchString(channel) {
		return errors.New("invalid identity or channel")
	}
	identity = protocol.Norm(identity)
	norm := protocol.Norm(channel)
	var joins []struct {
		client *model.Client
		line   string
		topic  string
		ch     *model.Channel
	}
	s.mu.Lock()
	ch := s.channels[norm]
	if ch == nil || !ch.PluginOwned {
		s.mu.Unlock()
		return errors.New("plugin channel does not exist")
	}
	if ch.OwnerIdentity != "" && ch.OwnerIdentity != identity {
		s.mu.Unlock()
		return errors.New("identity does not own plugin channel")
	}
	for c := range s.clients {
		if c.Registered && c.IdentityAuthedNick == identity {
			if _, ok := ch.Members[c]; ok {
				continue
			}
			ch.Members[c] = &model.Membership{Op: ch.OwnerIdentity == identity}
			ch.SessionParticipants[c] = struct{}{}
			joins = append(joins, struct {
				client *model.Client
				line   string
				topic  string
				ch     *model.Channel
			}{c, fmt.Sprintf(":%s JOIN %s", s.prefix(c), ch.Name), ch.Topic, ch})
		}
	}
	s.mu.Unlock()
	for _, j := range joins {
		s.sendLine(j.client, j.line)
		if j.topic == "" {
			s.numeric(j.client, "331", j.ch.Name+" :No topic is set")
		} else {
			s.numeric(j.client, "332", j.ch.Name+" :"+j.topic)
		}
		s.sendNames(j.client, j.ch)
	}
	return nil
}

func (s *Server) ClientInChannel(clientID, channel string) bool {
	if !protocol.ChannelRE.MatchString(channel) {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ch := s.channels[protocol.Norm(channel)]
	if ch == nil {
		return false
	}
	for c := range s.clients {
		if c.ID == clientID {
			_, ok := ch.Members[c]
			return ok
		}
	}
	return false
}

func (s *Server) pluginLiveAllowed() bool { return s.protection.GuardLevel() != 3 }

func (s *Server) clientByID(id string) *model.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for c := range s.clients {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func (s *Server) clientsByIdentity(identity string) []*model.Client {
	identity = protocol.Norm(identity)
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*model.Client
	for c := range s.clients {
		if c.Registered && c.IdentityAuthedNick == identity {
			out = append(out, c)
		}
	}
	return out
}

func (s *Server) SendNotice(clientID, text string) error {
	if !protocol.ValidText(text, 400) {
		return errors.New("invalid plugin notice")
	}
	c := s.clientByID(clientID)
	if c == nil {
		return errors.New("client not connected")
	}
	s.notice(c, text)
	return nil
}

func (s *Server) SendNoticeToIdentity(identity, text string) error {
	if !protocol.NickRE.MatchString(identity) || !protocol.ValidText(text, 400) {
		return errors.New("invalid plugin notice")
	}
	for _, c := range s.clientsByIdentity(identity) {
		s.notice(c, text)
	}
	return nil
}

func (s *Server) pluginChannelLine(chName, from, text string) (string, error) {
	if !protocol.ChannelRE.MatchString(chName) || !validPluginSender(from) || !protocol.ValidText(text, 400) {
		return "", errors.New("invalid plugin message")
	}
	return fmt.Sprintf(":%s!plugin@%s PRIVMSG %s :%s", from, s.cfg.ServerName, chName, text), nil
}

func (s *Server) SendChannelMessage(channel, from, text string) error {
	if !s.pluginLiveAllowed() {
		return errors.New("server is read-only")
	}
	line, err := s.pluginChannelLine(channel, from, text)
	if err != nil {
		return err
	}
	s.mu.RLock()
	ch := s.channels[protocol.Norm(channel)]
	if ch == nil {
		s.mu.RUnlock()
		return errors.New("channel not found")
	}
	var recipients []*model.Client
	for c := range ch.Members {
		recipients = append(recipients, c)
	}
	s.mu.RUnlock()
	for _, c := range recipients {
		s.sendLine(c, line)
	}
	return nil
}

func (s *Server) SendChannelMessageToClient(clientID, channel, from, text string) error {
	if !s.pluginLiveAllowed() {
		return errors.New("server is read-only")
	}
	line, err := s.pluginChannelLine(channel, from, text)
	if err != nil {
		return err
	}
	c := s.clientByID(clientID)
	if c == nil {
		return errors.New("client not connected")
	}
	s.sendLine(c, line)
	return nil
}

func (s *Server) SendChannelMessageToIdentity(channel, identity, from, text string) error {
	if !s.pluginLiveAllowed() {
		return errors.New("server is read-only")
	}
	line, err := s.pluginChannelLine(channel, from, text)
	if err != nil || !protocol.NickRE.MatchString(identity) {
		if err != nil {
			return err
		}
		return errors.New("invalid identity")
	}
	for _, c := range s.clientsByIdentity(identity) {
		s.sendLine(c, line)
	}
	return nil
}

func (s *Server) directLine(targetNick, from, text string) (string, error) {
	if !protocol.NickRE.MatchString(targetNick) || !validPluginSender(from) || !protocol.ValidText(text, 400) {
		return "", errors.New("invalid plugin direct message")
	}
	return fmt.Sprintf(":%s!plugin@%s PRIVMSG %s :%s", from, s.cfg.ServerName, targetNick, text), nil
}

func (s *Server) SendDirectMessageToClient(clientID, from, text string) error {
	if !s.pluginLiveAllowed() {
		return errors.New("server is read-only")
	}
	c := s.clientByID(clientID)
	if c == nil || !c.Registered {
		return errors.New("client not connected")
	}
	line, err := s.directLine(c.Nick, from, text)
	if err != nil {
		return err
	}
	s.sendLine(c, line)
	return nil
}

func (s *Server) SendDirectMessageToIdentity(identity, from, text string) error {
	if !s.pluginLiveAllowed() {
		return errors.New("server is read-only")
	}
	if !protocol.NickRE.MatchString(identity) {
		return errors.New("invalid identity")
	}
	for _, c := range s.clientsByIdentity(identity) {
		line, err := s.directLine(c.Nick, from, text)
		if err != nil {
			return err
		}
		s.sendLine(c, line)
	}
	return nil
}

func (s *Server) DeliverSyntheticHistory(clientID, channel string, items []pluginapi.SyntheticHistoryItem) error {
	if !protocol.ChannelRE.MatchString(channel) || len(items) > 100 {
		return errors.New("invalid synthetic history")
	}
	c := s.clientByID(clientID)
	if c == nil || !c.Registered || !c.Capabilities["ircapybara/history"] {
		return errors.New("client does not support synthetic history")
	}
	for _, item := range items {
		if !validPluginSender(item.From) || !protocol.ValidText(item.Text, 400) {
			return errors.New("invalid synthetic history item")
		}
		tags := "@ircapybara-history=1"
		if c.Capabilities["server-time"] {
			t := item.At
			if t.IsZero() {
				t = time.Now()
			}
			tags += ";time=" + t.UTC().Format(time.RFC3339Nano)
		}
		line := fmt.Sprintf("%s :%s!plugin@%s PRIVMSG %s :%s", tags, item.From, s.cfg.ServerName, channel, item.Text)
		s.sendLine(c, line)
	}
	return nil
}

func (s *Server) Clients() []pluginapi.ClientView {
	s.mu.RLock()
	out := make([]pluginapi.ClientView, 0, len(s.clients))
	for c := range s.clients {
		out = append(out, s.clientView(c))
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Nick) < strings.ToLower(out[j].Nick) })
	return out
}

func (s *Server) UserCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for c := range s.clients {
		if c.Registered {
			n++
		}
	}
	return n
}

var _ pluginapi.Host = (*Server)(nil)
