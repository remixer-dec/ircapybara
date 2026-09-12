package server

import (
	"bufio"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"ircapybara/internal/config"
	"ircapybara/internal/model"
	"ircapybara/internal/protection"
	"ircapybara/internal/protocol"
	"ircapybara/internal/security"
	"ircapybara/internal/storage"
	"ircapybara/pkg/pluginapi"
)

type Server struct {
	cfg     config.Config
	store   *storage.Store
	plugins []pluginapi.Plugin

	mu                 sync.RWMutex
	clients            map[*model.Client]struct{}
	channels           map[string]*model.Channel
	identities         map[string]model.Credential
	trustedIdentityIPs map[string]map[string]struct{}

	protection     *protection.Manager
	operCred       model.Credential
	serverPassCred *model.Credential
	startedAt      time.Time
	nextClientID   atomic.Uint64
}

func New(cfg config.Config, store *storage.Store, plugins []pluginapi.Plugin) (*Server, error) {
	channels, err := store.LoadChannels()
	if err != nil {
		return nil, err
	}
	for _, ch := range channels {
		ch.Modes.TopicOps = true
	}
	identities, err := store.LoadIdentities()
	if err != nil {
		return nil, err
	}
	bans, err := store.LoadBans()
	if err != nil {
		return nil, err
	}

	operSalt, operHash, err := security.NewPasswordHash(cfg.OperPassword)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:                cfg,
		store:              store,
		plugins:            plugins,
		clients:            make(map[*model.Client]struct{}),
		channels:           channels,
		identities:         identities,
		trustedIdentityIPs: make(map[string]map[string]struct{}),
		protection:         protection.New(bans, cfg.GuardLevel, cfg.GuardAllowedIPs, cfg.GuardAllowedUsers),
		operCred:           model.Credential{NickDisplay: cfg.OperName, Salt: operSalt, Hash: operHash},
		startedAt:          time.Now(),
	}
	s.cfg.OperPassword = ""
	if cfg.ServerPassword != "" {
		salt, hash, err := security.NewPasswordHash(cfg.ServerPassword)
		if err != nil {
			return nil, err
		}
		s.serverPassCred = &model.Credential{Salt: salt, Hash: hash}
		s.cfg.ServerPassword = ""
	}
	for _, p := range s.plugins {
		if err := p.Init(s); err != nil {
			return nil, fmt.Errorf("plugin %s init: %w", p.Name(), err)
		}
	}
	return s, nil
}

func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp4", s.cfg.ListenAddr)
	if err != nil {
		return err
	}
	defer ln.Close()

	if s.cfg.TLS {
		tc, err := tlsConfig(s.cfg.TLSCertFile, s.cfg.TLSKeyFile, s.cfg.ServerName, s.cfg.TLSAutoGenerate)
		if err != nil {
			return err
		}
		ln = tls.NewListener(ln, tc)
		log.Printf("IRCapybara listening with TLS on %s", s.cfg.ListenAddr)
	} else {
		log.Printf("IRCapybara listening WITHOUT TLS on %s", s.cfg.ListenAddr)
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Temporary() {
				continue
			}
			return err
		}
		ip := remoteIP(conn.RemoteAddr())
		if ip == nil {
			_ = conn.Close()
			continue
		}
		if !s.acceptAllowed(ip) {
			log.Printf("connection rejected from %s by admission policy", ip.String())
			_ = conn.Close()
			continue
		}
		c := &model.Client{
			ID:           fmt.Sprintf("c%d", s.nextClientID.Add(1)),
			Conn:         conn,
			RemoteIP:     ip,
			Connected:    time.Now(),
			LastActivity: time.Now(),
			Tokens:       s.cfg.RateBurst,
			TokenUpdated: time.Now(),
			ServerPassOK: s.serverPassCred == nil,
			Capabilities: make(map[string]bool),
		}
		s.mu.Lock()
		s.clients[c] = struct{}{}
		s.mu.Unlock()
		for _, p := range s.plugins {
			p.OnConnect(s.clientView(c))
		}
		go s.serveClient(c)
	}
}

