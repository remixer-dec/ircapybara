package pluginutil

import (
	"context"
	"errors"
	"html"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	htmlTagRE   = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRE     = regexp.MustCompile(`[\t\r\n ]+`)
	controlRE   = regexp.MustCompile(`[\x00-\x1f\x7f]`)
	safeURLHost = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
)

func CleanExternalText(s string, maxRunes int) string {
	s = html.UnescapeString(htmlTagRE.ReplaceAllString(s, " "))
	s = controlRE.ReplaceAllString(s, " ")
	s = strings.TrimSpace(spaceRE.ReplaceAllString(s, " "))
	if maxRunes < 1 {
		return ""
	}
	r := []rune(s)
	if len(r) > maxRunes {
		r = r[:maxRunes]
		s = strings.TrimSpace(string(r))
	}
	if !utf8.ValidString(s) {
		return ""
	}
	return s
}

// PlainText keeps chat layout and literal markup, including source code.
func PlainText(s string, maxRunes int) string {
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(s)
	var out strings.Builder
	for _, r := range s {
		if maxRunes <= 0 {
			break
		}
		if r != '\n' && r != '\t' && !unicode.IsPrint(r) && !unicode.Is(unicode.Zs, r) {
			continue
		}
		out.WriteRune(r)
		maxRunes--
	}
	return out.String()
}

// HTMLText strips inline markup without inserting spaces into words. Block
// boundaries add a newline only when needed, while each explicit br counts.
var markupRE = regexp.MustCompile(`(?is)<!--.*?-->|</?([a-z][a-z0-9:-]*)\b(?:[^>"']|"[^"]*"|'[^']*')*>`)

func HTMLText(s string, maxRunes int) string {
	var out strings.Builder
	start := 0
	lineBreak := true
	write := func(part string) {
		if part == "" {
			return
		}
		part = html.UnescapeString(part)
		out.WriteString(part)
		lineBreak = strings.HasSuffix(part, "\n")
	}
	for _, m := range markupRE.FindAllStringSubmatchIndex(s, -1) {
		write(s[start:m[0]])
		start = m[1]
		if m[2] < 0 {
			continue
		}
		switch strings.ToLower(s[m[2]:m[3]]) {
		case "br":
			out.WriteByte('\n')
			lineBreak = true
		case "p", "div", "li", "pre", "blockquote":
			if !lineBreak {
				out.WriteByte('\n')
				lineBreak = true
			}
		}
	}
	write(s[start:])
	return PlainText(strings.Trim(out.String(), "\r\n"), maxRunes)
}

func ChunkText(s string, maxRunes int) []string {
	s = CleanExternalText(s, 10000)
	if s == "" || maxRunes < 1 {
		return nil
	}
	r := []rune(s)
	var out []string
	for len(r) > 0 {
		n := maxRunes
		if len(r) < n {
			n = len(r)
		}
		part := strings.TrimSpace(string(r[:n]))
		if part != "" {
			out = append(out, part)
		}
		r = r[n:]
	}
	return out
}

func PublicHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil || !safeURLHost.MatchString(host) {
				return nil, errors.New("unsafe remote address")
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, resolved := range ips {
				ip := resolved.IP
				if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
					continue
				}
				return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			}
			return nil, errors.New("remote host has no allowed public address")
		},
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     30 * time.Second,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || req.URL.Scheme != "https" || !safeURLHost.MatchString(req.URL.Hostname()) {
				return errors.New("unsafe redirect")
			}
			return nil
		},
	}
}
