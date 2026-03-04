package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"
)

// Trickle protocol constants matching PyTrickle / go-livepeer.
const (
	trickleInitialSeq   = -2
	trickleReadBufSize  = 32 * 1024 // 32KB chunks
	trickleMaxRetries   = 5
	trickleRetryDelay   = 500 * time.Millisecond
	headerTrickleSeq    = "Lp-Trickle-Seq"
	headerTrickleClosed = "Lp-Trickle-Closed"
	headerTrickleLatest = "Lp-Trickle-Latest"
	statusNoData        = 470
)

// trickleSubscribe reads MPEG-TS segments from a Trickle subscribe URL and writes them to dst.
// Implements: GET {url}/{seq}, starting at seq=-2, incrementing from Lp-Trickle-Seq header.
// Status 200=data, 404=stream gone, 470=no data (reset via Lp-Trickle-Latest header).
func trickleSubscribe(ctx context.Context, subscribeURL string, dst io.WriteCloser) {
	defer dst.Close()
	seq := trickleInitialSeq
	retries := 0

	for {
		if ctx.Err() != nil {
			return
		}

		url := fmt.Sprintf("%s/%d", subscribeURL, seq)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			log.Printf("[trickle-sub] request error: %v", err)
			return
		}
		req.Header.Set("Connection", "close")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			retries++
			if retries > trickleMaxRetries {
				log.Printf("[trickle-sub] max retries exceeded for seq %d", seq)
				return
			}
			log.Printf("[trickle-sub] fetch error (retry %d/%d): %v", retries, trickleMaxRetries, err)
			time.Sleep(trickleRetryDelay)
			continue
		}

		switch resp.StatusCode {
		case http.StatusOK:
			retries = 0
			// Stream data to dst in chunks
			buf := make([]byte, trickleReadBufSize)
			if _, err := io.CopyBuffer(dst, resp.Body, buf); err != nil {
				resp.Body.Close()
				if ctx.Err() != nil {
					return
				}
				log.Printf("[trickle-sub] write error: %v", err)
				return
			}
			resp.Body.Close()

			// Check for end-of-stream
			if resp.Header.Get(headerTrickleClosed) != "" {
				log.Printf("[trickle-sub] stream closed at seq %d", seq)
				return
			}

			// Advance sequence from response header
			if seqStr := resp.Header.Get(headerTrickleSeq); seqStr != "" {
				if nextSeq, err := strconv.Atoi(seqStr); err == nil {
					seq = nextSeq + 1
				} else {
					seq++
				}
			} else {
				seq++
			}

		case http.StatusNotFound:
			resp.Body.Close()
			log.Printf("[trickle-sub] stream gone (404) at seq %d", seq)
			return

		case statusNoData:
			resp.Body.Close()
			// Reset to latest available segment
			if latestStr := resp.Header.Get(headerTrickleLatest); latestStr != "" {
				if latest, err := strconv.Atoi(latestStr); err == nil {
					seq = latest
					log.Printf("[trickle-sub] no data, reset to latest seq %d", seq)
				}
			}
			retries++
			if retries > trickleMaxRetries {
				log.Printf("[trickle-sub] max retries exceeded (no data)")
				return
			}
			time.Sleep(trickleRetryDelay)

		default:
			resp.Body.Close()
			log.Printf("[trickle-sub] unexpected status %d at seq %d", resp.StatusCode, seq)
			retries++
			if retries > trickleMaxRetries {
				return
			}
			time.Sleep(trickleRetryDelay)
		}
	}
}

// tricklePublish reads MPEG-TS data from src and publishes segments to a Trickle publish URL.
// Implements: POST {url}/{idx}, starting at idx=0, incrementing after each segment.
// Each segment is sent as a chunked POST with Content-Type: video/mp4 and Connection: close.
func tricklePublish(ctx context.Context, publishURL string, src io.Reader) {
	idx := 0
	buf := make([]byte, trickleReadBufSize)

	for {
		if ctx.Err() != nil {
			return
		}

		// Read a chunk from ffmpeg stdout
		n, readErr := src.Read(buf)
		if n == 0 && readErr != nil {
			if readErr != io.EOF {
				log.Printf("[trickle-pub] read error: %v", readErr)
			}
			return
		}

		if n > 0 {
			url := fmt.Sprintf("%s/%d", publishURL, idx)
			// Create a reader from the chunk
			body := io.NopCloser(io.LimitReader(bytesReader(buf[:n]), int64(n)))

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
			if err != nil {
				log.Printf("[trickle-pub] request error: %v", err)
				return
			}
			req.Header.Set("Content-Type", "video/mp4")
			req.Header.Set("Connection", "close")
			req.ContentLength = int64(n)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("[trickle-pub] publish error at idx %d: %v", idx, err)
				return
			}
			resp.Body.Close()

			if resp.StatusCode >= 400 {
				log.Printf("[trickle-pub] publish returned %d at idx %d", resp.StatusCode, idx)
				return
			}

			idx++
		}

		if readErr == io.EOF {
			log.Printf("[trickle-pub] source EOF after %d segments", idx)
			return
		}
	}
}

