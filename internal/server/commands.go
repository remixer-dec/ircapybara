package server

import (
	"fmt"
	"log"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"ircapybara/internal/model"
	"ircapybara/internal/protection"
	"ircapybara/internal/protocol"
	"ircapybara/internal/security"
	"ircapybara/pkg/pluginapi"
)

var (
	pingArgsRE = regexp.MustCompile(`^:?([A-Za-z0-9._:/-]{1,128})$`)
	quitArgsRE = regexp.MustCompile(`^:?(.{0,160})$`)
	capArgsRE  = regexp.MustCompile(`^(?:LS(?: 302)?|END)$`)
)

func (s *Server) dispatch(c *model.Client, cmd protocol.Command) (bool, string) {
	switch cmd.Name {
	case "CAP":
		return s.cmdCAP(c, cmd.Args)
	case "PASS":
		return s.cmdPASS(c, cmd.Args)
	case "IDENTIFY":
		return s.cmdIDENTIFY(c, cmd.Args)
	case "NICK":
		return s.cmdNICK(c, cmd.Args)
	case "USER":
		return s.cmdUSER(c, cmd.Args)
	case "PING":
		return s.cmdPING(c, cmd.Args)
	case "PONG":
		return s.cmdPONG(c, cmd.Args)
	case "QUIT":
		return s.cmdQUIT(c, cmd.Args)
	case "CAPTCHA":
		return s.cmdCAPTCHA(c, cmd.Args)
	case "REGISTER":
		return s.cmdREGISTER(c, cmd.Args)
	case "JOIN":
		return s.cmdJOIN(c, cmd.Args)
	case "PART":
		return s.cmdPART(c, cmd.Args)
	case "PRIVMSG":
		return s.cmdMessage(c, cmd.Args, false)
	case "NOTICE":
		return s.cmdMessage(c, cmd.Args, true)
	case "TOPIC":
		return s.cmdTOPIC(c, cmd.Args)
	case "NAMES":
		return s.cmdNAMES(c, cmd.Args)
	case "WHO":
		return s.cmdWHO(c, cmd.Args)
	case "WHOIS":
		return s.cmdWHOIS(c, cmd.Args)
	case "LIST":
		return s.cmdLIST(c, cmd.Args)
	case "MOTD":
		return s.cmdMOTD(c, cmd.Args)
	case "MODE":
		return s.cmdMODE(c, cmd.Args)
	case "KICK":
		return s.cmdKICK(c, cmd.Args)
	case "INVITE":
		return s.cmdINVITE(c, cmd.Args)
	case "OPER":
		return s.cmdOPER(c, cmd.Args)
	case "KILL":
		return s.cmdKILL(c, cmd.Args)
	case "CHANADD":
		return s.cmdCHANADD(c, cmd.Args)
	case "GUARD":
		return s.cmdGUARD(c, cmd.Args)
	case "BANADD":
		return s.cmdBANADD(c, cmd.Args)
	case "BANDEL":
		return s.cmdBANDEL(c, cmd.Args)
	case "BANLIST":
		return s.cmdBANLIST(c, cmd.Args)
	case "XMODE":
		return s.cmdXMODE(c, cmd.Args)
	default:
		s.numeric(c, "421", cmd.Name+" :Unknown command")
		return false, ""
	}
}

func (s *Server) requireRegistered(c *model.Client) bool {
	if c.Registered {
		return true
	}
	s.numeric(c, "451", ":You have not registered")
	return false
}

func (s *Server) requireOper(c *model.Client) bool {
	if !s.requireRegistered(c) {
		return false
	}
	if c.IsOper {
		return true
	}
	s.numeric(c, "481", ":Permission denied - IRC operator required")
	return false
}

func (s *Server) badParams(c *model.Client, name string) {
	s.numeric(c, "461", name+" :Invalid parameters")
}

func (s *Server) cmdCAP(c *model.Client, args string) (bool, string) {
	if !capArgsRE.MatchString(args) {
		s.badParams(c, "CAP")
		return false, ""
	}
	if strings.HasPrefix(args, "LS") {
		s.sendLine(c, ":"+s.cfg.ServerName+" CAP * LS :")
	}
	return false, ""
}

func (s *Server) cmdPASS(c *model.Client, args string) (bool, string) {
	if c.Registered {
		s.numeric(c, "462", ":You may not reregister")
		return false, ""
	}
	if !protocol.PassRE.MatchString(args) {
		s.badParams(c, "PASS")
		return false, ""
	}
	if s.serverPassCred == nil {
		c.ServerPassOK = true
		return false, ""
	}
	if !security.VerifyPassword(args, s.serverPassCred.Salt, s.serverPassCred.Hash) {
		s.numeric(c, "464", ":Password incorrect")
		return false, ""
	}
	c.ServerPassOK = true
	return s.maybeRegister(c)
}

func (s *Server) cmdIDENTIFY(c *model.Client, args string) (bool, string) {
	m := protocol.IdentifyArgsRE.FindStringSubmatch(args)
	if m == nil {
		s.badParams(c, "IDENTIFY")
		return false, ""
	}
	norm := protocol.Norm(m[1])
	cred, ok := s.identity(norm)
	if !ok || !security.VerifyPassword(m[2], cred.Salt, cred.Hash) {
		s.numeric(c, "464", ":Identity password incorrect")
		return false, ""
	}
	c.IdentityAuthedNick = norm
	s.notice(c, "Identity authenticated for "+cred.NickDisplay)
	return false, ""
}

