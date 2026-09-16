package protection

import (
	"net"
	"strings"
	"sync"

	"ircapybara/internal/model"
	"ircapybara/internal/protocol"
)

type GuardSnapshot struct {
	IPs   []string
	Users []string
}

type Manager struct {
	mu           sync.RWMutex
	bans         []model.Ban
	guardLevel   int
	allowedIPs   []string
	allowedUsers map[string]struct{}
}

func New(bans []model.Ban, guardLevel int, allowedIPs, allowedUsers []string) *Manager {
	m := &Manager{bans: append([]model.Ban(nil), bans...), guardLevel: guardLevel, allowedIPs: append([]string(nil), allowedIPs...), allowedUsers: make(map[string]struct{})}
	for _, u := range allowedUsers {
		m.allowedUsers[protocol.Norm(u)] = struct{}{}
	}
	return m
}

func (m *Manager) GuardLevel() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.guardLevel
}

func (m *Manager) SetGuard(level int, snapshot GuardSnapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.guardLevel = level
	if level == 0 {
		m.allowedIPs = nil
		m.allowedUsers = make(map[string]struct{})
		return
	}
	m.allowedIPs = append([]string(nil), snapshot.IPs...)
	m.allowedUsers = make(map[string]struct{})
	if level == 1 {
		for _, u := range snapshot.Users {
			m.allowedUsers[protocol.Norm(u)] = struct{}{}
		}
	}
}

func matchIPSpec(spec string, ip net.IP) bool {
	if strings.Contains(spec, "/") {
		_, network, err := net.ParseCIDR(spec)
		return err == nil && network.Contains(ip)
	}
	parsed := net.ParseIP(spec)
	return parsed != nil && parsed.Equal(ip)
}

func (m *Manager) GuardAllowsIP(ip net.IP) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.guardLevel == 0 {
		return true
	}
	for _, spec := range m.allowedIPs {
		if matchIPSpec(spec, ip) {
			return true
		}
	}
	return false
}

// GuardAllowsIdentity is checked once USER is available. Level 1 accepts a
// snapshotted IP OR username; level 2 accepts only a snapshotted IP.
func (m *Manager) GuardAllowsIdentity(ip net.IP, username string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.guardLevel == 0 {
		return true
	}
	for _, spec := range m.allowedIPs {
		if matchIPSpec(spec, ip) {
			return true
		}
	}
	if m.guardLevel == 1 {
		_, ok := m.allowedUsers[protocol.Norm(username)]
		return ok
	}
	return false
}

func banMatches(b model.Ban, ip net.IP, username string) bool {
	switch b.Kind {
	case "IP":
		parsed := net.ParseIP(b.Value)
		return parsed != nil && parsed.Equal(ip)
	case "CIDR":
		_, network, err := net.ParseCIDR(b.Value)
		return err == nil && network.Contains(ip)
	case "USER":
		return protocol.Norm(username) == protocol.Norm(b.Value)
	default:
		return false
	}
}

func (m *Manager) ServerBanned(ip net.IP, username string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, b := range m.bans {
		if b.Scope == "SERVER" && banMatches(b, ip, username) {
			return true
		}
	}
	return false
}

func (m *Manager) ChannelBanned(channelNorm string, ip net.IP, username string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, b := range m.bans {
		if b.Scope == "CHANNEL" && b.ChannelNorm == channelNorm && banMatches(b, ip, username) {
			return true
		}
	}
	return false
}

func (m *Manager) AddBan(b model.Ban) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bans = append(m.bans, b)
}

func (m *Manager) DeleteBan(scope, channelNorm, kind, value string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, b := range m.bans {
		if b.Scope == scope && b.ChannelNorm == channelNorm && b.Kind == kind && b.Value == value {
			m.bans = append(m.bans[:i], m.bans[i+1:]...)
			return true
		}
	}
	return false
}

func (m *Manager) ListBans(scope, channelNorm string) []model.Ban {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []model.Ban
	for _, b := range m.bans {
		if b.Scope == scope && b.ChannelNorm == channelNorm {
			out = append(out, b)
		}
	}
	return out
}
