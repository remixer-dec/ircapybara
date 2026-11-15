package core

import (
	"errors"
	"os"
)

// Event names emitted through Host.Emit. The frontend subscribes to the same
// names on every platform.
const (
	EventState    = "ircapybara:state"
	EventTransfer = "ircapybara:transfer"
	EventTrace    = "ircapybara:trace"
)

// Platform identifiers reported to the frontend through Bootstrap.
const (
	PlatformDesktop = "desktop"
	PlatformAndroid = "android"
)

// LocalFile describes a file the user picked for upload. Path must be readable
// with the standard os package for the lifetime of the upload.
type LocalFile struct {
	Path string `json:"path"`
	Name string `json:"name,omitempty"`
	MIME string `json:"mime,omitempty"`
}

// Host is everything the client core needs from the platform shell. The core
// never talks to a window toolkit directly; the desktop shell implements Host
// with Wails, the Android shell with a gomobile-bound Kotlin object.
//
// Chooser methods may block until the user answers. They are only called from
// UI-initiated requests, never from the IRC read loop.
type Host interface {
	// Emit delivers an event to the frontend. The payload is JSON-serialisable.
	Emit(event string, payload any)

	// ChooseUpload asks the user for a file to attach. ok is false when the
	// user cancelled.
	ChooseUpload() (file LocalFile, ok bool, err error)

	// ChooseDownloadPath returns the local filesystem path a verified
	// attachment should be written to, or "" when the user cancelled.
	ChooseDownloadPath(fileID, suggestedName, mime string) (string, error)

	// PublishDownload is called once after a download was verified and written
	// to path. It returns the user-facing location, which is what the frontend
	// sees and what FileExists and RevealFile receive later. Platforms without
	// a separate public location return path unchanged.
	PublishDownload(path, name, mime string) (string, error)

	// FileExists reports whether a location returned by PublishDownload still
	// exists.
	FileExists(location string) bool

	// RevealFile shows a downloaded file to the user: the containing folder on
	// desktop, a viewer chooser on mobile.
	RevealFile(location, mime string) error

	// ChooseCertificate asks for a certificate or public-key file to pin and
	// returns a readable local path, or "" when the user cancelled.
	ChooseCertificate() (string, error)

	// PrepareNotifications reports whether system notifications can be shown,
	// asking the user for permission when the platform requires it.
	PrepareNotifications() (bool, error)

	// Notify shows a system notification.
	Notify(title, subtitle, body string, sound bool) error
}

// NopHost is a Host without any platform behind it. It is the default when no
// host is configured and keeps the core usable in tests and headless tools.
type NopHost struct{}

var errNoHost = errors.New("not available on this platform")

func (NopHost) Emit(string, any)                                  {}
func (NopHost) ChooseUpload() (LocalFile, bool, error)            { return LocalFile{}, false, errNoHost }
func (NopHost) ChooseDownloadPath(_, _, _ string) (string, error) { return "", errNoHost }
func (NopHost) PublishDownload(path, _, _ string) (string, error) { return path, nil }
func (NopHost) RevealFile(_, _ string) error                      { return errNoHost }
func (NopHost) ChooseCertificate() (string, error)                { return "", errNoHost }
func (NopHost) PrepareNotifications() (bool, error)               { return false, nil }
func (NopHost) Notify(_, _, _ string, _ bool) error               { return errNoHost }

func (NopHost) FileExists(location string) bool {
	info, err := os.Stat(location)
	return err == nil && !info.IsDir()
}
