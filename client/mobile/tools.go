//go:build tools

package mobile

// gomobile bind refuses to run unless golang.org/x/mobile/bind is part of the
// module graph. This file keeps the requirement in go.mod across `go mod tidy`
// without linking anything into a build.
import _ "golang.org/x/mobile/bind"
