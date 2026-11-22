package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ircapybara/internal/plugins/pluginutil"
	"ircapybara/internal/protocol"
	"ircapybara/pkg/pluginapi"
)

const maxMirrorPosts = 100

var (
	configEntryRE = regexp.MustCompile(`^(rss|tg)\|(#[A-Za-z0-9_-]{1,48})\|([^|;\x00-\x20]{1,512})\|(1h|24h)$`)
	tgNameRE      = regexp.MustCompile(`^[A-Za-z0-9_]{5,32}$`)
	tgPostRE      = regexp.MustCompile(`data-post="([A-Za-z0-9_]{5,32})/([0-9]{1,20})"`)
	tgTextRE = regexp.MustCompile(`<div class="tgme_widget_message_text[^"]*"[^>]*>`)
 tgDivRE = regexp.MustCompile(`(?i)</?div\b[^>]*>`)
	tgTimeRE      = regexp.MustCompile(`<time[^>]{0,512}datetime="([^"]{1,64})"`)
)

type Post struct {
	ID   string
	Text string
	At   time.Time
}

type source struct {
	kind     string
	channel  string
	value    string
	interval time.Duration

	mu     sync.RWMutex
	lastID string
	posts  []Post
	seen   map[string]struct{}
}

type Plugin struct {
	host    pluginapi.Host
	sources []*source
	http    *http.Client
}

func NewFromEnv() (*Plugin, error) {
	raw := strings.TrimSpace(os.Getenv("IRCAPYBARA_MIRROR_SOURCES"))
	if raw == "" {
		return nil, nil
	}
	p := &Plugin{http: pluginutil.PublicHTTPClient(20 * time.Second)}
	seenChannels := make(map[string]struct{})
	for _, entry := range strings.Split(raw, ";") {
		m := configEntryRE.FindStringSubmatch(entry)
		if m == nil {
			return nil, errors.New("invalid IRCAPYBARA_MIRROR_SOURCES entry")
		}
		kind, channel, value := m[1], m[2], m[3]
		if _, exists := seenChannels[strings.ToLower(channel)]; exists {
			return nil, fmt.Errorf("mirror channel %s configured more than once", channel)
		}
		seenChannels[strings.ToLower(channel)] = struct{}{}
		if kind == "rss" {
			u, err := url.Parse(value)
			if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
				return nil, errors.New("RSS source must be an https URL without credentials or fragments")
			}
		} else if !tgNameRE.MatchString(value) {
			return nil, errors.New("Telegram mirror source must be a public channel username")
		}
		interval := time.Hour
		if m[4] == "24h" {
			interval = 24 * time.Hour
		}
		p.sources = append(p.sources, &source{kind: kind, channel: channel, value: value, interval: interval, seen: make(map[string]struct{})})
	}
	return p, nil
}

func (p *Plugin) Name() string { return "MirrorPlugin" }

func (p *Plugin) Init(host pluginapi.Host) error {
	p.host = host
	for _, src := range p.sources {
		topic := "RSS mirror: " + src.value
		if src.kind == "tg" {
			topic = "Telegram mirror: https://t.me/" + src.value
		}
		if err := host.EnsurePluginChannel(pluginapi.ChannelSpec{Name: src.channel, Topic: topic, HistoryEnabled: false}); err != nil {
			return err
		}
		go p.runSource(src)
	}
	return nil
}

func (p *Plugin) OnConnect(pluginapi.ClientView)             {}
func (p *Plugin) OnDisconnect(pluginapi.ClientView)          {}
func (p *Plugin) BeforeCommand(pluginapi.CommandEvent) error { return nil }
func (p *Plugin) BeforeMessage(e pluginapi.MessageEvent) (pluginapi.MessageEvent, error) {
	return e, nil
}

func (p *Plugin) OnJoin(e pluginapi.JoinEvent) {
	for _, src := range p.sources {
		if !strings.EqualFold(src.channel, e.Channel) {
			continue
		}
		src.mu.RLock()
		posts := append([]Post(nil), src.posts...)
		src.mu.RUnlock()
		if len(posts) == 0 {
			return
		}
		from := src.sender()
		if e.Client.HistoryCap {
			items := make([]pluginapi.SyntheticHistoryItem, 0, len(posts))
			for _, post := range posts {
				items = append(items, pluginapi.SyntheticHistoryItem{At: post.At, From: from, Text: renderPost(post)})
			}
			if err := p.host.DeliverSyntheticHistory(e.Client.ID, src.channel, items); err != nil {
				log.Printf("mirror history %s/%s: %v", src.channel, e.Client.Nick, err)
			}
			return
		}
		for _, post := range posts {
			if err := p.host.SendChannelMessageToClient(e.Client.ID, src.channel, from, renderPost(post)); err != nil {
				return
			}
		}
	}
}