func remoteIP(addr net.Addr) net.IP {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

func (s *Server) acceptAllowed(ip net.IP) bool {
	if s.protection.ServerBanned(ip, "") {
		return false
	}
	if s.protection.GuardLevel() >= 2 && !s.protection.GuardAllowsIP(ip) {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.clients) >= s.cfg.MaxClients {
		return false
	}
	n := 0
	for c := range s.clients {
		if c.RemoteIP.Equal(ip) {
			n++
		}
	}
	return n < s.cfg.MaxPerIP
}

func (s *Server) serveClient(c *model.Client) {
	reason := "Connection closed"
	defer func() {
		s.removeClient(c, reason)
		_ = c.Conn.Close()
		for _, p := range s.plugins {
			p.OnDisconnect(s.clientView(c))
		}
	}()

	if tc, ok := c.Conn.(*tls.Conn); ok {
		_ = tc.SetDeadline(time.Now().Add(15 * time.Second))
		if err := tc.Handshake(); err != nil {
			reason = "TLS handshake failed"
			log.Printf("TLS handshake failed from %s: %v", c.RemoteIP.String(), err)
			return
		}
		_ = tc.SetDeadline(time.Time{})
	}

	r := bufio.NewReaderSize(c.Conn, protocol.MaxLineBytes+2)
	for {
		raw, err := r.ReadSlice('\n')
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				s.sendError(c, "Input line too long")
				reason = "Protocol violation"
				log.Printf("protocol violation from %s: input line too long", c.RemoteIP.String())
			} else if !errors.Is(err, io.EOF) {
				log.Printf("client read error from %s: %v", c.RemoteIP.String(), err)
			}
			return
		}
		if len(raw) == 0 {
			continue
		}
		raw = raw[:len(raw)-1]
		if len(raw) > 0 && raw[len(raw)-1] == '\r' {
			raw = raw[:len(raw)-1]
		}
		if len(raw) == 0 || len(raw) > protocol.MaxLineBytes || !utf8.Valid(raw) {
			s.sendError(c, "Invalid IRC input")
			reason = "Protocol violation"
			log.Printf("protocol violation from %s: invalid line length or UTF-8", c.RemoteIP.String())
			return
		}
		if !s.consumeToken(c) {
			s.sendError(c, "Flood protection")
			reason = "Flood protection"
			return
		}
		cmd, err := protocol.ParseLine(string(raw))
		if err != nil {
			s.sendError(c, "Invalid IRC input")
			reason = "Protocol violation"
			log.Printf("protocol violation from %s: invalid command envelope", c.RemoteIP.String())
			return
		}
		c.LastActivity = time.Now()

		if c.Registered && c.Captcha.Required && !c.Captcha.Solved {
			switch cmd.Name {
			case "CAPTCHA", "PING", "PONG", "QUIT":
			default:
				continue
			}
		}

		if c.Registered && c.IdentityReadOnly && !s.identityReadOnlyCommandAllowed(cmd) {
			if cmd.Name != "NOTICE" {
				s.notice(c, "Registered nickname is read-only until IDENTIFY succeeds")
			}
			continue
		}

		if s.protection.GuardLevel() == 3 && !s.readOnlyCommandAllowed(cmd) {
			if cmd.Name != "NOTICE" {
				s.notice(c, "Guard level 3: server is read-only")
			}
			continue
		}

		event := pluginapi.CommandEvent{Client: s.clientView(c), Command: cmd.Name, Args: cmd.Args}
		denied := false
		for _, p := range s.plugins {
			if err := p.BeforeCommand(event); err != nil {
				denied = true
				break
			}
		}
		if denied {
			continue
		}
		handled := false
		for _, p := range s.plugins {
			if h, ok := p.(pluginapi.CommandHandler); ok && h.HandleCommand(event) {
				handled = true
				break
			}
		}
		if handled {
			continue
		}

		disconnect, why := s.dispatch(c, cmd)
		if disconnect {
			if why != "" {
				reason = why
			}
			return
		}
	}
}

