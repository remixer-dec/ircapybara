package core

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"ircapybara/client/frontend"
	"ircapybara/client/internal/conversation"
	"ircapybara/client/internal/persist"
	"ircapybara/client/internal/protocol"
	"ircapybara/client/internal/session"
	"ircapybara/client/internal/transport"
	"ircapybara/client/internal/xfer"
)

type ConnectionConfig struct {
	Host                  string   `json:"host"`
	Port                  int      `json:"port"`
	Nick                  string   `json:"nick"`
	DisplayName           string   `json:"displayName"`
	ServerLabel           string   `json:"serverLabel"`
	ServerPassword        string   `json:"serverPassword"`
	IdentityNick          string   `json:"identityNick"`
	IdentityPassword      string   `json:"identityPassword"`
	AutoJoin              []string `json:"autoJoin"`
	AllowSelfSigned       bool     `json:"allowSelfSigned"`
	PinServerCertificate  bool     `json:"pinServerCertificate"`
	PinnedCertificatePath string   `json:"pinnedCertificatePath"`
}

type ProtocolTraceEntry struct {
	Seq       uint64    `json:"seq"`
	Direction string    `json:"direction"`
	Time      time.Time `json:"time"`
	Line      string    `json:"line"`
}

type TransferUpdate struct {
	FileID        string `json:"fileId"`
	Status        string `json:"status"`
	Received      int64  `json:"received,omitempty"`
	Total         int64  `json:"total,omitempty"`
	Percent       int    `json:"percent,omitempty"`
	ReceivedParts int    `json:"receivedParts,omitempty"`
	TotalParts    int    `json:"totalParts,omitempty"`
	RetryRounds   int    `json:"retryRounds,omitempty"`
	Path          string `json:"path,omitempty"`
	Error         string `json:"error,omitempty"`
}

type ClientSnapshot struct {
	State          session.State               `json:"state"`
	StateDetail    string                      `json:"stateDetail,omitempty"`
	Nick           string                      `json:"nick,omitempty"`
	Server         string                      `json:"server,omitempty"`
	Extended       bool                        `json:"extended"`
	Registered     bool                        `json:"registered"`
	Identified     bool                        `json:"identified"`
	IdentityStatus string                      `json:"identityStatus,omitempty"`
	IdentityDetail string                      `json:"identityDetail,omitempty"`
	CaptchaWord    string                      `json:"captchaWord,omitempty"`
	CaptchaPrompt  string                      `json:"captchaPrompt,omitempty"`
	Profiles       map[string]UserProfile      `json:"profiles,omitempty"`
	Conversations  []conversation.Conversation `json:"conversations"`
}

// UserProfile is the small, server-provided part of a user's WHOIS record.
// The renderer combines it with locally known message/presence history.
type UserProfile struct {
	Nick        string    `json:"nick"`
	Username    string    `json:"username,omitempty"`
	Host        string    `json:"host,omitempty"`
	Realname    string    `json:"realname,omitempty"`
	Channels    []string  `json:"channels,omitempty"`
	Operator    bool      `json:"operator,omitempty"`
	Online      bool      `json:"online"`
	Loading     bool      `json:"loading,omitempty"`
	Error       string    `json:"error,omitempty"`
	IdleSeconds int       `json:"idleSeconds,omitempty"`
	LastActive  time.Time `json:"lastActive,omitempty"`
	SignOn      time.Time `json:"signOn,omitempty"`
}

type Bootstrap struct {
	Snapshot  ClientSnapshot   `json:"snapshot"`
	Settings  persist.Settings `json:"settings"`
	Platform  string           `json:"platform"`
	Transfers []TransferUpdate `json:"transfers"`
}

// Options configures a client core for one platform shell.
type Options struct {
	// Host provides the platform services. Nil selects NopHost.
	Host Host
	// Platform is reported to the frontend; see the Platform constants.
	Platform string
	// ConfigDir is where non-secret settings are stored. Empty selects the
	// per-user configuration directory of the operating system.
	ConfigDir string
}

type attachmentMeta struct {
	Sender string
	Target string
	FileID string
	Name   string
	MIME   string
	Size   int64
}

type historyRequest struct {
	Until     time.Time
	Remaining int
}

// App is the platform-independent IRCapybara client. Every exported method is
// part of the frontend API: the desktop shell binds them through Wails and the
// mobile shell dispatches to them by name, so only add exported methods that
// are safe to call from the UI.
type App struct {
	ctx      context.Context
	cancel   context.CancelFunc
	host     Host
	platform string

	mu                   sync.Mutex
	client               *transport.Client
	generation           uint64
	sess                 *session.Store
	convs                *conversation.Store
	prefs                *persist.Store
	transfers            *xfer.Manager
	cfg                  ConnectionConfig
	extendedSent         bool
	joined               bool
	registrationFinished bool
	identifySent         bool
	pendingAutoJoin      []string
	coreAdvertised       bool
	xferAdvertised       bool
	xferVersion          int
	namesMembers         map[string][]conversation.Member
	listRequested        bool
	capEnded             bool
	capLSComplete        bool
	capOffered           map[string]bool
	historyPending       map[string]historyRequest
	profiles             map[string]UserProfile

	attachMu    sync.RWMutex
	attachments map[string]attachmentMeta

	traceMu sync.RWMutex
	trace   []ProtocolTraceEntry

	transferMu       sync.RWMutex
	transferStates   map[string]TransferUpdate
	transferPercents map[string]int
	transferWarnings map[string]bool
	published        map[string]string

	msgSeq      atomic.Uint64
	traceSeq    atomic.Uint64
	lastInbound atomic.Int64
}

