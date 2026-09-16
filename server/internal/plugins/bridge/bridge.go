package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"ircapybara/internal/plugins/pluginutil"
	"ircapybara/pkg/pluginapi"
)

var (
	botTokenRE    = regexp.MustCompile(`^[0-9]{6,12}:[A-Za-z0-9_-]{30,80}$`)
	bridgeArgsRE  = regexp.MustCompile(`^tg:(-?[1-9][0-9]{4,19}) ([A-Za-z][A-Za-z0-9_-]{0,20})$`)
	callbackRE    = regexp.MustCompile(`^ircbind:([A-Za-z][A-Za-z0-9_\-\[\]\\` + "`" + `^{}]{0,30})$`)
	nameCleanRE   = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
	firstLetterRE = regexp.MustCompile(`^[A-Za-z]`)
	callbackIDRE  = regexp.MustCompile(`^[^\x00-\x20\x7f]{1,256}$`)
)

type binding struct {
	ChatID   int64
	Identity string
	Name     string
	Target   string
	Sender   string
}

type outbound struct {
	ChatID   int64
	Text     string
	ClientID string
}

type Plugin struct {
	host  pluginapi.Host
	token string
	mode  string
	http  *http.Client

	mu       sync.RWMutex
	byChat   map[int64]*binding
	byTarget map[string]*binding
	pending  map[int64]string
	offset   int64
	out      chan outbound
}

func NewFromEnv() (*Plugin, error) {
	token := strings.TrimSpace(os.Getenv("IRCAPYBARA_BRIDGE_TG_BOT_TOKEN"))
	if token == "" {
		return nil, nil
	}
	if !botTokenRE.MatchString(token) {
		return nil, errors.New("invalid IRCAPYBARA_BRIDGE_TG_BOT_TOKEN format")
	}
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("IRCAPYBARA_BRIDGE_TG_MODE")))
	if mode == "" {
		mode = "channel"
	}
	if mode != "channel" && mode != "dm" {
		return nil, errors.New("IRCAPYBARA_BRIDGE_TG_MODE must be channel or dm")
	}
	return &Plugin{
		token: token, mode: mode, http: pluginutil.PublicHTTPClient(35 * time.Second),
		byChat: make(map[int64]*binding), byTarget: make(map[string]*binding), pending: make(map[int64]string),
		out: make(chan outbound, 64),
	}, nil
}

func (p *Plugin) Name() string { return "BridgePlugin" }

func (p *Plugin) Init(host pluginapi.Host) error {
	p.host = host
	go p.outboundLoop()
	go p.pollLoop()
	return nil
}

func (p *Plugin) OnConnect(pluginapi.ClientView)             {}
func (p *Plugin) OnDisconnect(pluginapi.ClientView)          {}
func (p *Plugin) BeforeCommand(pluginapi.CommandEvent) error { return nil }
func (p *Plugin) OnJoin(pluginapi.JoinEvent)                 {}

func (p *Plugin) HandleCommand(e pluginapi.CommandEvent) bool {
	if e.Command != "BRIDGE" {
		return false
	}
	if !e.Client.Registered {
		_ = p.host.SendNotice(e.Client.ID, "BRIDGE requires a registered IRC connection")
		return true
	}
	if e.Client.Identity == "" {
		_ = p.host.SendNotice(e.Client.ID, "BRIDGE requires an authenticated registered identity; REGISTER or IDENTIFY first")
		return true
	}
	m := bridgeArgsRE.FindStringSubmatch(e.Args)
	if m == nil {
		_ = p.host.SendNotice(e.Client.ID, "Usage: BRIDGE tg:<chat_id> <name>")
		return true
	}
	chatID, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		_ = p.host.SendNotice(e.Client.ID, "Invalid Telegram chat id")
		return true
	}
	b, err := p.bind(chatID, e.Client.Identity, m[2])
	if err != nil {
		_ = p.host.SendNotice(e.Client.ID, "Bridge not created: "+err.Error())
		return true
	}
	if p.mode == "channel" {
		_ = p.host.EnsureIdentityInChannel(e.Client.Identity, b.Target)
	}
	_ = p.host.SendNotice(e.Client.ID, fmt.Sprintf("Telegram bridge ready: %s -> chat %d", b.Target, chatID))
	p.deliverPending(chatID, b)
	return true
}

