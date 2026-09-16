package xfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultMaxFileSize int64 = 15 * 1024 * 1024
	// 96 KiB still fits below IRCapybara's 132 KiB protocol-line ceiling after
	// Base64 expansion and command framing. Current XFER v2 servers use 24,000.
	MaxChunkSize                   = 96 * 1024
	MaxTransferParts               = 1_000_000
	DefaultMaxRetransmissionRounds = 3
)

var (
	unsafeName = regexp.MustCompile(`[\x00-\x1f<>:"/\\|?*]`)

	ErrUnknownDownload      = errors.New("unknown download")
	ErrTransferNotStarted   = errors.New("download metadata has not been received")
	ErrMetadataConflict     = errors.New("transfer metadata changed during resume")
	ErrConflictingDuplicate = errors.New("conflicting duplicate chunk")
	ErrRetryLimit           = errors.New("automatic retransmission limit reached")
	ErrMissingRequestActive = errors.New("a missing-part request is already in progress")
	ErrHashMismatch         = errors.New("sha256 mismatch")
)

type PendingUpload struct {
	Target string
	Path   string
	Name   string
	MIME   string
	Size   int64
}

type DownloadStatus string

const (
	StatusQueued           DownloadStatus = "queued"
	StatusDownloading      DownloadStatus = "downloading"
	StatusReceivingMissing DownloadStatus = "receiving-missing"
	StatusVerifying        DownloadStatus = "verifying"
	StatusVerified         DownloadStatus = "verified"
	StatusComplete         DownloadStatus = "complete"
	StatusInterrupted      DownloadStatus = "interrupted"
	StatusRetryAvailable   DownloadStatus = "retry-available"
	StatusExpired          DownloadStatus = "expired"
	StatusUnauthorized     DownloadStatus = "unauthorized"
)

type DownloadSnapshot struct {
	FileID               string
	Filename             string
	MIME                 string
	SavePath             string
	Size                 int64
	ChunkSize            int
	TotalParts           int
	ReceivedBytes        int64
	ReceivedParts        int
	ExpectedSHA256       string
	VerifiedSHA256       string
	EndSeen              bool
	Complete             bool
	Status               DownloadStatus
	RetransmissionRounds int
	LastActivity         time.Time
}

type ChunkResult struct {
	Duplicate     bool
	ReceivedBytes int64
	ReceivedParts int
	TotalParts    int
	TotalSize     int64
}

type EndResult struct {
	Complete      bool
	Path          string
	SHA256        string
	MissingRanges string
	ReceivedBytes int64
	TotalSize     int64
}

type download struct {
	FileID                string
	Filename              string
	MIME                  string
	SavePath              string
	Size                  int64
	attachmentSize        int64
	ChunkSize             int
	TotalParts            int
	Parts                 [][]byte
	Received              []bool
	ReceivedBytes         int64
	ReceivedParts         int
	ExpectedSHA256        string
	VerifiedSHA256        string
	VerifiedTempPath      string
	EndSeen               bool
	Complete              bool
	MetadataReady         bool
	Status                DownloadStatus
	RetransmissionRounds  int
	MissingRequestPending bool
	LastActivity          time.Time
}

type uploadSession struct {
	PendingUpload
	FileID    string
	ChunkSize int
	SHA256    string
}

type Manager struct {
	mu            sync.Mutex
	maxFileSize   int64
	uploads       []PendingUpload
	activeUploads map[string]*uploadSession
	downloads     map[string]*download
}

func New(maxFileSize int64) *Manager {
	if maxFileSize <= 0 {
		maxFileSize = DefaultMaxFileSize
	}
	return &Manager{
		maxFileSize:   maxFileSize,
		downloads:     map[string]*download{},
		activeUploads: map[string]*uploadSession{},
	}
}

func SanitizeFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = unsafeName.ReplaceAllString(name, "_")
	name = strings.Trim(name, ". ")
	if name == "" || name == "." || name == ".." {
		return "attachment.bin"
	}
	if len([]rune(name)) > 180 {
		r := []rune(name)
		name = string(r[:180])
	}
	return name
}

func (m *Manager) QueueUpload(u PendingUpload) error {
	if u.Size < 0 || u.Size > m.maxFileSize {
		return fmt.Errorf("file size exceeds %d bytes", m.maxFileSize)
	}
	u.Name = SanitizeFilename(u.Name)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.uploads) >= 4 {
		return errors.New("too many pending uploads")
	}
	m.uploads = append(m.uploads, u)
	return nil
}

