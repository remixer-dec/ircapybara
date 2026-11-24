package persist

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

const (
	TLSModeAllowSelfSigned = "allow-self-signed"
	TLSModeSystem          = "system"
	TLSModePinned          = "pinned"
)

type Settings struct {
	Host                  string   `json:"host,omitempty"`
	Port                  int      `json:"port,omitempty"`
	Nick                  string   `json:"nick,omitempty"`
	DisplayName           string   `json:"displayName,omitempty"`
	ServerLabel           string   `json:"serverLabel,omitempty"`
	AutoJoin              []string `json:"autoJoin,omitempty"`
	Theme                 string   `json:"theme,omitempty"`
	TLSMode               string   `json:"tlsMode,omitempty"`
	PinnedCertificatePath string   `json:"pinnedCertificatePath,omitempty"`
	// PinnedDMs are direct-message targets kept in the sidebar across restarts.
	PinnedDMs []string `json:"pinnedDMs,omitempty"`
	// ExtendedNoticeServers lists the servers whose extended-mode notice has
	// already been shown once.
	ExtendedNoticeServers []string `json:"extendedNoticeServers,omitempty"`
}

// Store intentionally excludes passwords and other credentials.
type Store struct{ path string }

func New() (*Store, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return NewAt(filepath.Join(dir, "IRCapybara"))
}

// NewAt stores settings in dir. Mobile platforms have no per-user configuration
// directory that os.UserConfigDir can discover, so their shells pass the
// application's private files directory.
func NewAt(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &Store{path: filepath.Join(dir, "settings.json")}, nil
}

func (s *Store) Load() (Settings, error) {
	var out Settings
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return Settings{}, err
	}
	return out, nil
}

func (s *Store) Save(v Settings) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) SaveTheme(theme string) error {
	v, err := s.Load()
	if err != nil {
		return err
	}
	v.Theme = theme
	return s.Save(v)
}
