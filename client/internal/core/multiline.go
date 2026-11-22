package core

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"ircapybara/client/internal/protocol"
)

const maxChatBytes = 4 * 32768

type multilineLimits struct{ bytes, lines int }

func parseMultilineLimits(value string) multilineLimits {
	var limits multilineLimits
	for _, token := range strings.Split(value, ",") {
		key, raw, _ := strings.Cut(token, "=")
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			continue
		}
		switch key {
		case "max-bytes":
			limits.bytes = n
		case "max-lines":
			limits.lines = n
		}
	}
	return limits
}

func outgoingChat(target, text, id string, limits multilineLimits, legacy bool) ([]string, error) {
	header := "PRIVMSG " + target + " :"
	if !strings.Contains(text, "\n") && len(header)+len(text) <= 510 {
		return []string{header + text}, nil
	}
	batch := limits.bytes > 0
	if !batch && legacy {
		return []string{header + protocol.EncodeMessageText(text)}, nil
	}
	if batch && len(text) > limits.bytes {
		return nil, fmt.Errorf("message exceeds server multiline limit of %d bytes", limits.bytes)
	}
	if strings.Trim(text, "\n") == "" {
		return nil, errors.New("message contains only blank lines")
	}
	var lines []string
	if batch {
		lines = append(lines, "BATCH +"+id+" draft/multiline "+target)
	}
	for _, line := range strings.Split(text, "\n") {
		if !batch && line == "" {
			continue
		}
		concat := false
		for {
			tag := ""
			if batch {
				tag = "@batch=" + id
				if concat {
					tag += ";draft/multiline-concat"
				}
				tag += " "
			}
			n := min(len(line), 510-len(tag)-len(header))
			for n > 0 && n < len(line) && !utf8.RuneStart(line[n]) {
				n--
			}
			if n < 0 || n == 0 && line != "" {
				return nil, errors.New("target exceeds IRC line limit")
			}
			lines = append(lines, tag+header+line[:n])
			if batch && limits.lines > 0 && len(lines)-1 > limits.lines {
				return nil, fmt.Errorf("message exceeds server multiline limit of %d lines", limits.lines)
			}
			line = line[n:]
			if line == "" {
				break
			}
			concat = true
		}
	}
	if batch {
		lines = append(lines, "BATCH -"+id)
	}
	return lines, nil
}

type receivedBatch struct {
	open    protocol.Message
	sender  *protocol.Prefix
	command string
	body    strings.Builder
	lines   int
	expiry  *time.Timer
}

// Each server batch has its own reference. History and live traffic may
// interleave. Unrecognized batch types pass their contents through normally.
func (a *App) receiveBatch(msg protocol.Message) (protocol.Message, bool) {
	ref, tagged := msg.Tags["batch"]
	if msg.Command != "BATCH" && !tagged {
		return msg, true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.capEnabled["batch"] {
		return msg, false
	}
	if msg.Command == "BATCH" {
		if len(msg.Params) == 0 || len(msg.Params[0]) < 2 || len(msg.Params[0]) > 129 {
			return msg, false
		}
		id := msg.Params[0][1:]
		b := a.batches[id]
		switch msg.Params[0][0] {
		case '+':
			if b != nil {
				a.removeBatch(id)
				return msg, false
			}
			if len(msg.Params) < 2 || len(a.batches) >= 16 {
				return msg, false
			}
			if msg.Params[1] == "draft/multiline" && (!a.capEnabled["draft/multiline"] || len(msg.Params) != 3 || !validIRCAtom(msg.Params[2])) {
				return msg, false
			}
			if a.batches == nil {
				a.batches = make(map[string]*receivedBatch)
			}
			b = &receivedBatch{open: msg}
			a.batches[id] = b
			b.expiry = time.AfterFunc(30*time.Second, func() {
				a.mu.Lock()
				defer a.mu.Unlock()
				if a.batches[id] == b {
					a.removeBatch(id)
				}
			})
		case '-':
			if b == nil {
				return msg, false
			}
			a.removeBatch(id)
			if len(msg.Params) != 1 || b.command == "" || strings.Trim(b.body.String(), "\n") == "" {
				return msg, false
			}
			result := b.open
			result.Prefix = b.sender
			result.Command = b.command
			result.Params = []string{b.open.Params[2], b.body.String()}
			return result, true
		}
		return msg, false
	}
	b := a.batches[ref]
	if b == nil {
		return msg, false
	}
	if b.open.Params[1] != "draft/multiline" {
		return msg, true
	}
	if (msg.Command != "PRIVMSG" && msg.Command != "NOTICE") || len(msg.Params) != 2 || !strings.EqualFold(msg.Params[0], b.open.Params[2]) ||
		msg.Prefix == nil || (b.sender != nil && b.sender.Raw != msg.Prefix.Raw) || (b.command != "" && msg.Command != b.command) {
		a.removeBatch(ref)
		return msg, false
	}
	sep := ""
	_, concat := msg.Tags["draft/multiline-concat"]
	if b.lines > 0 && !concat {
		sep = "\n"
	}
	if (concat && msg.Params[1] == "") || b.lines >= 32769 || b.body.Len()+len(sep)+len(msg.Params[1]) > maxChatBytes {
		a.removeBatch(ref)
		return msg, false
	}
	b.sender = msg.Prefix
	b.command = msg.Command
	b.body.WriteString(sep)
	b.body.WriteString(msg.Params[1])
	b.lines++
	return msg, false
}

func (a *App) removeBatch(id string) {
	a.batches[id].expiry.Stop()
	delete(a.batches, id)
}

func (a *App) resetChatCapabilities() {
	for id := range a.batches {
		a.removeBatch(id)
	}
	a.capEnabled = make(map[string]bool)
	a.multiline = multilineLimits{}
}
