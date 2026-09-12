package protocol

import "testing"

func TestCommandArgumentRegexes(t *testing.T) {
	tests := []struct {
		name string
		re   interface{ MatchString(string) bool }
		good []string
		bad  []string
	}{
		{"nick", NickRE, []string{"alice", "Alice_2"}, []string{"2alice", "a b", "alice\r"}},
		{"user", UserArgsRE, []string{"alice 0 * :Alice Example", "alice 0 * Alice"}, []string{"alice x * :Alice", "alice 0 * Alice Example"}},
		{"join", JoinArgsRE, []string{"#general", "#general swordfish"}, []string{"general", "#bad name with spaces"}},
		{"part", PartArgsRE, []string{"#general", "#general :bye"}, []string{"general"}},
		{"message", MessageArgsRE, []string{"#general :hello", "alice :hello"}, []string{"#general hello", "#bad name :x"}},
		{"topic", TopicArgsRE, []string{"#general", "#general :new topic", "#general :"}, []string{"general :topic"}},
		{"who", WhoArgsRE, []string{"#general", "ali*", "192.0.2.*"}, []string{"bad mask"}},
		{"whois", WhoIsArgsRE, []string{"alice"}, []string{"alice,bob", "2alice"}},
		{"mode", ModeArgsRE, []string{"#general", "#general +m", "#general +s", "#general -H", "#general +o alice", "#general +b user:alice"}, []string{"#general +vv alice", "#general b", "#general b user:alice"}},
		{"mode-ban-query", ModeBanListArgsRE, []string{"#general b"}, []string{"#general", "#general b user:alice", "#general +b"}},
		{"kick", KickArgsRE, []string{"#general alice", "#general alice :reason"}, []string{"alice #general"}},
		{"invite", InviteArgsRE, []string{"alice #general"}, []string{"#general alice"}},
		{"oper", OperArgsRE, []string{"admin password1"}, []string{"admin short"}},
		{"identify", IdentifyArgsRE, []string{"alice password1", "password1"}, []string{"alice short", "short"}},
		{"chanadd", ChanAddArgsRE, []string{"#ops :Operations", "#secret PRIVATE :Secret", "#scratch PRIVATE NOHISTORY :Scratch"}, []string{"#ops Operations", "#ops NOHISTORY :Bad", "#ops PRIVATE SOMETIMES :Bad"}},
		{"history", HistoryArgsRE, []string{"#general", "#general 100"}, []string{"general 10", "#general many"}},
		{"banadd", BanAddArgsRE, []string{"SERVER IP 192.0.2.1", "#general USER alice"}, []string{"SERVER HOST example.com"}},
	}
	for _, tc := range tests {
		for _, s := range tc.good {
			if !tc.re.MatchString(s) {
				t.Errorf("%s should accept %q", tc.name, s)
			}
		}
		for _, s := range tc.bad {
			if tc.re.MatchString(s) {
				t.Errorf("%s should reject %q", tc.name, s)
			}
		}
	}
}
