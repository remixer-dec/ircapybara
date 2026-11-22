package frontend

import (
	"embed"
	"strings"
)

//go:embed dist/assets/capybara
var avatarAssets embed.FS

// CapybaraAvatars returns the sorted filenames of the bundled SVG artwork.
func CapybaraAvatars() []string {
	entries, err := avatarAssets.ReadDir("dist/assets/capybara")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(strings.ToLower(entry.Name()), ".svg") {
			names = append(names, entry.Name())
		}
	}
	return names
}
