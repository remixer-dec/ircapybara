package protocol

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// IRCapybara intentionally permits large UTF-8 chat payloads for extended clients.
	// MaxMessageRunes is the user-visible text limit; MaxLineBytes is a defensive
	// transport ceiling allowing the worst-case 4-byte UTF-8 encoding plus command
	// metadata. These are server-specific limits, not the historical RFC 512-byte limit.
	MaxMessageRunes = 32768
	MaxLineRunes    = MaxMessageRunes + 256
	MaxLineBytes    = MaxLineRunes*4 + 2
)

type Command struct {
	Name string
	Args string
}

var (
	envelopeRE = regexp.MustCompile(`^([A-Za-z]+)(?: ([^\x00-\x20\x7f][^\x00-\x1f\x7f]*))?$`)

	NoArgsRE           = regexp.MustCompile(`^$`)
	PasswordRE         = regexp.MustCompile(`^[\x21-\x7e]{8,128}$`)
	PassRE             = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`)
	NickRE             = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_\-\[\]\\` + "`" + `^{}]{0,30}$`)
	UsernameRE         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)
	ChannelRE          = regexp.MustCompile(`^#[A-Za-z0-9_-]{1,48}$`)
	KeyRE              = regexp.MustCompile(`^[\x21-\x7e]{1,64}$`)
	SafeTokenRE        = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)
	SearchMaskRE       = regexp.MustCompile(`^[#A-Za-z0-9*?._@:/-]{1,64}$`)
	ModeRE             = regexp.MustCompile(`^[+-][obiktlmsH]$`)
	BanMaskRE          = regexp.MustCompile(`^(?:ip:[0-9A-Fa-f:.]+|cidr:[0-9A-Fa-f:.]+/[0-9]{1,3}|user:[A-Za-z0-9._-]{1,32})$`)
	BanKindRE          = regexp.MustCompile(`^(?:IP|CIDR|USER)$`)
	IPOrCIDRValueRE    = regexp.MustCompile(`^[0-9A-Fa-f:.]+(?:/[0-9]{1,3})?$`)
	IntegerRE          = regexp.MustCompile(`^[0-9]{1,9}$`)
	CaptchaRE          = regexp.MustCompile(`^([a-f0-9]{8}) ([0-9]{1,3})$`)
	XModeRE            = regexp.MustCompile(`^(?:PLAIN|EXTENDED)$`)
	TransferFileIDRE   = regexp.MustCompile(`^[a-f0-9]{24}$`)
	TransferHashRE     = regexp.MustCompile(`^[a-f0-9]{64}$`)
	TransferMimeRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+-]{0,31}/[A-Za-z0-9][A-Za-z0-9.+-]{0,31}$`)
	TransferFilenameRE = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{M}\p{N}._ -]{0,79}$`)

	UserArgsRE          = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]{0,31}) [0-9]{1,3} \* (?::(.{1,80})|([^\x00-\x20\x7f]{1,80}))$`)
	JoinArgsRE          = regexp.MustCompile(`^(#[A-Za-z0-9_-]{1,48}(?:,#[A-Za-z0-9_-]{1,48}){0,15})(?: ([\x21-\x7e]{1,64}(?:,[\x21-\x7e]{1,64}){0,15}))?$`)
	PartArgsRE          = regexp.MustCompile(`^(#[A-Za-z0-9_-]{1,48})(?: :(.{0,160}))?$`)
	MessageArgsRE       = regexp.MustCompile(`^(#[A-Za-z0-9_-]{1,48}|[A-Za-z][A-Za-z0-9_\-\[\]\\` + "`" + `^{}]{0,30}) :(.+)$`)
	TopicArgsRE         = regexp.MustCompile(`^(#[A-Za-z0-9_-]{1,48})(?: :(.{0,180}))?$`)
	SingleChannelRE     = regexp.MustCompile(`^(#[A-Za-z0-9_-]{1,48})$`)
	WhoArgsRE           = regexp.MustCompile(`^([#A-Za-z0-9*?._@:/-]{1,64})$`)
	WhoIsArgsRE         = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_\-\[\]\\` + "`" + `^{}]{0,30})(?: ([A-Za-z][A-Za-z0-9_\-\[\]\\` + "`" + `^{}]{0,30}))?$`)
	ModeArgsRE          = regexp.MustCompile(`^(#[A-Za-z0-9_-]{1,48})(?: ([+-][obiktlmsH])(?: ([^\x00-\x20\x7f]{1,128}))?)?$`)
	ModeBanListArgsRE   = regexp.MustCompile(`^(#[A-Za-z0-9_-]{1,48}) b$`)
	KickArgsRE          = regexp.MustCompile(`^(#[A-Za-z0-9_-]{1,48}) ([A-Za-z][A-Za-z0-9_\-\[\]\\` + "`" + `^{}]{0,30})(?: :(.{0,160}))?$`)
	InviteArgsRE        = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_\-\[\]\\` + "`" + `^{}]{0,30}) (#[A-Za-z0-9_-]{1,48})$`)
	OperArgsRE          = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]{0,31}) ([\x21-\x7e]{8,128})$`)
	KillArgsRE          = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_\-\[\]\\` + "`" + `^{}]{0,30})(?: :(.{1,160}))?$`)
	IdentifyArgsRE      = regexp.MustCompile(`^(?:([A-Za-z][A-Za-z0-9_\-\[\]\\` + "`" + `^{}]{0,30}) )?([\x21-\x7e]{8,128})$`)
	ChanAddArgsRE       = regexp.MustCompile(`^(#[A-Za-z0-9_-]{1,48})(?: (PUBLIC|PRIVATE)(?: (HISTORY|NOHISTORY))?)? :(.{0,180})$`)
	GuardArgsRE         = regexp.MustCompile(`^([0123])$`)
	HistoryArgsRE       = regexp.MustCompile(`^(#[A-Za-z0-9_-]{1,48})(?: ([0-9]{1,3}))?$`)
	BanAddArgsRE        = regexp.MustCompile(`^(SERVER|#[A-Za-z0-9_-]{1,48}) (IP|CIDR|USER) ([^\x00-\x20\x7f]{1,128})$`)
	BanDelArgsRE        = regexp.MustCompile(`^(SERVER|#[A-Za-z0-9_-]{1,48}) (IP|CIDR|USER) ([^\x00-\x20\x7f]{1,128})$`)
	BanListArgsRE       = regexp.MustCompile(`^(SERVER|#[A-Za-z0-9_-]{1,48})$`)
	TransferStartArgsRE = regexp.MustCompile(`^START (#[A-Za-z0-9_-]{1,48}|[A-Za-z][A-Za-z0-9_\-\[\]\\` + "`" + `^{}]{0,30}) ([0-9]{1,9}) ([A-Za-z0-9][A-Za-z0-9.+-]{0,31}/[A-Za-z0-9][A-Za-z0-9.+-]{0,31}) :(.{1,80})$`)
	TransferChunkArgsRE = regexp.MustCompile(`^CHUNK ([a-f0-9]{24}) ([0-9]{1,7}) ([A-Za-z0-9+/]{2,320}={0,2})$`)
	TransferEndArgsRE   = regexp.MustCompile(`^END ([a-f0-9]{24}) ([a-f0-9]{64})$`)
	RecvArgsRE          = regexp.MustCompile(`^([a-f0-9]{24})$`)
	HelpArgsRE          = regexp.MustCompile(`^(?:([A-Za-z][A-Za-z0-9_-]{0,31}))?$`)
)

func ParseLine(raw string) (Command, error) {
	if len(raw) == 0 || len(raw) > MaxLineBytes || !utf8.ValidString(raw) || utf8.RuneCountInString(raw) > MaxLineRunes {
		return Command{}, errors.New("invalid line length or utf-8")
	}
	m := envelopeRE.FindStringSubmatch(raw)
	if m == nil {
		return Command{}, errors.New("invalid command envelope")
	}
	return Command{Name: strings.ToUpper(m[1]), Args: m[2]}, nil
}

// ValidText rejects IRC/terminal control characters. ASCII is naturally valid UTF-8;
// other accepted text must be Unicode letters, marks, numbers, punctuation, symbols,
// or ordinary space separators.
var textRE = regexp.MustCompile(`^[\x20-\x7e\p{L}\p{M}\p{N}\p{P}\p{S}\p{Zs}]*$`)

func ValidText(s string, maxRunes int) bool {
	return utf8.ValidString(s) && len([]rune(s)) <= maxRunes && textRE.MatchString(s)
}

func Norm(s string) string { return strings.ToLower(s) }
