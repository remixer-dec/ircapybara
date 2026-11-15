// Package mobile is the gomobile binding of the IRCapybara client core.
//
// It is built with `gomobile bind` into an Android library and driven by a thin
// Kotlin shell that hosts the shared web frontend in a WebView. The exported
// API deliberately uses only types gomobile can bridge (strings, booleans and
// errors); structured values cross the boundary as JSON.
//
// Nothing here depends on gomobile at compile time, so the package builds and
// tests like any other Go package.
package mobile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"ircapybara/client/internal/core"
	"ircapybara/client/internal/session"
	"ircapybara/client/internal/xfer"
)

// Host is implemented by the platform shell (Kotlin on Android).
//
// Methods can be called from arbitrary Go threads, never from the platform's
// main thread, so implementations may block while they wait for the user.
type Host interface {
	// Emit delivers a frontend event. payloadJSON is a complete JSON value.
	Emit(event string, payloadJSON string)

	// ConnectionChanged tells the shell whether a server session is alive so it
	// can keep the process running in the background while one is. label is
	// the server's display name.
	ConnectionChanged(active bool, label string)

	// ChooseUpload lets the user pick a file and returns a JSON object
	// {"path": ..., "name": ..., "mime": ...} whose path is readable with
	// ordinary file APIs, or "" when the user cancelled.
	ChooseUpload() (string, error)

	// PublishDownload moves a verified download out of app-private storage into
	// the user's shared downloads and returns a location string the shell can
	// later resolve in FileExists and OpenFile.
	PublishDownload(path string, name string, mime string) (string, error)

	// FileExists reports whether a published download is still present.
	FileExists(location string) bool

	// OpenFile shows a published download to the user.
	OpenFile(location string, mime string) error

	// ChooseCertificate lets the user pick a certificate or public-key file and
	// returns a readable local path, or "" when the user cancelled.
	ChooseCertificate() (string, error)

	// PrepareNotifications reports whether notifications may be posted,
	// requesting the permission first if needed.
	PrepareNotifications() (bool, error)

	// ShowNotification posts a message notification. (Not "Notify": the Java
	// binding would overload Object.notify.)
	ShowNotification(title string, subtitle string, body string, sound bool) error
}

// Bridge owns one client core for the lifetime of the app process.
type Bridge struct {
	app     *core.App
	adapter *hostAdapter
	methods map[string]reflect.Value
}

// Frontend methods that must not be reachable from the web view.
var hiddenMethods = map[string]bool{"Shutdown": true}

