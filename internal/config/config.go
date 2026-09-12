package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

type SeedChannel struct {
	Name  string
	Topic string
}

type Config struct {
	ListenAddr        string
	ServerName        string
	DatabasePath      string
	TLS               bool
	TLSCertFile       string
	TLSKeyFile        string
	TLSAutoGenerate   bool
	ServerPassword    string
	OperName          string
	OperPassword      string
	CaptchaEnabled    bool
	GuardLevel        int
	GuardAllowedIPs   []string
	GuardAllowedUsers []string
	InitialChannels   []SeedChannel
	MaxClients        int
	MaxPerIP          int
	RatePerSecond     float64
	RateBurst         float64
}

var (
	reListen     = regexp.MustCompile(`^(?:\[[0-9A-Fa-f:]+\]|[0-9A-Za-z._-]*):[0-9]{1,5}$`)
	reServerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,126}$`)
	rePath       = regexp.MustCompile(`^/[A-Za-z0-9_./-]{1,240}$`)
	reSecret     = regexp.MustCompile(`^[\x21-\x7e]{0,128}$`)
	reOperName   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,31}$`)
	reChannels   = regexp.MustCompile(`^(?:#[A-Za-z0-9_-]{1,48}\|[^;\r\n]{0,180})(?:;#[A-Za-z0-9_-]{1,48}\|[^;\r\n]{0,180})*$`)
	reIPList     = regexp.MustCompile(`^(?:[0-9A-Fa-f:.]+(?:/[0-9]{1,3})?)(?:,[0-9A-Fa-f:.]+(?:/[0-9]{1,3})?)*$`)
	reUserList   = regexp.MustCompile(`^(?:[A-Za-z0-9._-]{1,32})(?:,[A-Za-z0-9._-]{1,32})*$`)
	reConfigText = regexp.MustCompile(`^[\x20-\x7e\p{L}\p{M}\p{N}\p{P}\p{S}\p{Zs}]*$`)
)

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func boolEnv(key string, def bool) (bool, error) {
	raw := getenv(key, strconv.FormatBool(def))
	if raw != "true" && raw != "false" {
		return false, fmt.Errorf("%s must be true or false", key)
	}
	return raw == "true", nil
}

func intEnv(key string, def, min, max int) (int, error) {
	raw := getenv(key, strconv.Itoa(def))
	if !regexp.MustCompile(`^[0-9]{1,9}$`).MatchString(raw) {
		return 0, fmt.Errorf("%s is not a valid integer", key)
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("%s must be between %d and %d", key, min, max)
	}
	return n, nil
}

func floatEnv(key string, def float64, min, max float64) (float64, error) {
	raw := getenv(key, strconv.FormatFloat(def, 'f', -1, 64))
	if !regexp.MustCompile(`^[0-9]{1,6}(?:\.[0-9]{1,3})?$`).MatchString(raw) {
		return 0, fmt.Errorf("%s is not a valid decimal", key)
	}
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("%s must be between %.2f and %.2f", key, min, max)
	}
	return n, nil
}