func (m *Manager) PopUpload() (PendingUpload, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.uploads) == 0 {
		return PendingUpload{}, false
	}
	u := m.uploads[0]
	m.uploads = append([]PendingUpload(nil), m.uploads[1:]...)
	return u, true
}

func (m *Manager) CancelUpload(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.uploads) - 1; i >= 0; i-- {
		if m.uploads[i].Path == path {
			m.uploads = append(m.uploads[:i], m.uploads[i+1:]...)
			return
		}
	}
}

func (m *Manager) BeginUpload(fileID string, u PendingUpload, chunkSize int) error {
	if fileID == "" {
		return errors.New("missing file id")
	}
	if chunkSize <= 0 || chunkSize > MaxChunkSize {
		return errors.New("invalid transfer chunk size")
	}
	parts := 0
	if u.Size > 0 {
		parts = int((u.Size + int64(chunkSize) - 1) / int64(chunkSize))
	}
	if parts > MaxTransferParts {
		return errors.New("transfer has too many parts")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activeUploads[fileID] = &uploadSession{PendingUpload: u, FileID: fileID, ChunkSize: chunkSize}
	return nil
}

func (m *Manager) StreamUpload(fileID string, parts []int, send func(part int, b64 string) error) (string, int64, error) {
	m.mu.Lock()
	u := m.activeUploads[fileID]
	m.mu.Unlock()
	if u == nil {
		return "", 0, errors.New("unknown upload")
	}
	sha, size, err := streamUploadParts(u.Path, u.ChunkSize, parts, send)
	if err != nil {
		return "", size, err
	}
	if len(parts) == 0 {
		m.mu.Lock()
		if current := m.activeUploads[fileID]; current != nil {
			current.SHA256 = sha
		}
		m.mu.Unlock()
	} else if u.SHA256 != "" {
		if !strings.EqualFold(sha, u.SHA256) {
			return "", size, errors.New("upload changed since the initial transfer")
		}
		sha = u.SHA256
	}
	return sha, size, nil
}

func (m *Manager) UploadTotalParts(fileID string) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.activeUploads[fileID]
	if u == nil || u.ChunkSize <= 0 {
		return 0, false
	}
	if u.Size == 0 {
		return 0, true
	}
	return int((u.Size + int64(u.ChunkSize) - 1) / int64(u.ChunkSize)), true
}

func (m *Manager) UploadSHA(fileID string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.activeUploads[fileID]
	if u == nil || u.SHA256 == "" {
		return "", false
	}
	return u.SHA256, true
}

func (m *Manager) CompleteUpload(fileID string) {
	m.mu.Lock()
	delete(m.activeUploads, fileID)
	m.mu.Unlock()
}

// StreamUpload is retained for callers/tests that do not need retransmission.
func StreamUpload(path string, chunkSize int, send func(part int, b64 string) error) (string, int64, error) {
	return streamUploadParts(path, chunkSize, nil, send)
}

// streamUploadParts sends all parts when parts is empty; otherwise it sends only
// the requested part indexes. The returned size/hash always describe the whole file.
func streamUploadParts(path string, chunkSize int, parts []int, send func(part int, b64 string) error) (string, int64, error) {
	if chunkSize <= 0 || chunkSize > MaxChunkSize {
		return "", 0, errors.New("invalid transfer chunk size")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !st.Mode().IsRegular() {
		return "", 0, errors.New("upload is no longer a regular file")
	}
	total := st.Size()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", 0, err
	}
	sha := hex.EncodeToString(h.Sum(nil))

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	totalParts := 0
	if total > 0 {
		totalParts = int((total + int64(chunkSize) - 1) / int64(chunkSize))
	}
	selected := parts
	if len(selected) == 0 {
		selected = make([]int, totalParts)
		for i := range selected {
			selected[i] = i
		}
	} else {
		selected = append([]int(nil), selected...)
		sort.Ints(selected)
	}
	buf := make([]byte, chunkSize)
	for _, part := range selected {
		if part < 0 || part >= totalParts {
			return "", total, fmt.Errorf("requested upload part %d is out of range", part)
		}
		offset := int64(part) * int64(chunkSize)
		want := chunkSize
		if remaining := total - offset; remaining < int64(want) {
			want = int(remaining)
		}
		n, err := f.ReadAt(buf[:want], offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return "", total, err
		}
		if n != want {
			return "", total, errors.New("upload changed while reading")
		}
		if err := send(part, base64.StdEncoding.EncodeToString(buf[:n])); err != nil {
			return "", total, err
		}
	}
	return sha, total, nil
}

