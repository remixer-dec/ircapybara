package youtubeffmpeg

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	youtube "github.com/kkdai/youtube/v2"
	ffmpeg "github.com/u2takey/ffmpeg-go"

	"ircapybara/internal/protocol"
	"ircapybara/pkg/pluginapi"
)

const jobQueueSize = 8

const defaultSourceMaxMB = 128

var (
	boolRE             = regexp.MustCompile(`^(?:true|false)$`)
	modeRE             = regexp.MustCompile(`^(?:channel|dm|both)$`)
	compressionModeRE  = regexp.MustCompile(`^(?:none|zip|gzip)$`)
	ffmpegPathRE       = regexp.MustCompile(`^(?:[A-Za-z0-9._-]{1,64}|(?:/|\./)[A-Za-z0-9_./-]{1,240})$`)
	youtubeURLRE       = regexp.MustCompile(`^https://(?:(?:www|m)\.)?(?:youtube\.com/(?:watch\?v=|shorts/)[A-Za-z0-9_-]{11}(?:[&?][A-Za-z0-9_=&%.-]{0,200})?|youtu\.be/[A-Za-z0-9_-]{11}(?:\?[A-Za-z0-9_=&%.-]{0,200})?)$`)
	legacyPresetPairRE = regexp.MustCompile(`^([A-Za-z0-9:_-]{1,64})=([^;\r\n]{0,512})$`)
	outputFormatRE     = regexp.MustCompile(`^[A-Za-z0-9]{1,16}$`)
	templateRE         = regexp.MustCompile(`^[A-Za-z0-9 _.,(){}-]{1,120}$`)
)

var youtubeDefaultClientMu sync.Mutex

type youtubeProfile struct {
	name string
	info youtube.ClientInfo
}

var visionOSClient = youtube.ClientInfo{
	// This reduced profile is intentionally compatible with kkdai/youtube's
	// exported ClientInfo. A current Go consumer has verified it against the
	// live Innertube API specifically to avoid Android/Android-VR googlevideo
	// 403s. Keep it independent of upstream's package-global DefaultClient.
	Name:        "VISIONOS",
	Version:     "1.02",
	DeviceModel: "RealityDevice17,1",
	UserAgent:   "Mozilla/5.0 (Macintosh; Intel Mac OS X 15_7_3) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15",
}

func youtubeProfiles() []youtubeProfile {
	return []youtubeProfile{
		{name: "visionos", info: visionOSClient},
		{name: "android", info: youtube.AndroidClient},
		{name: "web", info: youtube.WebClient},
		{name: "web-embedded", info: youtube.EmbeddedClient},
		// Android VR remains a last fallback. The active shoce/youtube fork
		// documents that it may only serve a prefix of some googlevideo streams.
		{name: "android-vr", info: youtube.AndroidVRClient},
	}
}

type job struct {
	client       pluginapi.ClientView
	url          string
	conversation string
	key          string
}

type Plugin struct {
	host pluginapi.Host

	mode             string
	channel          string
	private          bool
	nick             string
	ffmpegPath       string
	presetArgs       []string
	format           string
	filenameTemplate string
	sourceMaxBytes   int64
	compressionMode  string

	queue   chan job
	mu      sync.Mutex
	pending map[string]bool
}