func (s *Server) consumeToken(c *model.Client) bool {
	now := time.Now()
	elapsed := now.Sub(c.TokenUpdated).Seconds()
	c.Tokens += elapsed * s.cfg.RatePerSecond
	if c.Tokens > s.cfg.RateBurst {
		c.Tokens = s.cfg.RateBurst
	}
	c.TokenUpdated = now
	if c.Tokens < 1 {
		return false
	}
	c.Tokens--
	return true
}

func (s *Server) clientView(c *model.Client) pluginapi.ClientView {
	return pluginapi.ClientView{
		ID: c.ID, Nick: c.Nick, Username: c.Username, Identity: c.IdentityAuthedNick,
		IP: c.RemoteIP.String(), Registered: c.Registered, IsOper: c.IsOper,
		Extended: c.ExtendedMode, HistoryCap: c.Capabilities["ircapybara/history"],
	}
}

func (s *Server) prefix(c *model.Client) string {
	nick := c.Nick
	if nick == "" {
		nick = "*"
	}
	user := c.Username
	if user == "" {
		user = "unknown"
	}
	return nick + "!" + user + "@" + c.RemoteIP.String()
}

func (s *Server) sendLine(c *model.Client, line string) {
	if strings.ContainsAny(line, "\r\n") {
		return
	}
	if len(line) > protocol.MaxLineBytes {
		for len(line) > protocol.MaxLineBytes {
			_, size := utf8.DecodeLastRuneInString(line)
			if size <= 0 {
				return
			}
			line = line[:len(line)-size]
		}
	}
	c.WriteMu.Lock()
	defer c.WriteMu.Unlock()
	_ = c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(c.Conn, line+"\r\n")
}

func (s *Server) target(c *model.Client) string {
	if c.Nick == "" {
		return "*"
	}
	return c.Nick
}

func (s *Server) numeric(c *model.Client, code string, args string) {
	s.sendLine(c, fmt.Sprintf(":%s %s %s %s", s.cfg.ServerName, code, s.target(c), args))
}

func (s *Server) notice(c *model.Client, text string) {
	if !protocol.ValidText(text, 400) {
		return
	}
	s.sendLine(c, fmt.Sprintf(":%s NOTICE %s :%s", s.cfg.ServerName, s.target(c), text))
}

func (s *Server) sendError(c *model.Client, text string) { s.sendLine(c, "ERROR :"+text) }

func (s *Server) findClientByNick(nick string) *model.Client {
	norm := protocol.Norm(nick)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for c := range s.clients {
		if protocol.Norm(c.Nick) == norm && c.Nick != "" {
			return c
		}
	}
	return nil
}

func (s *Server) nickInUseLocked(nick string, except *model.Client) bool {
	norm := protocol.Norm(nick)
	for c := range s.clients {
		if c != except && c.Nick != "" && protocol.Norm(c.Nick) == norm {
			return true
		}
	}
	return false
}

func (s *Server) identity(norm string) (model.Credential, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cred, ok := s.identities[norm]
	return cred, ok
}

func (s *Server) rememberIdentityIP(ip net.IP, identity string) {
	if !s.cfg.IdentifyTrustIP || ip == nil || !protocol.NickRE.MatchString(identity) {
		return
	}
	ipKey := ip.String()
	identity = protocol.Norm(identity)
	s.mu.Lock()
	if s.trustedIdentityIPs == nil {
		s.trustedIdentityIPs = make(map[string]map[string]struct{})
	}
	set := s.trustedIdentityIPs[ipKey]
	if set == nil {
		set = make(map[string]struct{})
		s.trustedIdentityIPs[ipKey] = set
	}
	set[identity] = struct{}{}
	s.mu.Unlock()
}