// BeginDownload records the user's save destination before the server answers
// RECV with XFER START. Chunk geometry is supplied later by StartDownload.
func (m *Manager) BeginDownload(fileID string, size int64, filename, mime, savePath string) error {
	if fileID == "" {
		return errors.New("missing file id")
	}
	if size < 0 || size > m.maxFileSize {
		return fmt.Errorf("declared size exceeds %d bytes", m.maxFileSize)
	}
	if strings.TrimSpace(savePath) == "" {
		return errors.New("missing save path")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if d := m.downloads[fileID]; d != nil {
		if d.Status == StatusDownloading || d.Status == StatusReceivingMissing || d.Status == StatusVerifying {
			return errors.New("download already active")
		}
		// A deliberate fresh RECV keeps the destination but starts from no chunks.
		d.Filename = SanitizeFilename(filename)
		d.MIME = mime
		d.SavePath = savePath
		d.Size = size
		d.attachmentSize = size
		resetDownloadChunks(d)
		d.MetadataReady = false
		d.Status = StatusQueued
		d.LastActivity = time.Now()
		return nil
	}
	m.downloads[fileID] = &download{
		FileID: fileID, Filename: SanitizeFilename(filename), MIME: mime,
		SavePath: savePath, Size: size, attachmentSize: size,
		Status: StatusQueued, LastActivity: time.Now(),
	}
	return nil
}

func (m *Manager) StartDownload(fileID string, size int64, chunkSize, totalParts int, filename, mime string, resume bool) (DownloadSnapshot, error) {
	if size < 0 || size > m.maxFileSize {
		return DownloadSnapshot{}, fmt.Errorf("declared size exceeds %d bytes", m.maxFileSize)
	}
	if chunkSize <= 0 || chunkSize > MaxChunkSize {
		return DownloadSnapshot{}, errors.New("invalid transfer chunk size")
	}
	expectedParts := 0
	if size > 0 {
		expectedParts = int((size + int64(chunkSize) - 1) / int64(chunkSize))
	}
	if totalParts != expectedParts {
		return DownloadSnapshot{}, fmt.Errorf("invalid part count: got %d want %d", totalParts, expectedParts)
	}
	if totalParts > MaxTransferParts {
		return DownloadSnapshot{}, errors.New("transfer has too many parts")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.downloads[fileID]
	if d == nil {
		return DownloadSnapshot{}, ErrUnknownDownload
	}
	if d.attachmentSize != size {
		resetDownloadChunks(d)
		d.Status = StatusRetryAvailable
		return snapshotDownload(d), ErrMetadataConflict
	}
	if resume && d.MetadataReady {
		if d.Size != size || d.ChunkSize != chunkSize || d.TotalParts != totalParts {
			resetDownloadChunks(d)
			d.MetadataReady = false
			d.Status = StatusRetryAvailable
			return snapshotDownload(d), ErrMetadataConflict
		}
		// Preserve Parts, Received, and ExpectedSHA256 exactly as required by v2.
		d.MissingRequestPending = false
		d.EndSeen = false
	} else {
		d.Size = size
		d.ChunkSize = chunkSize
		d.TotalParts = totalParts
		d.Parts = make([][]byte, totalParts)
		d.Received = make([]bool, totalParts)
		d.ReceivedBytes = 0
		d.ReceivedParts = 0
		d.ExpectedSHA256 = ""
		d.VerifiedSHA256 = ""
		d.EndSeen = false
		d.Complete = false
		d.RetransmissionRounds = 0
		d.MissingRequestPending = false
	}
	d.MetadataReady = true
	if filename != "" {
		d.Filename = SanitizeFilename(filename)
	}
	if mime != "" {
		d.MIME = mime
	}
	d.Status = StatusDownloading
	d.LastActivity = time.Now()
	return snapshotDownload(d), nil
}

func (m *Manager) AcceptChunk(fileID string, part int, encoded string) (ChunkResult, error) {
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return ChunkResult{}, fmt.Errorf("invalid base64 chunk: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.downloads[fileID]
	if d == nil {
		return ChunkResult{}, ErrUnknownDownload
	}
	if !d.MetadataReady {
		return ChunkResult{}, ErrTransferNotStarted
	}
	if d.Status == StatusRetryAvailable || d.Status == StatusExpired || d.Status == StatusUnauthorized || d.Complete {
		return ChunkResult{}, errors.New("transfer is not accepting chunks")
	}
	if part < 0 || part >= d.TotalParts {
		return ChunkResult{}, fmt.Errorf("chunk part %d is out of range", part)
	}
	want := d.ChunkSize
	if part == d.TotalParts-1 {
		last := d.Size - int64(d.TotalParts-1)*int64(d.ChunkSize)
		if last < 0 || last > int64(d.ChunkSize) {
			return ChunkResult{}, errors.New("invalid final-part geometry")
		}
		want = int(last)
	}
	if len(data) != want {
		return ChunkResult{}, fmt.Errorf("chunk %d has %d bytes, expected %d", part, len(data), want)
	}
	if d.Received[part] {
		if bytes.Equal(d.Parts[part], data) {
			d.LastActivity = time.Now()
			return chunkResult(d, true), nil
		}
		d.Status = StatusRetryAvailable
		d.MissingRequestPending = false
		return ChunkResult{}, ErrConflictingDuplicate
	}
	if d.ReceivedBytes+int64(len(data)) > d.Size {
		d.Status = StatusRetryAvailable
		return ChunkResult{}, errors.New("received data exceeds declared size")
	}
	d.Parts[part] = append([]byte(nil), data...)
	d.Received[part] = true
	d.ReceivedBytes += int64(len(data))
	d.ReceivedParts++
	d.LastActivity = time.Now()
	if d.Status == StatusReceivingMissing {
		// Remain in the recovery state until the retransmission's END arrives.
	} else {
		d.Status = StatusDownloading
	}
	return chunkResult(d, false), nil
}

func (m *Manager) EndDownload(fileID, expectedSHA string) (EndResult, error) {
	expectedSHA = strings.ToLower(strings.TrimSpace(expectedSHA))
	if len(expectedSHA) != 64 {
		return EndResult{}, errors.New("invalid sha256 value")
	}
	if _, err := hex.DecodeString(expectedSHA); err != nil {
		return EndResult{}, errors.New("invalid sha256 value")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.downloads[fileID]
	if d == nil {
		return EndResult{}, ErrUnknownDownload
	}
	if !d.MetadataReady {
		return EndResult{}, ErrTransferNotStarted
	}
	if d.Complete {
		if d.VerifiedSHA256 != "" && !strings.EqualFold(d.VerifiedSHA256, expectedSHA) {
			return EndResult{}, errors.New("conflicting transfer hash")
		}
		return EndResult{Complete: true, Path: d.SavePath, SHA256: d.VerifiedSHA256, ReceivedBytes: d.Size, TotalSize: d.Size}, nil
	}
	if d.VerifiedSHA256 != "" {
		if !strings.EqualFold(d.VerifiedSHA256, expectedSHA) {
			d.Status = StatusRetryAvailable
			return EndResult{}, errors.New("conflicting transfer hash")
		}
		return EndResult{Complete: true, Path: d.SavePath, SHA256: d.VerifiedSHA256, ReceivedBytes: d.Size, TotalSize: d.Size}, nil
	}
	if d.ExpectedSHA256 != "" && !strings.EqualFold(d.ExpectedSHA256, expectedSHA) {
		d.Status = StatusRetryAvailable
		d.MissingRequestPending = false
		return EndResult{}, errors.New("conflicting transfer hash")
	}
	d.ExpectedSHA256 = expectedSHA
	d.EndSeen = true
	d.LastActivity = time.Now()
	missing := missingRangesLocked(d)
	if missing != "" {
		d.Status = StatusReceivingMissing
		return EndResult{MissingRanges: missing, ReceivedBytes: d.ReceivedBytes, TotalSize: d.Size}, nil
	}

	d.Status = StatusVerifying
	path, actual, err := verifyAndSaveLocked(d)
	if err != nil {
		if errors.Is(err, ErrHashMismatch) {
			// Keep negotiated metadata/save path, but discard untrustworthy chunk
			// bytes so a retry can request the complete missing bitmap cleanly.
			d.Parts = make([][]byte, d.TotalParts)
			d.Received = make([]bool, d.TotalParts)
			d.ReceivedBytes = 0
			d.ReceivedParts = 0
			d.EndSeen = false
			d.Complete = false
			d.MissingRequestPending = false
			d.Status = StatusRetryAvailable
			d.ExpectedSHA256 = expectedSHA
		}
		return EndResult{ReceivedBytes: d.ReceivedBytes, TotalSize: d.Size}, err
	}
	d.VerifiedSHA256 = actual
	d.Status = StatusVerified
	return EndResult{Complete: true, Path: path, SHA256: actual, ReceivedBytes: d.Size, TotalSize: d.Size}, nil
}

func verifyAndSaveLocked(d *download) (string, string, error) {
	if d.ReceivedParts != d.TotalParts || d.ReceivedBytes != d.Size {
		return "", "", errors.New("download is incomplete")
	}
	if d.SavePath == "" {
		return "", "", errors.New("missing save path")
	}
	tempPattern := "." + SanitizeFilename(filepath.Base(d.SavePath)) + ".ircapybara-*"
	f, err := os.CreateTemp(filepath.Dir(d.SavePath), tempPattern)
	if err != nil {
		return "", "", err
	}
	temp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(temp)
	}
	h := sha256.New()
	var written int64
	for part := 0; part < d.TotalParts; part++ {
		if !d.Received[part] {
			cleanup()
			return "", "", errors.New("download is incomplete")
		}
		chunk := d.Parts[part]
		if written+int64(len(chunk)) > d.Size {
			cleanup()
			return "", "", errors.New("assembled file exceeds declared size")
		}
		if _, err := f.Write(chunk); err != nil {
			cleanup()
			return "", "", err
		}
		_, _ = h.Write(chunk)
		written += int64(len(chunk))
	}
	if written != d.Size {
		cleanup()
		return "", "", fmt.Errorf("size mismatch: got %d want %d", written, d.Size)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(actual, d.ExpectedSHA256) {
		cleanup()
		return "", actual, ErrHashMismatch
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return "", "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(temp)
		return "", "", err
	}
	// Keep the verified bytes private until RECV ACK has actually been sent.
	// The user-selected final path is only populated by MarkAckSent.
	d.VerifiedTempPath = temp
	return d.SavePath, actual, nil
}

func (m *Manager) MarkAckSent(fileID, sha string) (DownloadSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.downloads[fileID]
	if d == nil {
		return DownloadSnapshot{}, ErrUnknownDownload
	}
	if d.Status != StatusVerified || d.VerifiedSHA256 == "" || !strings.EqualFold(d.VerifiedSHA256, strings.TrimSpace(sha)) {
		return snapshotDownload(d), errors.New("download has not been verified")
	}
	if d.VerifiedTempPath != "" {
		if err := os.Rename(d.VerifiedTempPath, d.SavePath); err != nil {
			return snapshotDownload(d), err
		}
		d.VerifiedTempPath = ""
	}
	d.Complete = true
	d.Status = StatusComplete
	d.LastActivity = time.Now()
	return snapshotDownload(d), nil
}

func (m *Manager) MarkServerAck(fileID string) (DownloadSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.downloads[fileID]
	if d == nil {
		return DownloadSnapshot{}, ErrUnknownDownload
	}
	if !d.Complete {
		return snapshotDownload(d), errors.New("server acknowledged transfer before local verification acknowledgement")
	}
	d.Complete = true
	d.Status = StatusComplete
	d.LastActivity = time.Now()
	// The server has acknowledged our verified hash; resumable chunk state is no
	// longer needed. Keep metadata/path so "Show in Folder" continues working.
	d.Parts = nil
	d.Received = nil
	d.ReceivedParts = d.TotalParts
	d.ReceivedBytes = d.Size
	return snapshotDownload(d), nil
}

func (m *Manager) MissingRequest(fileID string, automatic bool) (string, DownloadSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.downloads[fileID]
	if d == nil {
		return "", DownloadSnapshot{}, ErrUnknownDownload
	}
	if !d.MetadataReady {
		return "", snapshotDownload(d), ErrTransferNotStarted
	}
	if d.MissingRequestPending {
		return "", snapshotDownload(d), ErrMissingRequestActive
	}
	ranges := missingRangesLocked(d)
	if ranges == "" {
		return "", snapshotDownload(d), nil
	}
	if automatic && d.RetransmissionRounds >= DefaultMaxRetransmissionRounds {
		d.Status = StatusRetryAvailable
		return "", snapshotDownload(d), ErrRetryLimit
	}
	if automatic {
		d.RetransmissionRounds++
	}
	d.MissingRequestPending = true
	d.Status = StatusReceivingMissing
	d.LastActivity = time.Now()
	return ranges, snapshotDownload(d), nil
}

func (m *Manager) PrepareFullRestart(fileID string) (DownloadSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.downloads[fileID]
	if d == nil {
		return DownloadSnapshot{}, ErrUnknownDownload
	}
	resetDownloadChunks(d)
	d.MetadataReady = false
	d.Status = StatusQueued
	d.LastActivity = time.Now()
	return snapshotDownload(d), nil
}

func (m *Manager) MarkInterrupted(fileID string) (DownloadSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.downloads[fileID]
	if d == nil {
		return DownloadSnapshot{}, ErrUnknownDownload
	}
	if !d.Complete && d.Status != StatusExpired && d.Status != StatusUnauthorized {
		d.Status = StatusInterrupted
		d.MissingRequestPending = false
	}
	return snapshotDownload(d), nil
}

func (m *Manager) MarkRetryAvailable(fileID string) (DownloadSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.downloads[fileID]
	if d == nil {
		return DownloadSnapshot{}, ErrUnknownDownload
	}
	if !d.Complete {
		d.Status = StatusRetryAvailable
		d.MissingRequestPending = false
	}
	return snapshotDownload(d), nil
}

func (m *Manager) MarkInterruptedAll() []DownloadSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]DownloadSnapshot, 0)
	for _, d := range m.downloads {
		if d.Complete || d.Status == StatusExpired || d.Status == StatusUnauthorized {
			continue
		}
		d.Status = StatusInterrupted
		d.MissingRequestPending = false
		out = append(out, snapshotDownload(d))
	}
	return out
}

func (m *Manager) ResumeCandidates() []DownloadSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]DownloadSnapshot, 0)
	for _, d := range m.downloads {
		if d.Complete || d.Status == StatusExpired || d.Status == StatusUnauthorized {
			continue
		}
		out = append(out, snapshotDownload(d))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FileID < out[j].FileID })
	return out
}

