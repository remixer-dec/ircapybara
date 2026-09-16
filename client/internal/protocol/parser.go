package protocol

import (
	"errors"
	"strings"
)

var ErrEmptyLine = errors.New("empty IRC line")

func Parse(line string) (Message, error) {
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	if line == "" {
		return Message{}, ErrEmptyLine
	}

	m := Message{Raw: line, Tags: map[string]string{}}
	rest := line

	if strings.HasPrefix(rest, "@") {
		sp := strings.IndexByte(rest, ' ')
		if sp < 0 {
			return Message{}, errors.New("tag-only IRC line")
		}
		parseTags(rest[1:sp], m.Tags)
		rest = strings.TrimLeft(rest[sp+1:], " ")
	}

	if strings.HasPrefix(rest, ":") {
		sp := strings.IndexByte(rest, ' ')
		if sp < 0 {
			return Message{}, errors.New("prefix-only IRC line")
		}
		p := ParsePrefix(rest[1:sp])
		m.Prefix = &p
		rest = strings.TrimLeft(rest[sp+1:], " ")
	}

	if rest == "" {
		return Message{}, errors.New("missing IRC command")
	}

	if sp := strings.IndexByte(rest, ' '); sp >= 0 {
		m.Command = strings.ToUpper(rest[:sp])
		rest = strings.TrimLeft(rest[sp+1:], " ")
	} else {
		m.Command = strings.ToUpper(rest)
		return m, nil
	}

	for rest != "" {
		if rest[0] == ':' {
			m.Params = append(m.Params, rest[1:])
			break
		}
		sp := strings.IndexByte(rest, ' ')
		if sp < 0 {
			m.Params = append(m.Params, rest)
			break
		}
		m.Params = append(m.Params, rest[:sp])
		rest = strings.TrimLeft(rest[sp+1:], " ")
	}

	return m, nil
}

func parseTags(raw string, out map[string]string) {
	if raw == "" {
		return
	}
	for _, item := range strings.Split(raw, ";") {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			out[key] = ""
			continue
		}
		out[key] = unescapeTag(value)
	}
}

func unescapeTag(v string) string {
	var b strings.Builder
	b.Grow(len(v))
	escaped := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !escaped {
			if c == '\\' {
				escaped = true
			} else {
				b.WriteByte(c)
			}
			continue
		}
		switch c {
		case ':':
			b.WriteByte(';')
		case 's':
			b.WriteByte(' ')
		case 'r':
			b.WriteByte('\r')
		case 'n':
			b.WriteByte('\n')
		case '\\':
			b.WriteByte('\\')
		default:
			b.WriteByte(c)
		}
		escaped = false
	}
	if escaped {
		b.WriteByte('\\')
	}
	return b.String()
}
