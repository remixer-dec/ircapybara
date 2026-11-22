package server

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"ircapybara/internal/model"
	"ircapybara/internal/protocol"
)

const (
	multilineBytes   = protocol.MaxMessageRunes * 4
	multilineLines   = 512
	multilineTimeout = 30 * time.Second
)

var batchSequence atomic.Uint64
var batchIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

type chatMessage struct {
	prefix, verb, target, text string
	history                    bool
	at                         time.Time
}

func (s *Server) sendChat(c *model.Client, msg chatMessage) {
	header := ":" + msg.prefix + " " + msg.verb + " " + msg.target + " :"
	tags := ""
	if msg.history {
		var values []string
		if c.HasCaps("ircapybara/history") {
			values = append(values, "ircapybara-history=1")
		}
		if c.HasCaps("server-time") {
			if msg.at.IsZero() {
				msg.at = time.Now()
			}
			values = append(values, "time="+msg.at.UTC().Format(time.RFC3339Nano))
		}
		if len(values) > 0 {
			tags = "@" + strings.Join(values, ";") + " "
		}
	}
	body := protocol.DecodeMessageText(msg.text)
	if !strings.Contains(body, "\n") && len(tags)+len(header)+len(body) <= 510 {
		s.sendLine(c, tags+header+body)
		return
	}
	group := c.HasCaps("batch", "draft/multiline")
	id := strconv.FormatUint(batchSequence.Add(1), 36)
	var wire strings.Builder
	if group {
		fmt.Fprintf(&wire, "%s:%s BATCH +%s draft/multiline %s\r\n", tags, msg.prefix, id, msg.target)
	}
	for _, line := range strings.Split(body, "\n") {
		if !group && line == "" {
			continue
		}
		concat := false
		for {
			tag := tags
			if group {
				tag = "@batch=" + id
				if concat {
					tag += ";draft/multiline-concat"
				}
				tag += " "
			}
			n := min(len(line), 510-len(header)-len(tag))
			if n < 0 {
				return
			}
			for n < len(line) && n > 0 && !utf8.RuneStart(line[n]) {
				n--
			}
			if n == 0 && line != "" {
				return
			}
			wire.WriteString(tag + header + line[:n] + "\r\n")
			line = line[n:]
			if line == "" {
				break
			}
			concat = true
		}
	}
	if group {
		wire.WriteString(":" + msg.prefix + " BATCH -" + id + "\r\n")
	}
	c.WriteMu.Lock()
	defer c.WriteMu.Unlock()
	if c.Conn.SetWriteDeadline(time.Now().Add(10*time.Second)) == nil {
		_, _ = io.WriteString(c.Conn, wire.String())
	}
}

// A client may open one batch at a time. Its connection's read loop owns it.
type incomingBatch struct {
	id, target, verb string
	body             strings.Builder
	lines            int
}

func (b *incomingBatch) accept(cmd protocol.Command) (complete protocol.Command, failure string) {
	const invalid = "MULTILINE_INVALID"
	if cmd.Name == "BATCH" && len(cmd.Tags) == 0 {
		if b.id == "" {
			fields := strings.Fields(cmd.Args)
			if len(fields) != 3 || !strings.HasPrefix(fields[0], "+") {
				return complete, invalid
			}
			id := fields[0][1:]
			if !batchIDPattern.MatchString(id) {
				return complete, "INVALID_REFTAG *"
			}
			if fields[1] != "draft/multiline" {
				return complete, "UNKNOWN_TYPE " + id + " " + fields[1]
			}
			if !validChatTarget(fields[2]) {
				return complete, invalid
			}
			b.id, b.target = id, fields[2]
			return complete, ""
		}
		if cmd.Args != "-"+b.id || b.lines == 0 || strings.Trim(b.body.String(), "\n") == "" {
			return complete, invalid
		}
		text := b.body.String()
		if !protocol.ValidMessageText(text, protocol.MaxMessageRunes) {
			return complete, invalid
		}
		// The logical command returns to normal dispatch, including plugin and
		// channel policy checks, exactly once.
		return protocol.Command{Name: b.verb, Args: b.target + " :" + text}, ""
	}
	if b.id == "" || cmd.Tags["batch"] != b.id || (cmd.Name != "PRIVMSG" && cmd.Name != "NOTICE") {
		return complete, invalid
	}
	_, concat := cmd.Tags["draft/multiline-concat"]
	if len(cmd.Tags) > 1 && (!concat || len(cmd.Tags) != 2 || cmd.Tags["draft/multiline-concat"] != "") {
		return complete, invalid
	}
	target, body, ok := strings.Cut(cmd.Args, " ")
	if !ok || !validChatTarget(target) {
		return complete, invalid
	}
	if !strings.EqualFold(b.target, target) {
		return complete, "MULTILINE_INVALID_TARGET " + b.target + " " + target
	}
	if !strings.HasPrefix(body, ":") && strings.Contains(body, " ") {
		return complete, invalid
	}
	body = strings.TrimPrefix(body, ":")
	if !protocol.ValidMessageText(body, protocol.MaxMessageRunes) || (concat && body == "") || (b.lines > 0 && cmd.Name != b.verb) {
		return complete, invalid
	}
	if b.lines >= multilineLines {
		return complete, fmt.Sprintf("MULTILINE_MAX_LINES %d", multilineLines)
	}
	separator := ""
	if b.lines > 0 && !concat {
		separator = "\n"
	}
	if b.body.Len()+len(separator)+len(body) > multilineBytes {
		return complete, fmt.Sprintf("MULTILINE_MAX_BYTES %d", multilineBytes)
	}
	b.verb = cmd.Name
	b.lines++
	b.body.WriteString(separator)
	b.body.WriteString(body)
	return complete, ""
}

func validChatTarget(target string) bool {
	return protocol.ChannelRE.MatchString(target) || protocol.NickRE.MatchString(target)
}
