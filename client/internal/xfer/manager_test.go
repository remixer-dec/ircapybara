package xfer

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func hashHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func prepareDownload(t *testing.T, m *Manager, id string, data []byte, chunk int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), id+".bin")
	if err := m.BeginDownload(id, int64(len(data)), id+".bin", "application/octet-stream", path); err != nil {
		t.Fatal(err)
	}
	parts := 0
	if len(data) > 0 {
		parts = (len(data) + chunk - 1) / chunk
	}
	if _, err := m.StartDownload(id, int64(len(data)), chunk, parts, id+".bin", "application/octet-stream", false); err != nil {
		t.Fatal(err)
	}
	return path
}

func sendPart(t *testing.T, m *Manager, id string, data []byte, chunk, part int) ChunkResult {
	t.Helper()
	start := part * chunk
	end := start + chunk
	if end > len(data) {
		end = len(data)
	}
	res, err := m.AcceptChunk(id, part, b64(data[start:end]))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func finishAndAck(t *testing.T, m *Manager, id string, data []byte) string {
	t.Helper()
	end, err := m.EndDownload(id, hashHex(data))
	if err != nil {
		t.Fatal(err)
	}
	if !end.Complete || end.Path == "" {
		t.Fatalf("expected verified result: %+v", end)
	}
	if _, err := m.MarkAckSent(id, end.SHA256); err != nil {
		t.Fatal(err)
	}
	return end.Path
}

func TestSanitizeFilename(t *testing.T) {
	got := SanitizeFilename("../../bad:<name>.txt")
	if got != "bad__name_.txt" {
		t.Fatalf("unexpected %q", got)
	}
}

func TestOrderedDownload(t *testing.T) {
	data := []byte("abcdefghijklm")
	m := New(1024)
	path := prepareDownload(t, m, "ordered", data, 3)
	for part := 0; part < 5; part++ {
		sendPart(t, m, "ordered", data, 3, part)
	}
	gotPath := finishAndAck(t, m, "ordered", data)
	if gotPath != path {
		t.Fatalf("got path %q want %q", gotPath, path)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(data) {
		t.Fatalf("bad file %q", got)
	}
}

func TestOutOfOrderDownload(t *testing.T) {
	data := []byte("abcdefghijklm")
	m := New(1024)
	prepareDownload(t, m, "ooo", data, 3)
	for _, part := range []int{0, 2, 1, 4, 3} {
		sendPart(t, m, "ooo", data, 3, part)
	}
	finishAndAck(t, m, "ooo", data)
}

func TestDuplicateDownloadChunk(t *testing.T) {
	data := []byte("abcdefghi")
	m := New(1024)
	prepareDownload(t, m, "dup", data, 3)
	sendPart(t, m, "dup", data, 3, 0)
	sendPart(t, m, "dup", data, 3, 1)
	res := sendPart(t, m, "dup", data, 3, 1)
	if !res.Duplicate || res.ReceivedBytes != 6 {
		t.Fatalf("duplicate affected progress: %+v", res)
	}
	sendPart(t, m, "dup", data, 3, 2)
	finishAndAck(t, m, "dup", data)
}

func TestConflictingDuplicateFailsIntegrity(t *testing.T) {
	data := []byte("abcdef")
	m := New(1024)
	prepareDownload(t, m, "conflict", data, 3)
	sendPart(t, m, "conflict", data, 3, 0)
	if _, err := m.AcceptChunk("conflict", 0, b64([]byte("xyz"))); err != ErrConflictingDuplicate {
		t.Fatalf("got %v want conflicting duplicate", err)
	}
	s, _ := m.DownloadSnapshot("conflict")
	if s.Status != StatusRetryAvailable {
		t.Fatalf("unexpected status %q", s.Status)
	}
}

func TestMissingPartRequestsRange(t *testing.T) {
	data := []byte("abcdefghijklmno")
	m := New(1024)
	prepareDownload(t, m, "missing", data, 3)
	for _, part := range []int{0, 1, 3, 4} {
		sendPart(t, m, "missing", data, 3, part)
	}
	end, err := m.EndDownload("missing", hashHex(data))
	if err != nil {
		t.Fatal(err)
	}
	if end.MissingRanges != "2" {
		t.Fatalf("got missing %q", end.MissingRanges)
	}
	ranges, _, err := m.MissingRequest("missing", true)
	if err != nil || ranges != "2" {
		t.Fatalf("request got %q %v", ranges, err)
	}
}

func TestFormatMissingRanges(t *testing.T) {
	if got := FormatRanges([]int{1, 2, 3, 8, 12, 13}); got != "1-3,8,12-13" {
		t.Fatalf("got %q", got)
	}
	parts, err := ParseRanges("1-3,8,12-13", 20)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{1, 2, 3, 8, 12, 13}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("got %v", parts)
		}
	}
}