func New(opts Options) *App {
	var p *persist.Store
	if opts.ConfigDir != "" {
		p, _ = persist.NewAt(opts.ConfigDir)
	} else {
		p, _ = persist.New()
	}
	host := opts.Host
	if host == nil {
		host = NopHost{}
	}
	platform := opts.Platform
	if platform == "" {
		platform = PlatformDesktop
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &App{
		ctx:              ctx,
		cancel:           cancel,
		host:             host,
		platform:         platform,
		sess:             session.NewStore(),
		convs:            conversation.NewStore(300),
		prefs:            p,
		transfers:        xfer.New(xfer.DefaultMaxFileSize),
		attachments:      map[string]attachmentMeta{},
		namesMembers:     map[string][]conversation.Member{},
		capOffered:       map[string]bool{},
		historyPending:   map[string]historyRequest{},
		profiles:         map[string]UserProfile{},
		transferStates:   map[string]TransferUpdate{},
		transferPercents: map[string]int{},
		transferWarnings: map[string]bool{},
		published:        map[string]string{},
	}
}

// Shutdown closes the connection and releases transfer state. The App must not
// be used afterwards.
func (a *App) Shutdown() {
	a.mu.Lock()
	c := a.client
	a.client = nil
	a.generation++
	a.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
	a.transfers.Reset()
	a.cancel()
}

func (a *App) Bootstrap() Bootstrap {
	var settings persist.Settings
	if a.prefs != nil {
		if got, err := a.prefs.Load(); err == nil {
			settings = got
		}
	}
	if settings.Port == 0 {
		settings.Port = 6697
	}
	if settings.Theme != "dark" && settings.Theme != "light" {
		settings.Theme = "light"
	}
	if len(settings.AutoJoin) == 0 {
		settings.AutoJoin = []string{"#general"}
	}
	if settings.TLSMode == "" {
		settings.TLSMode = persist.TLSModeAllowSelfSigned
	}
	return Bootstrap{Snapshot: a.snapshot(), Settings: settings, Platform: a.platform, Transfers: a.transferSnapshot()}
}

// GetCapybaraAvatars returns the SVG artwork available in either shell.
func (a *App) GetCapybaraAvatars() []string {
	return frontend.CapybaraAvatars()
}

func (a *App) SetTheme(theme string) error {
	theme = strings.ToLower(strings.TrimSpace(theme))
	if theme != "light" && theme != "dark" {
		return errors.New("invalid theme")
	}
	if a.prefs == nil {
		return nil
	}
	return a.prefs.SaveTheme(theme)
}

func (a *App) PrepareSystemNotifications() (bool, error) {
	return a.host.PrepareNotifications()
}

func (a *App) SendSystemNotification(title, subtitle, body string, playSound bool) error {
	title = strings.TrimSpace(title)
	subtitle = strings.TrimSpace(subtitle)
	body = strings.TrimSpace(body)
	if title == "" {
		title = "IRCapybara"
	}
	return a.host.Notify(title, subtitle, body, playSound)
}

func (a *App) PickTLSCertificate() (string, error) {
	return a.host.ChooseCertificate()
}

func (a *App) Connect(cfg ConnectionConfig) error {
	cfg.Host = strings.TrimSpace(cfg.Host)
	cfg.Nick = strings.TrimSpace(cfg.Nick)
	cfg.DisplayName = strings.TrimSpace(cfg.DisplayName)
	cfg.ServerLabel = strings.TrimSpace(cfg.ServerLabel)
	cfg.IdentityNick = strings.TrimSpace(cfg.IdentityNick)
	cfg.PinnedCertificatePath = strings.TrimSpace(cfg.PinnedCertificatePath)
	if cfg.Port == 0 {
		cfg.Port = 6697
	}
	if cfg.Host == "" {
		return errors.New("server host is required")
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return errors.New("invalid server port")
	}
	if !validIRCAtom(cfg.Nick) {
		return errors.New("nickname contains invalid characters")
	}
	if cfg.DisplayName == "" {
		cfg.DisplayName = cfg.Nick
	}
	if strings.ContainsAny(cfg.DisplayName, "\r\n") {
		return errors.New("display name contains newline")
	}
	if strings.ContainsAny(cfg.ServerPassword, "\r\n") {
		return errors.New("server password contains newline")
	}
	if cfg.IdentityPassword != "" && !validCredentialArg(cfg.IdentityPassword) {
		return errors.New("identity password cannot contain spaces or control characters")
	}
	if cfg.IdentityNick != "" && !validIRCAtom(cfg.IdentityNick) {
		return errors.New("identity nickname contains invalid characters")
	}
	if strings.ContainsAny(cfg.ServerLabel, "\r\n") {
		return errors.New("server display name contains newline")
	}
	if cfg.PinServerCertificate && cfg.PinnedCertificatePath == "" {
		return errors.New("choose a server certificate or public key to pin")
	}

	var pinnedCertificate []byte
	if cfg.PinServerCertificate {
		var err error
		pinnedCertificate, err = os.ReadFile(cfg.PinnedCertificatePath)
		if err != nil {
			return fmt.Errorf("read pinned TLS certificate/public key: %w", err)
		}
	}
	cfg.AutoJoin = autoJoinTargets(cfg.AutoJoin)

	a.mu.Lock()
	previousCfg := a.cfg
	if a.client != nil {
		a.mu.Unlock()
		return errors.New("already connected")
	}
	a.mu.Unlock()
	preserveTransfers := strings.EqualFold(previousCfg.Host, cfg.Host) && previousCfg.Port == cfg.Port && strings.EqualFold(previousCfg.Nick, cfg.Nick) && a.transfers.HasResumableDownloads()

	a.mu.Lock()
	c := transport.New()
	a.client = c
	a.cfg = cfg
	a.extendedSent = false
	a.joined = false
	a.registrationFinished = false
	a.identifySent = false
	a.pendingAutoJoin = nil
	a.coreAdvertised = false
	a.xferAdvertised = false
	a.xferVersion = 0
	a.namesMembers = map[string][]conversation.Member{}
	a.listRequested = false
	a.capEnded = false
	a.capLSComplete = false
	a.capOffered = map[string]bool{}
	a.historyPending = map[string]historyRequest{}
	a.profiles = map[string]UserProfile{}
	gen := a.generation + 1
	a.generation = gen
	a.mu.Unlock()

	if !preserveTransfers {
		a.transfers.Reset()
		a.resetTransferStates()
		a.attachMu.Lock()
		a.attachments = map[string]attachmentMeta{}
		a.attachMu.Unlock()
		a.convs.Reset("Connecting to your IRCapybara server…")
	} else {
		a.addStatus("Network", "Reconnecting; partial attachment downloads will resume when transfer support is available.")
	}
	a.resetProtocolTrace()
	a.sess.Reset(cfg.Nick, serverDisplay(cfg))
	a.sess.SetState(session.Connecting, "Opening connection")
	a.emitState()
	if cfg.PinServerCertificate {
		a.sess.SetState(session.TLS, "Verifying the pinned server certificate")
	} else if cfg.AllowSelfSigned {
		a.sess.SetState(session.TLS, "Opening encrypted TLS connection (certificate verification disabled)")
	} else {
		a.sess.SetState(session.TLS, "Verifying the TLS connection")
	}
	a.emitState()

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	if err := c.Connect(a.ctx, transport.Config{
		Address:              addr,
		AllowInsecureTLS:     cfg.AllowSelfSigned && !cfg.PinServerCertificate,
		PinnedCertificatePEM: pinnedCertificate,
	}); err != nil {
		a.mu.Lock()
		if a.client == c {
			a.client = nil
		}
		a.mu.Unlock()
		a.sess.SetState(session.Disconnected, err.Error())
		a.emitState()
		return err
	}

	a.sess.SetState(session.CAP, "Waiting for the server capability list")
	a.emitState()

	// Start consuming before registration commands are written. Some local IRC
	// servers answer CAP LS immediately and a bounded event queue should never be
	// the thing deciding whether registration succeeds.
	go a.consume(c, gen)
	go a.transferRecoveryLoop(c, gen)

	lines := []string{"CAP LS 302"}
	if cfg.ServerPassword != "" {
		lines = append(lines, "PASS :"+cfg.ServerPassword)
	}
	lines = append(lines,
		"NICK "+cfg.Nick,
		"USER "+cfg.Nick+" 0 * :"+cfg.DisplayName,
	)
	for _, line := range lines {
		if err := a.sendOn(c, line); err != nil {
			_ = c.Close()
			a.mu.Lock()
			if a.client == c {
				a.client = nil
			}
			a.mu.Unlock()
			a.sess.SetState(session.Disconnected, err.Error())
			a.emitState()
			return err
		}
	}

	a.mu.Lock()
	a.cfg.ServerPassword = ""
	a.mu.Unlock()

	// CAP is negotiated from the server's LS reply rather than ending it before
	// the reply arrives. A short fallback prevents a broken/non-CAP server from
	// leaving the UI at "Negotiating" forever.
	go a.capNegotiationFallback(c, gen)

	if a.prefs != nil {
		theme := "light"
		if current, err := a.prefs.Load(); err == nil && (current.Theme == "light" || current.Theme == "dark") {
			theme = current.Theme
		}
		tlsMode := persist.TLSModeSystem
		if cfg.PinServerCertificate {
			tlsMode = persist.TLSModePinned
		} else if cfg.AllowSelfSigned {
			tlsMode = persist.TLSModeAllowSelfSigned
		}
		_ = a.prefs.Save(persist.Settings{
			Host: cfg.Host, Port: cfg.Port, Nick: cfg.Nick, DisplayName: cfg.DisplayName,
			ServerLabel: cfg.ServerLabel, AutoJoin: cfg.AutoJoin, Theme: theme, TLSMode: tlsMode,
			PinnedCertificatePath: cfg.PinnedCertificatePath,
		})
	}
	return nil
}

func (a *App) Disconnect() error {
	a.mu.Lock()
	c := a.client
	a.client = nil
	a.generation++
	a.registrationFinished = false
	a.identifySent = false
	a.pendingAutoJoin = nil
	a.historyPending = map[string]historyRequest{}
	a.capEnded = false
	a.cfg.ServerPassword = ""
	a.cfg.IdentityPassword = ""
	a.mu.Unlock()
	if c != nil {
		_ = a.sendOn(c, "QUIT :Leaving")
		_ = c.Close()
	}
	a.interruptActiveDownloads("Connection closed")
	a.convs.MarkDisconnected()
	a.sess.SetState(session.Disconnected, "")
	a.sess.SetRegistered(false)
	a.sess.SetIdentityStatus("none", "")
	a.sess.SetExtended(false)
	a.sess.ClearCaptcha()
	a.emitState()
	return nil
}

func (a *App) SendMessage(target, text string) error {
	snap := a.sess.Snapshot()
	if snap.State != session.Ready {
		if snap.State == session.ReadOnly || snap.State == session.GuardReadOnly {
			return errors.New("the server has restricted this session to read-only access")
		}
		return errors.New("messaging is not available until the connection is ready")
	}
	target = strings.TrimSpace(target)
	if !validIRCAtom(target) {
		return errors.New("invalid conversation target")
	}
	if text == "" {
		return errors.New("message is empty")
	}
	if utf8.RuneCountInString(text) > 32768 {
		return errors.New("message exceeds 32,768 Unicode characters")
	}
	line := "PRIVMSG " + target + " :" + protocol.EncodeMessageText(text)
	if len(line)+2 > transport.MaxLineBytes {
		return errors.New("encoded IRC line exceeds transport limit")
	}
	if err := a.send(line); err != nil {
		return err
	}

	nick := snap.Nick
	kind := conversationKind(target, false)
	a.convs.AddMessage(target, displayTarget(target), kind, conversation.Message{
		ID: a.nextID(), Target: target, Sender: nick, Text: text, Time: time.Now(),
	})
	a.emitState()
	return nil
}

func (a *App) Join(target string) error {
	target = strings.TrimSpace(target)
	if !strings.HasPrefix(target, "#") || !validIRCAtom(target) {
		return errors.New("enter a valid channel such as #general")
	}
	return a.send("JOIN " + target)
}

func (a *App) OpenDirectMessage(target string) error {
	target = strings.TrimSpace(target)
	if !validIRCAtom(target) || strings.HasPrefix(target, "#") {
		return errors.New("invalid direct-message target")
	}
	a.convs.Ensure(target, target, conversation.VirtualDM)
	a.emitState()
	return nil
}

func (a *App) CloseConversation(target string) error {
	target = strings.TrimSpace(target)
	if target == "" || strings.EqualFold(target, "status") || !validIRCAtom(target) {
		return errors.New("invalid conversation")
	}
	conv, ok := a.convs.Get(target)
	if !ok {
		return nil
	}
	if strings.HasPrefix(target, "#") && conv.Joined {
		if err := a.send("PART " + target + " :Closed from client"); err != nil {
			return err
		}
		a.forgetJoinedChannel(target)
	}
	a.convs.Remove(target)
	a.emitState()
	return nil
}

func (a *App) RefreshChannelList() error {
	if !a.sess.Snapshot().Registered {
		return errors.New("connect to the server first")
	}
	return a.send("LIST")
}

func (a *App) RequestMembers(target string) error {
	target = strings.TrimSpace(target)
	if !strings.HasPrefix(target, "#") || !validIRCAtom(target) {
		return errors.New("invalid channel")
	}
	if !a.sess.Snapshot().Registered {
		return errors.New("connect to the server first")
	}
	a.mu.Lock()
	delete(a.namesMembers, strings.ToLower(target))
	a.mu.Unlock()
	return a.send("NAMES " + target)
}

// RequestProfile asks the server for the live WHOIS record for a nickname.
// The result arrives asynchronously as standard IRC numerics and is included
// in the next client snapshot.
func (a *App) RequestProfile(nick string) error {
	nick = strings.TrimSpace(nick)
	if !validIRCAtom(nick) || strings.HasPrefix(nick, "#") {
		return errors.New("invalid nickname")
	}
	if !a.sess.Snapshot().Registered {
		return errors.New("connect to the server first")
	}
	a.mu.Lock()
	if a.profiles == nil {
		a.profiles = map[string]UserProfile{}
	}
	a.profiles[strings.ToLower(nick)] = UserProfile{Nick: nick, Loading: true}
	a.mu.Unlock()
	if err := a.send("WHOIS " + nick); err != nil {
		a.mu.Lock()
		profile := a.profiles[strings.ToLower(nick)]
		profile.Nick, profile.Loading, profile.Error = nick, false, err.Error()
		a.profiles[strings.ToLower(nick)] = profile
		a.mu.Unlock()
		a.emitState()
		return err
	}
	a.emitState()
	return nil
}

func (a *App) updateProfile(nick string, update func(*UserProfile)) {
	nick = strings.TrimSpace(nick)
	if nick == "" || update == nil {
		return
	}
	key := strings.ToLower(nick)
	a.mu.Lock()
	if a.profiles == nil {
		a.profiles = map[string]UserProfile{}
	}
	profile := a.profiles[key]
	if profile.Nick == "" {
		profile.Nick = nick
	}
	update(&profile)
	a.profiles[key] = profile
	a.mu.Unlock()
	a.emitState()
}

func (a *App) GetProtocolTrace() []ProtocolTraceEntry {
	return a.protocolTraceSnapshot()
}

func (a *App) Identify(password string) error {
	line, err := identifyCommand("", password)
	if err != nil {
		return err
	}
	if err := a.send(line); err != nil {
		return err
	}
	a.mu.Lock()
	a.identifySent = true
	a.mu.Unlock()
	a.sess.SetIdentityStatus("pending", "Waiting for identity confirmation from the server")
	a.emitState()
	a.startIdentityConfirmationTimeout()
	return nil
}

func (a *App) IdentifyAs(nick, password string) error {
	line, err := identifyCommand(nick, password)
	if err != nil {
		return err
	}
	if err := a.send(line); err != nil {
		return err
	}
	a.mu.Lock()
	a.identifySent = true
	a.mu.Unlock()
	a.sess.SetIdentityStatus("pending", "Waiting for identity confirmation from the server")
	a.emitState()
	a.startIdentityConfirmationTimeout()
	return nil
}

func (a *App) SolveCaptcha(word, answer string) error {
	word, answer = strings.TrimSpace(word), strings.TrimSpace(answer)
	if !validIRCAtom(word) || !validIRCAtom(answer) {
		return errors.New("invalid captcha response")
	}
	return a.send("CAPTCHA " + word + " " + answer)
}

func (a *App) RequestHistory(target string, limit int) error {
	if !validIRCAtom(target) {
		return errors.New("invalid target")
	}
	count := limit
	if count == 0 {
		count = 20
	}
	if count < 1 || count > 100 {
		return errors.New("history limit must be 1–100")
	}
	a.mu.Lock()
	if a.historyPending == nil {
		a.historyPending = map[string]historyRequest{}
	}
	a.historyPending[strings.ToLower(target)] = historyRequest{Until: time.Now().Add(1500 * time.Millisecond), Remaining: count}
	a.mu.Unlock()
	line := "HISTORY " + target
	if limit != 0 {
		line = fmt.Sprintf("HISTORY %s %d", target, limit)
	}
	if err := a.send(line); err != nil {
		a.mu.Lock()
		delete(a.historyPending, strings.ToLower(target))
		a.mu.Unlock()
		return err
	}
	return nil
}

func (a *App) RequestHelp(topic string) error {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return a.send("HELP")
	}
	if !validIRCAtom(topic) {
		return errors.New("invalid help topic")
	}
	return a.send("HELP " + strings.ToUpper(topic))
}