func (s *Server) cmdNICK(c *model.Client, args string) (bool, string) {
	if !protocol.NickRE.MatchString(args) {
		s.numeric(c, "432", args+" :Erroneous nickname")
		return false, ""
	}
	if !c.ServerPassOK {
		s.numeric(c, "464", ":Server password required first")
		return false, ""
	}
	norm := protocol.Norm(args)
	if _, protected := s.identity(norm); protected && c.IdentityAuthedNick != norm {
		s.numeric(c, "464", args+" :Nickname is registered; use IDENTIFY first")
		return false, ""
	}

	oldPrefix := s.prefix(c)
	oldNick := c.Nick
	recipients := make(map[*model.Client]struct{})
	s.mu.Lock()
	if s.nickInUseLocked(args, c) {
		s.mu.Unlock()
		s.numeric(c, "433", args+" :Nickname is already in use")
		return false, ""
	}
	if c.Registered && oldNick != "" {
		for _, ch := range s.channels {
			if _, ok := ch.Members[c]; ok {
				for member := range ch.Members {
					recipients[member] = struct{}{}
				}
			}
		}
	}
	c.Nick = args
	s.mu.Unlock()
	if c.Registered && oldNick != "" && oldNick != args {
		line := fmt.Sprintf(":%s NICK :%s", oldPrefix, args)
		for member := range recipients {
			s.sendLine(member, line)
		}
	}
	return s.maybeRegister(c)
}

func (s *Server) cmdUSER(c *model.Client, args string) (bool, string) {
	if c.Registered {
		s.numeric(c, "462", ":You may not reregister")
		return false, ""
	}
	m := protocol.UserArgsRE.FindStringSubmatch(args)
	if m == nil || !protocol.UsernameRE.MatchString(m[1]) || !protocol.ValidText(m[2], 80) {
		s.badParams(c, "USER")
		return false, ""
	}
	c.Username, c.Realname = m[1], m[2]
	if s.protection.ServerBanned(c.RemoteIP, c.Username) {
		s.sendError(c, "Banned from server")
		return true, "Server ban"
	}
	if !s.protection.GuardAllowsIdentity(c.RemoteIP, c.Username) {
		s.sendError(c, "Server guard mode")
		return true, "Guard mode"
	}
	return s.maybeRegister(c)
}

func (s *Server) cmdPING(c *model.Client, args string) (bool, string) {
	m := pingArgsRE.FindStringSubmatch(args)
	if m == nil {
		s.badParams(c, "PING")
		return false, ""
	}
	s.sendLine(c, fmt.Sprintf(":%s PONG %s :%s", s.cfg.ServerName, s.cfg.ServerName, m[1]))
	return false, ""
}

func (s *Server) cmdPONG(c *model.Client, args string) (bool, string) {
	if pingArgsRE.FindStringSubmatch(args) == nil {
		s.badParams(c, "PONG")
	}
	return false, ""
}

func (s *Server) cmdQUIT(c *model.Client, args string) (bool, string) {
	reason := "Client Quit"
	if args != "" {
		m := quitArgsRE.FindStringSubmatch(args)
		if m == nil || !protocol.ValidText(m[1], 160) {
			s.badParams(c, "QUIT")
			return false, ""
		}
		if m[1] != "" {
			reason = m[1]
		}
	}
	return true, reason
}

func (s *Server) cmdCAPTCHA(c *model.Client, args string) (bool, string) {
	m := protocol.CaptchaRE.FindStringSubmatch(args)
	if m == nil {
		return false, ""
	}
	if !c.Captcha.Required || c.Captcha.Solved {
		return false, ""
	}
	answer, _ := strconv.Atoi(m[2])
	if m[1] != c.Captcha.ID || answer != c.Captcha.Answer {
		s.notice(c, "CAPTCHA incorrect")
		return false, ""
	}
	c.Captcha.Solved = true
	s.notice(c, "CAPTCHA solved")
	return false, ""
}

func (s *Server) cmdREGISTER(c *model.Client, args string) (bool, string) {
	if !s.requireRegistered(c) {
		return false, ""
	}
	if !protocol.PasswordRE.MatchString(args) {
		s.badParams(c, "REGISTER")
		return false, ""
	}
	norm := protocol.Norm(c.Nick)
	s.mu.RLock()
	_, exists := s.identities[norm]
	s.mu.RUnlock()
	if exists {
		s.notice(c, "Nickname is already registered")
		return false, ""
	}
	salt, hash, err := security.NewPasswordHash(args)
	if err != nil {
		s.notice(c, "Registration failed")
		return false, ""
	}
	if err := s.store.RegisterIdentity(c.Nick, salt, hash); err != nil {
		log.Printf("register %s: %v", c.Nick, err)
		s.notice(c, "Registration failed")
		return false, ""
	}
	s.mu.Lock()
	s.identities[norm] = model.Credential{NickDisplay: c.Nick, Salt: salt, Hash: hash}
	s.mu.Unlock()
	c.IdentityAuthedNick = norm
	s.notice(c, "Nickname registered; use IDENTIFY <nick> <password> before claiming it on future connections")
	return false, ""
}

