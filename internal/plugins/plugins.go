package plugins

import (
	"ircapybara/internal/plugins/bridge"
	"ircapybara/internal/plugins/chatcompletions"
	"ircapybara/internal/plugins/mirror"
	"ircapybara/pkg/pluginapi"
)

// Builtins is the compile-time plugin registry. Every plugin is part of the
// static Go binary; environment configuration only enables/configures it.
func Builtins() ([]pluginapi.Plugin, error) {
	var out []pluginapi.Plugin
	m, err := mirror.NewFromEnv()
	if err != nil {
		return nil, err
	}
	if m != nil {
		out = append(out, m)
	}
	b, err := bridge.NewFromEnv()
	if err != nil {
		return nil, err
	}
	if b != nil {
		out = append(out, b)
	}
	c, err := chatcompletions.NewFromEnv()
	if err != nil {
		return nil, err
	}
	if c != nil {
		out = append(out, c)
	}
	return out, nil
}