func (a *App) ResetAI() error {
	if a.sess.Snapshot().State != session.Ready {
		return errors.New("this session is currently read-only")
	}
	return a.send("AIRESET")
}

func (a *App) PickAndUpload(target string) error {
	snap := a.sess.Snapshot()
	if snap.State != session.Ready {
		return errors.New("this session is currently read-only")
	}
	if !snap.Extended {
		return errors.New("attachments require IRCapybara extended mode")
	}
	if !validIRCAtom(target) {
		return errors.New("invalid upload target")
	}
	picked, ok, err := a.host.ChooseUpload()
	if err != nil || !ok || picked.Path == "" {
		return err
	}
	path := picked.Path
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("only regular files can be uploaded")
	}
	if st.Size() > xfer.DefaultMaxFileSize {
		return fmt.Errorf("file exceeds 15 MiB limit")
	}
	name := strings.TrimSpace(picked.Name)
	if name == "" {
		name = filepath.Base(path)
	}
	name = xfer.SanitizeFilename(name)
	mt := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if mt == "" {
		mt = strings.TrimSpace(picked.MIME)
	}
	if semi := strings.IndexByte(mt, ';'); semi >= 0 {
		mt = mt[:semi]
	}
	if !validMIMEToken(mt) {
		mt = "application/octet-stream"
	}
	u := xfer.PendingUpload{Target: target, Path: path, Name: name, MIME: mt, Size: st.Size()}
	if err := a.transfers.QueueUpload(u); err != nil {
		return err
	}
	if err := a.send(fmt.Sprintf("XFER START %s %d %s :%s", target, st.Size(), mt, name)); err != nil {
		a.transfers.CancelUpload(path)
		return err
	}
	return nil
}

func (a *App) DownloadAttachment(fileID string) error {
	sessionSnapshot := a.sess.Snapshot()
	if sessionSnapshot.State == session.Disconnected {
		return errors.New("reconnect before downloading this attachment")
	}
	if !sessionSnapshot.Extended {
		return errors.New("attachment downloads require IRCapybara extended mode")
	}
	a.attachMu.RLock()
	meta, ok := a.attachments[fileID]
	a.attachMu.RUnlock()
	if !ok {
		return errors.New("unknown attachment")
	}
	name := xfer.SanitizeFilename(meta.Name)
	path, err := a.host.ChooseDownloadPath(fileID, name, meta.MIME)
	if err != nil || path == "" {
		return err
	}
	a.forgetPublished(fileID)
	if err := a.transfers.BeginDownload(fileID, meta.Size, name, meta.MIME, path); err != nil {
		return err
	}
	a.setTransferUpdate(TransferUpdate{FileID: fileID, Status: string(xfer.StatusQueued), Total: meta.Size, Percent: 0})
	if err := a.send("RECV " + fileID); err != nil {
		a.transfers.CancelDownload(fileID)
		a.failDownload(fileID, meta.Size, err)
		return err
	}
	return nil
}

func (a *App) RetryDownload(fileID string, fullRestart bool) error {
	sessionSnapshot := a.sess.Snapshot()
	if sessionSnapshot.State == session.Disconnected {
		return errors.New("reconnect before retrying this attachment")
	}
	if !sessionSnapshot.Extended {
		return errors.New("attachment downloads require IRCapybara extended mode")
	}
	snapshot, ok := a.transfers.DownloadSnapshot(fileID)
	if !ok {
		// If resumable state is gone, fall back to the normal flow and ask for a
		// save destination again.
		return a.DownloadAttachment(fileID)
	}
	if snapshot.Complete {
		return nil
	}
	if !fullRestart && snapshot.VerifiedSHA256 != "" {
		// Verification already succeeded; a previous local finalisation/ACK path
		// was interrupted. RECV ACK is idempotent and the staged file can be
		// promoted without retransmitting content.
		if err := a.send("RECV ACK " + fileID + " " + snapshot.VerifiedSHA256); err != nil {
			return err
		}
		complete, err := a.transfers.MarkAckSent(fileID, snapshot.VerifiedSHA256)
		if err != nil {
			return err
		}
		a.completeDownload(fileID, complete.Size, complete.SavePath)
		return nil
	}
	if fullRestart || snapshot.ChunkSize <= 0 || snapshot.TotalParts <= 0 {
		fresh, err := a.transfers.PrepareFullRestart(fileID)
		if err != nil {
			return err
		}
		a.emitDownloadSnapshot(fresh, "")
		return a.send("RECV " + fileID)
	}
	// Prefer a bitmap-based retry whenever the transfer geometry is still known.
	// Even a hash-mismatch retry can use this: all received bits were cleared, so
	// the missing range simply covers the whole file without discarding metadata.
	return a.requestMissingDownload(fileID, false)
}

func (a *App) DownloadedFileExists(fileID string) bool {
	state, ok := a.transferState(fileID)
	if !ok || state.Status != "complete" || strings.TrimSpace(state.Path) == "" {
		return false
	}
	if !a.host.FileExists(state.Path) {
		a.forgetPublished(fileID)
		a.setTransferUpdate(TransferUpdate{FileID: fileID, Status: "available", Total: state.Total})
		return false
	}
	return true
}

func (a *App) RevealDownloadedFile(fileID string) error {
	state, ok := a.transferState(fileID)
	if !ok || state.Status != "complete" || strings.TrimSpace(state.Path) == "" {
		return errors.New("this attachment has not been downloaded yet")
	}
	if !a.host.FileExists(state.Path) {
		a.forgetPublished(fileID)
		a.setTransferUpdate(TransferUpdate{FileID: fileID, Status: "available", Total: state.Total})
		return errors.New("the downloaded file is no longer at its saved location")
	}
	mt := ""
	a.attachMu.RLock()
	if meta, ok := a.attachments[fileID]; ok {
		mt = meta.MIME
	}
	a.attachMu.RUnlock()
	return a.host.RevealFile(state.Path, mt)
}

// CheckConnection probes a connection that may have died silently, for example
// after the device changed networks or woke from sleep. If the server does not
// answer in time the session is dropped so the UI can offer a reconnect.
func (a *App) CheckConnection() error {
	a.mu.Lock()
	c := a.client
	gen := a.generation
	a.mu.Unlock()
	if c == nil {
		return nil
	}
	sent := time.Now().UnixNano()
	if err := a.sendOn(c, "PING :ircapybara-probe"); err != nil {
		a.dropConnection(c, gen, "Connection lost")
		return nil
	}
	timeout := probeTimeout
	go func() {
		select {
		case <-time.After(timeout):
		case <-c.Done():
			return
		}
		if a.lastInbound.Load() < sent {
			a.dropConnection(c, gen, "Connection lost: the server stopped responding")
		}
	}()
	return nil
}

// A variable so tests can shorten it.
var probeTimeout = 15 * time.Second

func (a *App) send(line string) error {
	a.mu.Lock()
	c := a.client
	a.mu.Unlock()
	if c == nil {
		return errors.New("not connected")
	}
	return a.sendOn(c, line)
}

func (a *App) sendOn(c *transport.Client, line string) error {
	if c == nil {
		return errors.New("not connected")
	}
	a.traceProtocol("out", redactProtocolLine(line))
	return c.Send(line)
}

func (a *App) consume(c *transport.Client, gen uint64) {
	for {
		select {
		case ev := <-c.Events():
			if !a.dispatch(c, gen, ev) {
				return
			}
		case <-c.Done():
			// The reader queues its final events before Done closes, so drain
			// them in order. If the transport was closed locally the session
			// state is already updated and dropConnection is a no-op.
			for {
				select {
				case ev := <-c.Events():
					if !a.dispatch(c, gen, ev) {
						return
					}
				default:
					a.dropConnection(c, gen, "Connection closed")
					return
				}
			}
		}
	}
}

// dispatch handles one transport event and reports whether the consumer should
// keep reading.
func (a *App) dispatch(c *transport.Client, gen uint64, ev transport.Event) bool {
	a.mu.Lock()
	current := a.client == c && a.generation == gen
	a.mu.Unlock()
	if !current {
		return false
	}
	switch ev.Type {
	case transport.EventLine:
		a.handleLine(ev.Line)
	case transport.EventError:
		a.addStatus("Network", ev.Err.Error())
	case transport.EventDisconnected:
		a.dropConnection(c, gen, "Connection closed")
		return false
	}
	return true
}

// dropConnection tears down the session state for a connection that ended
// without the user asking for it. It is a no-op when c is no longer current.
func (a *App) dropConnection(c *transport.Client, gen uint64, reason string) {
	a.mu.Lock()
	if a.client != c || a.generation != gen {
		a.mu.Unlock()
		return
	}
	a.client = nil
	a.generation++
	a.registrationFinished = false
	a.identifySent = false
	a.pendingAutoJoin = nil
	a.historyPending = map[string]historyRequest{}
	a.cfg.ServerPassword = ""
	a.cfg.IdentityPassword = ""
	a.mu.Unlock()
	_ = c.Close()
	a.interruptActiveDownloads(reason)
	a.convs.MarkDisconnected()
	a.sess.SetState(session.Disconnected, reason)
	a.sess.SetRegistered(false)
	a.sess.SetIdentityStatus("none", "")
	a.sess.SetExtended(false)
	a.sess.ClearCaptcha()
	a.emitState()
}