func (s *Server) cmdJOIN(c *model.Client, args string) (bool, string) {
	if !s.requireRegistered(c) {
		return false, ""
	}
	m := protocol.JoinArgsRE.FindStringSubmatch(args)
	if m == nil {
		s.badParams(c, "JOIN")
		return false, ""
	}
	name, key := m[1], m[2]
	norm := protocol.Norm(name)

	s.mu.Lock()
	ch, ok := s.channels[norm]
	if !ok {
		s.mu.Unlock()
		s.notice(c, "Channel "+name+" does not exist")
		return false, ""
	}
	if _, member := ch.Members[c]; member {
		s.mu.Unlock()
		return false, ""
	}
	if s.protection.ChannelBanned(norm, c.RemoteIP, c.Username) {
		s.mu.Unlock()
		s.numeric(c, "474", name+" :Cannot join channel (+b)")
		return false, ""
	}
	if ch.Modes.InviteOnly && !ch.Invited[c] && !c.IsOper {
		s.mu.Unlock()
		s.numeric(c, "473", name+" :Cannot join channel (+i)")
		return false, ""
	}
	if ch.Modes.Key != "" && ch.Modes.Key != key && !c.IsOper {
		s.mu.Unlock()
		s.numeric(c, "475", name+" :Cannot join channel (+k)")
		return false, ""
	}
	if ch.Modes.Limit > 0 && len(ch.Members) >= ch.Modes.Limit && !c.IsOper {
		s.mu.Unlock()
		s.numeric(c, "471", name+" :Cannot join channel (+l)")
		return false, ""
	}
	ch.Members[c] = &model.Membership{}
	delete(ch.Invited, c)
	recipients := make([]*model.Client, 0, len(ch.Members))
	for member := range ch.Members {
		recipients = append(recipients, member)
	}
	topic := ch.Topic
	s.mu.Unlock()

	line := fmt.Sprintf(":%s JOIN %s", s.prefix(c), ch.Name)
	for _, member := range recipients {
		s.sendLine(member, line)
	}
	if topic == "" {
		s.numeric(c, "331", ch.Name+" :No topic is set")
	} else {
		s.numeric(c, "332", ch.Name+" :"+topic)
	}
	s.sendNames(c, ch)
	if c.ExtendedMode {
		s.notice(c, "IRCapybara extended mode active; rendering format v0 is passthrough")
	} else {
		s.notice(c, "IRCapybara extension available: XMODE EXTENDED (rendering format reserved)")
	}
	for _, p := range s.plugins {
		p.OnJoin(pluginapi.JoinEvent{Client: s.clientView(c), Channel: ch.Name})
	}
	return false, ""
}

func (s *Server) cmdPART(c *model.Client, args string) (bool, string) {
	if !s.requireRegistered(c) {
		return false, ""
	}
	m := protocol.PartArgsRE.FindStringSubmatch(args)
	if m == nil || !protocol.ValidText(m[2], 160) {
		s.badParams(c, "PART")
		return false, ""
	}
	norm := protocol.Norm(m[1])
	s.mu.Lock()
	ch, ok := s.channels[norm]
	if !ok {
		s.mu.Unlock()
		s.numeric(c, "403", m[1]+" :No such channel")
		return false, ""
	}
	if _, ok := ch.Members[c]; !ok {
		s.mu.Unlock()
		s.numeric(c, "442", ch.Name+" :You're not on that channel")
		return false, ""
	}
	recipients := make([]*model.Client, 0, len(ch.Members))
	for member := range ch.Members {
		recipients = append(recipients, member)
	}
	delete(ch.Members, c)
	reason := m[2]
	s.mu.Unlock()
	line := fmt.Sprintf(":%s PART %s", s.prefix(c), ch.Name)
	if reason != "" {
		line += " :" + reason
	}
	for _, member := range recipients {
		s.sendLine(member, line)
	}
	return false, ""
}

func (s *Server) cmdMessage(c *model.Client, args string, notice bool) (bool, string) {
	if !c.Registered {
		if !notice {
			s.numeric(c, "451", ":You have not registered")
		}
		return false, ""
	}
	m := protocol.MessageArgsRE.FindStringSubmatch(args)
	if m == nil || !protocol.ValidText(maybe(m, 2), 400) {
		if !notice {
			s.badParams(c, "PRIVMSG")
		}
		return false, ""
	}
	event := pluginapi.MessageEvent{Client: s.clientView(c), Target: m[1], Text: m[2], Notice: notice}
	for _, p := range s.plugins {
		next, err := p.BeforeMessage(event)
		if err != nil {
			return false, ""
		}
		event = next
	}
	if !(protocol.ChannelRE.MatchString(event.Target) || protocol.NickRE.MatchString(event.Target)) || !protocol.ValidText(event.Text, 400) {
		return false, ""
	}
	verb := "PRIVMSG"
	if notice {
		verb = "NOTICE"
	}
	line := fmt.Sprintf(":%s %s %s :%s", s.prefix(c), verb, event.Target, event.Text)

	if strings.HasPrefix(event.Target, "#") {
		norm := protocol.Norm(event.Target)
		s.mu.RLock()
		ch, ok := s.channels[norm]
		if !ok {
			s.mu.RUnlock()
			if !notice {
				s.numeric(c, "403", event.Target+" :No such channel")
			}
			return false, ""
		}
		membership, joined := ch.Members[c]
		if !joined {
			s.mu.RUnlock()
			if !notice {
				s.numeric(c, "404", ch.Name+" :Cannot send to channel; join first")
			}
			return false, ""
		}
		if ch.Modes.Moderated && !membership.Op && !c.IsOper {
			s.mu.RUnlock()
			if !notice {
				s.numeric(c, "404", ch.Name+" :Cannot send to channel (+m)")
			}
			return false, ""
		}
		recipients := make([]*model.Client, 0, len(ch.Members))
		for member := range ch.Members {
			if member != c {
				recipients = append(recipients, member)
			}
		}
		s.mu.RUnlock()
		for _, member := range recipients {
			s.sendLine(member, line)
		}
		return false, ""
	}
	target := s.findClientByNick(event.Target)
	if target == nil || !target.Registered {
		if !notice {
			s.numeric(c, "401", event.Target+" :No such nick")
		}
		return false, ""
	}
	s.sendLine(target, line)
	return false, ""
}

func maybe(m []string, i int) string {
	if i >= 0 && i < len(m) {
		return m[i]
	}
	return ""
}