func TestResumePreservesChunksAndExpectedHash(t *testing.T) {
	data := []byte("abcdefghijklmno")
	m := New(1024)
	prepareDownload(t, m, "resume", data, 3)
	sendPart(t, m, "resume", data, 3, 0)
	sendPart(t, m, "resume", data, 3, 2)
	end, err := m.EndDownload("resume", hashHex(data))
	if err != nil || end.MissingRanges == "" {
		t.Fatalf("end=%+v err=%v", end, err)
	}
	if _, _, err := m.MissingRequest("resume", true); err != nil {
		t.Fatal(err)
	}
	before, _ := m.DownloadSnapshot("resume")
	if _, err := m.StartDownload("resume", int64(len(data)), 3, 5, "resume.bin", "application/octet-stream", true); err != nil {
		t.Fatal(err)
	}
	after, _ := m.DownloadSnapshot("resume")
	if after.ReceivedParts != before.ReceivedParts || after.ReceivedBytes != before.ReceivedBytes || after.ExpectedSHA256 != before.ExpectedSHA256 {
		t.Fatalf("resume lost state: before=%+v after=%+v", before, after)
	}
}

func TestEndBeforeEarlierChunkCallbackFinishes(t *testing.T) {
	data := []byte("abcdefghi")
	m := New(1024)
	prepareDownload(t, m, "end-first", data, 3)
	sendPart(t, m, "end-first", data, 3, 0)
	sendPart(t, m, "end-first", data, 3, 2)
	end, err := m.EndDownload("end-first", hashHex(data))
	if err != nil || end.MissingRanges != "1" {
		t.Fatalf("end=%+v err=%v", end, err)
	}
	// A callback for part 1 may finish after END was dispatched.
	sendPart(t, m, "end-first", data, 3, 1)
	end, err = m.EndDownload("end-first", hashHex(data))
	if err != nil || !end.Complete {
		t.Fatalf("second end=%+v err=%v", end, err)
	}
}

