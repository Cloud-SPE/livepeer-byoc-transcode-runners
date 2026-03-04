package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	transcode "transcode-core"
)

func init() {
	// Set up test hardware profile (no actual GPU needed)
	hw = transcode.HWProfile{
		GPUName:     "Test GPU",
		Vendor:      transcode.VendorNVIDIA,
		Encoders:    []string{"h264_nvenc", "hevc_nvenc"},
		HWAccels:    []string{"cuda"},
		MaxSessions: 5,
	}
	maxConcurrent = 2
}

func TestHandleStreamStart_Valid(t *testing.T) {
	// Clean up state
	streamsMu.Lock()
	streams = make(map[string]*Stream)
	streamsMu.Unlock()
	activeStreams.Store(0)

	params := TranscodeParams{
		VideoCodec: "h264",
		Bitrate:    "4M",
		Width:      1920,
		Height:     1080,
	}
	paramsJSON, _ := json.Marshal(params)

	reqBody := StreamStartRequest{
		SubscribeURL:     "http://gateway:9935/trickle/sub/123",
		PublishURL:       "http://gateway:9935/trickle/pub/456",
		ControlURL:       "http://gateway:9935/trickle/ctrl/789",
		EventsURL:        "http://gateway:9935/trickle/evt/abc",
		GatewayRequestID: "test-stream-1",
		Params:           paramsJSON,
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/stream/start", bytes.NewReader(body))
	w := httptest.NewRecorder()

	handleStreamStart(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp["stream_id"] != "test-stream-1" {
		t.Errorf("expected stream_id=test-stream-1, got %s", resp["stream_id"])
	}
	if resp["status"] != "running" {
		t.Errorf("expected status=running, got %s", resp["status"])
	}

	// Verify stream was created
	streamsMu.RLock()
	stream, ok := streams["test-stream-1"]
	streamsMu.RUnlock()
	if !ok {
		t.Fatal("stream not found in map")
	}

	// Clean up: stop the stream
	stream.cancel()
	<-stream.done

	streamsMu.Lock()
	delete(streams, "test-stream-1")
	streamsMu.Unlock()
}

func TestHandleStreamStart_MissingFields(t *testing.T) {
	tests := []struct {
		name string
		body StreamStartRequest
	}{
		{
			name: "missing subscribe_url",
			body: StreamStartRequest{
				PublishURL: "http://example.com/pub",
				Params:     json.RawMessage(`{"video_codec":"h264","bitrate":"4M"}`),
			},
		},
		{
			name: "missing publish_url",
			body: StreamStartRequest{
				SubscribeURL: "http://example.com/sub",
				Params:       json.RawMessage(`{"video_codec":"h264","bitrate":"4M"}`),
			},
		},
		{
			name: "missing params",
			body: StreamStartRequest{
				SubscribeURL: "http://example.com/sub",
				PublishURL:   "http://example.com/pub",
			},
		},
		{
			name: "missing video_codec",
			body: StreamStartRequest{
				SubscribeURL: "http://example.com/sub",
				PublishURL:   "http://example.com/pub",
				Params:       json.RawMessage(`{"bitrate":"4M"}`),
			},
		},
		{
			name: "missing bitrate",
			body: StreamStartRequest{
				SubscribeURL: "http://example.com/sub",
				PublishURL:   "http://example.com/pub",
				Params:       json.RawMessage(`{"video_codec":"h264"}`),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.body)
			req := httptest.NewRequest(http.MethodPost, "/stream/start", bytes.NewReader(body))
			w := httptest.NewRecorder()

			handleStreamStart(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestHandleStreamStart_AtCapacity(t *testing.T) {
	// Clean up state
	streamsMu.Lock()
	streams = make(map[string]*Stream)
	streamsMu.Unlock()
	activeStreams.Store(int32(maxConcurrent))

	params := json.RawMessage(`{"video_codec":"h264","bitrate":"4M"}`)
	reqBody := StreamStartRequest{
		SubscribeURL: "http://example.com/sub",
		PublishURL:   "http://example.com/pub",
		Params:       params,
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/stream/start", bytes.NewReader(body))
	w := httptest.NewRecorder()

	handleStreamStart(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d: %s", w.Code, w.Body.String())
	}

	// Reset
	activeStreams.Store(0)
}

func TestHandleStreamStop_Valid(t *testing.T) {
	// Create a test stream
	streamsMu.Lock()
	streams = make(map[string]*Stream)
	done := make(chan struct{})
	close(done)
	streams["stop-test"] = &Stream{
		ID:     "stop-test",
		Status: "running",
		cancel: func() {},
		done:   done,
		events: make(chan Event, 16),
	}
	streamsMu.Unlock()

	body, _ := json.Marshal(map[string]string{"stream_id": "stop-test"})
	req := httptest.NewRequest(http.MethodPost, "/stream/stop", bytes.NewReader(body))
	w := httptest.NewRecorder()

	handleStreamStop(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Clean up
	streamsMu.Lock()
	delete(streams, "stop-test")
	streamsMu.Unlock()
}

func TestHandleStreamStop_NotFound(t *testing.T) {
	streamsMu.Lock()
	streams = make(map[string]*Stream)
	streamsMu.Unlock()

	body, _ := json.Marshal(map[string]string{"stream_id": "nonexistent"})
	req := httptest.NewRequest(http.MethodPost, "/stream/stop", bytes.NewReader(body))
	w := httptest.NewRecorder()

	handleStreamStop(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleStreamStatus_Valid(t *testing.T) {
	streamsMu.Lock()
	streams = make(map[string]*Stream)
	done := make(chan struct{})
	streams["status-test"] = &Stream{
		ID:        "status-test",
		Status:    "running",
		StartedAt: time.Now(),
		cancel:    func() {},
		done:      done,
		events:    make(chan Event, 16),
	}
	streamsMu.Unlock()

	body, _ := json.Marshal(map[string]string{"stream_id": "status-test"})
	req := httptest.NewRequest(http.MethodPost, "/stream/status", bytes.NewReader(body))
	w := httptest.NewRecorder()

	handleStreamStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp StreamStatusResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.StreamID != "status-test" {
		t.Errorf("expected stream_id=status-test, got %s", resp.StreamID)
	}
	if resp.Status != "running" {
		t.Errorf("expected status=running, got %s", resp.Status)
	}
	if resp.GPU != "Test GPU" {
		t.Errorf("expected gpu=Test GPU, got %s", resp.GPU)
	}

	// Clean up
	streamsMu.Lock()
	delete(streams, "status-test")
	streamsMu.Unlock()
}

func TestHandleStreamParams_Valid(t *testing.T) {
	streamsMu.Lock()
	streams = make(map[string]*Stream)
	done := make(chan struct{})
	streams["params-test"] = &Stream{
		ID:     "params-test",
		Status: "running",
		cancel: func() {},
		done:   done,
		events: make(chan Event, 16),
	}
	streamsMu.Unlock()

	newParams := TranscodeParams{
		VideoCodec: "hevc",
		Bitrate:    "6M",
		Width:      3840,
		Height:     2160,
	}
	paramsJSON, _ := json.Marshal(newParams)

	body, _ := json.Marshal(map[string]interface{}{
		"stream_id": "params-test",
		"params":    json.RawMessage(paramsJSON),
	})
	req := httptest.NewRequest(http.MethodPost, "/stream/params", bytes.NewReader(body))
	w := httptest.NewRecorder()

	handleStreamParams(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify pending params were set
	streamsMu.RLock()
	stream := streams["params-test"]
	streamsMu.RUnlock()

	stream.mu.Lock()
	if stream.PendingParams == nil {
		t.Error("expected PendingParams to be set")
	}
	stream.mu.Unlock()

	// Clean up
	streamsMu.Lock()
	delete(streams, "params-test")
	streamsMu.Unlock()
}

func TestHandleHealthz(t *testing.T) {
	activeStreams.Store(0)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	handleHealthz(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp["status"] != "ok" {
		t.Errorf("expected status=ok, got %v", resp["status"])
	}
	if resp["gpu"] != "Test GPU" {
		t.Errorf("expected gpu=Test GPU, got %v", resp["gpu"])
	}
}

func TestHandleStreamStart_MethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/stream/start", nil)
	w := httptest.NewRecorder()

	handleStreamStart(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}