func (a *App) handleLine(line string) {
	a.lastInbound.Store(time.Now().UnixNano())
	a.traceProtocol("in", redactProtocolLine(line))
	msg, err := protocol.Parse(line)
	if err != nil {
		a.addStatus("Protocol", "Ignored malformed server line")
		return
	}

	switch msg.Command {
	case "PING":
		if len(msg.Params) > 0 {
			_ = a.send("PONG :" + msg.Trailing())
		}
	case "CAP":
		a.handleCAP(msg)
	case "001":
		// IRC registration is complete at 001. Waiting for MOTD completion
		// (376/422) caused otherwise-valid servers to sit at Negotiating forever.
		a.mu.Lock()
		a.registrationFinished = true
		// A server that already emitted 001 has completed registration even if
		// its CAP implementation is incomplete. Stop the timeout fallback.
		a.capEnded = true
		needList := !a.listRequested
		if needList {
			a.listRequested = true
		}
		gen := a.generation
		a.mu.Unlock()
		previousState := a.sess.Snapshot().State
		a.sess.SetRegistered(true)
		if previousState != session.ReadOnly && previousState != session.GuardReadOnly && previousState != session.Captcha {
			a.sess.SetState(session.Ready, "Connected")
		}
		if needList {
			if err := a.send("LIST"); err != nil {
				a.addStatus("Channels", "Could not load the channel list: "+err.Error())
			}
		}
		a.emitState()
		go a.delayedAutoJoin(gen)
		if a.sess.Snapshot().Extended {
			go a.resumeInterruptedDownloads(gen)
		}
	case "376", "422":
		// MOTD completion is useful server output, but not a registration gate.
	case "005":
		a.mu.Lock()
		for _, p := range msg.Params {
			if p == "IRCAPYBARA=1" {
				a.coreAdvertised = true
			}
			if strings.HasPrefix(p, "IRCAPYBARA-XFER=") {
				version, err := strconv.Atoi(strings.TrimPrefix(p, "IRCAPYBARA-XFER="))
				if err == nil && version >= 1 {
					a.xferAdvertised = true
					if version > a.xferVersion {
						a.xferVersion = version
					}
				}
			}
		}
		shouldEnable := a.coreAdvertised && a.xferAdvertised && !a.extendedSent
		if shouldEnable {
			a.extendedSent = true
		}
		gen := a.generation
		a.mu.Unlock()
		if shouldEnable {
			if err := a.send("XMODE EXTENDED"); err != nil {
				a.addStatus("Protocol", "Could not enable extended mode: "+err.Error())
			} else {
				a.sess.SetExtended(true)
				a.emitState()
				go a.resumeInterruptedDownloads(gen)
			}
		}
	case "XMODE":
		if len(msg.Params) > 0 && strings.EqualFold(msg.Params[0], "EXTENDED") {
			a.sess.SetExtended(true)
			a.emitState()
			a.mu.Lock()
			gen := a.generation
			a.mu.Unlock()
			go a.resumeInterruptedDownloads(gen)
		}
	case "CAPTCHA":
		a.handleCaptcha(msg)
	case "IDENTIFY":
		a.handleIdentifyReply(msg)
	case "JOIN":
		if msg.Prefix != nil && len(msg.Params) > 0 {
			target := msg.Params[0]
			if strings.HasPrefix(target, "#") {
				a.convs.Ensure(target, displayTarget(target), conversation.PublicChannel)
				a.convs.UpdateMemberPresence(target, msg.Prefix.Name, true, false)
				a.convs.AddMessage(target, displayTarget(target), conversation.PublicChannel, conversation.Message{
					ID: a.nextID(), Target: target, Sender: msg.Prefix.Name, Text: "joined the channel", Time: time.Now(), System: true,
				})
				if strings.EqualFold(msg.Prefix.Name, a.sess.Snapshot().Nick) {
					a.convs.SetJoined(target, true)
					a.rememberJoinedChannel(target)
					_ = a.send("NAMES " + target)
					if strings.EqualFold(target, "#general") {
						a.flushPendingAutoJoin()
					}
				}
				a.emitState()
			}
		}
	case "PART":
		if msg.Prefix != nil && len(msg.Params) > 0 {
			target := msg.Params[0]
			if strings.HasPrefix(target, "#") {
				a.convs.Ensure(target, displayTarget(target), conversation.PublicChannel)
			}
			a.convs.UpdateMemberPresence(target, msg.Prefix.Name, false, false)
			if strings.HasPrefix(target, "#") {
				a.convs.AddMessage(target, displayTarget(target), conversation.PublicChannel, conversation.Message{
					ID: a.nextID(), Target: target, Sender: msg.Prefix.Name, Text: "left the channel", Time: time.Now(), System: true,
				})
			}
			if strings.EqualFold(msg.Prefix.Name, a.sess.Snapshot().Nick) {
				if _, ok := a.convs.Get(target); ok {
					a.convs.SetJoined(target, false)
				}
				a.forgetJoinedChannel(target)
			}
			a.emitState()
		}
	case "QUIT":
		if msg.Prefix != nil {
			a.convs.RemoveMemberEverywhere(msg.Prefix.Name)
			a.emitState()
		}
	case "NICK":
		if msg.Prefix != nil && len(msg.Params) > 0 {
			newNick := msg.Params[0]
			a.convs.RenameMember(msg.Prefix.Name, newNick)
			if strings.EqualFold(msg.Prefix.Name, a.sess.Snapshot().Nick) {
				a.sess.SetIdentity(newNick, a.sess.Snapshot().Server)
			}
			a.emitState()
		}
	case "KICK":
		if len(msg.Params) >= 2 {
			target, nick := msg.Params[0], msg.Params[1]
			a.convs.UpdateMemberPresence(target, nick, false, false)
			if strings.EqualFold(nick, a.sess.Snapshot().Nick) {
				if _, ok := a.convs.Get(target); ok {
					a.convs.SetJoined(target, false)
				}
				a.forgetJoinedChannel(target)
			}
			a.emitState()
		}
	case "311":
		// RPL_WHOISUSER: <me> <nick> <user> <host> * :<real name>
		if len(msg.Params) >= 5 {
			nick := msg.Params[1]
			a.updateProfile(nick, func(profile *UserProfile) {
				profile.Username = msg.Params[2]
				profile.Host = msg.Params[3]
				profile.Realname = msg.Trailing()
				profile.Online = true
				profile.Error = ""
			})
		}
	case "313":
		// RPL_WHOISOPERATOR: <me> <nick> :operator description
		if len(msg.Params) >= 2 {
			a.updateProfile(msg.Params[1], func(profile *UserProfile) {
				profile.Operator = true
				profile.Online = true
			})
		}
	case "317":
		// RPL_WHOISIDLE: <me> <nick> <seconds idle> <sign-on unix time>
		if len(msg.Params) >= 4 {
			nick := msg.Params[1]
			idle, idleErr := strconv.Atoi(msg.Params[2])
			signon, signonErr := strconv.ParseInt(msg.Params[3], 10, 64)
			a.updateProfile(nick, func(profile *UserProfile) {
				if idleErr == nil && idle >= 0 {
					profile.IdleSeconds = idle
					profile.LastActive = time.Now().Add(-time.Duration(idle) * time.Second)
				}
				if signonErr == nil && signon > 0 {
					profile.SignOn = time.Unix(signon, 0)
				}
				profile.Online = true
			})
		}
	case "318":
		// RPL_ENDOFWHOIS: the live profile request is complete.
		if len(msg.Params) >= 2 {
			a.updateProfile(msg.Params[1], func(profile *UserProfile) { profile.Loading = false })
		}
	case "319":
		// RPL_WHOISCHANNELS: <me> <nick> :[@]#channel ...
		if len(msg.Params) >= 2 {
			a.updateProfile(msg.Params[1], func(profile *UserProfile) {
				for _, channel := range strings.Fields(msg.Trailing()) {
					channel = strings.TrimPrefix(channel, "@")
					if channel != "" && !containsFold(profile.Channels, channel) {
						profile.Channels = append(profile.Channels, channel)
					}
				}
			})
		}
	case "401":
		// RPL_NOSUCHNICK: <me> <nick> :description
		if len(msg.Params) >= 2 {
			a.updateProfile(msg.Params[1], func(profile *UserProfile) {
				profile.Loading = false
				profile.Online = false
				profile.Error = msg.Trailing()
			})
		}
	case "321":
		// LIST header.
	case "322":
		// RPL_LIST: <me> <channel> <visible-users> :<topic>
		if len(msg.Params) >= 3 && strings.HasPrefix(msg.Params[1], "#") {
			members, _ := strconv.Atoi(msg.Params[2])
			topic := ""
			if len(msg.Params) >= 4 {
				topic = msg.Params[3]
			}
			a.convs.EnsureCatalogued(msg.Params[1], displayTarget(msg.Params[1]), conversation.PublicChannel, members, topic)
			a.emitState()
		}
	case "323":
		a.emitState()
	case "PRIVMSG":
		a.handlePrivmsg(msg)
	case "NOTICE":
		a.handleNotice(msg)
	case "XATTACH":
		a.handleAttachment(msg)
	case "XFER":
		a.handleXfer(msg)
	case "MODE":
		if len(msg.Params) >= 2 && strings.HasPrefix(msg.Params[0], "#") {
			a.applyChannelModes(msg.Params[0], msg.Params[1])
		}
	case "324":
		if len(msg.Params) >= 3 && strings.HasPrefix(msg.Params[1], "#") {
			a.applyChannelModes(msg.Params[1], msg.Params[2])
		}
	case "332":
		if len(msg.Params) >= 3 {
			a.convs.SetTopic(msg.Params[1], msg.Params[2])
			a.emitState()
		}
	case "353":
		if len(msg.Params) >= 4 {
			target := msg.Params[len(msg.Params)-2]
			rawNames := strings.Fields(msg.Params[len(msg.Params)-1])
			members := make([]conversation.Member, 0, len(rawNames))
			for _, raw := range rawNames {
				member, ok := parseNamesMember(raw)
				if ok {
					members = append(members, member)
				}
			}
			a.mu.Lock()
			key := strings.ToLower(target)
			a.namesMembers[key] = append(a.namesMembers[key], members...)
			a.mu.Unlock()
		}
	case "366":
		if len(msg.Params) >= 2 {
			target := msg.Params[1]
			a.mu.Lock()
			key := strings.ToLower(target)
			members := append([]conversation.Member(nil), a.namesMembers[key]...)
			delete(a.namesMembers, key)
			a.mu.Unlock()
			a.convs.SetOnlineMembers(target, members)
			a.emitState()
		}
	case "704", "705", "706":
		a.addStatus("Help", msg.Trailing())
	case "524":
		a.addStatus("Help", "No help exists for that topic.")
	case "403", "405", "471", "473", "474", "475", "476", "477":
		text := msg.Trailing()
		a.applyServerPolicyFromText(text)
		a.addStatus("Server", text)
		for _, p := range msg.Params {
			if strings.EqualFold(p, "#general") {
				a.flushPendingAutoJoin()
				break
			}
		}
	default:
		// Unknown protocol elements remain non-fatal. Still inspect their
		// human-readable parameters for explicit identity/read-only policy so a
		// server can report those states through a custom numeric or command.
		if a.applyServerPolicyFromText(strings.Join(msg.Params, " ")) {
			a.emitState()
		}
		return
	}
}

