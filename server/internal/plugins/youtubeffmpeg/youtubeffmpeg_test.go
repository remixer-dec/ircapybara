package youtubeffmpeg

import (
	"strings"
	"testing"

	youtube "github.com/kkdai/youtube/v2"
)

func TestParsePresetAllowsArbitraryAdminOptions(t *testing.T) {
	args, format, err := parsePreset(`-vn -c:a libopus -b:a 6k -ac 1 -ar 16000 -application voip -frame_duration 60 -compression_level 10 -movflags +faststart`)
	if err != nil {
		t.Fatal(err)
	}
	if format != "mp4" || !containsArg(args, "-application") || !containsArg(args, "-frame_duration") {
		t.Fatalf("unexpected preset: %#v format=%q", args, format)
	}
	legacy, _, err := parsePreset("vn=;c:a=libopus;application=voip")
	if err != nil || !containsArg(legacy, "-vn") || !containsArg(legacy, "-application") {
		t.Fatalf("legacy unrestricted preset failed: %#v err=%v", legacy, err)
	}
}

func TestPresetInjectionKeepsServerFileLimitLast(t *testing.T) {
	argv := []string{"ffmpeg", "-i", "in", "-fs", "15728641", "out.mp4", "-y"}
	preset := []string{"-fs", "999999999", "-vn", "-c:a", "libopus"}
	got, err := injectPresetArgs(argv, preset)
	if err != nil {
		t.Fatal(err)
	}
	lastFS := -1
	for i, arg := range got {
		if arg == "-fs" {
			lastFS = i
		}
	}
	if lastFS < 0 || lastFS+1 >= len(got) || got[lastFS+1] != "15728641" {
		t.Fatalf("server -fs must remain final: %#v", got)
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func TestYouTubeURLAllowlist(t *testing.T) {
	good := []string{
		"https://www.youtube.com/watch?v=BaW_jenozKc",
		"https://youtu.be/BaW_jenozKc",
		"https://youtube.com/shorts/BaW_jenozKc",
	}
	bad := []string{
		"http://www.youtube.com/watch?v=BaW_jenozKc",
		"https://evil.example/watch?v=BaW_jenozKc",
		"https://www.youtube.com.evil.example/watch?v=BaW_jenozKc",
	}
	for _, v := range good {
		if !youtubeURLRE.MatchString(v) {
			t.Errorf("expected allowed URL %q", v)
		}
	}
	for _, v := range bad {
		if youtubeURLRE.MatchString(v) {
			t.Errorf("expected rejected URL %q", v)
		}
	}
}

func TestChooseSourcePlanStaysInsideLimits(t *testing.T) {
	formats := youtube.FormatList{
		{MimeType: "video/mp4", AudioChannels: 2, ContentLength: 4_000_000, Width: 640, Height: 360, Bitrate: 1},
		{MimeType: "video/mp4", AudioChannels: 2, ContentLength: 14_000_000, Width: 1280, Height: 720, Bitrate: 2},
		{MimeType: "video/mp4", AudioChannels: 2, ContentLength: 40_000_000, Width: 1920, Height: 1080, Bitrate: 3},
		{MimeType: "video/webm", AudioChannels: 2, ContentLength: 1_000_000, Width: 3840, Height: 2160, Bitrate: 4},
	}
	tests := []struct {
		name            string
		sourceBudget    int64
		attachmentLimit int64
	}{
		{"source budget", 15_000_000, 50_000_000},
		{"attachment limit", 50_000_000, 15_000_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := chooseSourcePlan(formats, tt.sourceBudget, tt.attachmentLimit, 0, intentAV, "mp4")
			if plan == nil || len(plan.items) != 1 {
				t.Fatalf("expected one source stream, got %+v", plan)
			}
			item := plan.items[0]
			if item.role != roleCombined || item.format.MimeType != "video/mp4" || item.format.Width != 1280 {
				t.Fatalf("expected a combined 720p MP4 stream, got %+v", item)
			}
		})
	}
}

func TestOutputFilenameIsBoundedAndKeepsExtension(t *testing.T) {
	p := &Plugin{format: "mp4", filenameTemplate: "{title}-{id}.{ext}"}
	name := p.outputFilename(strings.Repeat("A", 200), "BaW_jenozKc")
	if len([]rune(name)) > 80 || !strings.HasSuffix(name, ".mp4") {
		t.Fatalf("unsafe output filename %q", name)
	}
}
