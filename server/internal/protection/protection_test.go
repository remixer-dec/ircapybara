package protection

import (
	"net"
	"testing"

	"ircapybara/internal/model"
)

func TestBans(t *testing.T) {
	m := New([]model.Ban{
		{Scope: "SERVER", Kind: "CIDR", Value: "192.0.2.0/24"},
		{Scope: "CHANNEL", ChannelNorm: "#general", Kind: "USER", Value: "noisy"},
	}, 0, nil, nil)
	if !m.ServerBanned(net.ParseIP("192.0.2.42"), "alice") {
		t.Fatal("CIDR ban did not match")
	}
	if m.ServerBanned(net.ParseIP("198.51.100.1"), "alice") {
		t.Fatal("unrelated IP matched")
	}
	if !m.ChannelBanned("#general", net.ParseIP("198.51.100.1"), "NOISY") {
		t.Fatal("username ban should be case-insensitive")
	}
}

func TestGuardLevels(t *testing.T) {
	ip := net.ParseIP("192.0.2.10")
	other := net.ParseIP("198.51.100.10")
	m := New(nil, 0, nil, nil)
	m.SetGuard(1, GuardSnapshot{IPs: []string{ip.String()}, Users: []string{"alice"}})
	if !m.GuardAllowsIdentity(other, "Alice") {
		t.Fatal("level 1 should allow snapshotted username")
	}
	m.SetGuard(2, GuardSnapshot{IPs: []string{ip.String()}, Users: []string{"alice"}})
	if m.GuardAllowsIdentity(other, "alice") {
		t.Fatal("level 2 must ignore username")
	}
	if !m.GuardAllowsIdentity(ip, "nobody") {
		t.Fatal("level 2 should allow snapshotted IP")
	}
	m.SetGuard(3, GuardSnapshot{IPs: []string{ip.String()}, Users: []string{"alice"}})
	if m.GuardAllowsIdentity(other, "alice") {
		t.Fatal("level 3 must ignore username")
	}
	if !m.GuardAllowsIdentity(ip, "nobody") {
		t.Fatal("level 3 should allow snapshotted IP")
	}
}