func (a *App) handleCAP(msg protocol.Message) {
	idx := -1
	sub := ""
	for i, p := range msg.Params {
		u := strings.ToUpper(p)
		if u == "LS" || u == "ACK" || u == "NAK" || u == "NEW" || u == "DEL" {
			idx, sub = i, u
			break
		}
	}
	if idx < 0 {
		return
	}
	switch sub {
	case "LS":
		caps := ""
		if len(msg.Params) > idx+1 {
			caps = msg.Params[len(msg.Params)-1]
		}
		a.mu.Lock()
		for _, capName := range strings.Fields(caps) {
			capName = strings.TrimPrefix(capName, "-")
			if eq := strings.IndexByte(capName, '='); eq >= 0 {
				capName = capName[:eq]
			}
			if capName != "" && capName != "*" {
				a.capOffered[strings.ToLower(capName)] = true
			}
		}
		continuation := len(msg.Params) > idx+1 && msg.Params[idx+1] == "*"
		if !continuation {
			a.capLSComplete = true
		}
		a.mu.Unlock()
		if continuation {
			a.sess.SetState(session.CAP, "Receiving the server capability list")
			a.emitState()
			return
		}
		a.requestPreferredCapabilities()
	case "ACK", "NAK":
		a.endCAPNegotiation()
	}
}

func (a *App) requestPreferredCapabilities() {
	a.mu.Lock()
	if a.capEnded {
		a.mu.Unlock()
		return
	}
	offered := make(map[string]bool, len(a.capOffered))
	for k, v := range a.capOffered {
		offered[k] = v
	}
	a.mu.Unlock()

	wanted := make([]string, 0, 2)
	for _, capName := range []string{"server-time", "ircapybara/history"} {
		if offered[capName] {
			wanted = append(wanted, capName)
		}
	}
	if len(wanted) == 0 {
		a.endCAPNegotiation()
		return
	}
	a.sess.SetState(session.CAP, "Requesting capabilities: "+strings.Join(wanted, ", "))
	a.emitState()
	if err := a.send("CAP REQ :" + strings.Join(wanted, " ")); err != nil {
		a.addStatus("Protocol", "Capability request failed: "+err.Error())
		a.endCAPNegotiation()
	}
}

func (a *App) endCAPNegotiation() {
	a.mu.Lock()
	if a.capEnded {
		a.mu.Unlock()
		return
	}
	a.capEnded = true
	a.mu.Unlock()
	if err := a.send("CAP END"); err != nil {
		a.addStatus("Protocol", "Could not finish capability negotiation: "+err.Error())
		return
	}
	a.sess.SetState(session.CAP, "Capabilities negotiated; waiting for IRC registration")
	a.emitState()
}

func (a *App) capNegotiationFallback(c *transport.Client, gen uint64) {
	time.Sleep(5 * time.Second)
	a.mu.Lock()
	current := a.client == c && a.generation == gen
	ended := a.capEnded
	a.mu.Unlock()
	if !current || ended {
		return
	}
	a.addStatus("Protocol", "Capability negotiation timed out; continuing registration. Open Protocol Log to inspect the exchange.")
	a.endCAPNegotiation()
}

func (a *App) delayedAutoJoin(gen uint64) {
	time.Sleep(400 * time.Millisecond)
	a.mu.Lock()
	current := a.generation == gen && a.client != nil
	a.mu.Unlock()
	if current && a.sess.Snapshot().CaptchaWord == "" {
		a.maybeAutoIdentify()
		a.autoJoin()
	}
}

func (a *App) handleIdentifyReply(msg protocol.Message) {
	if len(msg.Params) == 0 {
		return
	}

	text := strings.TrimSpace(strings.Join(msg.Params, " "))
	before := a.sess.Snapshot()
	if a.applyServerPolicyFromText(text) {
		after := a.sess.Snapshot()
		if !before.Identified && after.Identified {
			a.addStatus("Identity", "Identity authenticated successfully.")
		} else if before.IdentityStatus != "failed" && after.IdentityStatus == "failed" {
			a.addStatus("Identity", after.IdentityDetail)
		} else {
			a.emitState()
		}
		return
	}

	success := false
	failure := false
	for _, param := range msg.Params {
		switch strings.ToUpper(strings.TrimSpace(param)) {
		case "OK", "SUCCESS", "SUCCEEDED", "ACCEPTED", "AUTHENTICATED", "IDENTIFIED":
			success = true
		case "FAIL", "FAILED", "ERROR", "INVALID", "DENIED", "REJECTED":
			failure = true
		}
	}

	switch {
	case success && !failure:
		a.sess.SetIdentified(true)
		if a.sess.Snapshot().State == session.ReadOnly {
			a.sess.SetState(session.Ready, "Connected")
		}
		a.addStatus("Identity", "Identity authenticated successfully.")
	case failure:
		if text == "" {
			text = "Identity authentication failed."
		}
		a.sess.SetIdentityStatus("failed", text)
		a.addStatus("Identity", text)
	default:
		// Keep the request pending, but surface the server reply in Home so the
		// user has visible feedback even for a future/unknown IDENTIFY syntax.
		a.addStatus("Identity", text)
	}
}

func (a *App) startIdentityConfirmationTimeout() {
	a.mu.Lock()
	gen := a.generation
	a.mu.Unlock()
	go func() {
		time.Sleep(8 * time.Second)
		a.mu.Lock()
		current := a.generation == gen && a.client != nil
		a.mu.Unlock()
		snap := a.sess.Snapshot()
		if current && snap.IdentityStatus == "pending" {
			a.sess.SetIdentityStatus("failed", "No identity confirmation was received from the server")
			a.emitState()
		}
	}()
}

func (a *App) applyServerPolicyFromText(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	if strings.Contains(lower, "successfully identified") || strings.Contains(lower, "identified successfully") || strings.Contains(lower, "identification successful") || strings.Contains(lower, "you are now identified") || strings.Contains(lower, "identity authenticated") || strings.Contains(lower, "authentication successful") || strings.Contains(lower, "now logged in") || strings.Contains(lower, "identified as") || strings.Contains(lower, "authenticated as") || strings.Contains(lower, "identity successful") || strings.Contains(lower, "identify successful") || strings.Contains(lower, "identify ok") || lower == "identified" || lower == "authenticated" {
		a.sess.SetIdentified(true)
		if a.sess.Snapshot().State == session.ReadOnly {
			a.sess.SetState(session.Ready, "Connected")
		}
		return true
	}
	identityContext := strings.Contains(lower, "identify") || strings.Contains(lower, "identified") || strings.Contains(lower, "identification") || strings.Contains(lower, "identity") || strings.Contains(lower, "authenticate") || strings.Contains(lower, "authentication")
	pendingIdentity := a.sess.Snapshot().IdentityStatus == "pending"
	identityFailure := strings.Contains(lower, "invalid password") || strings.Contains(lower, "bad password") || strings.Contains(lower, "wrong password") || strings.Contains(lower, "identify failed") || strings.Contains(lower, "identification failed") || strings.Contains(lower, "authentication failed") || strings.Contains(lower, "not registered") || strings.Contains(lower, "unknown identity")
	if identityFailure && (identityContext || pendingIdentity) {
		a.sess.SetIdentityStatus("failed", text)
		return true
	}
	// An unregistered nickname is a normal writable IRC session. Only enter
	// read-only mode when the server explicitly says this particular session is
	// restricted pending identity authentication.
	mustIdentifyForAction := strings.Contains(lower, "must identify") && (strings.Contains(lower, "send") || strings.Contains(lower, "write") || strings.Contains(lower, "action"))
	readonly := strings.Contains(lower, "read-only") || strings.Contains(lower, "read only") ||
		strings.Contains(lower, "identify to send") || strings.Contains(lower, "must identify to send") ||
		strings.Contains(lower, "authenticate to send") || strings.Contains(lower, "identify before sending") || mustIdentifyForAction
	if readonly {
		if strings.Contains(lower, "guard") {
			a.sess.SetState(session.GuardReadOnly, text)
		} else {
			a.sess.SetState(session.ReadOnly, text)
		}
		return true
	}
	return false
}

func (a *App) handleCaptcha(msg protocol.Message) {
	if len(msg.Params) == 0 {
		return
	}
	sub := strings.ToUpper(msg.Params[0])
	if sub == "REQUIRED" {
		word := ""
		if len(msg.Params) >= 2 {
			word = strings.TrimSuffix(msg.Params[1], ":")
		}
		if word == "" {
			// Some servers may encode "word:" at the beginning of trailing text.
			fields := strings.Fields(msg.Trailing())
			if len(fields) > 0 {
				word = strings.TrimSuffix(fields[0], ":")
			}
		}
		prompt := strings.Join(msg.Params[2:], " ")
		if prompt == "" {
			prompt = "Solve the challenge requested by the server."
		}
		a.sess.SetCaptcha(word, prompt)
		a.sess.SetState(session.Captcha, "Complete the server challenge to continue")
		a.emitState()
		return
	}
	if sub == "OK" || sub == "ACCEPTED" || sub == "SUCCESS" {
		a.sess.ClearCaptcha()
		a.sess.SetState(session.Ready, "Connected")
		a.maybeAutoIdentify()
		a.autoJoin()
		a.emitState()
	}
}

func (a *App) handlePrivmsg(msg protocol.Message) {
	if msg.Prefix == nil || len(msg.Params) < 2 {
		return
	}
	target := msg.Params[0]
	sender := msg.Prefix.Name
	synthetic := strings.EqualFold(msg.Prefix.User, "plugin") || strings.EqualFold(msg.Prefix.Host, "server")
	convID, name, kind := target, displayTarget(target), conversationKind(target, false)
	snap := a.sess.Snapshot()
	if strings.EqualFold(target, snap.Nick) {
		convID, name = sender, sender
		if synthetic {
			kind = conversation.VirtualDM
		} else {
			kind = conversation.DM
		}
	}
	ts := time.Now()
	rawServerTime := msg.Tags["time"]
	if rawServerTime != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, rawServerTime); err == nil {
			ts = parsed
		}
	}
	taggedHistory := msg.Tags["ircapybara-history"] == "1"
	historical := taggedHistory
	if !historical && strings.HasPrefix(target, "#") {
		a.mu.Lock()
		key := strings.ToLower(target)
		if req, ok := a.historyPending[key]; ok {
			if time.Now().Before(req.Until) && req.Remaining > 0 {
				historical = true
				req.Remaining--
				if req.Remaining == 0 {
					delete(a.historyPending, key)
				} else {
					a.historyPending[key] = req
				}
			} else {
				delete(a.historyPending, key)
			}
		}
		a.mu.Unlock()
	}
	dedupeKey := ""
	if taggedHistory && rawServerTime != "" {
		// Plugin history has a reliable server-time tag. Include it in the replay
		// key so two legitimate posts with identical text are not collapsed.
		dedupeKey = strings.ToLower(sender) + "\x00" + strings.ToLower(convID) + "\x00" + rawServerTime + "\x00" + msg.Params[1]
	}
	m := conversation.Message{
		ID: a.nextID(), Target: convID, Sender: sender, Text: protocol.DecodeMessageText(msg.Params[1]), Time: ts,
		Historical: historical, DedupeKey: dedupeKey, Synthetic: synthetic,
	}
	if historical {
		if !a.convs.AddHistoricalMessage(convID, name, kind, m) {
			return
		}
	} else {
		a.convs.AddMessage(convID, name, kind, m)
	}
	a.emitState()
}