func (m *Manager) MarkUnavailable(fileID string, unauthorized bool) (DownloadSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.downloads[fileID]
	if d == nil {
		return DownloadSnapshot{}, ErrUnknownDownload
	}
	if d.VerifiedTempPath != "" {
		_ = os.Remove(d.VerifiedTempPath)
		d.VerifiedTempPath = ""
	}
	if unauthorized {
		d.Status = StatusUnauthorized
	} else {
		d.Status = StatusExpired
	}
	d.MissingRequestPending = false
	d.Parts = nil
	d.Received = nil
	return snapshotDownload(d), nil
}

func (m *Manager) DownloadSnapshot(fileID string) (DownloadSnapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.downloads[fileID]
	if d == nil {
		return DownloadSnapshot{}, false
	}
	return snapshotDownload(d), true
}

func (m *Manager) DownloadProgress(fileID string) (received, total int64, ok bool) {
	s, ok := m.DownloadSnapshot(fileID)
	if !ok {
		return 0, 0, false
	}
	return s.ReceivedBytes, s.Size, true
}

func (m *Manager) IsDownloadActive(fileID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.downloads[fileID]
	if d == nil {
		return false
	}
	switch d.Status {
	case StatusQueued, StatusDownloading, StatusReceivingMissing, StatusVerifying, StatusVerified, StatusInterrupted:
		return true
	default:
		return false
	}
}

