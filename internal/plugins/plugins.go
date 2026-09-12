package plugins

import "ircapybara/pkg/pluginapi"

// Builtins is the compile-time plugin registry. Add Go plugins here (or return
// them from another package) so the production binary remains fully static.
func Builtins() []pluginapi.Plugin {
	return nil
}