func (s *Server) cmdTOPIC(c *model.Client, args string) (bool, string) {
	if !s.requireRegistered(c) {
		return false, ""
	}
	m := protocol.TopicArgsRE.FindStringSubmatch(args)
	if m == nil || !protocol.ValidText(maybe(m, 2), 180) {
		s.badParams(c, "TOPIC")
		return false, ""
	}
	norm := protocol.Norm(m[1])
	s.mu.Lock()
	ch, ok := s.channels[norm]
	if !ok {
		s.mu.Unlock()
		s.numeric(c, "403", m[1]+" :No such channel")
		return false, ""
	}
	membership, joined := ch.Members[c]
	if len(m) < 3 || m[2] == "" && !strings.Contains(args, " :") {
		topic := ch.Topic
		s.mu.Unlock()
		if topic == "" {
			s.numeric(c, "331", ch.Name+" :No topic is set")
		} else {
			s.numeric(c, "332", ch.Name+" :"+topic)
		}
		return false, ""
	}
	if !joined {
		s.mu.Unlock()
		s.numeric(c, "442", ch.Name+" :You're not on that channel")
		return false, ""
	}
	if ch.Modes.TopicOps && !membership.Op && !c.IsOper {
		s.mu.Unlock()
		s.numeric(c, "482", ch.Name+" :You're not channel operator")
		return false, ""
	}
	newTopic := m[2]
	if err := s.store.SetTopic(norm, newTopic); err != nil {
		s.mu.Unlock()
		log.Printf("set topic %s: %v", ch.Name, err)
		s.numeric(c, "400", ":Storage error")
		return false, ""
	}
	ch.Topic = newTopic
	recipients := make([]*model.Client, 0, len(ch.Members))
	for member := range ch.Members {
		recipients = append(recipients, member)
	}
	s.mu.Unlock()
	line := fmt.Sprintf(":%s TOPIC %s :%s", s.prefix(c), ch.Name, newTopic)
	for _, member := range recipients {
		s.sendLine(member, line)
	}
	return false, ""
}

func (s *Server) sendNames(c *model.Client, ch *model.Channel) {
	s.mu.RLock()
	names := make([]string, 0, len(ch.Members))
	for member, membership := range ch.Members {
		n := member.Nick
		if membership.Op {
			n = "@" + n
		}
		names = append(names, n)
	}
	s.mu.RUnlock()
	sort.Strings(names)
	const chunkLimit = 300
	var chunk string
	flush := func() {
		if chunk == "" {
			return
		}
		s.numeric(c, "353", "= "+ch.Name+" :"+chunk)
		chunk = ""
	}
	for _, n := range names {
		if len(chunk)+len(n)+1 > chunkLimit {
			flush()
		}
		if chunk != "" {
			chunk += " "
		}
		chunk += n
	}
	flush()
	s.numeric(c, "366", ch.Name+" :End of NAMES list")
}

func (s *Server) cmdNAMES(c *model.Client, args string) (bool, string) {
	if !s.requireRegistered(c) {
		return false, ""
	}
	m := protocol.SingleChannelRE.FindStringSubmatch(args)
	if m == nil {
		s.badParams(c, "NAMES")
		return false, ""
	}
	s.mu.RLock()
	ch := s.channels[protocol.Norm(m[1])]
	s.mu.RUnlock()
	if ch == nil {
		s.numeric(c, "403", m[1]+" :No such channel")
		return false, ""
	}
	s.sendNames(c, ch)
	return false, ""
}

func wildcardRE(mask string) (*regexp.Regexp, error) {
	q := regexp.QuoteMeta(mask)
	q = strings.ReplaceAll(q, `\*`, `.*`)
	q = strings.ReplaceAll(q, `\?`, `.`)
	return regexp.Compile(`(?i)^` + q + `$`)
}

func (s *Server) cmdWHO(c *model.Client, args string) (bool, string) {
	if !s.requireRegistered(c) {
		return false, ""
	}
	m := protocol.WhoArgsRE.FindStringSubmatch(args)
	if m == nil {
		s.badParams(c, "WHO")
		return false, ""
	}
	mask := m[1]
	if strings.HasPrefix(mask, "#") {
		s.mu.RLock()
		ch := s.channels[protocol.Norm(mask)]
		if ch == nil {
			s.mu.RUnlock()
			s.numeric(c, "315", mask+" :End of WHO list")
			return false, ""
		}
		type row struct {
			client *model.Client
			op     bool
		}
		rows := make([]row, 0, len(ch.Members))
		for member, ms := range ch.Members {
			rows = append(rows, row{member, ms.Op})
		}
		s.mu.RUnlock()
		for _, r := range rows {
			flags := "H"
			if r.op {
				flags += "@"
			}
			s.numeric(c, "352", fmt.Sprintf("%s %s %s %s %s %s :0 %s", ch.Name, r.client.Username, r.client.RemoteIP.String(), s.cfg.ServerName, r.client.Nick, flags, r.client.Realname))
		}
		s.numeric(c, "315", mask+" :End of WHO list")
		return false, ""
	}
	re, err := wildcardRE(mask)
	if err != nil {
		s.badParams(c, "WHO")
		return false, ""
	}
	s.mu.RLock()
	clients := make([]*model.Client, 0, len(s.clients))
	for other := range s.clients {
		if other.Registered {
			clients = append(clients, other)
		}
	}
	s.mu.RUnlock()
	for _, other := range clients {
		if re.MatchString(other.Nick) || re.MatchString(other.Username) || re.MatchString(other.Realname) || re.MatchString(other.RemoteIP.String()) {
			flags := "H"
			if other.IsOper {
				flags += "*"
			}
			s.numeric(c, "352", fmt.Sprintf("* %s %s %s %s %s :0 %s", other.Username, other.RemoteIP.String(), s.cfg.ServerName, other.Nick, flags, other.Realname))
		}
	}
	s.numeric(c, "315", mask+" :End of WHO list")
	return false, ""
}