// Start creates the client core. configDir holds the settings file and
// downloadDir receives verified downloads before they are published; both must
// be app-private directories.
func Start(host Host, configDir string, downloadDir string) (*Bridge, error) {
	if host == nil {
		return nil, errors.New("mobile host is required")
	}
	if strings.TrimSpace(configDir) == "" || strings.TrimSpace(downloadDir) == "" {
		return nil, errors.New("config and download directories are required")
	}
	for _, dir := range []string{configDir, downloadDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	adapter := newHostAdapter(host, downloadDir)
	app := core.New(core.Options{Host: adapter, Platform: core.PlatformAndroid, ConfigDir: configDir})

	methods := map[string]reflect.Value{}
	value := reflect.ValueOf(app)
	for i := 0; i < value.NumMethod(); i++ {
		name := value.Type().Method(i).Name
		if !hiddenMethods[name] {
			methods[name] = value.Method(i)
		}
	}
	return &Bridge{app: app, adapter: adapter, methods: methods}, nil
}

// Invoke calls a frontend API method by name. argsJSON is a JSON array with one
// element per parameter. The result is the method's return value as JSON, or
// "null" for methods that return nothing.
func (b *Bridge) Invoke(method string, argsJSON string) (result string, err error) {
	defer func() {
		if r := recover(); r != nil {
			result, err = "", fmt.Errorf("%s failed: %v", method, r)
		}
	}()

	fn, ok := b.methods[method]
	if !ok {
		return "", fmt.Errorf("unknown method %q", method)
	}
	var raw []json.RawMessage
	if strings.TrimSpace(argsJSON) != "" {
		if err := json.Unmarshal([]byte(argsJSON), &raw); err != nil {
			return "", fmt.Errorf("%s: arguments must be a JSON array", method)
		}
	}
	fnType := fn.Type()
	if len(raw) != fnType.NumIn() {
		return "", fmt.Errorf("%s expects %d argument(s), got %d", method, fnType.NumIn(), len(raw))
	}
	in := make([]reflect.Value, len(raw))
	for i := range raw {
		arg := reflect.New(fnType.In(i))
		if err := json.Unmarshal(raw[i], arg.Interface()); err != nil {
			return "", fmt.Errorf("%s: invalid argument %d", method, i+1)
		}
		in[i] = arg.Elem()
	}

	out := fn.Call(in)
	if n := len(out); n > 0 && fnType.Out(n-1) == errorType {
		if failure, _ := out[n-1].Interface().(error); failure != nil {
			return "", failure
		}
		out = out[:n-1]
	}
	if len(out) == 0 {
		return "null", nil
	}
	encoded, err := json.Marshal(out[0].Interface())
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

var errorType = reflect.TypeOf((*error)(nil)).Elem()

// CheckConnection probes the server after the device changed networks or woke
// up, so a silently dead connection turns into a visible disconnect.
func (b *Bridge) CheckConnection() {
	_ = b.app.CheckConnection()
}

// Disconnect ends the server session, if any.
func (b *Bridge) Disconnect() {
	_ = b.app.Disconnect()
}

// Stop shuts the client core down. The Bridge must not be used afterwards.
func (b *Bridge) Stop() {
	b.app.Shutdown()
	b.adapter.stop()
}

// hostAdapter turns the JSON-oriented mobile Host into a core.Host.
type hostAdapter struct {
	host        Host
	downloadDir string

	mu      sync.Mutex
	pending *core.ClientSnapshot
	active  bool
	label   string

	wake chan struct{}
	done chan struct{}
	once sync.Once
}

// A state snapshot contains every open conversation. Pushing one into the web
// view per IRC line would make a busy channel or a history replay stall the UI,
// so snapshots are coalesced: only the newest one is delivered, at most once
// per interval. The frontend derives unread counts from message IDs, so it
// tolerates skipped intermediate snapshots.
const stateInterval = 40 * time.Millisecond

func newHostAdapter(host Host, downloadDir string) *hostAdapter {
	h := &hostAdapter{
		host:        host,
		downloadDir: downloadDir,
		wake:        make(chan struct{}, 1),
		done:        make(chan struct{}),
	}
	go h.flushLoop()
	return h
}

func (h *hostAdapter) stop() {
	h.once.Do(func() { close(h.done) })
}

func (h *hostAdapter) flushLoop() {
	for {
		select {
		case <-h.wake:
		case <-h.done:
			return
		}
		h.mu.Lock()
		snapshot := h.pending
		h.pending = nil
		h.mu.Unlock()
		if snapshot != nil {
			h.deliverState(*snapshot)
		}
		select {
		case <-time.After(stateInterval):
		case <-h.done:
			return
		}
	}
}

func (h *hostAdapter) deliverState(snapshot core.ClientSnapshot) {
	active := snapshot.State != session.Disconnected
	h.mu.Lock()
	changed := active != h.active || (active && snapshot.Server != h.label)
	h.active, h.label = active, snapshot.Server
	h.mu.Unlock()
	if changed {
		h.host.ConnectionChanged(active, snapshot.Server)
	}
	h.emitJSON(core.EventState, snapshot)
}

func (h *hostAdapter) emitJSON(event string, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	h.host.Emit(event, string(encoded))
}

func (h *hostAdapter) Emit(event string, payload any) {
	if snapshot, ok := payload.(core.ClientSnapshot); ok && event == core.EventState {
		h.mu.Lock()
		h.pending = &snapshot
		h.mu.Unlock()
		select {
		case h.wake <- struct{}{}:
		default:
		}
		return
	}
	h.emitJSON(event, payload)
}

func (h *hostAdapter) ChooseUpload() (core.LocalFile, bool, error) {
	raw, err := h.host.ChooseUpload()
	if err != nil || strings.TrimSpace(raw) == "" {
		return core.LocalFile{}, false, err
	}
	var file core.LocalFile
	if err := json.Unmarshal([]byte(raw), &file); err != nil {
		return core.LocalFile{}, false, errors.New("the file picker returned an invalid result")
	}
	if file.Path == "" {
		return core.LocalFile{}, false, nil
	}
	return file, true, nil
}

// Mobile downloads need no save dialog: the file is assembled in app-private
// storage and PublishDownload moves it to the shared Downloads collection. The
// server-chosen file ID is hashed so it can never steer the path.
func (h *hostAdapter) ChooseDownloadPath(fileID, suggestedName, _ string) (string, error) {
	sum := sha256.Sum256([]byte(fileID))
	dir := filepath.Join(h.downloadDir, hex.EncodeToString(sum[:8]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, xfer.SanitizeFilename(suggestedName)), nil
}

func (h *hostAdapter) PublishDownload(path, name, mime string) (string, error) {
	return h.host.PublishDownload(path, name, mime)
}

func (h *hostAdapter) FileExists(location string) bool { return h.host.FileExists(location) }

func (h *hostAdapter) RevealFile(location, mime string) error {
	return h.host.OpenFile(location, mime)
}

func (h *hostAdapter) ChooseCertificate() (string, error) { return h.host.ChooseCertificate() }

func (h *hostAdapter) PrepareNotifications() (bool, error) { return h.host.PrepareNotifications() }

func (h *hostAdapter) Notify(title, subtitle, body string, sound bool) error {
	return h.host.ShowNotification(title, subtitle, body, sound)
}