func NewFromEnv() (*Plugin, error) {
	enabled := strings.TrimSpace(os.Getenv("IRCAPYBARA_YTDL_ENABLED"))
	if enabled == "" {
		enabled = "false"
	}
	if !boolRE.MatchString(enabled) {
		return nil, errors.New("IRCAPYBARA_YTDL_ENABLED must be true or false")
	}
	if enabled != "true" {
		return nil, nil
	}
	mode := strings.TrimSpace(os.Getenv("IRCAPYBARA_YTDL_MODE"))
	if mode == "" {
		mode = "channel"
	}
	if !modeRE.MatchString(mode) {
		return nil, errors.New("IRCAPYBARA_YTDL_MODE must be channel, dm, or both")
	}
	channel := strings.TrimSpace(os.Getenv("IRCAPYBARA_YTDL_CHANNEL"))
	if channel == "" {
		channel = "#yt-dl"
	}
	if !protocol.ChannelRE.MatchString(channel) {
		return nil, errors.New("invalid IRCAPYBARA_YTDL_CHANNEL")
	}
	privateRaw := strings.TrimSpace(os.Getenv("IRCAPYBARA_YTDL_PRIVATE"))
	if privateRaw == "" {
		privateRaw = "false"
	}
	if !boolRE.MatchString(privateRaw) {
		return nil, errors.New("IRCAPYBARA_YTDL_PRIVATE must be true or false")
	}
	nick := strings.TrimSpace(os.Getenv("IRCAPYBARA_YTDL_NICK"))
	if nick == "" {
		nick = "yt-dl"
	}
	if !protocol.NickRE.MatchString(nick) {
		return nil, errors.New("invalid IRCAPYBARA_YTDL_NICK")
	}
	ffmpegPath := strings.TrimSpace(os.Getenv("IRCAPYBARA_YTDL_FFMPEG_PATH"))
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	if !ffmpegPathRE.MatchString(ffmpegPath) {
		return nil, errors.New("invalid IRCAPYBARA_YTDL_FFMPEG_PATH")
	}
	presetRaw := strings.TrimSpace(os.Getenv("IRCAPYBARA_YTDL_FFMPEG_PRESET"))
	if presetRaw == "" {
		presetRaw = "-c:v copy -c:a copy -movflags +faststart -f mp4"
	}
	preset, format, err := parsePreset(presetRaw)
	if err != nil {
		return nil, err
	}
	filenameTemplate := strings.TrimSpace(os.Getenv("IRCAPYBARA_YTDL_FILENAME_TEMPLATE"))
	if filenameTemplate == "" {
		filenameTemplate = "{title}-{id}.{ext}"
	}
	if !templateRE.MatchString(filenameTemplate) {
		return nil, errors.New("invalid IRCAPYBARA_YTDL_FILENAME_TEMPLATE")
	}
	probe := strings.NewReplacer("{title}", "title", "{id}", "abcdefghijk", "{ext}", "mp4").Replace(filenameTemplate)
	if strings.ContainsAny(probe, "{}") {
		return nil, errors.New("filename template has an unknown placeholder")
	}
	sourceMaxMBRaw := strings.TrimSpace(os.Getenv("IRCAPYBARA_YTDL_SOURCE_MAX_MB"))
	if sourceMaxMBRaw == "" {
		sourceMaxMBRaw = strconv.Itoa(defaultSourceMaxMB)
	}
	sourceMaxMB, err := strconv.ParseInt(sourceMaxMBRaw, 10, 64)
	if err != nil || sourceMaxMB < 1 || sourceMaxMB > 1024 {
		return nil, errors.New("IRCAPYBARA_YTDL_SOURCE_MAX_MB must be an integer from 1 to 1024")
	}
	compressionMode := strings.ToLower(strings.TrimSpace(os.Getenv("IRCAPYBARA_YTDL_COMPRESSION_MODE")))
	if compressionMode == "" {
		compressionMode = "none"
	}
	if !compressionModeRE.MatchString(compressionMode) {
		return nil, errors.New("IRCAPYBARA_YTDL_COMPRESSION_MODE must be none, zip, or gzip")
	}
	return &Plugin{
		mode: mode, channel: channel, private: privateRaw == "true", nick: nick,
		ffmpegPath: ffmpegPath, presetArgs: preset, format: format, filenameTemplate: filenameTemplate,
		sourceMaxBytes: sourceMaxMB * 1024 * 1024, compressionMode: compressionMode,
		queue: make(chan job, jobQueueSize), pending: make(map[string]bool),
	}, nil
}

func parsePreset(raw string) ([]string, string, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > 4096 || strings.ContainsAny(raw, "\x00\r\n") {
		return nil, "", errors.New("invalid IRCAPYBARA_YTDL_FFMPEG_PRESET")
	}
	var args []string
	var err error
	// Backward compatibility for the earlier key=value;key=value syntax. Keys are
	// no longer allowlisted: this is trusted administrator configuration.
	if !strings.HasPrefix(raw, "-") && strings.Contains(raw, "=") {
		for _, item := range strings.Split(raw, ";") {
			m := legacyPresetPairRE.FindStringSubmatch(item)
			if m == nil {
				return nil, "", errors.New("invalid legacy IRCAPYBARA_YTDL_FFMPEG_PRESET pair")
			}
			args = append(args, "-"+m[1])
			if m[2] != "" {
				args = append(args, m[2])
			}
		}
	} else {
		args, err = splitAdminArgs(raw)
		if err != nil {
			return nil, "", err
		}
	}
	if len(args) == 0 || len(args) > 128 {
		return nil, "", errors.New("ffmpeg preset must contain 1..128 argv elements")
	}
	format := "mp4"
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-f" {
			candidate := strings.ToLower(args[i+1])
			if outputFormatRE.MatchString(candidate) {
				format = candidate
			} else {
				// The admin option is still passed unchanged to FFmpeg. We only need a
				// filesystem-safe extension for our managed temporary/output filename.
				format = "bin"
			}
		}
	}
	return args, format, nil
}

// splitAdminArgs tokenizes trusted administrator configuration into argv without
// invoking a shell. It supports ordinary whitespace, single/double quotes, and
// backslash escaping; no expansion, substitution, globbing, or shell operators run.
func splitAdminArgs(raw string) ([]string, error) {
	var out []string
	var b strings.Builder
	var quote rune
	escaped := false
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for _, r := range raw {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if quote != '\'' && r == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		if r == ' ' || r == '\t' {
			flush()
			continue
		}
		b.WriteRune(r)
		if b.Len() > 1024 {
			return nil, errors.New("ffmpeg preset argv element too long")
		}
	}
	if escaped || quote != 0 {
		return nil, errors.New("unterminated escape or quote in ffmpeg preset")
	}
	flush()
	return out, nil
}

func injectPresetArgs(argv, preset []string) ([]string, error) {
	// ffmpeg-go compiles the hard server -fs limit as an output option. Insert
	// administrator options immediately before it so the server-owned limit is
	// the final -fs applied to the managed output.
	idx := -1
	for i := 1; i < len(argv); i++ {
		if argv[i] == "-fs" {
			idx = i
		}
	}
	if idx < 0 {
		return nil, errors.New("compiled ffmpeg command is missing server file-size limit")
	}
	out := make([]string, 0, len(argv)+len(preset))
	out = append(out, argv[:idx]...)
	out = append(out, preset...)
	out = append(out, argv[idx:]...)
	return out, nil
}

func (p *Plugin) Name() string { return "YouTubeFFmpeg" }