func (s *source) sender() string {
	if s.kind == "tg" {
		if protocol.NickRE.MatchString(s.value) {
			return s.value
		}
		candidate := "tg-" + s.value
		if len(candidate) > 31 {
			candidate = candidate[:31]
		}
		if protocol.NickRE.MatchString(candidate) {
			return candidate
		}
		return "tg-mirror"
	}
	return "rss-mirror"
}

func (p *Plugin) runSource(src *source) {
	p.poll(src, true)
	t := time.NewTicker(src.interval)
	defer t.Stop()
	for range t.C {
		p.poll(src, false)
	}
}

func (p *Plugin) poll(src *source, initial bool) {
	var posts []Post
	var err error
	if src.kind == "rss" {
		posts, err = p.fetchRSS(src.value)
	} else {
		posts, err = p.fetchTelegramPreview(src.value)
	}
	if err != nil {
		log.Printf("mirror %s fetch %s: %v", src.kind, src.channel, err)
		return
	}
	if len(posts) > maxMirrorPosts {
		posts = posts[len(posts)-maxMirrorPosts:]
	}

	src.mu.Lock()
	if initial || src.lastID == "" {
		src.posts = append([]Post(nil), posts...)
		src.seen = make(map[string]struct{}, len(posts))
		for _, post := range posts {
			src.seen[post.ID] = struct{}{}
		}
		if len(posts) > 0 {
			src.lastID = posts[len(posts)-1].ID
		}
		src.mu.Unlock()
		return
	}
	var fresh []Post
	for _, post := range posts {
		if _, ok := src.seen[post.ID]; !ok {
			fresh = append(fresh, post)
		}
	}
	if len(fresh) > 0 {
		src.posts = append(src.posts, fresh...)
		if len(src.posts) > maxMirrorPosts {
			src.posts = append([]Post(nil), src.posts[len(src.posts)-maxMirrorPosts:]...)
		}
		src.seen = make(map[string]struct{}, len(src.posts))
		for _, post := range src.posts {
			src.seen[post.ID] = struct{}{}
		}
		src.lastID = src.posts[len(src.posts)-1].ID
	}
	src.mu.Unlock()

	for _, post := range fresh {
		if err := p.host.SendChannelMessage(src.channel, src.sender(), renderPost(post)); err != nil {
			log.Printf("mirror %s post %s: %v", src.kind, src.channel, err)
		}
	}
}

type rssDoc struct {
	Channel struct {
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
	Entries []atomEntry `xml:"entry"`
}

type rssItem struct {
	GUID        string `xml:"guid"`
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
}

type atomEntry struct {
	ID      string `xml:"id"`
	Title   string `xml:"title"`
	Summary string `xml:"summary"`
	Content string `xml:"content"`
	Updated string `xml:"updated"`
	Link    struct {
		Href string `xml:"href,attr"`
	} `xml:"link"`
}

func (p *Plugin) fetchRSS(rawURL string) ([]Post, error) {
	body, err := p.fetch(rawURL)
	if err != nil {
		return nil, err
	}
	return parseRSS(body)
}

func parseRSS(body []byte) ([]Post, error) {
	var doc rssDoc
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse RSS/Atom: %w", err)
	}
	var out []Post
	for _, item := range doc.Channel.Items {
		title := pluginutil.HTMLText(item.Title, 220)
		link := safeFeedLink(item.Link)
		desc := pluginutil.HTMLText(item.Description, 220)
		text := mirrorText(title, desc, link)
		if text == "" {
			continue
		}
		id := strings.TrimSpace(item.GUID)
		if id == "" {
			id = hashID(item.Title + "\x00" + item.Link + "\x00" + item.PubDate + "\x00" + item.Description)
		}
		out = append(out, Post{ID: id, Text: text, At: parseFeedTime(item.PubDate)})
	}
	for _, item := range doc.Entries {
		title := pluginutil.HTMLText(item.Title, 220)
		link := safeFeedLink(item.Link.Href)
		desc := pluginutil.HTMLText(item.Summary, 220)
		if desc == "" {
			desc = pluginutil.HTMLText(item.Content, 220)
		}
		text := mirrorText(title, desc, link)
		if text == "" {
			continue
		}
		id := strings.TrimSpace(item.ID)
		if id == "" {
			id = hashID(item.Title + "\x00" + item.Link.Href + "\x00" + item.Updated + "\x00" + item.Summary)
		}
		out = append(out, Post{ID: id, Text: text, At: parseFeedTime(item.Updated)})
	}
	orderPosts(out)
	return out, nil
}