func (m *Manager) StalledDownloads(now time.Time, idle time.Duration) []DownloadSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]DownloadSnapshot, 0)
	for _, d := range m.downloads {
		if !d.MetadataReady || d.Complete {
			continue
		}
		if d.Status != StatusDownloading && d.Status != StatusReceivingMissing {
			continue
		}
		if !d.LastActivity.IsZero() && now.Sub(d.LastActivity) >= idle {
			// An unanswered RECV MISSING request is allowed to age out so the
			// bounded retry policy can issue another recovery round.
			d.MissingRequestPending = false
			out = append(out, snapshotDownload(d))
		}
	}
	return out
}

func (m *Manager) CancelDownload(fileID string) {
	m.mu.Lock()
	if d := m.downloads[fileID]; d != nil && d.VerifiedTempPath != "" {
		_ = os.Remove(d.VerifiedTempPath)
	}
	delete(m.downloads, fileID)
	m.mu.Unlock()
}

func (m *Manager) Reset() {
	m.mu.Lock()
	for _, d := range m.downloads {
		if d.VerifiedTempPath != "" {
			_ = os.Remove(d.VerifiedTempPath)
		}
	}
	m.uploads = nil
	m.activeUploads = map[string]*uploadSession{}
	m.downloads = map[string]*download{}
	m.mu.Unlock()
}

