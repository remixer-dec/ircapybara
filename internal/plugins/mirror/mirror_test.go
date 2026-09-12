package mirror

import (
	"strings"
	"testing"
	"time"
)

func TestParseRSSOldestToNewest(t *testing.T) {
	body := []byte(`<?xml version="1.0"?><rss><channel>
<item><guid>2</guid><title>Second</title><link>https://example.com/2</link><pubDate>Sat, 12 Sep 2026 11:00:00 +0000</pubDate></item>
<item><guid>1</guid><title>First</title><link>https://example.com/1</link><pubDate>Sat, 12 Sep 2026 10:00:00 +0000</pubDate></item>
</channel></rss>`)
	posts, err := parseRSS(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 2 || posts[0].ID != "1" || posts[1].ID != "2" {
		t.Fatalf("unexpected order: %#v", posts)
	}
	if !strings.Contains(posts[1].Text, "https://example.com/2") {
		t.Fatalf("missing link in %q", posts[1].Text)
	}
}

func TestParseTelegramPreview(t *testing.T) {
	html := `<div class="tgme_widget_message" data-post="samplechan/41"><div class="tgme_widget_message_text js-message_text" dir="auto">hello <b>world</b></div><time datetime="2026-09-12T10:00:00+00:00"></time></div>
<div class="tgme_widget_message" data-post="samplechan/42"><a class="tgme_widget_message_photo_wrap"></a><time datetime="2026-09-12T11:00:00+00:00"></time></div>`
	posts, err := parseTelegramPreview(html)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 2 {
		t.Fatalf("got %d posts", len(posts))
	}
	if posts[0].ID != "samplechan/41" || !strings.Contains(posts[0].Text, "hello world") {
		t.Fatalf("unexpected first post: %#v", posts[0])
	}
	if !strings.Contains(posts[1].Text, "[image]") {
		t.Fatalf("unexpected media post: %#v", posts[1])
	}
	if !posts[0].At.Equal(time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected timestamp: %s", posts[0].At)
	}
}