func (p *Plugin) HelpModules() map[string][]string {
	return map[string][]string{
		"BRIDGE": {
			"Syntax: BRIDGE tg:<chat_id> <name>",
			"Example: BRIDGE tg:-1001234567890 family",
			"Creates your Telegram-backed private channel or virtual DM; requires an authenticated registered identity.",
		},
	}
}

func (p *Plugin) BeforeMessage(e pluginapi.MessageEvent) (pluginapi.MessageEvent, error) {
	p.mu.RLock()
	b := p.byTarget[strings.ToLower(e.Target)]
	p.mu.RUnlock()
	if b == nil {
		return e, nil
	}
	if e.Client.Identity == "" || e.Client.Identity != b.Identity {
		_ = p.host.SendNotice(e.Client.ID, "This Telegram bridge belongs to another identity")
		return e, pluginapi.ErrHandled
	}
	if p.mode == "channel" && !p.host.ClientInChannel(e.Client.ID, b.Target) {
		_ = p.host.SendNotice(e.Client.ID, "Join "+b.Target+" before sending to Telegram")
		return e, pluginapi.ErrHandled
	}
	select {
	case p.out <- outbound{ChatID: b.ChatID, Text: e.Text, ClientID: e.Client.ID}:
	default:
		_ = p.host.SendNotice(e.Client.ID, "Telegram bridge queue is full; message was not sent")
	}
	return e, pluginapi.ErrHandled
}

func (p *Plugin) bind(chatID int64, identity, name string) (*binding, error) {
	identity = strings.ToLower(identity)
	target, sender := "tg-"+name, "tg-"+name
	if p.mode == "channel" {
		target = "#" + target
	}
	key := strings.ToLower(target)
	p.mu.Lock()
	if existing := p.byChat[chatID]; existing != nil {
		if existing.Identity != identity {
			p.mu.Unlock()
			return nil, errors.New("that Telegram chat is already bound to another IRC identity")
		}
		p.mu.Unlock()
		return existing, nil
	}
	if existing := p.byTarget[key]; existing != nil {
		p.mu.Unlock()
		return nil, errors.New("that bridge name is already in use")
	}
	b := &binding{ChatID: chatID, Identity: identity, Name: name, Target: target, Sender: sender}
	p.byChat[chatID] = b
	p.byTarget[key] = b
	p.mu.Unlock()
	if p.mode == "channel" {
		if err := p.host.EnsurePluginChannel(pluginapi.ChannelSpec{
			Name: target, Topic: "Telegram bridge", Private: true, InviteOnly: true,
			HistoryEnabled: false, OwnerIdentity: identity,
		}); err != nil {
			p.mu.Lock()
			delete(p.byChat, chatID)
			delete(p.byTarget, key)
			p.mu.Unlock()
			return nil, err
		}
	}
	return b, nil
}

func (p *Plugin) outboundLoop() {
	for req := range p.out {
		if err := p.sendMessage(req.ChatID, req.Text, nil); err != nil {
			log.Printf("telegram bridge send failed: %v", err)
			_ = p.host.SendNotice(req.ClientID, "Telegram send failed")
		}
	}
}