func (m *Manager) HasResumableDownloads() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.downloads {
		if !d.Complete && d.Status != StatusExpired && d.Status != StatusUnauthorized {
			return true
		}
	}
	return false
}

func snapshotDownload(d *download) DownloadSnapshot {
	return DownloadSnapshot{
		FileID: d.FileID, Filename: d.Filename, MIME: d.MIME, SavePath: d.SavePath,
		Size: d.Size, ChunkSize: d.ChunkSize, TotalParts: d.TotalParts,
		ReceivedBytes: d.ReceivedBytes, ReceivedParts: d.ReceivedParts,
		ExpectedSHA256: d.ExpectedSHA256, VerifiedSHA256: d.VerifiedSHA256, EndSeen: d.EndSeen, Complete: d.Complete,
		Status: d.Status, RetransmissionRounds: d.RetransmissionRounds, LastActivity: d.LastActivity,
	}
}

func chunkResult(d *download, duplicate bool) ChunkResult {
	return ChunkResult{Duplicate: duplicate, ReceivedBytes: d.ReceivedBytes, ReceivedParts: d.ReceivedParts, TotalParts: d.TotalParts, TotalSize: d.Size}
}

func resetDownloadChunks(d *download) {
	if d.VerifiedTempPath != "" {
		_ = os.Remove(d.VerifiedTempPath)
		d.VerifiedTempPath = ""
	}
	d.Parts = nil
	d.Received = nil
	d.ReceivedBytes = 0
	d.ReceivedParts = 0
	d.ChunkSize = 0
	d.TotalParts = 0
	d.ExpectedSHA256 = ""
	d.VerifiedSHA256 = ""
	d.EndSeen = false
	d.Complete = false
	d.MissingRequestPending = false
	d.RetransmissionRounds = 0
}