func (p *Plugin) Init(host pluginapi.Host) error {
	p.host = host
	if p.mode == "channel" || p.mode == "both" {
		if err := host.EnsurePluginChannel(pluginapi.ChannelSpec{
			Name: p.channel, Topic: "YouTube media retrieval for content you are authorized to download",
			Private: p.private, InviteOnly: false, HistoryEnabled: false,
		}); err != nil {
			return err
		}
	}
	go p.worker()
	return nil
}

func (p *Plugin) OnConnect(pluginapi.ClientView)             {}
func (p *Plugin) OnDisconnect(pluginapi.ClientView)          {}
func (p *Plugin) BeforeCommand(pluginapi.CommandEvent) error { return nil }
func (p *Plugin) OnJoin(pluginapi.JoinEvent)                 {}

func (p *Plugin) BeforeMessage(e pluginapi.MessageEvent) (pluginapi.MessageEvent, error) {
	conversation := ""
	matched := false
	if (p.mode == "channel" || p.mode == "both") && strings.EqualFold(e.Target, p.channel) {
		matched = true
		conversation = p.channel
		if !p.host.ClientInChannel(e.Client.ID, p.channel) {
			_ = p.host.SendNotice(e.Client.ID, "Join "+p.channel+" before submitting a YouTube URL")
			return e, pluginapi.ErrHandled
		}
	}
	if (p.mode == "dm" || p.mode == "both") && strings.EqualFold(e.Target, p.nick) {
		matched = true
		conversation = e.Client.Nick
	}
	if !matched {
		return e, nil
	}
	if e.Notice {
		_ = p.host.SendNotice(e.Client.ID, "YouTubeFFmpeg accepts PRIVMSG only")
		return e, pluginapi.ErrHandled
	}
	if !e.Client.Extended {
		_ = p.host.SendNotice(e.Client.ID, "YouTube downloads require XMODE EXTENDED")
		return e, pluginapi.ErrHandled
	}
	url := strings.TrimSpace(e.Text)
	if !youtubeURLRE.MatchString(url) {
		_ = p.progress(e.Client, conversation, "rejected: send one canonical YouTube URL")
		return e, pluginapi.ErrHandled
	}
	key := sessionKey(e.Client)
	p.mu.Lock()
	if !e.Client.IsOper && p.pending[key] {
		p.mu.Unlock()
		_ = p.progress(e.Client, conversation, "rejected: you already have an active or queued request")
		return e, pluginapi.ErrHandled
	}
	if !e.Client.IsOper {
		p.pending[key] = true
	}
	j := job{client: e.Client, url: url, conversation: conversation, key: key}
	select {
	case p.queue <- j:
		position := len(p.queue)
		if position < 1 {
			position = 1
		}
		p.mu.Unlock()
		_ = p.progress(e.Client, conversation, fmt.Sprintf("queued (position %d)", position))
	default:
		if !e.Client.IsOper {
			delete(p.pending, key)
		}
		p.mu.Unlock()
		_ = p.progress(e.Client, conversation, "rejected: queue is full")
	}
	return e, pluginapi.ErrHandled
}

func sessionKey(c pluginapi.ClientView) string {
	if c.Identity != "" {
		return "identity:" + c.Identity
	}
	return "session:" + c.ID
}

func (p *Plugin) worker() {
	for j := range p.queue {
		p.runJob(j)
		if !j.client.IsOper {
			p.mu.Lock()
			delete(p.pending, j.key)
			p.mu.Unlock()
		}
	}
}

func (p *Plugin) progress(c pluginapi.ClientView, conversation, text string) error {
	text = strings.TrimSpace(text)
	if !protocol.ValidText(text, 390) {
		return errors.New("invalid progress text")
	}
	if strings.HasPrefix(conversation, "#") {
		return p.host.SendChannelMessageToClient(c.ID, conversation, p.nick, text)
	}
	return p.host.SendDirectMessageToClient(c.ID, p.nick, text)
}

type sourceRole string

const (
	roleCombined sourceRole = "combined"
	roleVideo    sourceRole = "video"
	roleAudio    sourceRole = "audio"
)

type sourceItem struct {
	format youtube.Format
	role   sourceRole
}

type sourcePlan struct {
	items []sourceItem
	label string
}

type mediaIntent int

const (
	intentAV mediaIntent = iota
	intentAudio
	intentVideo
)