func (s *Server) cmdWHOIS(c *model.Client, args string) (bool, string) {
	if !s.requireRegistered(c) {
		return false, ""
	}
	m := protocol.WhoIsArgsRE.FindStringSubmatch(args)
	if m == nil {
		s.badParams(c, "WHOIS")
		return false, ""
	}
	target := s.findClientByNick(m[1])
	if target == nil || !target.Registered {
		s.numeric(c, "401", m[1]+" :No such nick")
		return false, ""
	}
	s.numeric(c, "311", fmt.Sprintf("%s %s %s * :%s", target.Nick, target.Username, target.RemoteIP.String(), target.Realname))
	s.mu.RLock()
	var channels []string
	for _, ch := range s.channels {
		if ms, ok := ch.Members[target]; ok {
			name := ch.Name
			if ms.Op {
				name = "@" + name
			}
			channels = append(channels, name)
		}
	}
	s.mu.RUnlock()
	sort.Strings(channels)
	if len(channels) > 0 {
		s.sendChunkedNumeric(c, "319", target.Nick+" :", channels, 280)
	}
	if target.IsOper {
		s.numeric(c, "313", target.Nick+" :is an IRC operator")
	}
	s.numeric(c, "318", target.Nick+" :End of WHOIS list")
	return false, ""
}

func (s *Server) sendChunkedNumeric(c *model.Client, code, prefix string, items []string, limit int) {
	chunk := prefix
	for _, item := range items {
		addition := item
		if chunk != prefix {
			addition = " " + item
		}
		if len(chunk)+len(addition) > limit {
			s.numeric(c, code, chunk)
			chunk = prefix + item
		} else {
			chunk += addition
		}
	}
	if chunk != prefix {
		s.numeric(c, code, chunk)
	}
}

func (s *Server) cmdLIST(c *model.Client, args string) (bool, string) {
	if !s.requireRegistered(c) {
		return false, ""
	}
	if args != "" {
		s.badParams(c, "LIST")
		return false, ""
	}
	s.numeric(c, "321", "Channel :Users Name")
	for _, ch := range s.sortedChannels() {
		s.mu.RLock()
		count := len(ch.Members)
		topic := ch.Topic
		s.mu.RUnlock()
		s.numeric(c, "322", fmt.Sprintf("%s %d :%s", ch.Name, count, topic))
	}
	s.numeric(c, "323", ":End of LIST")
	return false, ""
}

func (s *Server) cmdMOTD(c *model.Client, args string) (bool, string) {
	if !s.requireRegistered(c) {
		return false, ""
	}
	if args != "" {
		s.badParams(c, "MOTD")
		return false, ""
	}
	s.numeric(c, "375", ":- IRCapybara Message of the Day -")
	s.numeric(c, "372", ":- Single-server, simplicity-first IRC")
	s.numeric(c, "376", ":End of MOTD")
	return false, ""
}

func (s *Server) channelAdminLocked(c *model.Client, ch *model.Channel) bool {
	if c.IsOper {
		return true
	}
	ms, ok := ch.Members[c]
	return ok && ms.Op
}

func (s *Server) cmdMODE(c *model.Client, args string) (bool, string) {
	if !s.requireRegistered(c) {
		return false, ""
	}
	m := protocol.ModeArgsRE.FindStringSubmatch(args)
	if m == nil {
		s.badParams(c, "MODE")
		return false, ""
	}
	norm := protocol.Norm(m[1])
	s.mu.RLock()
	ch := s.channels[norm]
	s.mu.RUnlock()
	if ch == nil {
		s.numeric(c, "403", m[1]+" :No such channel")
		return false, ""
	}
	if m[2] == "" {
		s.sendModeState(c, ch)
		return false, ""
	}
	mode, param := m[2], m[3]
	if !protocol.ModeRE.MatchString(mode) {
		s.numeric(c, "472", mode+" :Unknown mode")
		return false, ""
	}

	if mode == "+b" && param == "" {
		for _, b := range s.protection.ListBans("CHANNEL", norm) {
			s.numeric(c, "367", ch.Name+" "+banMask(b))
		}
		s.numeric(c, "368", ch.Name+" :End of channel ban list")
		return false, ""
	}

	s.mu.Lock()
	if !s.channelAdminLocked(c, ch) {
		s.mu.Unlock()
		s.numeric(c, "482", ch.Name+" :You're not channel operator")
		return false, ""
	}
	switch mode {
	case "+o", "-o":
		if !protocol.NickRE.MatchString(param) {
			s.mu.Unlock()
			s.badParams(c, "MODE")
			return false, ""
		}
		var target *model.Client
		var ms *model.Membership
		for member, membership := range ch.Members {
			if protocol.Norm(member.Nick) == protocol.Norm(param) {
				target, ms = member, membership
				break
			}
		}
		if target == nil {
			s.mu.Unlock()
			s.numeric(c, "441", param+" "+ch.Name+" :They aren't on that channel")
			return false, ""
		}
		ms.Op = mode == "+o"
	case "+i":
		if param != "" {
			s.mu.Unlock()
			s.badParams(c, "MODE")
			return false, ""
		}
		ch.Modes.InviteOnly = true
	case "-i":
		if param != "" {
			s.mu.Unlock()
			s.badParams(c, "MODE")
			return false, ""
		}
		ch.Modes.InviteOnly = false
	case "+m":
		if param != "" {
			s.mu.Unlock()
			s.badParams(c, "MODE")
			return false, ""
		}
		ch.Modes.Moderated = true
	case "-m":
		if param != "" {
			s.mu.Unlock()
			s.badParams(c, "MODE")
			return false, ""
		}
		ch.Modes.Moderated = false
	case "+t":
		if param != "" {
			s.mu.Unlock()
			s.badParams(c, "MODE")
			return false, ""
		}
		ch.Modes.TopicOps = true
	case "-t":
		if param != "" {
			s.mu.Unlock()
			s.badParams(c, "MODE")
			return false, ""
		}
		ch.Modes.TopicOps = false
	case "+k":
		if !protocol.KeyRE.MatchString(param) {
			s.mu.Unlock()
			s.badParams(c, "MODE")
			return false, ""
		}
		ch.Modes.Key = param
	case "-k":
		if param != "" {
			s.mu.Unlock()
			s.badParams(c, "MODE")
			return false, ""
		}
		ch.Modes.Key = ""
	case "+l":
		if !protocol.IntegerRE.MatchString(param) {
			s.mu.Unlock()
			s.badParams(c, "MODE")
			return false, ""
		}
		n, _ := strconv.Atoi(param)
		if n < 1 || n > s.cfg.MaxClients {
			s.mu.Unlock()
			s.badParams(c, "MODE")
			return false, ""
		}
		ch.Modes.Limit = n
	case "-l":
		if param != "" {
			s.mu.Unlock()
			s.badParams(c, "MODE")
			return false, ""
		}
		ch.Modes.Limit = 0
	case "+b", "-b":
		s.mu.Unlock()
		return s.changeChannelBan(c, ch, mode == "+b", param)
	default:
		s.mu.Unlock()
		s.numeric(c, "472", mode+" :Unknown mode")
		return false, ""
	}
	recipients := make([]*model.Client, 0, len(ch.Members))
	for member := range ch.Members {
		recipients = append(recipients, member)
	}
	s.mu.Unlock()
	line := fmt.Sprintf(":%s MODE %s %s", s.prefix(c), ch.Name, mode)
	if param != "" {
		line += " " + param
	}
	for _, member := range recipients {
		s.sendLine(member, line)
	}
	return false, ""
}