func (a *App) handleNotice(msg protocol.Message) {
	sender := "Server"
	if msg.Prefix != nil && msg.Prefix.Name != "" {
		sender = msg.Prefix.Name
	}
	text := protocol.DecodeMessageText(msg.Trailing())
	if text == "" {
		return
	}
	a.applyServerPolicyFromText(text)
	if a.handleTransferNotice(text) {
		return
	}
	a.addStatus(sender, text)
}

func (a *App) handleTransferNotice(text string) bool {
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "unknown download") && !strings.Contains(lower, "expired") && !strings.Contains(lower, "unauthor") && !strings.Contains(lower, "forbidden") && !strings.Contains(lower, "access denied") {
		return false
	}
	unauthorized := strings.Contains(lower, "unauthor") || strings.Contains(lower, "forbidden") || strings.Contains(lower, "access denied")
	candidates := a.transfers.ResumeCandidates()
	matched := false
	for _, snapshot := range candidates {
		if !strings.Contains(lower, strings.ToLower(snapshot.FileID)) && len(candidates) != 1 {
			continue
		}
		if updated, err := a.transfers.MarkUnavailable(snapshot.FileID, unauthorized); err == nil {
			detail := "Attachment expired or is no longer available on the server."
			if unauthorized {
				detail = "This session is not authorized to access the attachment."
			}
			a.emitDownloadSnapshot(updated, detail)
			a.warnTransferOnce(snapshot.FileID, detail)
			matched = true
		}
	}
	return matched
}

func (a *App) handleAttachment(msg protocol.Message) {
	if msg.Prefix == nil || len(msg.Params) < 5 {
		return
	}
	size, err := strconv.ParseInt(msg.Params[2], 10, 64)
	if err != nil || size < 0 || size > xfer.DefaultMaxFileSize {
		return
	}
	target, fileID, mt, name := msg.Params[0], msg.Params[1], msg.Params[3], xfer.SanitizeFilename(msg.Params[4])
	if !validIRCAtom(fileID) {
		return
	}
	sender := msg.Prefix.Name
	meta := attachmentMeta{Sender: sender, Target: target, FileID: fileID, Name: name, MIME: mt, Size: size}
	a.attachMu.Lock()
	if len(a.attachments) > 512 {
		a.attachments = map[string]attachmentMeta{}
	}
	a.attachments[fileID] = meta
	a.attachMu.Unlock()
	synthetic := strings.EqualFold(msg.Prefix.User, "plugin") || strings.EqualFold(msg.Prefix.Host, "server")
	convID, convName, kind := target, displayTarget(target), conversationKind(target, false)
	snap := a.sess.Snapshot()
	if strings.EqualFold(target, snap.Nick) {
		convID, convName = sender, sender
		if synthetic {
			kind = conversation.VirtualDM
		} else {
			kind = conversation.DM
		}
	}
	a.convs.AddMessage(convID, convName, kind, conversation.Message{
		ID: a.nextID(), Target: convID, Sender: sender, Time: time.Now(),
		Synthetic:  synthetic,
		Attachment: &conversation.Attachment{FileID: fileID, Filename: name, MIME: mt, Size: size, Status: "available"},
	})
	a.emitState()
}

func (a *App) handleXfer(msg protocol.Message) {
	if len(msg.Params) == 0 {
		return
	}
	subcommand := strings.ToUpper(msg.Params[0])
	switch subcommand {
	case "READY":
		// Upload negotiation: XFER READY <nick> <fileid> <chunk-size> ...
		if len(msg.Params) < 4 || !strings.EqualFold(msg.Params[1], a.sess.Snapshot().Nick) {
			return
		}
		fileID := msg.Params[2]
		chunk, err := strconv.Atoi(msg.Params[3])
		if err != nil || !validIRCAtom(fileID) {
			return
		}
		upload, ok := a.transfers.PopUpload()
		if !ok {
			return
		}
		if err := a.transfers.BeginUpload(fileID, upload, chunk); err != nil {
			a.addStatus("Transfer", "Upload rejected: "+err.Error())
			return
		}
		go a.streamUpload(fileID, upload)

	case "START", "RESUME":
		// XFER START/RESUME <recipient> <fileid> <size> <chunk-size> <parts> <mime> :<filename>
		if len(msg.Params) < 8 || !strings.EqualFold(msg.Params[1], a.sess.Snapshot().Nick) {
			return
		}
		fileID := msg.Params[2]
		if !validIRCAtom(fileID) {
			return
		}
		size, errSize := strconv.ParseInt(msg.Params[3], 10, 64)
		chunkSize, errChunk := strconv.Atoi(msg.Params[4])
		totalParts, errParts := strconv.Atoi(msg.Params[5])
		if errSize != nil || errChunk != nil || errParts != nil {
			a.failTransferOnce(fileID, "Malformed transfer metadata")
			return
		}
		snapshot, err := a.transfers.StartDownload(fileID, size, chunkSize, totalParts, msg.Params[7], msg.Params[6], subcommand == "RESUME")
		if err != nil {
			if errors.Is(err, xfer.ErrUnknownDownload) {
				a.warnTransferOnce(fileID, "Ignored transfer data for an unknown download.")
				return
			}
			a.failTransferOnce(fileID, err.Error())
			return
		}
		a.emitDownloadSnapshot(snapshot, "")

	case "CHUNK":
		if len(msg.Params) < 4 {
			return
		}
		fileID := msg.Params[1]
		part, err := strconv.Atoi(msg.Params[2])
		if err != nil {
			return
		}
		result, err := a.transfers.AcceptChunk(fileID, part, msg.Params[3])
		if err != nil {
			switch {
			case errors.Is(err, xfer.ErrUnknownDownload), errors.Is(err, xfer.ErrTransferNotStarted):
				a.warnTransferOnce(fileID, "Ignored chunks for an unknown or inactive transfer.")
			case errors.Is(err, xfer.ErrConflictingDuplicate):
				if snapshot, markErr := a.transfers.MarkRetryAvailable(fileID); markErr == nil {
					a.emitDownloadSnapshot(snapshot, "Conflicting duplicate chunk; retry is available.")
				}
				a.warnTransferOnce(fileID, "Transfer integrity conflict detected; retry is available.")
			default:
				// Malformed Base64, invalid part lengths, and out-of-range parts are
				// not allowed to destroy correctly received v2 chunks. The bad part
				// remains absent and END/inactivity recovery requests it again.
				a.warnTransferOnce(fileID, "Ignored a malformed transfer chunk; missing data will be requested again.")
			}
			return
		}
		if !result.Duplicate {
			a.updateDownloadProgressV2(fileID, result)
		}

	case "END":
		if len(msg.Params) < 3 {
			return
		}
		fileID := msg.Params[1]
		if before, ok := a.transfers.DownloadSnapshot(fileID); ok && before.ReceivedParts == before.TotalParts {
			percent := 100
			if before.Size > 0 && before.ReceivedBytes < before.Size {
				percent = int((before.ReceivedBytes * 100) / before.Size)
			}
			a.setTransferUpdate(TransferUpdate{FileID: fileID, Status: string(xfer.StatusVerifying), Received: before.ReceivedBytes, Total: before.Size, Percent: percent, ReceivedParts: before.ReceivedParts, TotalParts: before.TotalParts, Path: before.SavePath})
		}
		result, err := a.transfers.EndDownload(fileID, msg.Params[2])
		if err != nil {
			if errors.Is(err, xfer.ErrUnknownDownload) {
				return
			}
			a.failTransferOnce(fileID, "Download verification failed: "+err.Error())
			return
		}
		if result.MissingRanges != "" {
			a.requestMissingDownload(fileID, true)
			return
		}
		if !result.Complete {
			return
		}
		if verified, ok := a.transfers.DownloadSnapshot(fileID); ok {
			a.emitDownloadSnapshot(verified, "")
			if verified.Complete {
				// Harmless duplicate END after an already acknowledged transfer.
				return
			}
		}
		// The file is not exposed as complete until the verified hash has been
		// acknowledged back to the server.
		if err := a.send("RECV ACK " + fileID + " " + result.SHA256); err != nil {
			if snapshot, markErr := a.transfers.MarkInterrupted(fileID); markErr == nil {
				a.emitDownloadSnapshot(snapshot, "Verified, but acknowledgement could not be sent: "+err.Error())
			}
			return
		}
		snapshot, err := a.transfers.MarkAckSent(fileID, result.SHA256)
		if err != nil {
			a.failTransferOnce(fileID, err.Error())
			return
		}
		a.completeDownload(fileID, snapshot.Size, snapshot.SavePath)

	case "ACK":
		// XFER ACK <recipient> <fileid>
		if len(msg.Params) < 3 || !strings.EqualFold(msg.Params[1], a.sess.Snapshot().Nick) {
			return
		}
		fileID := msg.Params[2]
		if snapshot, err := a.transfers.MarkServerAck(fileID); err == nil {
			a.emitDownloadSnapshot(snapshot, "")
		}

	case "MISSING":
		// Upload recovery: XFER MISSING <nick> <fileid> :2-4,9
		if len(msg.Params) < 4 || !strings.EqualFold(msg.Params[1], a.sess.Snapshot().Nick) {
			return
		}
		fileID := msg.Params[2]
		maxParts, ok := a.transfers.UploadTotalParts(fileID)
		if !ok || maxParts == 0 {
			return
		}
		parts, err := xfer.ParseRanges(msg.Params[3], maxParts)
		if err != nil {
			a.addStatus("Transfer", "Server requested invalid upload part ranges.")
			return
		}
		go func() {
			sha, _, err := a.transfers.StreamUpload(fileID, parts, func(part int, b64 string) error {
				return a.send(fmt.Sprintf("XFER CHUNK %s %d %s", fileID, part, b64))
			})
			if err != nil {
				a.addStatus("Transfer", "Upload retransmission failed: "+err.Error())
				return
			}
			if err := a.send("XFER END " + fileID + " " + sha); err != nil {
				a.addStatus("Transfer", "Upload retransmission failed: "+err.Error())
			}
		}()

	case "STORED":
		if len(msg.Params) >= 3 {
			a.transfers.CompleteUpload(msg.Params[2])
			a.addStatus("Transfer", "Upload stored successfully.")
		}

	case "EXPIRED", "UNAUTHORIZED", "DENIED", "REJECTED", "ERROR", "FAIL":
		a.handleTransferAvailabilityFailure(msg)
	}
}