func (p *Plugin) runJob(j job) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	if err := p.progress(j.client, j.conversation, "fetching metadata"); err != nil {
		return
	}

	limit := p.host.MaxAttachmentBytes()
	video, inputPaths, plan, ok := p.downloadYouTubeSource(ctx, j, p.sourceMaxBytes, limit)
	if !ok {
		return
	}
	defer func() {
		for _, path := range inputPaths {
			_ = os.Remove(path)
		}
	}()
	_ = p.progress(j.client, j.conversation, "download complete; running ffmpeg")

	out, err := os.CreateTemp("", "ircapybara-ytdl-out-*."+p.format)
	if err != nil {
		_ = p.progress(j.client, j.conversation, "failed: temporary storage unavailable")
		return
	}
	outPath := out.Name()
	_ = out.Close()
	defer os.Remove(outPath)

	inputs := make([]*ffmpeg.Stream, 0, len(inputPaths))
	for i, path := range inputPaths {
		in := ffmpeg.Input(path, ffmpeg.KwArgs{"protocol_whitelist": "file,pipe"})
		switch plan.items[i].role {
		case roleAudio:
			inputs = append(inputs, in.Audio())
		case roleVideo:
			inputs = append(inputs, in.Video())
		default:
			inputs = append(inputs, in)
		}
	}

	stderr := &limitedLogWriter{limit: 4096}
	streamCmd := ffmpeg.Output(inputs, outPath, ffmpeg.KwArgs{"fs": strconv.FormatInt(limit+1, 10)}).
		OverWriteOutput().SetFfmpegPath(p.ffmpegPath).WithTimeout(4 * time.Minute).WithErrorOutput(stderr).Silent(true)
	cmd := streamCmd.Compile()
	cmd.Args, err = injectPresetArgs(cmd.Args, p.presetArgs)
	if err != nil {
		log.Printf("YouTubeFFmpeg preset injection: %v", err)
		_ = p.progress(j.client, j.conversation, "failed: invalid ffmpeg command")
		return
	}
	if err := cmd.Run(); err != nil {
		log.Printf("YouTubeFFmpeg ffmpeg: %v: %s", err, stderr.String())
		_ = p.progress(j.client, j.conversation, "failed: ffmpeg conversion failed")
		return
	}
	st, err := os.Stat(outPath)
	if err != nil || st.Size() < 1 || st.Size() > limit {
		_ = p.progress(j.client, j.conversation, "failed: converted file exceeds the attachment limit")
		return
	}
	data, err := os.ReadFile(outPath)
	if err != nil || int64(len(data)) != st.Size() {
		_ = p.progress(j.client, j.conversation, "failed: could not read converted output")
		return
	}

	filename := p.outputFilename(video.Title, video.ID)
	mime := outputMIME(p.format, p.presetArgs)
	if p.compressionMode != "none" {
		_ = p.progress(j.client, j.conversation, "compressing output as "+p.compressionMode)
		filename, mime, data, err = compressAttachment(p.compressionMode, filename, data)
		if err != nil {
			log.Printf("YouTubeFFmpeg %s compression: %v", p.compressionMode, err)
			_ = p.progress(j.client, j.conversation, "failed: output compression failed")
			return
		}
		if int64(len(data)) > limit {
			_ = p.progress(j.client, j.conversation, "failed: compressed attachment exceeds the attachment limit")
			return
		}
	}

	_ = p.progress(j.client, j.conversation, fmt.Sprintf("ffmpeg complete; publishing %s (%d bytes)", filename, len(data)))
	_, err = p.host.SendAttachmentToClient(j.client.ID, pluginapi.AttachmentSpec{
		Conversation: j.conversation, From: p.nick, Filename: filename, ContentType: mime, Data: data,
	})
	if err != nil {
		log.Printf("YouTubeFFmpeg attachment: %v", err)
		_ = p.progress(j.client, j.conversation, "failed: attachment window is unavailable")
		return
	}
	_ = p.progress(j.client, j.conversation, "ready; use the attachment download button")
}

// downloadYouTubeSource serially tries multiple Innertube client identities.
// kkdai/youtube exposes these identities through the package-global DefaultClient
// rather than a per-Client setter, so hold a package-local lock for the whole
// attempt sequence and restore the original value afterward. The YouTube plugin
// has one worker, so this adds no job-level serialization beyond what already
// exists.
func (p *Plugin) downloadYouTubeSource(ctx context.Context, j job, sourceBudget, attachmentLimit int64) (*youtube.Video, []string, *sourcePlan, bool) {
	youtubeDefaultClientMu.Lock()
	previousDefault := youtube.DefaultClient
	defer func() {
		youtube.DefaultClient = previousDefault
		youtubeDefaultClientMu.Unlock()
	}()

	intent := desiredMediaIntent(p.presetArgs, p.format)
	var lastErr error
	metadataWorked := false
	planFound := false
	for _, profile := range youtubeProfiles() {
		youtube.DefaultClient = profile.info
		_ = p.progress(j.client, j.conversation, "trying YouTube client "+profile.name)

		// The active shoce/youtube fork caps media ranges at 2 MiB and accepts
		// HTTP 206. Keep the request fan-out deliberately small on our one-worker
		// plugin as an additional bound.
		client := youtube.Client{ChunkSize: 2 * 1024 * 1024, MaxRoutines: 4}
		video, err := client.GetVideoContext(ctx, j.url)
		if err != nil {
			lastErr = err
			log.Printf("YouTubeFFmpeg %s metadata: %v", profile.name, err)
			continue
		}
		metadataWorked = true

		plan := chooseSourcePlan(video.Formats, sourceBudget, attachmentLimit, video.Duration, intent, p.format)
		if plan == nil {
			log.Printf("YouTubeFFmpeg %s: no suitable source plan fits %d byte source budget", profile.name, sourceBudget)
			continue
		}
		planFound = true
		_ = p.progress(j.client, j.conversation, fmt.Sprintf("%s: selected %s; downloading", profile.name, plan.label))

		paths, total, err := p.downloadSourcePlan(ctx, j, &client, profile.name, video, plan, sourceBudget)
		if err != nil {
			lastErr = err
			log.Printf("YouTubeFFmpeg %s download: %v", profile.name, err)
			_ = p.progress(j.client, j.conversation, profile.name+" source rejected/interrupted; trying fallback")
			continue
		}

		log.Printf("YouTubeFFmpeg: client %s succeeded (%d bytes, %s)", profile.name, total, plan.label)
		_ = p.progress(j.client, j.conversation, "YouTube client "+profile.name+" succeeded")
		return video, paths, plan, true
	}

	if lastErr != nil {
		log.Printf("YouTubeFFmpeg: all client profiles failed, last stream/metadata error: %v", lastErr)
	}
	switch {
	case !metadataWorked:
		_ = p.progress(j.client, j.conversation, "failed: YouTube metadata was rejected by all client profiles")
	case !planFound:
		_ = p.progress(j.client, j.conversation, fmt.Sprintf("failed: no suitable source streams fit the %d MiB source budget", sourceBudget/(1024*1024)))
	default:
		_ = p.progress(j.client, j.conversation, "failed: all YouTube client profiles rejected or interrupted the source streams")
	}
	return nil, nil, nil, false
}

