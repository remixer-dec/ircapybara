package server

import (
	"time"

	"ircapybara/internal/model"
)

const maxChannelHistory = 100

// appendChannelHistoryLocked is called with s.mu held. History is deliberately
// RAM-only; channel policy is durable, message contents are not.
func appendChannelHistoryLocked(ch *model.Channel, c *model.Client, text string, notice bool) {
	if !ch.HistoryEnabled {
		return
	}
	ch.History = append(ch.History, model.HistoryMessage{
		At: time.Now(), Nick: c.Nick, Username: c.Username, Host: c.RemoteIP.String(), Text: text, Notice: notice,
	})
	if len(ch.History) > maxChannelHistory {
		copy(ch.History, ch.History[len(ch.History)-maxChannelHistory:])
		ch.History = ch.History[:maxChannelHistory]
	}
}