func (s *Server) sendModeState(c *model.Client, ch *model.Channel) {
	s.mu.RLock()
	modes := "+"
	var params []string
	if ch.Modes.InviteOnly {
		modes += "i"
	}
	if ch.Modes.Moderated {
		modes += "m"
	}
	if ch.Modes.TopicOps {
		modes += "t"
	}
	if ch.Modes.Key != "" {
		modes += "k"
		params = append(params, "*")
	}
	if ch.Modes.Limit > 0 {
		modes += "l"
		params = append(params, strconv.Itoa(ch.Modes.Limit))
	}
	s.mu.RUnlock()
	out := ch.Name + " " + modes
	if len(params) > 0 {
		out += " " + strings.Join(params, " ")
	}
	s.numeric(c, "324", out)
}

func parseBan(kind, value string) (string, string, bool) {
	if !protocol.BanKindRE.MatchString(kind) {
		return "", "", false
	}
	switch kind {
	case "IP":
		if !protocol.IPOrCIDRValueRE.MatchString(value) || strings.Contains(value, "/") {
			return "", "", false
		}
		ip := net.ParseIP(value)
		if ip == nil {
			return "", "", false
		}
		return kind, ip.String(), true
	case "CIDR":
		if !protocol.IPOrCIDRValueRE.MatchString(value) || !strings.Contains(value, "/") {
			return "", "", false
		}
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			return "", "", false
		}
		return kind, network.String(), true
	case "USER":
		if !protocol.UsernameRE.MatchString(value) {
			return "", "", false
		}
		return kind, protocol.Norm(value), true
	}
	return "", "", false
}

func parseBanMask(mask string) (string, string, bool) {
	if !protocol.BanMaskRE.MatchString(mask) {
		return "", "", false
	}
	prefix, value, ok := strings.Cut(mask, ":")
	if !ok {
		return "", "", false
	}
	return parseBan(strings.ToUpper(prefix), value)
}

func banMask(b model.Ban) string { return strings.ToLower(b.Kind) + ":" + b.Value }

func (s *Server) changeChannelBan(c *model.Client, ch *model.Channel, add bool, mask string) (bool, string) {
	kind, value, ok := parseBanMask(mask)
	if !ok {
		s.badParams(c, "MODE")
		return false, ""
	}
	if add {
		for _, existing := range s.protection.ListBans("CHANNEL", ch.Norm) {
			if existing.Kind == kind && existing.Value == value {
				return false, ""
			}
		}
		id, err := s.store.AddBan("CHANNEL", ch.Norm, kind, value)
		if err != nil {
			log.Printf("add channel ban: %v", err)
			s.numeric(c, "400", ":Storage error")
			return false, ""
		}
		s.protection.AddBan(model.Ban{ID: id, Scope: "CHANNEL", ChannelNorm: ch.Norm, Kind: kind, Value: value})
	} else {
		deleted, err := s.store.DeleteBan("CHANNEL", ch.Norm, kind, value)
		if err != nil {
			log.Printf("delete channel ban: %v", err)
			s.numeric(c, "400", ":Storage error")
			return false, ""
		}
		if deleted {
			s.protection.DeleteBan("CHANNEL", ch.Norm, kind, value)
		}
	}
	line := fmt.Sprintf(":%s MODE %s %sb %s", s.prefix(c), ch.Name, map[bool]string{true: "+", false: "-"}[add], mask)
	s.broadcast(ch, nil, line)
	return false, ""
}