func TestVerifiedFileIsNotExposedBeforeAckSent(t *testing.T) {
	data := []byte("abcdef")
	m := New(1024)
	path := prepareDownload(t, m, "ack-gate", data, 3)
	sendPart(t, m, "ack-gate", data, 3, 0)
	sendPart(t, m, "ack-gate", data, 3, 1)
	end, err := m.EndDownload("ack-gate", hashHex(data))
	if err != nil || !end.Complete {
		t.Fatalf("end=%+v err=%v", end, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("final file became visible before RECV ACK was sent: %v", err)
	}
	if _, err := m.MarkAckSent("ack-gate", end.SHA256); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(data) {
		t.Fatalf("file after ACK: %q err=%v", got, err)
	}
}

func TestHashMismatchDoesNotAckOrDestroyMetadata(t *testing.T) {
	data := []byte("abcdef")
	m := New(1024)
	prepareDownload(t, m, "hash", data, 3)
	sendPart(t, m, "hash", data, 3, 0)
	sendPart(t, m, "hash", data, 3, 1)
	wrong := hashHex([]byte("ghijkl"))
	if _, err := m.EndDownload("hash", wrong); err != ErrHashMismatch {
		t.Fatalf("got %v", err)
	}
	s, ok := m.DownloadSnapshot("hash")
	if !ok || s.Status != StatusRetryAvailable || s.SavePath == "" || s.ReceivedBytes != 0 {
		t.Fatalf("unexpected retained state: %+v", s)
	}
	if _, err := m.MarkAckSent("hash", wrong); err == nil {
		t.Fatal("hash mismatch was incorrectly ACK-able")
	}
}

func TestReconnectPartialCanResume(t *testing.T) {
	data := []byte("abcdefghijkl")
	m := New(1024)
	prepareDownload(t, m, "reconnect", data, 3)
	for _, part := range []int{0, 2} {
		sendPart(t, m, "reconnect", data, 3, part)
	}
	interrupted := m.MarkInterruptedAll()
	if len(interrupted) != 1 || interrupted[0].Status != StatusInterrupted {
		t.Fatalf("bad interrupted state: %+v", interrupted)
	}
	if _, err := m.StartDownload("reconnect", int64(len(data)), 3, 4, "reconnect.bin", "application/octet-stream", true); err != nil {
		t.Fatal(err)
	}
	for _, part := range []int{1, 3} {
		sendPart(t, m, "reconnect", data, 3, part)
	}
	finishAndAck(t, m, "reconnect", data)
}

func TestDuplicateRetransmissionDoesNotIncreaseProgress(t *testing.T) {
	data := []byte("abcdef")
	m := New(1024)
	prepareDownload(t, m, "redup", data, 3)
	first := sendPart(t, m, "redup", data, 3, 0)
	second := sendPart(t, m, "redup", data, 3, 0)
	if first.ReceivedBytes != second.ReceivedBytes || second.ReceivedParts != 1 || !second.Duplicate {
		t.Fatalf("progress changed on duplicate: first=%+v second=%+v", first, second)
	}
}

func TestConcurrentDispatchProducesOriginalFile(t *testing.T) {
	const chunk = 240
	data := make([]byte, chunk*80+37)
	rnd := rand.New(rand.NewSource(42))
	_, _ = rnd.Read(data)
	m := New(int64(len(data) + 1))
	prepareDownload(t, m, "concurrent", data, chunk)
	parts := (len(data) + chunk - 1) / chunk
	order := rnd.Perm(parts)
	var wg sync.WaitGroup
	errCh := make(chan error, parts)
	for _, part := range order {
		part := part
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := part * chunk
			end := start + chunk
			if end > len(data) {
				end = len(data)
			}
			_, err := m.AcceptChunk("concurrent", part, b64(data[start:end]))
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	path := finishAndAck(t, m, "concurrent", data)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatal("concurrent file differs")
	}
}

func TestLarge15MiBDownload(t *testing.T) {
	if testing.Short() {
		t.Skip("large transfer")
	}
	const chunk = 24000
	data := make([]byte, DefaultMaxFileSize)
	for i := range data {
		data[i] = byte(i * 31)
	}
	m := New(DefaultMaxFileSize)
	prepareDownload(t, m, "large", data, chunk)
	parts := (len(data) + chunk - 1) / chunk
	for part := 0; part < parts; part++ {
		sendPart(t, m, "large", data, chunk, part)
	}
	finishAndAck(t, m, "large", data)
}

func TestMalformedBase64LeavesPartMissing(t *testing.T) {
	data := []byte("abcdef")
	m := New(1024)
	prepareDownload(t, m, "b64", data, 3)
	if _, err := m.AcceptChunk("b64", 0, "%%%not-base64%%%"); err == nil {
		t.Fatal("malformed base64 accepted")
	}
	end, err := m.EndDownload("b64", hashHex(data))
	if err != nil || end.MissingRanges != "0-1" {
		t.Fatalf("missing=%q err=%v", end.MissingRanges, err)
	}
}

func TestFinalPartSizeEnforced(t *testing.T) {
	data := []byte("abcde")
	m := New(1024)
	prepareDownload(t, m, "final", data, 3)
	if _, err := m.AcceptChunk("final", 1, b64([]byte("xyz"))); err == nil {
		t.Fatal("oversized final chunk accepted")
	}
}

func TestRetryLimitIsBounded(t *testing.T) {
	data := []byte("abcdef")
	m := New(1024)
	prepareDownload(t, m, "retry", data, 3)
	for round := 0; round < DefaultMaxRetransmissionRounds; round++ {
		if _, _, err := m.MissingRequest("retry", true); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		// A resume response ends the outstanding request without filling anything.
		if _, err := m.StartDownload("retry", int64(len(data)), 3, 2, "retry.bin", "application/octet-stream", true); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := m.MissingRequest("retry", true); err != ErrRetryLimit {
		t.Fatalf("got %v want retry limit", err)
	}
}

func TestStalledDownloadUsesInactivity(t *testing.T) {
	data := []byte("abcdef")
	m := New(1024)
	prepareDownload(t, m, "idle", data, 3)
	m.mu.Lock()
	m.downloads["idle"].LastActivity = time.Now().Add(-time.Minute)
	m.mu.Unlock()
	if got := m.StalledDownloads(time.Now(), 45*time.Second); len(got) != 1 {
		t.Fatalf("expected one stalled transfer, got %d", len(got))
	}
}

func TestUploadMissingPartRetransmission(t *testing.T) {
	data := []byte("abcdefghijkl")
	path := filepath.Join(t.TempDir(), "upload.bin")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(1024)
	u := PendingUpload{Target: "#general", Path: path, Name: "upload.bin", MIME: "application/octet-stream", Size: int64(len(data))}
	if err := m.BeginUpload("up", u, 3); err != nil {
		t.Fatal(err)
	}
	var all []int
	sha, size, err := m.StreamUpload("up", nil, func(part int, _ string) error { all = append(all, part); return nil })
	if err != nil || size != int64(len(data)) || sha != hashHex(data) {
		t.Fatalf("sha=%s size=%d err=%v", sha, size, err)
	}
	var resent []int
	sha2, _, err := m.StreamUpload("up", []int{1, 3}, func(part int, _ string) error { resent = append(resent, part); return nil })
	if err != nil || sha2 != sha {
		t.Fatalf("retransmit sha=%s err=%v", sha2, err)
	}
	if len(resent) != 2 || resent[0] != 1 || resent[1] != 3 {
		t.Fatalf("resent %v", resent)
	}
}