func (p *Plugin) downloadSourcePlan(ctx context.Context, j job, client *youtube.Client, profile string, video *youtube.Video, plan *sourcePlan, budget int64) ([]string, int64, error) {
	paths := make([]string, 0, len(plan.items))
	var total int64
	cleanup := func() {
		for _, path := range paths {
			_ = os.Remove(path)
		}
	}

	for _, item := range plan.items {
		remaining := budget - total
		if remaining < 1 {
			cleanup()
			return nil, 0, errors.New("source budget exhausted")
		}
		path, n, err := p.downloadSourceItem(ctx, j, client, profile, video, item, remaining)
		if err != nil {
			cleanup()
			return nil, 0, err
		}
		paths = append(paths, path)
		total += n
	}
	return paths, total, nil
}

func (p *Plugin) downloadSourceItem(ctx context.Context, j job, client *youtube.Client, profile string, video *youtube.Video, item sourceItem, budget int64) (string, int64, error) {
	// Do not clear ContentLength here. The active fork intentionally uses small
	// HTTP Range requests because googlevideo can reject full-file GETs. It also
	// probes an unknown length with bytes=0-0 before falling back.
	downloadFormat := item.format
	stream, reportedSize, err := client.GetStreamContext(ctx, video, &downloadFormat)
	if err != nil {
		return "", 0, err
	}
	if reportedSize > budget {
		_ = stream.Close()
		return "", 0, fmt.Errorf("reported %s stream size %d exceeds remaining source budget %d", item.role, reportedSize, budget)
	}

	in, err := os.CreateTemp("", "ircapybara-ytdl-in-*"+sourceExtension(item.format.MimeType))
	if err != nil {
		_ = stream.Close()
		return "", 0, err
	}
	path := in.Name()
	tracker := &progressWriter{dst: in, total: reportedSize, report: func(pct int) {
		_ = p.progress(j.client, j.conversation, fmt.Sprintf("%s %s download %d%%", profile, item.role, pct))
	}}
	n, copyErr := io.Copy(tracker, io.LimitReader(stream, budget+1))
	streamCloseErr := stream.Close()
	fileCloseErr := in.Close()

	if copyErr != nil {
		_ = os.Remove(path)
		return "", 0, copyErr
	}
	if streamCloseErr != nil {
		log.Printf("YouTubeFFmpeg %s %s stream close: %v", profile, item.role, streamCloseErr)
	}
	if fileCloseErr != nil {
		_ = os.Remove(path)
		return "", 0, fileCloseErr
	}
	if n > budget {
		_ = os.Remove(path)
		return "", 0, fmt.Errorf("downloaded %s stream size %d exceeds remaining source budget %d", item.role, n, budget)
	}
	if n < 1 {
		_ = os.Remove(path)
		return "", 0, fmt.Errorf("empty %s YouTube stream", item.role)
	}
	if reportedSize > 0 && n != reportedSize {
		_ = os.Remove(path)
		return "", 0, fmt.Errorf("short %s YouTube stream: expected %d bytes, got %d", item.role, reportedSize, n)
	}
	return path, n, nil
}

func desiredMediaIntent(args []string, format string) mediaIntent {
	for _, arg := range args {
		if arg == "-vn" {
			return intentAudio
		}
		if arg == "-an" {
			return intentVideo
		}
	}
	switch strings.ToLower(format) {
	case "mp3", "wav", "flac", "ogg", "opus", "m4a", "aac", "adts", "oga":
		return intentAudio
	default:
		return intentAV
	}
}