func (s *Server) cmdKICK(c *model.Client, args string) (bool, string) {
	if !s.requireRegistered(c) {
		return false, ""
	}
	m := protocol.KickArgsRE.FindStringSubmatch(args)
	if m == nil || !protocol.ValidText(maybe(m, 3), 160) {
		s.badParams(c, "KICK")
		return false, ""
	}
	norm := protocol.Norm(m[1])
	s.mu.Lock()
	ch := s.channels[norm]
	if ch == nil {
		s.mu.Unlock()
		s.numeric(c, "403", m[1]+" :No such channel")
		return false, ""
	}
	if !s.channelAdminLocked(c, ch) {
		s.mu.Unlock()
		s.numeric(c, "482", ch.Name+" :You're not channel operator")
		return false, ""
	}
	var target *model.Client
	for member := range ch.Members {
		if protocol.Norm(member.Nick) == protocol.Norm(m[2]) {
			target = member
			break
		}
	}
	if target == nil {
		s.mu.Unlock()
		s.numeric(c, "441", m[2]+" "+ch.Name+" :They aren't on that channel")
		return false, ""
	}
	recipients := make([]*model.Client, 0, len(ch.Members))
	for member := range ch.Members {
		recipients = append(recipients, member)
	}
	delete(ch.Members, target)
	reason := m[3]
	if reason == "" {
		reason = c.Nick
	}
	s.mu.Unlock()
	line := fmt.Sprintf(":%s KICK %s %s :%s", s.prefix(c), ch.Name, target.Nick, reason)
	for _, member := range recipients {
		s.sendLine(member, line)
	}
	return false, ""
}

func (s *Server) cmdINVITE(c *model.Client, args string) (bool, string) {
	if !s.requireRegistered(c) {
		return false, ""
	}
	m := protocol.InviteArgsRE.FindStringSubmatch(args)
	if m == nil {
		s.badParams(c, "INVITE")
		return false, ""
	}
	target := s.findClientByNick(m[1])
	if target == nil || !target.Registered {
		s.numeric(c, "401", m[1]+" :No such nick")
		return false, ""
	}
	norm := protocol.Norm(m[2])
	s.mu.Lock()
	ch := s.channels[norm]
	if ch == nil {
		s.mu.Unlock()
		s.numeric(c, "403", m[2]+" :No such channel")
		return false, ""
	}
	if !s.channelAdminLocked(c, ch) {
		s.mu.Unlock()
		s.numeric(c, "482", ch.Name+" :You're not channel operator")
		return false, ""
	}
	if _, already := ch.Members[target]; already {
		s.mu.Unlock()
		s.numeric(c, "443", target.Nick+" "+ch.Name+" :is already on channel")
		return false, ""
	}
	ch.Invited[target] = true
	s.mu.Unlock()
	s.numeric(c, "341", target.Nick+" "+ch.Name)
	s.sendLine(target, fmt.Sprintf(":%s INVITE %s :%s", s.prefix(c), target.Nick, ch.Name))
	return false, ""
}

func (s *Server) cmdOPER(c *model.Client, args string) (bool, string) {
	if !s.requireRegistered(c) {
		return false, ""
	}
	m := protocol.OperArgsRE.FindStringSubmatch(args)
	if m == nil {
		s.badParams(c, "OPER")
		return false, ""
	}
	if m[1] != s.cfg.OperName || !security.VerifyPassword(m[2], s.operCred.Salt, s.operCred.Hash) {
		s.numeric(c, "464", ":Password incorrect")
		return false, ""
	}
	c.IsOper = true
	s.numeric(c, "381", ":You are now an IRC operator")
	s.sendLine(c, fmt.Sprintf(":%s MODE %s +o", s.cfg.ServerName, c.Nick))
	return false, ""
}

func (s *Server) cmdKILL(c *model.Client, args string) (bool, string) {
	if !s.requireOper(c) {
		return false, ""
	}
	m := protocol.KillArgsRE.FindStringSubmatch(args)
	if m == nil || !protocol.ValidText(maybe(m, 2), 160) {
		s.badParams(c, "KILL")
		return false, ""
	}
	target := s.findClientByNick(m[1])
	if target == nil {
		s.numeric(c, "401", m[1]+" :No such nick")
		return false, ""
	}
	reason := m[2]
	if reason == "" {
		reason = "Killed by operator"
	}
	s.sendLine(target, fmt.Sprintf(":%s KILL %s :%s", s.prefix(c), target.Nick, reason))
	s.sendError(target, reason)
	s.removeClient(target, "Killed by "+c.Nick+": "+reason)
	_ = target.Conn.Close()
	return false, ""
}

func (s *Server) cmdCHANADD(c *model.Client, args string) (bool, string) {
	if !s.requireOper(c) {
		return false, ""
	}
	m := protocol.ChanAddArgsRE.FindStringSubmatch(args)
	if m == nil || !protocol.ValidText(maybe(m, 2), 180) {
		s.badParams(c, "CHANADD")
		return false, ""
	}
	norm := protocol.Norm(m[1])
	s.mu.RLock()
	_, exists := s.channels[norm]
	s.mu.RUnlock()
	if exists {
		s.notice(c, "Channel already exists")
		return false, ""
	}
	if err := s.store.AddChannel(m[1], m[2]); err != nil {
		log.Printf("add channel: %v", err)
		s.numeric(c, "400", ":Storage error")
		return false, ""
	}
	s.mu.Lock()
	s.channels[norm] = &model.Channel{Name: m[1], Norm: norm, Topic: m[2], Modes: model.ChannelModes{TopicOps: true}, Members: make(map[*model.Client]*model.Membership), Invited: make(map[*model.Client]bool)}
	s.mu.Unlock()
	s.notice(c, "Channel added: "+m[1])
	return false, ""
}