func (a *App) streamUpload(fileID string, upload xfer.PendingUpload) {
	sha, size, err := a.transfers.StreamUpload(fileID, nil, func(part int, b64 string) error {
		return a.send(fmt.Sprintf("XFER CHUNK %s %d %s", fileID, part, b64))
	})
	if err != nil {
		a.addStatus("Transfer", "Upload failed: "+err.Error())
		return
	}
	if size != upload.Size {
		a.addStatus("Transfer", "Upload aborted: file changed while reading")
		return
	}
	if err := a.send("XFER END " + fileID + " " + sha); err != nil {
		a.addStatus("Transfer", "Upload failed: "+err.Error())
	}
}

func (a *App) applyChannelModes(target, modes string) {
	sign := byte(0)
	privateSeen := false
	privateEnabled := false
	for i := 0; i < len(modes); i++ {
		switch modes[i] {
		case '+', '-':
			sign = modes[i]
		case 's':
			if sign != 0 {
				privateSeen = true
				privateEnabled = sign == '+'
			}
		}
	}
	if !privateSeen {
		return
	}
	if privateEnabled {
		a.convs.SetKind(target, conversation.PrivateChannel)
	} else {
		a.convs.SetKind(target, conversation.PublicChannel)
	}
	a.emitState()
}

func (a *App) maybeAutoIdentify() bool {
	snap := a.sess.Snapshot()
	if !snap.Registered || snap.Identified || snap.CaptchaWord != "" {
		return false
	}

	a.mu.Lock()
	if a.identifySent || !a.registrationFinished || a.cfg.IdentityPassword == "" {
		a.mu.Unlock()
		return false
	}
	password := a.cfg.IdentityPassword
	nick := a.cfg.IdentityNick
	a.cfg.IdentityPassword = ""
	a.identifySent = true
	a.mu.Unlock()

	line, err := identifyCommand(nick, password)
	if err != nil {
		a.mu.Lock()
		a.identifySent = false
		a.mu.Unlock()
		a.addStatus("Identity", "Automatic identification could not be sent: "+err.Error())
		return false
	}
	if err := a.send(line); err != nil {
		a.mu.Lock()
		a.identifySent = false
		a.mu.Unlock()
		a.addStatus("Identity", "Automatic identification could not be sent: "+err.Error())
		return false
	}
	a.sess.SetIdentityStatus("pending", "Waiting for identity confirmation from the server")
	a.emitState()
	a.startIdentityConfirmationTimeout()
	return true
}

func (a *App) autoJoin() {
	snap := a.sess.Snapshot()
	if !snap.Registered || snap.CaptchaWord != "" {
		return
	}
	a.mu.Lock()
	if a.joined || !a.registrationFinished {
		a.mu.Unlock()
		return
	}
	targets := autoJoinTargets(a.cfg.AutoJoin)
	a.cfg.AutoJoin = append([]string(nil), targets...)
	a.joined = true
	a.pendingAutoJoin = nil
	for _, target := range targets {
		if !strings.EqualFold(target, "#general") {
			a.pendingAutoJoin = append(a.pendingAutoJoin, target)
		}
	}
	a.mu.Unlock()

	// Join the primary room first. Once the server confirms #general, restore
	// every other channel remembered from prior successful joins.
	if err := a.send("JOIN #general"); err != nil {
		a.addStatus("Connection", "Auto-join failed: "+err.Error())
		a.flushPendingAutoJoin()
	}
}

func (a *App) flushPendingAutoJoin() {
	a.mu.Lock()
	targets := append([]string(nil), a.pendingAutoJoin...)
	a.pendingAutoJoin = nil
	a.mu.Unlock()
	if len(targets) == 0 {
		return
	}
	if err := a.send("JOIN " + strings.Join(targets, ",")); err != nil {
		a.addStatus("Connection", "Could not restore joined channels: "+err.Error())
	}
}

func (a *App) rememberJoinedChannel(target string) {
	if !strings.HasPrefix(target, "#") || !validIRCAtom(target) {
		return
	}
	a.mu.Lock()
	a.cfg.AutoJoin = autoJoinTargets(append(a.cfg.AutoJoin, target))
	targets := append([]string(nil), a.cfg.AutoJoin...)
	a.mu.Unlock()

	if a.prefs == nil {
		return
	}
	settings, err := a.prefs.Load()
	if err != nil {
		return
	}
	settings.AutoJoin = targets
	_ = a.prefs.Save(settings)
}

func (a *App) forgetJoinedChannel(target string) {
	if !strings.HasPrefix(target, "#") || strings.EqualFold(target, "#general") {
		return
	}
	a.mu.Lock()
	next := make([]string, 0, len(a.cfg.AutoJoin))
	for _, item := range a.cfg.AutoJoin {
		if !strings.EqualFold(item, target) {
			next = append(next, item)
		}
	}
	a.cfg.AutoJoin = autoJoinTargets(next)
	targets := append([]string(nil), a.cfg.AutoJoin...)
	a.mu.Unlock()
	if a.prefs == nil {
		return
	}
	settings, err := a.prefs.Load()
	if err != nil {
		return
	}
	settings.AutoJoin = targets
	_ = a.prefs.Save(settings)
}

func (a *App) resetTransferStates() {
	a.transferMu.Lock()
	a.transferStates = map[string]TransferUpdate{}
	a.transferPercents = map[string]int{}
	a.transferWarnings = map[string]bool{}
	a.published = map[string]string{}
	a.transferMu.Unlock()
}

func (a *App) transferSnapshot() []TransferUpdate {
	a.transferMu.RLock()
	defer a.transferMu.RUnlock()
	out := make([]TransferUpdate, 0, len(a.transferStates))
	for _, state := range a.transferStates {
		out = append(out, state)
	}
	return out
}

func (a *App) forgetPublished(fileID string) {
	a.transferMu.Lock()
	delete(a.published, fileID)
	a.transferMu.Unlock()
}

// publishDownload hands a verified file to the host exactly once and returns
// the location the frontend should refer to from then on.
func (a *App) publishDownload(fileID, path string) string {
	a.transferMu.RLock()
	location, done := a.published[fileID]
	a.transferMu.RUnlock()
	if done {
		return location
	}
	name, mt := filepath.Base(path), ""
	if snapshot, ok := a.transfers.DownloadSnapshot(fileID); ok {
		if snapshot.Filename != "" {
			name = snapshot.Filename
		}
		mt = snapshot.MIME
	}
	location, err := a.host.PublishDownload(path, name, mt)
	if err != nil {
		a.warnTransferOnce(fileID, "The download is complete but could not be moved to its final location: "+err.Error())
	}
	if err != nil || location == "" {
		location = path
	}
	a.transferMu.Lock()
	a.published[fileID] = location
	a.transferMu.Unlock()
	return location
}

func (a *App) transferState(fileID string) (TransferUpdate, bool) {
	a.transferMu.RLock()
	defer a.transferMu.RUnlock()
	state, ok := a.transferStates[fileID]
	return state, ok
}

func (a *App) setTransferUpdate(update TransferUpdate) {
	if update.FileID == "" {
		return
	}
	a.transferMu.Lock()
	if update.Status == string(xfer.StatusComplete) {
		if location, ok := a.published[update.FileID]; ok {
			update.Path = location
		}
	}
	a.transferStates[update.FileID] = update
	if update.Status != "downloading" {
		delete(a.transferPercents, update.FileID)
	}
	a.transferMu.Unlock()
	a.host.Emit(EventTransfer, update)
}

func (a *App) updateDownloadProgressV2(fileID string, result xfer.ChunkResult) {
	percent := 0
	if result.TotalSize > 0 {
		percent = int((result.ReceivedBytes * 100) / result.TotalSize)
		if percent > 100 {
			percent = 100
		}
	}
	status := "downloading"
	if snapshot, ok := a.transfers.DownloadSnapshot(fileID); ok && snapshot.Status == xfer.StatusReceivingMissing {
		status = string(xfer.StatusReceivingMissing)
	}
	a.transferMu.Lock()
	last, seen := a.transferPercents[fileID]
	if seen && percent < last {
		percent = last
	}
	a.transferPercents[fileID] = percent
	previous := a.transferStates[fileID]
	update := TransferUpdate{
		FileID: fileID, Status: status, Received: result.ReceivedBytes, Total: result.TotalSize,
		Percent: percent, ReceivedParts: result.ReceivedParts, TotalParts: result.TotalParts,
		Path: previous.Path,
	}
	a.transferStates[fileID] = update
	a.transferMu.Unlock()
	a.host.Emit(EventTransfer, update)
}

func (a *App) emitDownloadSnapshot(snapshot xfer.DownloadSnapshot, detail string) {
	if snapshot.FileID == "" {
		return
	}
	percent := 0
	if snapshot.Size > 0 {
		percent = int((snapshot.ReceivedBytes * 100) / snapshot.Size)
		if percent > 100 {
			percent = 100
		}
	}
	if snapshot.Complete {
		percent = 100
	}
	update := TransferUpdate{
		FileID: snapshot.FileID, Status: string(snapshot.Status), Received: snapshot.ReceivedBytes,
		Total: snapshot.Size, Percent: percent, ReceivedParts: snapshot.ReceivedParts,
		TotalParts: snapshot.TotalParts, RetryRounds: snapshot.RetransmissionRounds,
		Path: snapshot.SavePath, Error: detail,
	}
	a.setTransferUpdate(update)
}

func (a *App) requestMissingDownload(fileID string, automatic bool) error {
	ranges, snapshot, err := a.transfers.MissingRequest(fileID, automatic)
	if err != nil {
		if errors.Is(err, xfer.ErrMissingRequestActive) {
			return nil
		}
		if errors.Is(err, xfer.ErrRetryLimit) {
			a.emitDownloadSnapshot(snapshot, "Automatic recovery stopped after three retransmission rounds. Retry is available.")
			return nil
		}
		return err
	}
	if ranges == "" {
		return nil
	}
	a.emitDownloadSnapshot(snapshot, "")
	if err := a.send("RECV MISSING " + fileID + " " + ranges); err != nil {
		if interrupted, markErr := a.transfers.MarkInterrupted(fileID); markErr == nil {
			a.emitDownloadSnapshot(interrupted, "Could not request missing parts: "+err.Error())
		}
		return err
	}
	return nil
}