func missingRangesLocked(d *download) string {
	if !d.MetadataReady || d.TotalParts == 0 {
		return ""
	}
	missing := make([]int, 0)
	for i, got := range d.Received {
		if !got {
			missing = append(missing, i)
		}
	}
	return FormatRanges(missing)
}

func FormatRanges(parts []int) string {
	if len(parts) == 0 {
		return ""
	}
	parts = append([]int(nil), parts...)
	sort.Ints(parts)
	uniq := parts[:0]
	for _, p := range parts {
		if p < 0 || (len(uniq) > 0 && uniq[len(uniq)-1] == p) {
			continue
		}
		uniq = append(uniq, p)
	}
	var b strings.Builder
	for i := 0; i < len(uniq); {
		start := uniq[i]
		end := start
		j := i + 1
		for j < len(uniq) && uniq[j] == end+1 {
			end = uniq[j]
			j++
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		if start == end {
			b.WriteString(strconv.Itoa(start))
		} else {
			fmt.Fprintf(&b, "%d-%d", start, end)
		}
		i = j
	}
	return b.String()
}

func ParseRanges(raw string, maxParts int) ([]int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty part range")
	}
	seen := map[int]bool{}
	var out []int
	for _, token := range strings.Split(raw, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			return nil, errors.New("invalid empty part range")
		}
		if strings.Contains(token, "-") {
			loRaw, hiRaw, ok := strings.Cut(token, "-")
			if !ok || loRaw == "" || hiRaw == "" {
				return nil, errors.New("invalid part range")
			}
			lo, err1 := strconv.Atoi(loRaw)
			hi, err2 := strconv.Atoi(hiRaw)
			if err1 != nil || err2 != nil || lo < 0 || hi < lo {
				return nil, errors.New("invalid part range")
			}
			if maxParts > 0 && hi >= maxParts {
				return nil, errors.New("part range exceeds upload")
			}
			for p := lo; p <= hi; p++ {
				if !seen[p] {
					seen[p] = true
					out = append(out, p)
				}
			}
			continue
		}
		p, err := strconv.Atoi(token)
		if err != nil || p < 0 || (maxParts > 0 && p >= maxParts) {
			return nil, errors.New("invalid part number")
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Ints(out)
	return out, nil
}