func (p *Plugin) pollLoop() {
	// Drop any update backlog from before this process started. Bridge mappings
	// are RAM-only, so replaying old Telegram updates cannot be routed safely.
	if updates, err := p.getUpdates(-1, 0); err == nil && len(updates) > 0 {
		p.offset = updates[len(updates)-1].UpdateID + 1
	}
	for {
		updates, err := p.getUpdates(p.offset, 20)
		if err != nil {
			log.Printf("telegram bridge poll: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		for _, update := range updates {
			if update.UpdateID >= p.offset {
				p.offset = update.UpdateID + 1
			}
			if update.CallbackQuery != nil {
				p.handleCallback(update.CallbackQuery)
			}
			if update.Message != nil {
				p.handleIncoming(update.Message)
			}
		}
	}
}

type tgAPIResponse[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	Description string `json:"description"`
}

type tgUpdate struct {
	UpdateID      int64            `json:"update_id"`
	Message       *tgMessage       `json:"message"`
	CallbackQuery *tgCallbackQuery `json:"callback_query"`
}

type tgCallbackQuery struct {
	ID      string     `json:"id"`
	Data    string     `json:"data"`
	Message *tgMessage `json:"message"`
}

type tgMessage struct {
	Chat      tgChat            `json:"chat"`
	Text      string            `json:"text"`
	Photo     []json.RawMessage `json:"photo"`
	Video     json.RawMessage   `json:"video"`
	Sticker   json.RawMessage   `json:"sticker"`
	Animation json.RawMessage   `json:"animation"`
	Voice     json.RawMessage   `json:"voice"`
}

type tgChat struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

func (p *Plugin) getUpdates(offset int64, timeout int) ([]tgUpdate, error) {
	q := url.Values{}
	q.Set("offset", strconv.FormatInt(offset, 10))
	q.Set("limit", "50")
	q.Set("timeout", strconv.Itoa(timeout))
	var out []tgUpdate
	if err := p.apiGET("getUpdates", q, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *Plugin) handleIncoming(msg *tgMessage) {
	text := telegramMessageText(msg)
	if text == "" {
		return
	}
	p.mu.RLock()
	b := p.byChat[msg.Chat.ID]
	p.mu.RUnlock()
	if b != nil {
		p.deliver(b, text)
		return
	}
	p.setPending(msg.Chat.ID, text)
	count := p.host.UserCount()
	eligible := uniqueEligibleUsers(p.host.Clients())
	if count == 1 && len(eligible) == 1 {
		if b, err := p.bindAuto(msg.Chat, eligible[0].Identity); err == nil {
			_ = p.sendMessage(msg.Chat.ID, "IRCapybara bridge connected to "+eligible[0].Nick, nil)
			p.deliverPending(msg.Chat.ID, b)
		}
		return
	}
	if count > 1 && count < 10 && len(eligible) > 0 {
		rows := make([][]inlineButton, 0, len(eligible))
		for _, client := range eligible {
			rows = append(rows, []inlineButton{{Text: client.Nick, CallbackData: "ircbind:" + client.Identity}})
		}
		_ = p.sendMessage(msg.Chat.ID, "Select the IRC user for this Telegram dialog:", &inlineKeyboard{InlineKeyboard: rows})
		return
	}
	_ = p.sendMessage(msg.Chat.ID, "IRCapybara bridge: waiting for dialog initiation from IRC.", nil)
}

func (p *Plugin) handleCallback(q *tgCallbackQuery) {
	if q.Message == nil {
		return
	}
	m := callbackRE.FindStringSubmatch(q.Data)
	if m == nil {
		return
	}
	identity := strings.ToLower(m[1])
	var selected *pluginapi.ClientView
	for _, client := range p.host.Clients() {
		if client.Registered && client.Identity == identity {
			copy := client
			selected = &copy
			break
		}
	}
	if selected == nil {
		_ = p.answerCallback(q.ID, "IRC user is no longer available")
		return
	}
	b, err := p.bindAuto(q.Message.Chat, identity)
	if err != nil {
		_ = p.answerCallback(q.ID, "Bridge could not be created")
		return
	}
	_ = p.answerCallback(q.ID, "Connected to "+selected.Nick)
	_ = p.sendMessage(q.Message.Chat.ID, "IRCapybara bridge connected to "+selected.Nick, nil)
	p.deliverPending(q.Message.Chat.ID, b)
}

func (p *Plugin) bindAuto(chat tgChat, identity string) (*binding, error) {
	name := deriveName(chat)
	for attempt := 0; attempt < 3; attempt++ {
		candidate := name
		if attempt > 0 {
			candidate = pluginutil.CleanExternalText(fmt.Sprintf("%s-%d", name, abs64(chat.ID)%10000), 20)
		}
		candidate = strings.Trim(candidate, "-")
		if candidate == "" || !firstLetterRE.MatchString(candidate) {
			candidate = "chat" + strconv.FormatInt(abs64(chat.ID)%1000000, 10)
		}
		if b, err := p.bind(chat.ID, identity, candidate); err == nil {
			if p.mode == "channel" {
				_ = p.host.EnsureIdentityInChannel(identity, b.Target)
			}
			return b, nil
		}
	}
	return nil, errors.New("no available bridge name")
}

func deriveName(chat tgChat) string {
	name := chat.Username
	if name == "" {
		name = chat.Title
	}
	if name == "" {
		name = chat.FirstName
	}
	name = nameCleanRE.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if len(name) > 20 {
		name = name[:20]
	}
	if name == "" || !firstLetterRE.MatchString(name) {
		name = "chat" + strconv.FormatInt(abs64(chat.ID)%1000000, 10)
	}
	return name
}

func telegramMessageText(msg *tgMessage) string {
	if text := pluginutil.CleanExternalText(msg.Text, 390); text != "" {
		return text
	}
	switch {
	case len(msg.Photo) > 0:
		return "[image]"
	case len(msg.Video) > 0:
		return "[video]"
	case len(msg.Sticker) > 0:
		return "[sticker]"
	case len(msg.Animation) > 0:
		return "[gif]"
	case len(msg.Voice) > 0:
		return "[voice]"
	default:
		return "[message]"
	}
}

func uniqueEligibleUsers(clients []pluginapi.ClientView) []pluginapi.ClientView {
	seen := make(map[string]struct{})
	var out []pluginapi.ClientView
	for _, c := range clients {
		if !c.Registered || c.Identity == "" {
			continue
		}
		if _, ok := seen[c.Identity]; ok {
			continue
		}
		seen[c.Identity] = struct{}{}
		out = append(out, c)
	}
	return out
}

func (p *Plugin) setPending(chatID int64, text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.pending[chatID]; exists || len(p.pending) < 100 {
		p.pending[chatID] = text
	}
}

func (p *Plugin) deliverPending(chatID int64, b *binding) {
	p.mu.Lock()
	text := p.pending[chatID]
	delete(p.pending, chatID)
	p.mu.Unlock()
	if text != "" {
		p.deliver(b, text)
	}
}

func (p *Plugin) deliver(b *binding, text string) {
	if p.mode == "channel" {
		_ = p.host.EnsureIdentityInChannel(b.Identity, b.Target)
		_ = p.host.SendChannelMessageToIdentity(b.Target, b.Identity, b.Sender, text)
		return
	}
	_ = p.host.SendDirectMessageToIdentity(b.Identity, b.Sender, text)
}

type inlineKeyboard struct {
	InlineKeyboard [][]inlineButton `json:"inline_keyboard"`
}

type inlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

func (p *Plugin) sendMessage(chatID int64, text string, keyboard *inlineKeyboard) error {
	payload := struct {
		ChatID      int64           `json:"chat_id"`
		Text        string          `json:"text"`
		ReplyMarkup *inlineKeyboard `json:"reply_markup,omitempty"`
	}{ChatID: chatID, Text: pluginutil.CleanExternalText(text, 1000), ReplyMarkup: keyboard}
	return p.apiPOST("sendMessage", payload, nil)
}

func (p *Plugin) answerCallback(id, text string) error {
	if !callbackIDRE.MatchString(id) {
		return errors.New("invalid callback id")
	}
	payload := struct {
		CallbackQueryID string `json:"callback_query_id"`
		Text            string `json:"text"`
	}{CallbackQueryID: id, Text: pluginutil.CleanExternalText(text, 180)}
	return p.apiPOST("answerCallbackQuery", payload, nil)
}

func (p *Plugin) apiGET(method string, query url.Values, out any) error {
	u := "https://api.telegram.org/bot" + p.token + "/" + method + "?" + query.Encode()
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	return p.do(req, out)
}

func (p *Plugin) apiPOST(method string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, "https://api.telegram.org/bot"+p.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return p.do(req, out)
}

func (p *Plugin) do(req *http.Request, out any) error {
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	const max = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return err
	}
	if len(body) > max {
		return errors.New("Telegram response too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("Telegram HTTP status %d", resp.StatusCode)
	}
	if out == nil {
		var envelope tgAPIResponse[json.RawMessage]
		if err := json.Unmarshal(body, &envelope); err != nil {
			return err
		}
		if !envelope.OK {
			return errors.New("Telegram API rejected request")
		}
		return nil
	}
	raw := struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return err
	}
	if !raw.OK {
		return errors.New("Telegram API rejected request")
	}
	return json.Unmarshal(raw.Result, out)
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