func (s *Server) identityIPTrusted(ip net.IP, identity string) bool {
	if !s.cfg.IdentifyTrustIP || ip == nil || !protocol.NickRE.MatchString(identity) {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.trustedIdentityIPs[ip.String()][protocol.Norm(identity)]
	return ok
}

func (s *Server) maybeRegister(c *model.Client) (bool, string) {
	if c.Registered || c.CapNegotiating || c.Nick == "" || c.Username == "" || !c.ServerPassOK {
		return false, ""
	}
	if s.protection.ServerBanned(c.RemoteIP, c.Username) {
		s.sendError(c, "Banned from server")
		return true, "Server ban"
	}
	if !s.protection.GuardAllowsIdentity(c.RemoteIP, c.Username) {
		s.sendError(c, "Server guard mode")
		return true, "Guard mode"
	}
	c.Registered = true
	s.numeric(c, "001", ":Welcome to IRCapybara, "+c.Nick)
	s.numeric(c, "005", "UTF8ONLY CHANTYPES=# PREFIX=(o)@ CHANMODES=b,k,l,imtsH IRCAPYBARA=1 :are supported by this server")
	s.numeric(c, "375", ":- IRCapybara Message of the Day -")
	s.numeric(c, "372", ":- Single-server, simplicity-first IRC")
	s.numeric(c, "376", ":End of MOTD")
	if c.IdentityReadOnly {
		s.notice(c, "This registered nickname is connected read-only; use IDENTIFY <password> or IDENTIFY <nick> <password>")
	}
	if s.cfg.CaptchaEnabled {
		if err := s.issueCaptcha(c); err != nil {
			s.sendError(c, "Captcha unavailable")
			return true, "Captcha unavailable"
		}
	}
	return false, ""
}

func (s *Server) issueCaptcha(c *model.Client) error {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	id := hex.EncodeToString(b)
	nums := make([]byte, 2)
	if _, err := rand.Read(nums); err != nil {
		return err
	}
	a := int(nums[0]%20) + 1
	d := int(nums[1]%20) + 1
	c.Captcha = model.CaptchaState{Required: true, Solved: false, ID: id, Answer: a + d}
	s.notice(c, fmt.Sprintf("CAPTCHA REQUIRED %s: solve %d+%d; reply exactly: CAPTCHA %s <answer>", id, a, d, id))
	return nil
}

func (s *Server) removeClient(c *model.Client, reason string) {
	recipients := make(map[*model.Client]struct{})
	s.mu.Lock()
	if _, ok := s.clients[c]; !ok {
		s.mu.Unlock()
		return
	}
	delete(s.clients, c)
	for _, ch := range s.channels {
		delete(ch.SessionParticipants, c)
		delete(ch.Invited, c)
		if _, ok := ch.Members[c]; ok {
			delete(ch.Members, c)
			for other := range ch.Members {
				recipients[other] = struct{}{}
			}
		}
	}
	s.mu.Unlock()
	if c.Nick != "" {
		line := fmt.Sprintf(":%s QUIT :%s", s.prefix(c), reason)
		for other := range recipients {
			s.sendLine(other, line)
		}
	}
}

func (s *Server) channelRecipients(ch *model.Channel, except *model.Client) []*model.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.Client, 0, len(ch.Members))
	for c := range ch.Members {
		if c != except {
			out = append(out, c)
		}
	}
	return out
}

func (s *Server) broadcast(ch *model.Channel, except *model.Client, line string) {
	for _, c := range s.channelRecipients(ch, except) {
		s.sendLine(c, line)
	}
}

func (s *Server) sortedChannels() []*model.Channel {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.Channel, 0, len(s.channels))
	for _, ch := range s.channels {
		out = append(out, ch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Norm < out[j].Norm })
	return out
}