func Load() (Config, error) {
	cfg := Config{
		ListenAddr:     getenv("IRCAPYBARA_LISTEN_ADDR", ":6697"),
		ServerName:     getenv("IRCAPYBARA_SERVER_NAME", "ircapybara.local"),
		DatabasePath:   getenv("IRCAPYBARA_DB_PATH", "/data/ircapybara.db"),
		TLSCertFile:    getenv("IRCAPYBARA_TLS_CERT_FILE", "/data/tls/server.crt"),
		TLSKeyFile:     getenv("IRCAPYBARA_TLS_KEY_FILE", "/data/tls/server.key"),
		ServerPassword: getenv("IRCAPYBARA_SERVER_PASSWORD", ""),
		OperName:       getenv("IRCAPYBARA_OPER_NAME", "admin"),
		OperPassword:   getenv("IRCAPYBARA_OPER_PASSWORD", "change-me-now"),
	}
	var err error
	if cfg.TLS, err = boolEnv("IRCAPYBARA_TLS_ENABLED", true); err != nil {
		return Config{}, err
	}
	if cfg.TLSAutoGenerate, err = boolEnv("IRCAPYBARA_TLS_AUTOGEN", true); err != nil {
		return Config{}, err
	}
	if cfg.CaptchaEnabled, err = boolEnv("IRCAPYBARA_CAPTCHA_ENABLED", false); err != nil {
		return Config{}, err
	}
	if cfg.GuardLevel, err = intEnv("IRCAPYBARA_GUARD_LEVEL", 0, 0, 2); err != nil {
		return Config{}, err
	}
	if cfg.MaxClients, err = intEnv("IRCAPYBARA_MAX_CLIENTS", 256, 1, 10000); err != nil {
		return Config{}, err
	}
	if cfg.MaxPerIP, err = intEnv("IRCAPYBARA_MAX_PER_IP", 8, 1, 1024); err != nil {
		return Config{}, err
	}
	if cfg.RatePerSecond, err = floatEnv("IRCAPYBARA_RATE_PER_SECOND", 5, 0.1, 1000); err != nil {
		return Config{}, err
	}
	if cfg.RateBurst, err = floatEnv("IRCAPYBARA_RATE_BURST", 12, 1, 2000); err != nil {
		return Config{}, err
	}

	if !reListen.MatchString(cfg.ListenAddr) {
		return Config{}, errors.New("invalid IRCAPYBARA_LISTEN_ADDR")
	}
	if !reServerName.MatchString(cfg.ServerName) {
		return Config{}, errors.New("invalid IRCAPYBARA_SERVER_NAME")
	}
	if !rePath.MatchString(cfg.DatabasePath) || !rePath.MatchString(cfg.TLSCertFile) || !rePath.MatchString(cfg.TLSKeyFile) {
		return Config{}, errors.New("database and TLS paths must be absolute safe paths")
	}
	if !reSecret.MatchString(cfg.ServerPassword) || !reOperName.MatchString(cfg.OperName) || !reSecret.MatchString(cfg.OperPassword) || len(cfg.OperPassword) < 8 {
		return Config{}, errors.New("invalid server/operator credential configuration")
	}

	channelRaw := getenv("IRCAPYBARA_INITIAL_CHANNELS", "#general|General discussion;#dev|Development")
	if !utf8.ValidString(channelRaw) || !reChannels.MatchString(channelRaw) {
		return Config{}, errors.New("invalid IRCAPYBARA_INITIAL_CHANNELS")
	}
	for _, item := range strings.Split(channelRaw, ";") {
		parts := strings.SplitN(item, "|", 2)
		if len(parts) != 2 || !reConfigText.MatchString(parts[1]) {
			return Config{}, errors.New("invalid channel topic")
		}
		cfg.InitialChannels = append(cfg.InitialChannels, SeedChannel{Name: parts[0], Topic: parts[1]})
	}

	if raw := getenv("IRCAPYBARA_GUARD_ALLOWED_IPS", ""); raw != "" {
		if !reIPList.MatchString(raw) {
			return Config{}, errors.New("invalid IRCAPYBARA_GUARD_ALLOWED_IPS")
		}
		cfg.GuardAllowedIPs = strings.Split(raw, ",")
		for _, spec := range cfg.GuardAllowedIPs {
			if strings.Contains(spec, "/") {
				if _, _, err := net.ParseCIDR(spec); err != nil {
					return Config{}, errors.New("invalid guard CIDR")
				}
			} else if net.ParseIP(spec) == nil {
				return Config{}, errors.New("invalid guard IP")
			}
		}
	}
	if raw := getenv("IRCAPYBARA_GUARD_ALLOWED_USERS", ""); raw != "" {
		if !reUserList.MatchString(raw) {
			return Config{}, errors.New("invalid IRCAPYBARA_GUARD_ALLOWED_USERS")
		}
		cfg.GuardAllowedUsers = strings.Split(raw, ",")
	}
	if cfg.GuardLevel > 0 && len(cfg.GuardAllowedIPs) == 0 && len(cfg.GuardAllowedUsers) == 0 {
		return Config{}, errors.New("startup guard requires an explicit allowlist; runtime GUARD snapshots current clients")
	}
	return cfg, nil
}