// bytesReader returns a new io.Reader from a byte slice (avoids importing bytes in this file).
func bytesReader(data []byte) io.Reader {
	copied := make([]byte, len(data))
	copy(copied, data)
	return &sliceReader{data: copied}
}

type sliceReader struct {
	data []byte
	pos  int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// ControlMessage represents a message received on the control channel.
type ControlMessage struct {
	Type   string          `json:"type"`             // "keepalive", "params", "stop"
	Params json.RawMessage `json:"params,omitempty"` // updated params if type="params"
}

// controlSubscribe reads control messages from the gateway via the Trickle control channel.
// Receives keepalive (every ~10s) and params updates.
func controlSubscribe(ctx context.Context, controlURL string, stream *Stream) {
	seq := trickleInitialSeq
	retries := 0

	for {
		if ctx.Err() != nil {
			return
		}

		url := fmt.Sprintf("%s/%d", controlURL, seq)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			log.Printf("[control] request error: %v", err)
			return
		}
		req.Header.Set("Connection", "close")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			retries++
			if retries > trickleMaxRetries {
				log.Printf("[control] max retries exceeded")
				return
			}
			time.Sleep(trickleRetryDelay)
			continue
		}

		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			log.Printf("[control] channel gone (404)")
			return
		}

		if resp.StatusCode == statusNoData {
			resp.Body.Close()
			if latestStr := resp.Header.Get(headerTrickleLatest); latestStr != "" {
				if latest, err := strconv.Atoi(latestStr); err == nil {
					seq = latest
				}
			}
			time.Sleep(trickleRetryDelay)
			continue
		}

		if resp.StatusCode == http.StatusOK {
			retries = 0
			data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
			resp.Body.Close()
			if err != nil {
				log.Printf("[control] read error: %v", err)
				continue
			}

			// Check for end-of-stream
			if resp.Header.Get(headerTrickleClosed) != "" {
				log.Printf("[control] channel closed")
				return
			}

			// Parse control message
			var msg ControlMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				log.Printf("[control] parse error: %v", err)
			} else {
				switch msg.Type {
				case "keepalive":
					// Update last keepalive time
					stream.mu.Lock()
					stream.LastKeepalive = time.Now()
					stream.mu.Unlock()
				case "params":
					log.Printf("[control] params update received")
					stream.mu.Lock()
					stream.PendingParams = msg.Params
					stream.mu.Unlock()
				case "stop":
					log.Printf("[control] stop received")
					return
				}
			}

			// Advance sequence
			if seqStr := resp.Header.Get(headerTrickleSeq); seqStr != "" {
				if nextSeq, err := strconv.Atoi(seqStr); err == nil {
					seq = nextSeq + 1
				} else {
					seq++
				}
			} else {
				seq++
			}
		} else {
			resp.Body.Close()
			retries++
			if retries > trickleMaxRetries {
				return
			}
			time.Sleep(trickleRetryDelay)
		}
	}
}

// Event represents a status event sent to the gateway via the events channel.
type Event struct {
	Type    string  `json:"type"`              // "status", "error", "stats"
	Message string  `json:"message,omitempty"` // human-readable message
	FPS     float64 `json:"fps,omitempty"`     // current encoding FPS
	Uptime  float64 `json:"uptime,omitempty"`  // seconds since stream start
}

// eventsPublish sends status events to the gateway via the Trickle events channel.
func eventsPublish(ctx context.Context, eventsURL string, events <-chan Event) {
	idx := 0

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}

			data, err := json.Marshal(evt)
			if err != nil {
				log.Printf("[events] marshal error: %v", err)
				continue
			}

			url := fmt.Sprintf("%s/%d", eventsURL, idx)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytesReader(data))
			if err != nil {
				log.Printf("[events] request error: %v", err)
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Connection", "close")
			req.ContentLength = int64(len(data))

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("[events] publish error at idx %d: %v", idx, err)
				continue
			}
			resp.Body.Close()
			idx++
		}
	}
}