func (s *Server) cmdGUARD(c *model.Client, args string) (bool, string) {
	if !s.requireOper(c) {
		return false, ""
	}
	m := protocol.GuardArgsRE.FindStringSubmatch(args)
	if m == nil {
		s.badParams(c, "GUARD")
		return false, ""
	}
	level, _ := strconv.Atoi(m[1])
	snap := protection.GuardSnapshot{}
	if level > 0 {
		ipSeen := map[string]bool{}
		userSeen := map[string]bool{}
		s.mu.RLock()
		for other := range s.clients {
			if !other.Registered {
				continue
			}
			ip := other.RemoteIP.String()
			if !ipSeen[ip] {
				ipSeen[ip] = true
				snap.IPs = append(snap.IPs, ip)
			}
			if other.Username != "" && !userSeen[protocol.Norm(other.Username)] {
				userSeen[protocol.Norm(other.Username)] = true
				snap.Users = append(snap.Users, other.Username)
			}
		}
		s.mu.RUnlock()
	}
	s.protection.SetGuard(level, snap)
	s.notice(c, fmt.Sprintf("Guard mode set to %d; snapshot has %d IP(s), %d username(s)", level, len(snap.IPs), len(snap.Users)))
	return false, ""
}

func (s *Server) cmdBANADD(c *model.Client, args string) (bool, string) {
	if !s.requireOper(c) {
		return false, ""
	}
	m := protocol.BanAddArgsRE.FindStringSubmatch(args)
	if m == nil {
		s.badParams(c, "BANADD")
		return false, ""
	}
	kind, value, ok := parseBan(m[2], m[3])
	if !ok {
		s.badParams(c, "BANADD")
		return false, ""
	}
	scope, channelNorm := "SERVER", ""
	if m[1] != "SERVER" {
		channelNorm = protocol.Norm(m[1])
		scope = "CHANNEL"
		s.mu.RLock()
		chExists := s.channels[channelNorm] != nil
		s.mu.RUnlock()
		if !chExists {
			s.numeric(c, "403", m[1]+" :No such channel")
			return false, ""
		}
	}
	for _, b := range s.protection.ListBans(scope, channelNorm) {
		if b.Kind == kind && b.Value == value {
			s.notice(c, "Ban already exists")
			return false, ""
		}
	}
	id, err := s.store.AddBan(scope, channelNorm, kind, value)
	if err != nil {
		log.Printf("add ban: %v", err)
		s.numeric(c, "400", ":Storage error")
		return false, ""
	}
	s.protection.AddBan(model.Ban{ID: id, Scope: scope, ChannelNorm: channelNorm, Kind: kind, Value: value})
	s.notice(c, fmt.Sprintf("Ban added: %s %s %s", m[1], kind, value))
	if scope == "SERVER" {
		s.enforceServerBans()
	}
	return false, ""
}

func (s *Server) cmdBANDEL(c *model.Client, args string) (bool, string) {
	if !s.requireOper(c) {
		return false, ""
	}
	m := protocol.BanDelArgsRE.FindStringSubmatch(args)
	if m == nil {
		s.badParams(c, "BANDEL")
		return false, ""
	}
	kind, value, ok := parseBan(m[2], m[3])
	if !ok {
		s.badParams(c, "BANDEL")
		return false, ""
	}
	scope, channelNorm := "SERVER", ""
	if m[1] != "SERVER" {
		scope = "CHANNEL"
		channelNorm = protocol.Norm(m[1])
	}
	deleted, err := s.store.DeleteBan(scope, channelNorm, kind, value)
	if err != nil {
		log.Printf("delete ban: %v", err)
		s.numeric(c, "400", ":Storage error")
		return false, ""
	}
	if deleted {
		s.protection.DeleteBan(scope, channelNorm, kind, value)
		s.notice(c, "Ban removed")
	} else {
		s.notice(c, "Ban not found")
	}
	return false, ""
}

func (s *Server) cmdBANLIST(c *model.Client, args string) (bool, string) {
	if !s.requireOper(c) {
		return false, ""
	}
	m := protocol.BanListArgsRE.FindStringSubmatch(args)
	if m == nil {
		s.badParams(c, "BANLIST")
		return false, ""
	}
	scope, channelNorm := "SERVER", ""
	if m[1] != "SERVER" {
		scope = "CHANNEL"
		channelNorm = protocol.Norm(m[1])
	}
	bans := s.protection.ListBans(scope, channelNorm)
	if len(bans) == 0 {
		s.notice(c, "No bans for "+m[1])
		return false, ""
	}
	for _, b := range bans {
		s.notice(c, fmt.Sprintf("BAN %s %s %s", m[1], b.Kind, b.Value))
	}
	return false, ""
}

func (s *Server) enforceServerBans() {
	s.mu.RLock()
	var victims []*model.Client
	for other := range s.clients {
		if s.protection.ServerBanned(other.RemoteIP, other.Username) {
			victims = append(victims, other)
		}
	}
	s.mu.RUnlock()
	for _, v := range victims {
		s.sendError(v, "Banned from server")
		s.removeClient(v, "Server ban")
		_ = v.Conn.Close()
	}
}

func (s *Server) cmdXMODE(c *model.Client, args string) (bool, string) {
	if !s.requireRegistered(c) {
		return false, ""
	}
	if !protocol.XModeRE.MatchString(args) {
		s.badParams(c, "XMODE")
		return false, ""
	}
	c.ExtendedMode = args == "EXTENDED"
	if c.ExtendedMode {
		s.notice(c, "Extended mode enabled; rendering format v0 is passthrough")
	} else {
		s.notice(c, "Extended mode disabled")
	}
	return false, ""
}
