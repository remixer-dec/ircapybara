package chatcompletions

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
	"strings"
	"sync"
	"time"

	"ircapybara/internal/plugins/pluginutil"
	"ircapybara/internal/protocol"
	"ircapybara/pkg/pluginapi"
)

const maxConversationMessages = 24

var (
	modelRE          = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)
	reasoningRE      = regexp.MustCompile(`^(?:none|minimal|low|medium|high|xhigh|max)?$`)
	boolRE           = regexp.MustCompile(`^(?:true|false)$`)
	resetMessageRE   = regexp.MustCompile(`^(?:!reset|/reset)$`)
	modelNickBadRE   = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
	modelNickStartRE = regexp.MustCompile(`^[A-Za-z]`)
)

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Plugin struct {
	host      pluginapi.Host
	endpoint  string
	apiKey    string
	model     string
	reasoning string
	channel   string
	private   bool
	nick      string
	system    string
	http      *http.Client
	sem       chan struct{}

	mu       sync.Mutex
	sessions map[string][]message
	inflight map[string]bool
	epochs   map[string]uint64
}

func NewFromEnv() (*Plugin, error) {
	enabled := strings.TrimSpace(os.Getenv("IRCAPYBARA_CHAT_ENABLED"))
	if enabled == "" {
		enabled = "false"
	}
	if !boolRE.MatchString(enabled) {
		return nil, errors.New("IRCAPYBARA_CHAT_ENABLED must be true or false")
	}
	if enabled != "true" {
		return nil, nil
	}
	endpoint := strings.TrimSpace(os.Getenv("IRCAPYBARA_CHAT_ENDPOINT"))
	if endpoint == "" {
		endpoint = "https://api.openai.com/v1/chat/completions"
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("invalid IRCAPYBARA_CHAT_ENDPOINT")
	}
	model := strings.TrimSpace(os.Getenv("IRCAPYBARA_CHAT_MODEL"))
	if !modelRE.MatchString(model) {
		return nil, errors.New("IRCAPYBARA_CHAT_MODEL is required and invalid")
	}
	reasoning := strings.TrimSpace(os.Getenv("IRCAPYBARA_CHAT_REASONING_EFFORT"))
	if !reasoningRE.MatchString(reasoning) {
		return nil, errors.New("invalid IRCAPYBARA_CHAT_REASONING_EFFORT")
	}
	channel := strings.TrimSpace(os.Getenv("IRCAPYBARA_CHAT_CHANNEL"))
	if channel == "" {
		channel = "#ai"
	}
	if !protocol.ChannelRE.MatchString(channel) {
		return nil, errors.New("invalid IRCAPYBARA_CHAT_CHANNEL")
	}
	privateRaw := strings.TrimSpace(os.Getenv("IRCAPYBARA_CHAT_PRIVATE"))
	if privateRaw == "" {
		privateRaw = "false"
	}
	if !boolRE.MatchString(privateRaw) {
		return nil, errors.New("IRCAPYBARA_CHAT_PRIVATE must be true or false")
	}
	nick := strings.TrimSpace(os.Getenv("IRCAPYBARA_CHAT_NICK"))
	if nick == "" {
		nick = "chat-ai"
	}
	if !protocol.NickRE.MatchString(nick) {
		return nil, errors.New("invalid IRCAPYBARA_CHAT_NICK")
	}
	system := strings.TrimSpace(os.Getenv("IRCAPYBARA_CHAT_SYSTEM_PROMPT"))
	if system != "" && !protocol.ValidText(system, 500) {
		return nil, errors.New("invalid IRCAPYBARA_CHAT_SYSTEM_PROMPT")
	}
	return &Plugin{
		endpoint: endpoint, apiKey: strings.TrimSpace(os.Getenv("IRCAPYBARA_CHAT_API_KEY")),
		model: model, reasoning: reasoning, channel: channel, private: privateRaw == "true", nick: nick, system: system,
		http: &http.Client{Timeout: 90 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || req.URL.Scheme != u.Scheme || !strings.EqualFold(req.URL.Hostname(), u.Hostname()) {
				return errors.New("chat endpoint redirect rejected")
			}
			return nil
		}},
		sem: make(chan struct{}, 4), sessions: make(map[string][]message), inflight: make(map[string]bool), epochs: make(map[string]uint64),
	}, nil
}

func (p *Plugin) Name() string { return "ChatCompletions" }

func (p *Plugin) Init(host pluginapi.Host) error {
	p.host = host
	return host.EnsurePluginChannel(pluginapi.ChannelSpec{
		Name: p.channel, Topic: "Private-per-user AI chat", Private: p.private,
		InviteOnly: false, HistoryEnabled: false,
	})
}

func (p *Plugin) OnConnect(pluginapi.ClientView)             {}
func (p *Plugin) BeforeCommand(pluginapi.CommandEvent) error { return nil }
func (p *Plugin) OnJoin(pluginapi.JoinEvent)                 {}

func (p *Plugin) HandleCommand(e pluginapi.CommandEvent) bool {
	if e.Command != "AIRESET" {
		return false
	}
	if !protocol.NoArgsRE.MatchString(e.Args) {
		_ = p.host.SendNotice(e.Client.ID, "AIRESET takes no parameters")
		return true
	}
	p.reset(e.Client)
	return true
}

func (p *Plugin) OnDisconnect(c pluginapi.ClientView) {
	if c.Identity != "" {
		return
	}
	p.mu.Lock()
	delete(p.sessions, "session:"+c.ID)
	delete(p.inflight, "session:"+c.ID)
	delete(p.epochs, "session:"+c.ID)
	p.mu.Unlock()
}

