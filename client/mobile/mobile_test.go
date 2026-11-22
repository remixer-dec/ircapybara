package mobile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ircapybara/client/internal/core"
	"ircapybara/client/internal/session"
)

type fakeHost struct {
	mu          sync.Mutex
	events      []string
	states      []string
	connections []bool
}

func (h *fakeHost) Emit(event, payload string) {
	h.mu.Lock()
	h.events = append(h.events, event)
	if event == core.EventState {
		h.states = append(h.states, payload)
	}
	h.mu.Unlock()
}

func (h *fakeHost) ConnectionChanged(active bool, _ string) {
	h.mu.Lock()
	h.connections = append(h.connections, active)
	h.mu.Unlock()
}

func (h *fakeHost) ChooseUpload() (string, error)                       { return "", nil }
func (h *fakeHost) PublishDownload(_, n, _ string) (string, error)      { return "content://" + n, nil }
func (h *fakeHost) FileExists(string) bool                              { return true }
func (h *fakeHost) OpenFile(string, string) error                       { return nil }
func (h *fakeHost) ChooseCertificate() (string, error)                  { return "", nil }
func (h *fakeHost) PrepareNotifications() (bool, error)                 { return true, nil }
func (h *fakeHost) ShowNotification(string, string, string, bool) error { return nil }

func startBridge(t *testing.T) (*Bridge, *fakeHost, string) {
	t.Helper()
	host := &fakeHost{}
	config := t.TempDir()
	bridge, err := Start(host, config, filepath.Join(t.TempDir(), "downloads"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bridge.Stop)
	return bridge, host, config
}

func TestInvokeDispatchesFrontendAPI(t *testing.T) {
	bridge, _, config := startBridge(t)

	raw, err := bridge.Invoke("Bootstrap", "[]")
	if err != nil {
		t.Fatal(err)
	}
	var boot struct {
		Platform string `json:"platform"`
		Snapshot struct {
			State string `json:"state"`
		} `json:"snapshot"`
		Settings struct {
			Port int `json:"port"`
		} `json:"settings"`
	}
	if err := json.Unmarshal([]byte(raw), &boot); err != nil {
		t.Fatalf("bootstrap is not JSON: %v", err)
	}
	if boot.Platform != "android" || boot.Snapshot.State != "disconnected" || boot.Settings.Port != 6697 {
		t.Fatalf("unexpected bootstrap: %s", raw)
	}

	// A method with an argument and only an error result.
	if out, err := bridge.Invoke("SetTheme", `["dark"]`); err != nil || out != "null" {
		t.Fatalf("SetTheme = %q, %v", out, err)
	}
	saved, err := os.ReadFile(filepath.Join(config, "settings.json"))
	if err != nil || !strings.Contains(string(saved), `"dark"`) {
		t.Fatalf("theme was not written to the configured directory: %s (%v)", saved, err)
	}

	// A (value, error) method whose value comes from the host.
	if out, err := bridge.Invoke("PrepareSystemNotifications", ""); err != nil || out != "true" {
		t.Fatalf("PrepareSystemNotifications = %q, %v", out, err)
	}
}

func TestInvokeRejectsBadCalls(t *testing.T) {
	bridge, _, _ := startBridge(t)

	cases := []struct{ name, method, args, want string }{
		{"unknown method", "Nope", "[]", "unknown method"},
		{"hidden lifecycle method", "Shutdown", "[]", "unknown method"},
		{"too few arguments", "SetTheme", "[]", "expects 1 argument"},
		{"too many arguments", "Bootstrap", `[1]`, "expects 0 argument"},
		{"wrong argument type", "SetTheme", `[42]`, "invalid argument 1"},
		{"arguments not an array", "SetTheme", `{"theme":"dark"}`, "JSON array"},
		{"method's own error", "SetTheme", `["purple"]`, "invalid theme"},
		{"validation error from the core", "SendMessage", `["#general","hi"]`, "not available until the connection is ready"},
	}
	for _, tc := range cases {
		_, err := bridge.Invoke(tc.method, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want it to contain %q", tc.name, err, tc.want)
		}
	}
}

func TestStateEventsAreCoalescedAndDriveConnectionCallback(t *testing.T) {
	host := &fakeHost{}
	adapter := newHostAdapter(host, t.TempDir())
	t.Cleanup(adapter.stop)

	// Establish an active session before the burst so coalescing can skip any
	// of its intermediate snapshots and still deliver the disconnect callback.
	adapter.deliverState(core.ClientSnapshot{State: session.Ready, Server: "home"})

	const burst = 200
	for i := 0; i < burst; i++ {
		adapter.Emit(core.EventState, core.ClientSnapshot{State: session.Ready, Server: "home", StateDetail: strings.Repeat("x", i)})
	}
	adapter.Emit(core.EventState, core.ClientSnapshot{State: session.Disconnected, Server: "home"})
	adapter.Emit(core.EventTransfer, core.TransferUpdate{FileID: "f1", Status: "complete"})

	deadline := time.Now().Add(3 * time.Second)
	for {
		host.mu.Lock()
		n := len(host.states)
		last := ""
		if n > 0 {
			last = host.states[n-1]
		}
		host.mu.Unlock()
		if strings.Contains(last, `"disconnected"`) {
			if n >= burst/2 {
				t.Fatalf("%d of %d state snapshots reached the web view; they should be coalesced", n, burst+1)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the final state snapshot was never delivered")
		}
		time.Sleep(10 * time.Millisecond)
	}

	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.connections) != 2 || !host.connections[0] || host.connections[1] {
		t.Fatalf("connection callbacks = %v, want [true false]", host.connections)
	}
	sawTransfer := false
	for _, event := range host.events {
		sawTransfer = sawTransfer || event == core.EventTransfer
	}
	if !sawTransfer {
		t.Fatal("non-state events must be delivered immediately")
	}
}

func TestDownloadPathStaysInsideDownloadDir(t *testing.T) {
	dir := t.TempDir()
	adapter := newHostAdapter(&fakeHost{}, dir)
	t.Cleanup(adapter.stop)

	path, err := adapter.ChooseDownloadPath("../../etc/passwd", "../../evil.sh", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.Base(path) != "evil.sh" {
		t.Fatalf("download path %q escapes %q", path, dir)
	}
}