func chooseSourcePlan(formats youtube.FormatList, sourceBudget, attachmentLimit int64, duration time.Duration, intent mediaIntent, outputFormat string) *sourcePlan {
	combined := make([]youtube.Format, 0)
	videoOnly := make([]youtube.Format, 0)
	audioOnly := make([]youtube.Format, 0)
	for _, f := range formats {
		mime := strings.ToLower(strings.TrimSpace(strings.SplitN(f.MimeType, ";", 2)[0]))
		switch {
		case strings.HasPrefix(mime, "audio/") && f.AudioChannels > 0:
			audioOnly = append(audioOnly, f)
		case strings.HasPrefix(mime, "video/") && f.AudioChannels > 0:
			combined = append(combined, f)
		case strings.HasPrefix(mime, "video/"):
			videoOnly = append(videoOnly, f)
		}
	}

	combined = filterFormatsByBudget(combined, sourceBudget, duration)
	videoOnly = filterFormatsByBudget(videoOnly, sourceBudget, duration)
	audioOnly = filterFormatsByBudget(audioOnly, sourceBudget, duration)

	// YouTube may expose several language tracks, including AI auto-dubs. The
	// audioTrack ID carries the track role: .4 is the upload-language original,
	// .3 is a human dub and .10 is an automatic dub. audioIsDefault is locale
	// dependent and therefore must not outrank the original track. A missing
	// audioTrack means the video has only one soundtrack, which is inherently
	// the original/only track.
	preferredAudioOnly := preferOriginalAudioFormats(audioOnly)
	preferredCombined := preferOriginalAudioFormats(combined)

	switch intent {
	case intentAudio:
		if f := cheapestFormat(preferredAudioOnly, duration); f != nil {
			return &sourcePlan{items: []sourceItem{{format: *f, role: roleAudio}}, label: audioSourceLabel("audio-only stream", *f)}
		}
		if f := cheapestFormat(preferredCombined, duration); f != nil {
			return &sourcePlan{items: []sourceItem{{format: *f, role: roleAudio}}, label: audioSourceLabel("combined stream (audio track only)", *f)}
		}
		return nil
	case intentVideo:
		if f := cheapestVideoFormat(videoOnly, duration); f != nil {
			return &sourcePlan{items: []sourceItem{{format: *f, role: roleVideo}}, label: videoLabel(*f) + " video-only stream"}
		}
		if f := cheapestVideoFormat(combined, duration); f != nil {
			return &sourcePlan{items: []sourceItem{{format: *f, role: roleVideo}}, label: videoLabel(*f) + " combined stream (video track only)"}
		}
		return nil
	}

	// Preserve the efficient legacy behavior when a combined file already fits
	// the final attachment ceiling: prefer the best such stream because a copy
	// preset can publish it without a size-reducing transcode.
	var directlyPublishable []youtube.Format
	for _, f := range preferredCombined {
		if f.ContentLength > 0 && f.ContentLength <= attachmentLimit {
			directlyPublishable = append(directlyPublishable, f)
		}
	}

	// If a better audio role exists as an adaptive audio stream (for example
	// original .4 versus a combined German .10 AI dub), prefer separate A/V
	// before considering the lower-ranked combined stream. This prevents a small
	// dubbed muxed file from winning merely because it is cheaper.
	combinedAudioRank := bestAudioTrackRank(combined)
	adaptiveAudioRank := bestAudioTrackRank(audioOnly)
	preferSeparateForAudio := len(videoOnly) > 0 && len(preferredAudioOnly) > 0 && adaptiveAudioRank < combinedAudioRank

	if !preferSeparateForAudio {
		if f := bestVideoFormat(preferredFormats(directlyPublishable, outputFormat)); f != nil {
			return &sourcePlan{items: []sourceItem{{format: *f, role: roleCombined}}, label: audioSourceLabel(videoLabel(*f)+" combined stream", *f)}
		}

		// If the preset transcodes, a source may legitimately be much larger than
		// the final attachment. Choose the cheapest combined source first.
		if f := cheapestVideoFormat(preferredFormats(preferredCombined, outputFormat), duration); f != nil {
			return &sourcePlan{items: []sourceItem{{format: *f, role: roleCombined}}, label: audioSourceLabel(videoLabel(*f)+" combined stream", *f)}
		}
	}

	// Modern YouTube videos often expose useful qualities only as adaptive
	// video-only + audio-only streams. Download both within one aggregate source
	// budget and let FFmpeg map them into the managed output. Audio selection is
	// original-first, independently of size and request locale.
	v := cheapestVideoFormat(preferredFormats(videoOnly, outputFormat), duration)
	a := cheapestFormat(preferredFormats(preferredAudioOnly, outputFormat), duration)
	if v != nil && a != nil {
		vs := estimatedFormatSize(*v, duration)
		as := estimatedFormatSize(*a, duration)
		if vs == 0 || as == 0 || vs+as <= sourceBudget {
			return &sourcePlan{items: []sourceItem{{format: *v, role: roleVideo}, {format: *a, role: roleAudio}}, label: audioSourceLabel(videoLabel(*v)+" video + audio streams", *a)}
		}
	}

	// If the separate plan did not fit the source budget, retain the best
	// available combined fallback rather than failing the job outright.
	if preferSeparateForAudio {
		if f := bestVideoFormat(preferredFormats(directlyPublishable, outputFormat)); f != nil {
			return &sourcePlan{items: []sourceItem{{format: *f, role: roleCombined}}, label: audioSourceLabel(videoLabel(*f)+" combined stream", *f)}
		}
		if f := cheapestVideoFormat(preferredFormats(preferredCombined, outputFormat), duration); f != nil {
			return &sourcePlan{items: []sourceItem{{format: *f, role: roleCombined}}, label: audioSourceLabel(videoLabel(*f)+" combined stream", *f)}
		}
	}
	return nil
}

const (
	audioRankOriginal = iota
	audioRankDefaultUnknown
	audioRankUnknown
	audioRankHumanDub
	audioRankAutoDub
	audioRankDescriptive
)

// audioTrackRank orders audio roles independently of bitrate/size. YouTube's
// current track IDs encode acont: .4=original, .3=human dub, .10=AI auto-dub.
// audioIsDefault is only a fallback signal because it can follow request locale.
func audioTrackRank(f youtube.Format) int {
	t := f.AudioTrack
	if t == nil || strings.TrimSpace(t.ID) == "" {
		return audioRankOriginal
	}
	id := strings.ToLower(strings.TrimSpace(t.ID))
	name := strings.ToLower(strings.TrimSpace(t.DisplayName))
	if strings.HasSuffix(id, ".4") || strings.Contains(name, "original") {
		return audioRankOriginal
	}
	if strings.Contains(name, "descriptive") || strings.Contains(name, "audio description") {
		return audioRankDescriptive
	}
	if strings.HasSuffix(id, ".10") {
		return audioRankAutoDub
	}
	if strings.HasSuffix(id, ".3") {
		return audioRankHumanDub
	}
	if t.AudioIsDefault {
		return audioRankDefaultUnknown
	}
	return audioRankUnknown
}