func (p *Plugin) BeforeMessage(e pluginapi.MessageEvent) (pluginapi.MessageEvent, error) {
	if !strings.EqualFold(e.Target, p.channel) {
		return e, nil
	}
	if e.Notice {
		_ = p.host.SendNotice(e.Client.ID, "ChatCompletions accepts PRIVMSG only")
		return e, pluginapi.ErrHandled
	}
	if !p.host.ClientInChannel(e.Client.ID, p.channel) {
		_ = p.host.SendNotice(e.Client.ID, "Join "+p.channel+" before chatting with AI")
		return e, pluginapi.ErrHandled
	}
	if resetMessageRE.MatchString(e.Text) {
		p.reset(e.Client)
		return e, pluginapi.ErrHandled
	}
	key := p.sessionKey(e.Client)
	p.mu.Lock()
	if p.inflight[key] {
		p.mu.Unlock()
		_ = p.host.SendNotice(e.Client.ID, "AI request already in progress")
		return e, pluginapi.ErrHandled
	}
	select {
	case p.sem <- struct{}{}:
		p.inflight[key] = true
		p.sessions[key] = append(p.sessions[key], message{Role: "user", Content: e.Text})
		p.trimLocked(key)
		snapshot := append([]message(nil), p.sessions[key]...)
		epoch := p.epochs[key]
		p.mu.Unlock()
		go p.complete(e.Client.ID, key, epoch, snapshot)
	default:
		p.mu.Unlock()
		_ = p.host.SendNotice(e.Client.ID, "AI request limit reached; try again shortly")
	}
	return e, pluginapi.ErrHandled
}

func (p *Plugin) reset(c pluginapi.ClientView) {
	key := p.sessionKey(c)
	p.mu.Lock()
	delete(p.sessions, key)
	p.epochs[key]++
	inflight := p.inflight[key]
	p.mu.Unlock()
	if inflight {
		_ = p.host.SendNotice(c.ID, "AI context reset; current request will be discarded when it finishes")
	} else {
		_ = p.host.SendNotice(c.ID, "AI context reset")
	}
}

func (p *Plugin) sessionKey(c pluginapi.ClientView) string {
	if c.Identity != "" {
		return "identity:" + c.Identity
	}
	return "session:" + c.ID
}

func (p *Plugin) trimLocked(key string) {
	msgs := p.sessions[key]
	if len(msgs) > maxConversationMessages {
		p.sessions[key] = append([]message(nil), msgs[len(msgs)-maxConversationMessages:]...)
	}
}

func (p *Plugin) complete(clientID, key string, epoch uint64, conversation []message) {
	defer func() {
		<-p.sem
		p.mu.Lock()
		p.inflight[key] = false
		p.mu.Unlock()
	}()
	messages := make([]message, 0, len(conversation)+1)
	if p.system != "" {
		messages = append(messages, message{Role: "system", Content: p.system})
	}
	messages = append(messages, conversation...)
	answer, responseModel, err := p.request(messages)
	if err != nil {
		log.Printf("ChatCompletions request: %v", err)
		p.mu.Lock()
		if p.epochs[key] == epoch {
			current := p.sessions[key]
			if len(current) > 0 && current[len(current)-1].Role == "user" {
				p.sessions[key] = current[:len(current)-1]
			}
		}
		p.mu.Unlock()
		_ = p.host.SendNotice(clientID, "AI request failed")
		return
	}
	p.mu.Lock()
	if p.epochs[key] != epoch {
		p.mu.Unlock()
		return
	}
	p.sessions[key] = append(p.sessions[key], message{Role: "assistant", Content: answer})
	p.trimLocked(key)
	p.mu.Unlock()
	sender := modelSenderNick(responseModel, p.nick)
	for _, chunk := range pluginutil.ChunkText(answer, 390) {
		if err := p.host.SendChannelMessageToClient(clientID, p.channel, sender, chunk); err != nil {
			return
		}
	}
}

type requestBody struct {
	Model           string    `json:"model"`
	Messages        []message `json:"messages"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
}

type completionResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			Refusal string `json:"refusal"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (p *Plugin) request(messages []message) (string, string, error) {
	payload, err := json.Marshal(requestBody{Model: p.model, Messages: messages, ReasoningEffort: p.reasoning})
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequest(http.MethodPost, p.endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	const max = 2 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return "", "", err
	}
	if len(body) > max {
		return "", "", errors.New("chat response exceeds 2 MiB")
	}
	var decoded completionResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", "", fmt.Errorf("chat endpoint HTTP status %d", resp.StatusCode)
	}
	if decoded.Error != nil {
		return "", "", errors.New("chat endpoint returned an API error")
	}
	if len(decoded.Choices) == 0 {
		return "", "", errors.New("chat endpoint returned no choices")
	}
	text := decoded.Choices[0].Message.Content
	if text == "" {
		text = decoded.Choices[0].Message.Refusal
	}
	text = pluginutil.CleanExternalText(text, 8000)
	if text == "" {
		return "", "", errors.New("chat endpoint returned empty text")
	}
	return text, decoded.Model, nil
}

func modelSenderNick(raw, fallback string) string {
	raw = strings.TrimSpace(raw)
	if !modelRE.MatchString(raw) {
		return fallback
	}
	candidate := modelNickBadRE.ReplaceAllString(raw, "-")
	candidate = strings.Trim(candidate, "-")
	if candidate == "" {
		return fallback
	}
	if !modelNickStartRE.MatchString(candidate) {
		candidate = "ai-" + candidate
	}
	if len(candidate) > 31 {
		candidate = candidate[:31]
	}
	candidate = strings.TrimRight(candidate, "-")
	if protocol.NickRE.MatchString(candidate) {
		return candidate
	}
	return fallback
}
