package session

import "testing"

func TestResetClearsSessionFlags(t *testing.T) {
	s := NewStore()
	s.SetRegistered(true)
	s.SetIdentified(true)
	s.SetExtended(true)
	s.SetCaptcha("word", "prompt")
	s.Reset("alice", "irc.example:6697")
	snap := s.Snapshot()
	if snap.Nick != "alice" || snap.Server != "irc.example:6697" {
		t.Fatalf("identity not reset: %#v", snap)
	}
	if snap.Registered || snap.Identified || snap.Extended || snap.CaptchaWord != "" {
		t.Fatalf("session flags survived reset: %#v", snap)
	}
	if snap.State != Disconnected {
		t.Fatalf("state = %q", snap.State)
	}
}