func bestAudioTrackRank(formats []youtube.Format) int {
	if len(formats) == 0 {
		return int(^uint(0) >> 1)
	}
	best := audioTrackRank(formats[0])
	for i := 1; i < len(formats); i++ {
		if rank := audioTrackRank(formats[i]); rank < best {
			best = rank
		}
	}
	return best
}

func preferOriginalAudioFormats(formats []youtube.Format) []youtube.Format {
	if len(formats) == 0 {
		return formats
	}
	best := bestAudioTrackRank(formats)
	out := make([]youtube.Format, 0, len(formats))
	for _, f := range formats {
		if audioTrackRank(f) == best {
			out = append(out, f)
		}
	}
	return out
}

func audioSourceLabel(base string, f youtube.Format) string {
	t := f.AudioTrack
	if t == nil || strings.TrimSpace(t.ID) == "" {
		return base
	}
	if audioTrackRank(f) == audioRankOriginal {
		return base + " (original audio)"
	}
	name := strings.TrimSpace(t.DisplayName)
	if name != "" {
		return base + " (audio: " + name + ")"
	}
	return base
}

func filterFormatsByBudget(in []youtube.Format, budget int64, duration time.Duration) []youtube.Format {
	out := make([]youtube.Format, 0, len(in))
	for _, f := range in {
		sz := estimatedFormatSize(f, duration)
		if sz == 0 || sz <= budget {
			out = append(out, f)
		}
	}
	return out
}

func estimatedFormatSize(f youtube.Format, duration time.Duration) int64 {
	if f.ContentLength > 0 {
		return f.ContentLength
	}
	bitrate := f.AverageBitrate
	if bitrate <= 0 {
		bitrate = f.Bitrate
	}
	seconds := int64(duration / time.Second)
	if bitrate > 0 && seconds > 0 {
		return (int64(bitrate) * seconds) / 8
	}
	return 0
}

func cheapestFormat(formats []youtube.Format, duration time.Duration) *youtube.Format {
	if len(formats) == 0 {
		return nil
	}
	items := append([]youtube.Format(nil), formats...)
	sort.SliceStable(items, func(i, j int) bool {
		si := estimatedFormatSize(items[i], duration)
		sj := estimatedFormatSize(items[j], duration)
		if si == 0 && sj != 0 {
			return false
		}
		if sj == 0 && si != 0 {
			return true
		}
		if si != sj {
			return si < sj
		}
		return items[i].Bitrate < items[j].Bitrate
	})
	f := items[0]
	return &f
}

func cheapestVideoFormat(formats []youtube.Format, duration time.Duration) *youtube.Format {
	if len(formats) == 0 {
		return nil
	}
	items := append([]youtube.Format(nil), formats...)
	sort.SliceStable(items, func(i, j int) bool {
		si := estimatedFormatSize(items[i], duration)
		sj := estimatedFormatSize(items[j], duration)
		if si == 0 && sj != 0 {
			return false
		}
		if sj == 0 && si != 0 {
			return true
		}
		if si != sj {
			return si < sj
		}
		ai := items[i].Width * items[i].Height
		aj := items[j].Width * items[j].Height
		return ai < aj
	})
	f := items[0]
	return &f
}

func bestVideoFormat(formats []youtube.Format) *youtube.Format {
	if len(formats) == 0 {
		return nil
	}
	items := append([]youtube.Format(nil), formats...)
	sort.SliceStable(items, func(i, j int) bool {
		ai := items[i].Width * items[i].Height
		aj := items[j].Width * items[j].Height
		if ai != aj {
			return ai > aj
		}
		return items[i].Bitrate > items[j].Bitrate
	})
	f := items[0]
	return &f
}

func preferredFormats(formats []youtube.Format, outputFormat string) []youtube.Format {
	want := ""
	switch strings.ToLower(outputFormat) {
	case "mp4", "mov", "m4a":
		want = "mp4"
	case "webm":
		want = "webm"
	}
	if want == "" {
		return formats
	}
	preferred := make([]youtube.Format, 0, len(formats))
	for _, f := range formats {
		if strings.Contains(strings.ToLower(f.MimeType), want) {
			preferred = append(preferred, f)
		}
	}
	if len(preferred) > 0 {
		return preferred
	}
	return formats
}

func videoLabel(f youtube.Format) string {
	label := f.QualityLabel
	if label == "" {
		label = f.Quality
	}
	return safeProgressToken(label)
}

func sourceExtension(mime string) string {
	mime = strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]))
	switch mime {
	case "video/mp4":
		return ".mp4"
	case "audio/mp4":
		return ".m4a"
	case "video/webm", "audio/webm":
		return ".webm"
	default:
		return ".media"
	}
}