func (a *App) resumeInterruptedDownloads(gen uint64) {
	// Give registration/XMODE state a moment to settle when 005 and XMODE arrive
	// back-to-back. Duplicate resume attempts are suppressed by transfer state.
	time.Sleep(100 * time.Millisecond)
	a.mu.Lock()
	current := a.generation == gen && a.client != nil
	version := a.xferVersion
	a.mu.Unlock()
	sessionSnapshot := a.sess.Snapshot()
	if !current || !sessionSnapshot.Extended || !sessionSnapshot.Registered {
		return
	}
	for _, snapshot := range a.transfers.ResumeCandidates() {
		if snapshot.Status != xfer.StatusInterrupted {
			continue
		}
		if snapshot.VerifiedSHA256 != "" {
			if err := a.send("RECV ACK " + snapshot.FileID + " " + snapshot.VerifiedSHA256); err == nil {
				if complete, markErr := a.transfers.MarkAckSent(snapshot.FileID, snapshot.VerifiedSHA256); markErr == nil {
					a.completeDownload(snapshot.FileID, complete.Size, complete.SavePath)
				}
			}
			continue
		}
		if version >= 2 && snapshot.ChunkSize > 0 && snapshot.TotalParts > 0 {
			if err := a.requestMissingDownload(snapshot.FileID, true); err == nil {
				continue
			}
		}
		// v1 fallback, or state that lacks reliable v2 geometry.
		if _, err := a.transfers.PrepareFullRestart(snapshot.FileID); err == nil {
			_ = a.send("RECV " + snapshot.FileID)
		}
	}
}

func (a *App) transferRecoveryLoop(c *transport.Client, gen uint64) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		a.mu.Lock()
		current := a.client == c && a.generation == gen
		version := a.xferVersion
		a.mu.Unlock()
		if !current {
			return
		}
		if version < 2 || !a.sess.Snapshot().Extended {
			continue
		}
		for _, snapshot := range a.transfers.StalledDownloads(time.Now(), 45*time.Second) {
			_ = a.requestMissingDownload(snapshot.FileID, true)
		}
	}
}

func (a *App) warnTransferOnce(fileID, message string) {
	key := fileID + "|" + message
	a.transferMu.Lock()
	if a.transferWarnings[key] {
		a.transferMu.Unlock()
		return
	}
	a.transferWarnings[key] = true
	a.transferMu.Unlock()
	a.addStatus("Transfer", message)
}

func (a *App) failTransferOnce(fileID, detail string) {
	if snapshot, ok := a.transfers.DownloadSnapshot(fileID); ok {
		if snapshot.Status != xfer.StatusRetryAvailable {
			if marked, err := a.transfers.MarkRetryAvailable(fileID); err == nil {
				snapshot = marked
			}
		}
		a.emitDownloadSnapshot(snapshot, detail)
	}
	a.warnTransferOnce(fileID, "Download needs attention: "+detail)
}

func (a *App) failDownload(fileID string, total int64, err error) {
	detail := "Download failed"
	if err != nil && strings.TrimSpace(err.Error()) != "" {
		detail = err.Error()
	}
	a.setTransferUpdate(TransferUpdate{FileID: fileID, Status: string(xfer.StatusRetryAvailable), Total: total, Error: detail})
}

func (a *App) completeDownload(fileID string, total int64, path string) {
	path = a.publishDownload(fileID, path)
	a.setTransferUpdate(TransferUpdate{FileID: fileID, Status: string(xfer.StatusComplete), Received: total, Total: total, Percent: 100, Path: path})
}

func (a *App) interruptActiveDownloads(reason string) {
	for _, snapshot := range a.transfers.MarkInterruptedAll() {
		a.emitDownloadSnapshot(snapshot, reason)
	}
}

func (a *App) handleTransferAvailabilityFailure(msg protocol.Message) {
	text := strings.ToLower(strings.Join(msg.Params, " "))
	unauthorized := strings.Contains(text, "unauthor") || strings.Contains(text, "denied") || strings.Contains(text, "forbidden")
	candidates := a.transfers.ResumeCandidates()
	for _, snapshot := range candidates {
		explicitMatch := strings.Contains(text, strings.ToLower(snapshot.FileID)) || (len(msg.Params) > 2 && strings.EqualFold(msg.Params[2], snapshot.FileID))
		if !explicitMatch && len(candidates) != 1 {
			continue
		}
		if updated, err := a.transfers.MarkUnavailable(snapshot.FileID, unauthorized); err == nil {
			detail := "Attachment is no longer available on the server."
			if unauthorized {
				detail = "This session is not authorized to resume the attachment."
			}
			a.emitDownloadSnapshot(updated, detail)
			a.warnTransferOnce(snapshot.FileID, detail)
		}
	}
}

func (a *App) addStatus(sender, text string) {
	a.convs.AddMessage("status", "Home", conversation.Status, conversation.Message{ID: a.nextID(), Target: "status", Sender: sender, Text: text, Time: time.Now(), System: true})
	a.emitState()
}

func (a *App) snapshot() ClientSnapshot {
	s := a.sess.Snapshot()
	a.mu.Lock()
	profiles := make(map[string]UserProfile, len(a.profiles))
	for key, profile := range a.profiles {
		profile.Channels = append([]string(nil), profile.Channels...)
		profiles[key] = profile
	}
	a.mu.Unlock()
	return ClientSnapshot{
		State: s.State, StateDetail: s.StateDetail, Nick: s.Nick, Server: s.Server,
		Extended: s.Extended, Registered: s.Registered, Identified: s.Identified,
		IdentityStatus: s.IdentityStatus, IdentityDetail: s.IdentityDetail,
		CaptchaWord: s.CaptchaWord, CaptchaPrompt: s.CaptchaPrompt,
		Profiles:      profiles,
		Conversations: a.convs.Snapshot(),
	}
}

func (a *App) emitState() {
	a.host.Emit(EventState, a.snapshot())
}

func (a *App) resetProtocolTrace() {
	a.traceMu.Lock()
	a.trace = nil
	a.traceMu.Unlock()
	a.traceSeq.Store(0)
}

func (a *App) protocolTraceSnapshot() []ProtocolTraceEntry {
	a.traceMu.RLock()
	defer a.traceMu.RUnlock()
	return append([]ProtocolTraceEntry(nil), a.trace...)
}

func (a *App) traceProtocol(direction, line string) {
	if len(line) > 8192 {
		line = line[:8192] + " …[truncated]"
	}
	entry := ProtocolTraceEntry{Seq: a.traceSeq.Add(1), Direction: direction, Time: time.Now(), Line: line}
	a.traceMu.Lock()
	a.trace = append(a.trace, entry)
	// Keep enough transfer headers around to diagnose ordering problems without
	// reallocating the trace slice for every 240-byte file chunk.
	if len(a.trace) > 6000 {
		a.trace = append([]ProtocolTraceEntry(nil), a.trace[len(a.trace)-5000:]...)
	}
	a.traceMu.Unlock()
	// XFER CHUNK can arrive tens of thousands of times for one attachment. The
	// backend trace still records those exact headers, but streaming every chunk
	// into WebKit would make the diagnostics UI compete with the transfer itself.
	if !strings.HasPrefix(line, "XFER CHUNK ") {
		a.host.Emit(EventTrace, entry)
	}
}

func redactProtocolLine(line string) string {
	msg, err := protocol.Parse(line)
	if err != nil {
		return line
	}
	switch msg.Command {
	case "PASS":
		return "PASS <redacted>"
	case "IDENTIFY":
		if len(msg.Params) >= 2 {
			return "IDENTIFY " + msg.Params[0] + " <redacted>"
		}
		return "IDENTIFY <redacted>"
	case "CAPTCHA":
		if len(msg.Params) >= 1 {
			return "CAPTCHA " + msg.Params[0] + " <redacted>"
		}
	case "XFER":
		if len(msg.Params) >= 4 && strings.EqualFold(msg.Params[0], "CHUNK") {
			return fmt.Sprintf("XFER CHUNK %s %s <base64 payload: %d chars>", msg.Params[1], msg.Params[2], len(msg.Params[3]))
		}
	}
	return line
}

func (a *App) nextID() string { return fmt.Sprintf("m-%d", a.msgSeq.Add(1)) }

func identifyCommand(nick, password string) (string, error) {
	nick = strings.TrimSpace(nick)
	if !validCredentialArg(password) {
		return "", errors.New("identity password must be non-empty and contain no spaces or control characters")
	}
	if nick == "" {
		return "IDENTIFY " + password, nil
	}
	if !validIRCAtom(nick) {
		return "", errors.New("invalid identity nickname")
	}
	return "IDENTIFY " + nick + " " + password, nil
}

// validMIMEToken keeps a host-supplied media type from breaking the XFER START
// parameter list.
func validMIMEToken(value string) bool {
	if value == "" || len(value) > 127 || !strings.Contains(value, "/") {
		return false
	}
	for _, r := range value {
		if r <= ' ' || r >= 0x7f || r == ':' || r == ',' {
			return false
		}
	}
	return true
}

func validCredentialArg(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r <= ' ' || r == '\x7f' {
			return false
		}
	}
	return true
}

func parseNamesMember(raw string) (conversation.Member, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return conversation.Member{}, false
	}
	operator := false
	for len(raw) > 0 {
		switch raw[0] {
		case '@', '&', '~':
			operator = true
			raw = raw[1:]
		case '+', '%':
			raw = raw[1:]
		default:
			goto done
		}
	}

done:
	if bang := strings.IndexByte(raw, '!'); bang >= 0 {
		raw = raw[:bang]
	}
	if !validIRCAtom(raw) {
		return conversation.Member{}, false
	}
	return conversation.Member{Nick: raw, Online: true, Operator: operator}, true
}

func validIRCAtom(v string) bool {
	if v == "" || len(v) > 256 {
		return false
	}
	return !strings.ContainsAny(v, " \t\r\n\x00:")
}

func displayTarget(v string) string {
	if strings.HasPrefix(v, "#") {
		return strings.TrimPrefix(v, "#")
	}
	return v
}

func containsFold(items []string, want string) bool {
	for _, item := range items {
		if strings.EqualFold(item, want) {
			return true
		}
	}
	return false
}

func conversationKind(target string, private bool) conversation.Kind {
	if strings.HasPrefix(target, "#") {
		if private {
			return conversation.PrivateChannel
		}
		return conversation.PublicChannel
	}
	return conversation.DM
}

func cleanTargets(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.TrimSpace(t)
		if strings.HasPrefix(t, "#") && validIRCAtom(t) && !seen[strings.ToLower(t)] {
			seen[strings.ToLower(t)] = true
			out = append(out, t)
		}
	}
	return out
}

func autoJoinTargets(in []string) []string {
	clean := cleanTargets(in)
	out := make([]string, 0, len(clean)+1)
	out = append(out, "#general")
	for _, target := range clean {
		if !strings.EqualFold(target, "#general") {
			out = append(out, target)
		}
	}
	return out
}

func serverDisplay(cfg ConnectionConfig) string {
	if label := strings.TrimSpace(cfg.ServerLabel); label != "" {
		return label
	}
	return cfg.Host
}