func (p *Plugin) fetchTelegramPreview(channel string) ([]Post, error) {
	body, err := p.fetch("https://t.me/s/" + channel)
	if err != nil {
		return nil, err
	}
	return parseTelegramPreview(string(body))
}

func parseTelegramPreview(html string) ([]Post, error) {
	marks := tgPostRE.FindAllStringSubmatchIndex(html, -1)
	var out []Post
	for i, mark := range marks {
		if len(mark) < 6 {
			continue
		}
		postChannel := html[mark[2]:mark[3]]
		postID := html[mark[4]:mark[5]]
		start := mark[1]
		end := len(html)
		if i+1 < len(marks) {
			end = marks[i+1][0]
		}
		segment := html[start:end]
		text := ""
		if m := tgTextRE.FindStringIndex(segment); m != nil {
			inner := segment[m[1]:]
			depth := 1
			for _, tag := range tgDivRE.FindAllStringIndex(inner,-1) {
				if inner[tag[0]+1]=='/' { depth-- } else { depth++ }
				if depth==0 {
					text=pluginutil.HTMLText(inner[:tag[0]],300)
					break
				}
			}
		}
		if text == "" {
			switch {
			case strings.Contains(segment, "tgme_widget_message_photo_wrap"):
				text = "[image]"
			case strings.Contains(segment, "tgme_widget_message_video"):
				text = "[video]"
			case strings.Contains(segment, "tgme_widget_message_document"):
				text = "[file]"
			default:
				continue
			}
		}
		at := time.Time{}
		if m := tgTimeRE.FindStringSubmatch(segment); m != nil {
			at, _ = time.Parse(time.RFC3339, m[1])
		}
		link := "https://t.me/" + postChannel + "/" + postID
		text = mirrorText("", text, link)
		out = append(out, Post{ID: postChannel + "/" + postID, Text: text, At: at})
	}
	orderPosts(out)
	return out, nil
}

func (p *Plugin) fetch(rawURL string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "IRCapybara-Mirror/0.2")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	const max = 2 << 20
	r := io.LimitReader(resp.Body, max+1)
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if len(body) > max {
		return nil, errors.New("source response exceeds 2 MiB")
	}
	return body, nil
}

func safeFeedLink(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return ""
	}
	return pluginutil.PlainText(u.String(), 180)
}

func mirrorText(title, desc, link string) string {
	base := title
	if desc != "" && desc != title {
		if base != "" {
			base += "\n"
		}
		base += desc
	}
	if base == "" {
		base = "New post"
	}
	if link == "" {
		return pluginutil.PlainText(base, 390)
	}
	return pluginutil.PlainText(base, 389-utf8.RuneCountInString(link)) + "\n" + link
}

func renderPost(post Post) string {
	if post.At.IsZero() {
		return post.Text
	}
	// Keep the textual timestamp compact; server-time remains available as metadata.
	return "[" + post.At.Format("01-02 15:04") + "] " + post.Text
}

func (p *Plugin) HelpModules() map[string][]string {
	return map[string][]string{
		"MIRROR": {
			"RSS/TG mirror channels are configured by IRCAPYBARA_MIRROR_SOURCES.",
			"Example: rss|#rss-news|https://example.com/feed.xml|1h;tg|#tg-news|publicchannel|24h",
			"RSS/TG post timestamps are prepended when the source exposes a parseable date.",
		},
	}
}

func hashID(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func parseFeedTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano, time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}

func orderPosts(posts []Post) {
	hasTimes := false
	for _, p := range posts {
		if !p.At.IsZero() {
			hasTimes = true
			break
		}
	}
	if hasTimes {
		sort.SliceStable(posts, func(i, j int) bool {
			if posts[i].At.IsZero() != posts[j].At.IsZero() {
				return posts[i].At.IsZero()
			}
			return posts[i].At.Before(posts[j].At)
		})
		return
	}
	// RSS/Atom commonly lists newest first. With no timestamps, reverse so replay
	// remains oldest-to-newest and the newest item becomes lastID.
	for i, j := 0, len(posts)-1; i < j; i, j = i+1, j-1 {
		posts[i], posts[j] = posts[j], posts[i]
	}
}
