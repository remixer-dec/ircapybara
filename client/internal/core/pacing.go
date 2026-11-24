package core

import (
	"fmt"
	"sync"
	"time"
)

// The server meters XFER chunks with a token bucket (a burst of 500, refilled
// at 250 per second) and disconnects a client that overdraws it. Uploads stay
// comfortably inside that budget; without pacing, any file larger than about
// 120 KB ended the session.
const (
	uploadChunkBurst = 400
	uploadChunkRate  = 200 // chunks per second
)

// chunkPacer is a token bucket shared by every upload on the connection, since
// the server meters them together.
type chunkPacer struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// wait blocks until one more chunk may be sent.
func (p *chunkPacer) wait() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if p.last.IsZero() {
		p.tokens = uploadChunkBurst
	} else {
		p.tokens = min(uploadChunkBurst, p.tokens+now.Sub(p.last).Seconds()*uploadChunkRate)
	}
	p.last = now
	if p.tokens < 1 {
		delay := time.Duration((1 - p.tokens) / uploadChunkRate * float64(time.Second))
		time.Sleep(delay)
		p.tokens = 1
		p.last = now.Add(delay)
	}
	p.tokens--
}

func (a *App) sendUploadChunk(fileID string) func(part int, b64 string) error {
	return func(part int, b64 string) error {
		a.uploadPace.wait()
		return a.send(fmt.Sprintf("XFER CHUNK %s %d %s", fileID, part, b64))
	}
}
