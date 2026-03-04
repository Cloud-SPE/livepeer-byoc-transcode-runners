package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	transcode "transcode-core"
)

// ── Configuration ──

var (
	runnerAddr   = env("RUNNER_ADDR", ":8080")
	maxStreams   = envInt("MAX_STREAMS", 0) // 0 = use GPU MaxSessions
	restartLimit = envInt("RESTART_LIMIT", 3)
)

// ── Global state ──

var (
	streams       = make(map[string]*Stream)
	streamsMu     sync.RWMutex
	activeStreams atomic.Int32
	hw            transcode.HWProfile
	maxConcurrent int // resolved at startup from maxStreams or hw.MaxSessions
)

// ── Request / Response types ──

// StreamStartRequest matches the gateway->worker protocol from PyTrickle.
type StreamStartRequest struct {
	SubscribeURL     string          `json:"subscribe_url"`
	PublishURL       string          `json:"publish_url"`
	ControlURL       string          `json:"control_url"`
	EventsURL        string          `json:"events_url"`
	DataURL          string          `json:"data_url,omitempty"`
	GatewayRequestID string          `json:"gateway_request_id"`
	Params           json.RawMessage `json:"params"`
}

// TranscodeParams — parsed from StreamStartRequest.Params.
type TranscodeParams struct {
	VideoCodec   string `json:"video_codec"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
	Bitrate      string `json:"bitrate"`
	MaxRate      string `json:"max_rate,omitempty"`
	BufSize      string `json:"buf_size,omitempty"`
	FPS          int    `json:"fps,omitempty"`
	AudioCodec   string `json:"audio_codec,omitempty"`
	AudioBitrate string `json:"audio_bitrate,omitempty"`
}

// StreamStatusResponse is returned by the status endpoint.
type StreamStatusResponse struct {
	StreamID      string  `json:"stream_id"`
	Status        string  `json:"status"` // running, stopped, error
	EncodingFPS   float64 `json:"encoding_fps,omitempty"`
	Uptime        float64 `json:"uptime_seconds"`
	Restarts      int     `json:"restarts"`
	GPU           string  `json:"gpu,omitempty"`
	LastKeepalive string  `json:"last_keepalive,omitempty"`
}

// ── Stream ──

// Stream represents an active live transcode session.
type Stream struct {
	mu            sync.Mutex
	ID            string
	Status        string // running, stopped, error
	Params        transcode.LiveTranscodeParams
	RawParams     TranscodeParams
	SubscribeURL  string
	PublishURL    string
	ControlURL    string
	EventsURL     string
	EncodingFPS   float64
	Restarts      int
	StartedAt     time.Time
	LastKeepalive time.Time
	PendingParams json.RawMessage // set by control channel for mid-stream updates
	cancel        context.CancelFunc
	done          chan struct{}
	events        chan Event
}

// ── HTTP Handlers ──

func handleStreamStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read request body"})
		return
	}

	var req StreamStartRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	// Validate required fields
	if req.SubscribeURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "subscribe_url is required"})
		return
	}
	if req.PublishURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "publish_url is required"})
		return
	}
	if req.Params == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "params is required"})
		return
	}

	// Parse transcode params
	var params TranscodeParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid params: " + err.Error()})
		return
	}

	if params.VideoCodec == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "params.video_codec is required"})
		return
	}
	if params.Bitrate == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "params.bitrate is required"})
		return
	}

	// Check capacity
	current := int(activeStreams.Load())
	if maxConcurrent > 0 && current >= maxConcurrent {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"error":          "server at capacity",
			"active_streams": strconv.Itoa(current),
			"max_streams":    strconv.Itoa(maxConcurrent),
		})
		return
	}

	// Generate stream ID
	streamID := generateStreamID()
	if req.GatewayRequestID != "" {
		streamID = req.GatewayRequestID
	}

	// Convert to transcode-core params
	liveParams := transcode.LiveTranscodeParams{
		VideoCodec:   params.VideoCodec,
		Width:        params.Width,
		Height:       params.Height,
		Bitrate:      params.Bitrate,
		MaxRate:      params.MaxRate,
		BufSize:      params.BufSize,
		FPS:          params.FPS,
		AudioCodec:   params.AudioCodec,
		AudioBitrate: params.AudioBitrate,
	}

	ctx, cancel := context.WithCancel(context.Background())
	stream := &Stream{
		ID:           streamID,
		Status:       "running",
		Params:       liveParams,
		RawParams:    params,
		SubscribeURL: req.SubscribeURL,
		PublishURL:   req.PublishURL,
		ControlURL:   req.ControlURL,
		EventsURL:    req.EventsURL,
		StartedAt:    time.Now(),
		cancel:       cancel,
		done:         make(chan struct{}),
		events:       make(chan Event, 16),
	}

	streamsMu.Lock()
	streams[streamID] = stream
	streamsMu.Unlock()

	activeStreams.Add(1)
	go func() {
		defer func() {
			activeStreams.Add(-1)
			close(stream.done)
		}()
		runStream(ctx, stream, hw)
	}()

	log.Printf("[stream %s] started: codec=%s %dx%d @ %s",
		streamID, params.VideoCodec, params.Width, params.Height, params.Bitrate)

	writeJSON(w, http.StatusOK, map[string]string{
		"stream_id": streamID,
		"status":    "running",
	})
}

func handleStreamStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	streamID := extractStreamID(r)
	if streamID == "" {
		// Try reading from body
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req struct {
			StreamID string `json:"stream_id"`
		}
		json.Unmarshal(body, &req)
		streamID = req.StreamID
	}

	if streamID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "stream_id is required"})
		return
	}

	streamsMu.RLock()
	stream, ok := streams[streamID]
	streamsMu.RUnlock()

	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "stream not found"})
		return
	}

	stream.cancel()
	// Wait for cleanup with timeout
	select {
	case <-stream.done:
	case <-time.After(10 * time.Second):
		log.Printf("[stream %s] stop timeout, forcing", streamID)
	}

	stream.mu.Lock()
	stream.Status = "stopped"
	stream.mu.Unlock()

	log.Printf("[stream %s] stopped", streamID)

	writeJSON(w, http.StatusOK, map[string]string{
		"stream_id": streamID,
		"status":    "stopped",
	})
}

func handleStreamParams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read request body"})
		return
	}

	var req struct {
		StreamID string          `json:"stream_id"`
		Params   json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	streamID := req.StreamID
	if streamID == "" {
		streamID = extractStreamID(r)
	}

	if streamID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "stream_id is required"})
		return
	}

	streamsMu.RLock()
	stream, ok := streams[streamID]
	streamsMu.RUnlock()

	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "stream not found"})
		return
	}

	// Parse and validate new params
	var params TranscodeParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid params: " + err.Error()})
		return
	}

	// Update params (will trigger ffmpeg restart on next iteration)
	stream.mu.Lock()
	stream.PendingParams = req.Params
	stream.mu.Unlock()

	log.Printf("[stream %s] params update queued", streamID)

	writeJSON(w, http.StatusOK, map[string]string{
		"stream_id": streamID,
		"status":    "params_queued",
	})
}

func handleStreamStatus(w http.ResponseWriter, r *http.Request) {
	streamID := extractStreamID(r)
	if streamID == "" {
		// Try reading from body for POST
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			var req struct {
				StreamID string `json:"stream_id"`
			}
			json.Unmarshal(body, &req)
			streamID = req.StreamID
		}
	}

	if streamID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "stream_id is required"})
		return
	}

	streamsMu.RLock()
	stream, ok := streams[streamID]
	streamsMu.RUnlock()

	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "stream not found"})
		return
	}

	stream.mu.Lock()
	resp := StreamStatusResponse{
		StreamID:    stream.ID,
		Status:      stream.Status,
		EncodingFPS: stream.EncodingFPS,
		Uptime:      time.Since(stream.StartedAt).Seconds(),
		Restarts:    stream.Restarts,
		GPU:         hw.GPUName,
	}
	if !stream.LastKeepalive.IsZero() {
		resp.LastKeepalive = stream.LastKeepalive.UTC().Format(time.RFC3339)
	}
	stream.mu.Unlock()

	writeJSON(w, http.StatusOK, resp)
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":         "ok",
		"gpu":            hw.GPUName,
		"vram_mb":        hw.VRAM_MB,
		"active_streams": activeStreams.Load(),
		"max_streams":    maxConcurrent,
	})
}

// ── Stream Worker ──

func runStream(ctx context.Context, stream *Stream, hw transcode.HWProfile) {
	// Start events publisher
	go eventsPublish(ctx, stream.EventsURL, stream.events)

	// Start control subscriber if URL is provided
	if stream.ControlURL != "" {
		go controlSubscribe(ctx, stream.ControlURL, stream)
	}

	for attempt := 0; attempt <= restartLimit; attempt++ {
		if ctx.Err() != nil {
			return
		}

		// Check for pending params update
		stream.mu.Lock()
		if stream.PendingParams != nil {
			var params TranscodeParams
			if err := json.Unmarshal(stream.PendingParams, &params); err == nil {
				stream.Params = transcode.LiveTranscodeParams{
					VideoCodec:   params.VideoCodec,
					Width:        params.Width,
					Height:       params.Height,
					Bitrate:      params.Bitrate,
					MaxRate:      params.MaxRate,
					BufSize:      params.BufSize,
					FPS:          params.FPS,
					AudioCodec:   params.AudioCodec,
					AudioBitrate: params.AudioBitrate,
				}
				stream.RawParams = params
				log.Printf("[stream %s] params updated: codec=%s %dx%d @ %s",
					stream.ID, params.VideoCodec, params.Width, params.Height, params.Bitrate)
			}
			stream.PendingParams = nil
		}
		currentParams := stream.Params
		stream.mu.Unlock()

		cmd := transcode.LiveTranscodeCmd(currentParams, hw)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			log.Printf("[stream %s] stdin pipe error: %v", stream.ID, err)
			stream.mu.Lock()
			stream.Status = "error"
			stream.mu.Unlock()
			return
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			log.Printf("[stream %s] stdout pipe error: %v", stream.ID, err)
			stdin.Close()
			stream.mu.Lock()
			stream.Status = "error"
			stream.mu.Unlock()
			return
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			log.Printf("[stream %s] stderr pipe error: %v", stream.ID, err)
			stdin.Close()
			stdout.Close()
			stream.mu.Lock()
			stream.Status = "error"
			stream.mu.Unlock()
			return
		}

		if err := cmd.Start(); err != nil {
			log.Printf("[stream %s] ffmpeg start error: %v", stream.ID, err)
			stream.mu.Lock()
			stream.Status = "error"
			stream.mu.Unlock()
			return
		}

		log.Printf("[stream %s] ffmpeg started (attempt %d/%d), args: %s",
			stream.ID, attempt+1, restartLimit+1, strings.Join(cmd.Args, " "))

		// Launch I/O goroutines
		go trickleSubscribe(ctx, stream.SubscribeURL, stdin)
		go tricklePublish(ctx, stream.PublishURL, stdout)
		go monitorFFmpeg(stream, stderr)

		// Wait for ffmpeg to exit
		cmdErr := cmd.Wait()

		if ctx.Err() != nil {
			// Graceful stop
			log.Printf("[stream %s] stopped gracefully", stream.ID)
			return
		}

		if cmdErr != nil {
			log.Printf("[stream %s] ffmpeg exited with error: %v", stream.ID, cmdErr)
		}

		// Check for pending params (restart with new params, not counted as failure)
		stream.mu.Lock()
		hasPending := stream.PendingParams != nil
		if !hasPending {
			stream.Restarts++
		}
		stream.mu.Unlock()

		if hasPending {
			log.Printf("[stream %s] restarting ffmpeg with new params", stream.ID)
			continue
		}

		// Backoff before restart
		backoff := time.Duration(attempt+1) * 2 * time.Second
		log.Printf("[stream %s] restarting in %v (attempt %d/%d)", stream.ID, backoff, attempt+1, restartLimit)

		// Send error event
		select {
		case stream.events <- Event{
			Type:    "error",
			Message: fmt.Sprintf("ffmpeg crashed, restarting (attempt %d/%d)", attempt+1, restartLimit),
		}:
		default:
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
	}

	// All restarts exhausted
	stream.mu.Lock()
	stream.Status = "error"
	stream.mu.Unlock()
	log.Printf("[stream %s] max restarts exhausted", stream.ID)

	select {
	case stream.events <- Event{Type: "error", Message: "max restarts exhausted"}:
	default:
	}
}

// monitorFFmpeg reads ffmpeg stderr and updates stream metrics.
func monitorFFmpeg(stream *Stream, stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	scanner.Split(scanFFmpegLines)
	for scanner.Scan() {
		line := scanner.Text()
		if info, ok := transcode.ParseProgressLine(line); ok {
			stream.mu.Lock()
			stream.EncodingFPS = info.FPS
			stream.mu.Unlock()
		}
	}
}

// scanFFmpegLines splits on \r or \n (ffmpeg uses \r for progress).
func scanFFmpegLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// ── Helpers ──

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func generateStreamID() string {
	return fmt.Sprintf("ls-%d-%s", time.Now().UnixMilli(), randomHex(4))
}

func randomHex(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(time.Now().UnixNano() >> (i * 8))
	}
	result := make([]byte, n*2)
	const hextable = "0123456789abcdef"
	for i, v := range b {
		result[i*2] = hextable[v>>4]
		result[i*2+1] = hextable[v&0x0f]
	}
	return string(result)
}

// extractStreamID extracts a stream ID from the URL path.
// Expects /stream/{id}/... pattern, skipping known action segments.
func extractStreamID(r *http.Request) string {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	reserved := map[string]bool{"start": true, "stop": true, "params": true, "status": true}
	for i, p := range parts {
		if p == "stream" && i+1 < len(parts) {
			candidate := parts[i+1]
			if !reserved[candidate] {
				return candidate
			}
		}
	}
	return ""
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// ── Cleanup ──

func cleanupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		streamsMu.Lock()
		for id, stream := range streams {
			stream.mu.Lock()
			if stream.Status == "stopped" || stream.Status == "error" {
				// Remove streams that have been done for more than 5 minutes
				select {
				case <-stream.done:
					delete(streams, id)
					log.Printf("[cleanup] removed stream %s (status: %s)", id, stream.Status)
				default:
				}
			}
			stream.mu.Unlock()
		}
		streamsMu.Unlock()
	}
}

// ── Main ──

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("live-transcode-runner starting...")

	// Detect GPU
	hw = transcode.DetectGPU()
	if hw.IsGPUAvailable() {
		log.Printf("GPU detected: %s (%d MB VRAM)", hw.GPUName, hw.VRAM_MB)
		log.Printf("  Encoders: %s", strings.Join(hw.Encoders, ", "))
		log.Printf("  Decoders: %s", strings.Join(hw.Decoders, ", "))
		log.Printf("  HW Accels: %s", strings.Join(hw.HWAccels, ", "))
		if hw.MaxSessions > 0 {
			log.Printf("  Max concurrent sessions: %d", hw.MaxSessions)
		} else {
			log.Printf("  Max concurrent sessions: unlimited")
		}
	} else {
		log.Println("WARNING: No GPU detected — only software encoding will be available")
	}

	// Resolve max concurrent streams
	maxConcurrent = maxStreams
	if maxConcurrent <= 0 {
		maxConcurrent = hw.MaxSessions
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 5 // safe default
	}
	log.Printf("Max concurrent streams: %d", maxConcurrent)

	// Start cleanup goroutine
	go cleanupLoop()

	// HTTP routes
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/start", handleStreamStart)
	mux.HandleFunc("/stream/stop", handleStreamStop)
	mux.HandleFunc("/stream/params", handleStreamParams)
	mux.HandleFunc("/stream/status", handleStreamStatus)
	mux.HandleFunc("/healthz", handleHealthz)

	server := &http.Server{
		Addr:         runnerAddr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("Received %v, shutting down...", sig)

		// Stop all active streams
		streamsMu.RLock()
		for _, stream := range streams {
			stream.cancel()
		}
		streamsMu.RUnlock()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()

	log.Printf("Listening on %s", runnerAddr)
	log.Printf("Config: max_streams=%d, restart_limit=%d", maxConcurrent, restartLimit)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
	log.Println("Server stopped")
}
