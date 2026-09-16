package session

import "sync"

type State string

const (
	Disconnected  State = "disconnected"
	Connecting    State = "connecting"
	TLS           State = "tls"
	CAP           State = "cap-negotiation"
	Registered    State = "registered"
	Captcha       State = "captcha-pending"
	ReadOnly      State = "identity-read-only"
	Ready         State = "ready"
	GuardReadOnly State = "server-guard-read-only"
)

type Snapshot struct {
	State          State  `json:"state"`
	StateDetail    string `json:"stateDetail,omitempty"`
	Nick           string `json:"nick,omitempty"`
	Server         string `json:"server,omitempty"`
	Extended       bool   `json:"extended"`
	Registered     bool   `json:"registered"`
	Identified     bool   `json:"identified"`
	IdentityStatus string `json:"identityStatus,omitempty"`
	IdentityDetail string `json:"identityDetail,omitempty"`
	CaptchaWord    string `json:"captchaWord,omitempty"`
	CaptchaPrompt  string `json:"captchaPrompt,omitempty"`
}

type Store struct {
	mu   sync.RWMutex
	snap Snapshot
}

func NewStore() *Store {
	return &Store{snap: Snapshot{State: Disconnected}}
}

func (s *Store) Reset(nick, server string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = Snapshot{State: Disconnected, Nick: nick, Server: server, IdentityStatus: "none"}
}

func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap
}

func (s *Store) SetState(st State, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.State, s.snap.StateDetail = st, detail
}

func (s *Store) SetIdentity(nick, server string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Nick, s.snap.Server = nick, server
}

func (s *Store) SetRegistered(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Registered = v
}

func (s *Store) SetIdentified(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Identified = v
	if v {
		s.snap.IdentityStatus = "identified"
		s.snap.IdentityDetail = "Identity authenticated"
	}
}

func (s *Store) SetIdentityStatus(status, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.IdentityStatus = status
	s.snap.IdentityDetail = detail
	if status != "identified" {
		s.snap.Identified = false
	}
}

func (s *Store) SetExtended(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Extended = v
}

func (s *Store) SetCaptcha(word, prompt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.CaptchaWord, s.snap.CaptchaPrompt = word, prompt
}

func (s *Store) ClearCaptcha() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.CaptchaWord, s.snap.CaptchaPrompt = "", ""
}
