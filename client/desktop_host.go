package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"ircapybara/client/internal/core"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	notificationBackendNone         = "none"
	notificationBackendWails        = "wails"
	notificationBackendMacOSAScript = "macos-osascript"
)

// desktopHost implements core.Host on top of the Wails runtime. The Wails
// context only exists after startup, so every method tolerates being called
// before that.
type desktopHost struct {
	mu                  sync.RWMutex
	ctx                 context.Context
	notificationBackend string
}

func (h *desktopHost) start(ctx context.Context) {
	backend := chooseNotificationBackend()

	// Wails' macOS notification implementation calls UNUserNotificationCenter.
	// That API aborts the process when Wails is running from the Go build cache
	// (wails dev / go run), because there is no NSBundle application proxy.
	// Only initialise it when we are inside a real .app bundle. Development
	// builds use macOS' built-in osascript notification command instead.
	if backend == notificationBackendWails {
		if err := runtime.InitializeNotifications(ctx); err != nil {
			backend = notificationFallbackBackend()
		}
	}
	h.mu.Lock()
	h.ctx = ctx
	h.notificationBackend = backend
	h.mu.Unlock()
}

func (h *desktopHost) stop() {
	ctx, backend := h.state()
	if ctx != nil && backend == notificationBackendWails {
		runtime.CleanupNotifications(ctx)
	}
}

func (h *desktopHost) state() (context.Context, string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.ctx, h.notificationBackend
}

func (h *desktopHost) Emit(event string, payload any) {
	if ctx, _ := h.state(); ctx != nil {
		runtime.EventsEmit(ctx, event, payload)
	}
}

func (h *desktopHost) ChooseUpload() (core.LocalFile, bool, error) {
	ctx, _ := h.state()
	if ctx == nil {
		return core.LocalFile{}, false, errors.New("the window is not ready yet")
	}
	path, err := runtime.OpenFileDialog(ctx, runtime.OpenDialogOptions{Title: "Attach a file"})
	if err != nil || path == "" {
		return core.LocalFile{}, false, err
	}
	return core.LocalFile{Path: path}, true, nil
}

func (h *desktopHost) ChooseDownloadPath(_, suggestedName, _ string) (string, error) {
	ctx, _ := h.state()
	if ctx == nil {
		return "", errors.New("the window is not ready yet")
	}
	return runtime.SaveFileDialog(ctx, runtime.SaveDialogOptions{Title: "Save attachment", DefaultFilename: suggestedName})
}

// The save dialog already pointed the download at its final location.
func (h *desktopHost) PublishDownload(path, _, _ string) (string, error) { return path, nil }

func (h *desktopHost) FileExists(location string) bool {
	info, err := os.Stat(location)
	return err == nil && !info.IsDir()
}

func (h *desktopHost) RevealFile(location, _ string) error {
	var cmd *exec.Cmd
	switch goruntime.GOOS {
	case "darwin":
		cmd = exec.Command("open", "-R", location)
	case "windows":
		cmd = exec.Command("explorer", "/select,"+location)
	default:
		cmd = exec.Command("xdg-open", filepath.Dir(location))
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("show downloaded file: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func (h *desktopHost) ChooseCertificate() (string, error) {
	ctx, _ := h.state()
	if ctx == nil {
		return "", errors.New("the window is not ready yet")
	}
	return runtime.OpenFileDialog(ctx, runtime.OpenDialogOptions{
		Title:   "Choose server certificate or public key",
		Filters: []runtime.FileFilter{{DisplayName: "TLS certificates / public keys", Pattern: "*.pem;*.crt;*.cer;*.der;*.pub"}},
	})
}

func (h *desktopHost) PrepareNotifications() (bool, error) {
	ctx, backend := h.state()
	if ctx == nil {
		return false, nil
	}
	switch backend {
	case notificationBackendMacOSAScript:
		return true, nil
	case notificationBackendWails:
		if !runtime.IsNotificationAvailable(ctx) {
			return false, nil
		}
		authorized, err := runtime.CheckNotificationAuthorization(ctx)
		if err != nil {
			return false, err
		}
		if authorized {
			return true, nil
		}
		return runtime.RequestNotificationAuthorization(ctx)
	default:
		return false, nil
	}
}

func (h *desktopHost) Notify(title, subtitle, body string, playSound bool) error {
	ctx, backend := h.state()
	if ctx == nil {
		return errors.New("system notifications are not available")
	}
	switch backend {
	case notificationBackendMacOSAScript:
		return sendMacOSAScriptNotification(ctx, title, subtitle, body, playSound)
	case notificationBackendWails:
		if !runtime.IsNotificationAvailable(ctx) {
			return errors.New("system notifications are not available on this platform")
		}
		authorized, err := runtime.CheckNotificationAuthorization(ctx)
		if err != nil {
			return err
		}
		if !authorized {
			authorized, err = runtime.RequestNotificationAuthorization(ctx)
			if err != nil {
				return err
			}
			if !authorized {
				return errors.New("system notification permission was not granted")
			}
		}
		return runtime.SendNotification(ctx, runtime.NotificationOptions{
			ID:       fmt.Sprintf("ircapybara-%d", time.Now().UnixNano()),
			Title:    title,
			Subtitle: subtitle,
			Body:     body,
		})
	default:
		return errors.New("system notifications are not available on this platform")
	}
}

func chooseNotificationBackend() string {
	if goruntime.GOOS == "darwin" {
		if runningInsideMacAppBundle() {
			return notificationBackendWails
		}
		return notificationFallbackBackend()
	}
	return notificationBackendWails
}

func notificationFallbackBackend() string {
	if goruntime.GOOS == "darwin" {
		if _, err := exec.LookPath("osascript"); err == nil {
			return notificationBackendMacOSAScript
		}
	}
	return notificationBackendNone
}

func runningInsideMacAppBundle() bool {
	if goruntime.GOOS != "darwin" {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return pathLooksLikeMacAppBundle(exe)
}

func pathLooksLikeMacAppBundle(exe string) bool {
	clean := filepath.ToSlash(filepath.Clean(exe))
	lower := strings.ToLower(clean)
	marker := ".app/contents/macos/"
	idx := strings.LastIndex(lower, marker)
	if idx < 0 {
		return false
	}
	bundleRoot := clean[:idx+len(".app")]
	infoPath := filepath.FromSlash(bundleRoot + "/Contents/Info.plist")
	info, err := os.Stat(infoPath)
	return err == nil && !info.IsDir()
}

func sendMacOSAScriptNotification(ctx context.Context, title, subtitle, body string, playSound bool) error {
	path, err := exec.LookPath("osascript")
	if err != nil {
		return errors.New("macOS notification helper is unavailable")
	}
	script := macOSNotificationScript(title, subtitle, body, playSound)
	cmd := exec.CommandContext(ctx, path, "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			return fmt.Errorf("system notification failed: %s", detail)
		}
		return fmt.Errorf("system notification failed: %w", err)
	}
	return nil
}

func macOSNotificationScript(title, subtitle, body string, playSound bool) string {
	if body == "" {
		body = "New message"
	}
	script := "display notification " + appleScriptString(body) + " with title " + appleScriptString(title)
	if subtitle != "" {
		script += " subtitle " + appleScriptString(subtitle)
	}
	if playSound {
		script += " sound name " + appleScriptString("Glass")
	}
	return script
}

func appleScriptString(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return "\"" + value + "\""
}