func outputMIME(format string, args []string) string {
	audioOnly := false
	for _, arg := range args {
		if arg == "-vn" {
			audioOnly = true
			break
		}
	}
	switch strings.ToLower(format) {
	case "mp3":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	case "flac":
		return "audio/flac"
	case "ogg", "opus":
		return "audio/ogg"
	case "webm":
		if audioOnly {
			return "audio/webm"
		}
		return "video/webm"
	case "mkv", "matroska":
		return "video/x-matroska"
	case "mp4", "mov", "m4a":
		if audioOnly {
			return "audio/mp4"
		}
		return "video/mp4"
	default:
		return "application/octet-stream"
	}
}

func compressAttachment(mode, filename string, data []byte) (string, string, []byte, error) {
	switch mode {
	case "none":
		return filename, "application/octet-stream", data, nil
	case "zip":
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		zw.RegisterCompressor(zip.Deflate, func(w io.Writer) (io.WriteCloser, error) {
			return flate.NewWriter(w, flate.BestCompression)
		})
		h := &zip.FileHeader{Name: filename, Method: zip.Deflate}
		entry, err := zw.CreateHeader(h)
		if err != nil {
			return "", "", nil, err
		}
		if _, err := entry.Write(data); err != nil {
			_ = zw.Close()
			return "", "", nil, err
		}
		if err := zw.Close(); err != nil {
			return "", "", nil, err
		}
		return compressedFilename(filename, ".zip"), "application/zip", buf.Bytes(), nil
	case "gzip":
		var buf bytes.Buffer
		gw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err != nil {
			return "", "", nil, err
		}
		gw.Name = filename
		gw.ModTime = time.Time{}
		if _, err := gw.Write(data); err != nil {
			_ = gw.Close()
			return "", "", nil, err
		}
		if err := gw.Close(); err != nil {
			return "", "", nil, err
		}
		return compressedFilename(filename, ".gz"), "application/gzip", buf.Bytes(), nil
	default:
		return "", "", nil, errors.New("unsupported compression mode")
	}
}

func compressedFilename(filename, suffix string) string {
	r := []rune(filename)
	maxBase := 80 - len([]rune(suffix))
	if len(r) > maxBase {
		filename = strings.TrimSpace(string(r[:maxBase]))
	}
	filename = strings.TrimRight(filename, " .-_") + suffix
	if !protocol.TransferFilenameRE.MatchString(filename) {
		return "youtube-output" + suffix
	}
	return filename
}

func (p *Plugin) outputFilename(title, id string) string {
	ext := p.format
	if ext == "matroska" {
		ext = "mkv"
	}
	title = sanitizeFilenamePart(title, 54)
	if title == "" {
		title = "youtube"
	}
	id = sanitizeFilenamePart(id, 16)
	name := strings.NewReplacer("{title}", title, "{id}", id, "{ext}", ext).Replace(p.filenameTemplate)
	name = sanitizeFilenamePart(name, 80)
	suffix := "." + ext
	if !strings.HasSuffix(strings.ToLower(name), strings.ToLower(suffix)) {
		base := sanitizeFilenamePart(strings.TrimSuffix(name, "."), 80-len([]rune(suffix)))
		name = strings.TrimRight(base, ".") + suffix
	}
	if !protocol.TransferFilenameRE.MatchString(name) {
		name = "youtube-" + id + suffix
	}
	return name
}

func sanitizeFilenamePart(s string, max int) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-' || r == ' ' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), " .-_")
	r := []rune(out)
	if len(r) > max {
		out = strings.TrimSpace(string(r[:max]))
	}
	if out == "" || !protocol.TransferFilenameRE.MatchString(out) {
		fallback := "youtube-file.mp4"
		if protocol.TransferFilenameRE.MatchString(fallback) {
			return fallback
		}
	}
	return out
}

func (p *Plugin) HelpModules() map[string][]string {
	return map[string][]string{
		"YTDL": {
			"Send one canonical YouTube URL to " + p.channel + " (or DM " + p.nick + " when DM mode is enabled).",
			"One normal-user job may be active/queued at a time; the plugin reports progress privately and returns an XFER attachment.",
			"IRCAPYBARA_YTDL_FFMPEG_PRESET is trusted admin argv, e.g. -vn -c:a libopus -b:a 6k -ac 1 -ar 16000.",
			"IRCAPYBARA_YTDL_SOURCE_MAX_MB bounds temporary YouTube inputs separately from the final attachment limit (default 128 MiB).",
			"When YouTube exposes dubbed tracks, the plugin prefers the upload-language original audio over human or AI dubs.",
			"IRCAPYBARA_YTDL_COMPRESSION_MODE=none|zip|gzip optionally wraps the converted file before XFER.",
		},
	}
}

func safeProgressToken(s string) string {
	s = strings.TrimSpace(s)
	if !regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`).MatchString(s) {
		return "compatible"
	}
	return s
}

type progressWriter struct {
	dst     io.Writer
	total   int64
	written int64
	last    int
	report  func(int)
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	w.written += int64(n)
	if w.total > 0 {
		pct := int((w.written * 100) / w.total)
		bucket := (pct / 10) * 10
		if bucket >= 10 && bucket > w.last && bucket <= 100 {
			w.last = bucket
			w.report(bucket)
		}
	}
	return n, err
}

type limitedLogWriter struct {
	mu    sync.Mutex
	limit int
	b     strings.Builder
}

func (w *limitedLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	remaining := w.limit - w.b.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = w.b.Write(p[:remaining])
		} else {
			_, _ = w.b.Write(p)
		}
	}
	return len(p), nil
}
func (w *limitedLogWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.TrimSpace(w.b.String())
}
