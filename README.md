# Livepeer BYOC Transcode Runners

GPU-accelerated video transcoding services for the [Livepeer](https://livepeer.org) BYOC (Bring Your Own Cloud) network. Operators run these containers on their own GPU hardware to earn revenue by processing video transcoding jobs dispatched by the Livepeer protocol.

---

## Table of Contents

- [Overview](#overview)
- [Architecture](#architecture)
- [Runners](#runners)
  - [transcode-runner](#transcode-runner-vod)
  - [abr-runner](#abr-runner-abr-ladder)
  - [live-transcode-runner](#live-transcode-runner-live-streaming)
- [Shared Library: transcode-core](#shared-library-transcode-core)
- [Codec Support](#codec-support)
- [GPU Hardware Support](#gpu-hardware-support)
- [Build System](#build-system)
- [Deployment](#deployment)
- [Configuration](#configuration)
- [Presets](#presets)
- [API Reference](#api-reference)
- [Webhooks](#webhooks)
- [Project Structure](#project-structure)

---

## Overview

This repo contains three GPU-accelerated Docker services that plug into the Livepeer BYOC protocol:

| Runner | Purpose | BYOC Model | Capability Name |
|--------|---------|------------|-----------------|
| `transcode-runner` | Single-output VOD transcoding | Job | `video-transcode` |
| `abr-runner` | Multi-rendition ABR ladder | Job | `transcode-abr` |
| `live-transcode-runner` | Real-time live transcoding | Stream | `transcode-live` |

**How it works:**

1. An operator runs one or more runner containers on GPU hardware
2. A `register-capability` sidecar registers the runner with a Livepeer orchestrator
3. The orchestrator dispatches jobs from the Livepeer network to the runner
4. The runner transcodes the video using FFmpeg with GPU acceleration (NVENC/QSV/VAAPI)
5. Output is uploaded directly to object storage via pre-signed URLs — no intermediary

---

## Architecture

### I/O Model

All runners use an **HTTP pull + pre-signed PUT URL** pattern:

```
                          ┌─────────────────────┐
                          │   Object Storage     │
                          │   (S3/R2/GCS/MinIO)  │
                          └──────────┬──────────┘
                                     │
                    ┌────────────────▼─────────────────┐
                    │         Client / Gateway          │
                    │  1. Generates pre-signed URLs     │
                    │  2. Submits job to runner         │
                    └────────────────┬─────────────────┘
                                     │ POST /v1/video/transcode
                    ┌────────────────▼─────────────────┐
                    │         Transcode Runner          │
                    │  1. Download input (HTTP GET)     │
                    │  2. Probe with ffprobe            │
                    │  3. GPU transcode with ffmpeg     │
                    │  4. Upload output (HTTP PUT)      │
                    └──────────────────────────────────┘
```

- **Input:** Runner fetches the source file via HTTP(S) URL (e.g., a CDN link or pre-signed GET URL)
- **Output:** Runner uploads directly to object storage via pre-signed PUT URL(s)
- **No credentials are passed to the runner** — only time-limited pre-signed URLs

### BYOC Protocol Models

**Job Model** (`transcode-runner`, `abr-runner`):
- Client submits a job (`POST /v1/video/transcode`) and gets back a `job_id`
- Client polls for status (`POST /v1/video/transcode/status`) or receives webhook callbacks
- Job lifecycle: `queued → downloading → probing → encoding → uploading → complete`

**Stream Model** (`live-transcode-runner`):
- Gateway opens a persistent Trickle channel (subscribe URL → publish URL)
- Runner runs FFmpeg as a long-lived subprocess connected to the Trickle channel
- Bidirectional streaming over HTTP/2-compatible chunked transfer

### Multi-Stage Docker Build Strategy

All Dockerfiles use multi-stage builds with named `--target` stages, one per GPU vendor. A single Dockerfile produces three independent images:

```
docker build --target runtime-nvidia  # NVIDIA CUDA/NVENC
docker build --target runtime-intel   # Intel QSV/oneVPL
docker build --target runtime-amd     # AMD VAAPI
```

A shared `codecs-builder` image is built once and referenced as a build stage in all runner Dockerfiles — most codec libraries are compiled from source there and copied in. x265 is installed from the Ubuntu package manager directly in the ffmpeg build stages (the cmake-based source build does not produce a shared library reliably on Ubuntu 24.04).

```
codecs-builder image
  └── x264, SVT-AV1, libopus, libvpx, libzimg (compiled from source)
        │
        ├── ffmpeg-nvidia  (CUDA/NVENC build)
        ├── ffmpeg-intel   (QSV/oneVPL build)
        └── ffmpeg-amd     (VAAPI build)
              │
              └── runtime-nvidia / runtime-intel / runtime-amd
                    └── Go binary + FFmpeg binaries + codec libs
```

---

## Runners

### transcode-runner (VOD)

Single-input, single-output GPU transcoding for VOD content.

**Features:**
- H.264, HEVC, AV1 encoding (GPU-accelerated where supported)
- Subtitle burn-in (SRT/ASS)
- Watermark/logo overlay
- HDR-to-SDR tone mapping (auto-detected)
- Thumbnail extraction
- Container remuxing (copy mode, no re-encode)
- Webhook callbacks (HMAC-SHA256 signed)
- Preset-based configuration (YAML, embedded in binary)

**Environment variables:**

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNNER_ADDR` | `:8080` | Listen address |
| `MAX_QUEUE_SIZE` | `5` | Max concurrent jobs |
| `TEMP_DIR` | `/tmp/transcode` | Working directory for job files |
| `JOB_TTL_SECONDS` | `3600` | How long completed job records are kept |
| `PRESETS_FILE` | _(embedded)_ | Path to custom presets YAML (overrides built-in) |

**Endpoints:**

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/v1/video/transcode` | Submit a transcode job |
| `POST` | `/v1/video/transcode/status` | Poll job status |
| `GET` | `/v1/video/transcode/presets` | List available presets |
| `GET` | `/healthz` | Health check |

---

### abr-runner (ABR Ladder)

Multi-rendition ABR ladder generation with fMP4 byte-range HLS output.

**Features:**
- Sequential per-rendition encoding (minimizes peak GPU memory)
- Progressive upload: each rendition uploaded as it completes
- HLS master manifest generated after all renditions complete
- fMP4 single-file output with byte-range manifest (~9 files for 4 renditions vs thousands of segments)
- Per-rendition webhook callbacks (`job.rendition.complete`)

**Output format (fMP4 byte-range HLS):**

```
master.m3u8            ← HLS master manifest
1080p/
  playlist.m3u8        ← Per-rendition playlist (byte-range references)
  stream.mp4           ← Single fMP4 file (all segments)
720p/
  playlist.m3u8
  stream.mp4
...
```

This approach uses the same pre-signed URL model as single-output transcoding — one URL per file, no credentials needed.

**Environment variables:**

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNNER_ADDR` | `:8080` | Listen address |
| `MAX_QUEUE_SIZE` | `2` | Max concurrent ABR jobs (each uses multiple GPU sessions) |
| `TEMP_DIR` | `/tmp/abr` | Working directory |
| `JOB_TTL_SECONDS` | `3600` | Job record TTL |

**Endpoints:**

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/v1/video/transcode/abr` | Submit an ABR job |
| `POST` | `/v1/video/transcode/abr/status` | Poll ABR job status |
| `GET` | `/healthz` | Health check |

---

### live-transcode-runner (Live Streaming)

Real-time single-rendition live transcoding via the BYOC Stream Model and Trickle protocol. NVIDIA CUDA/NVENC only.

**Features:**
- FFmpeg subprocess with Trickle channel I/O (subscribe → transcode → publish)
- Mid-stream parameter changes via `stream/params`
- Auto-restart on FFmpeg crash (configurable limit)
- Graceful shutdown on `stream/stop`

**Environment variables:**

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNNER_ADDR` | `:8080` | Listen address |
| `MAX_STREAMS` | `0` | Max concurrent live streams (0 = use GPU session limit) |
| `RESTART_LIMIT` | `3` | Max FFmpeg restart attempts per stream |

---

## Shared Library: transcode-core

Go package (`transcode-core/`) shared across all runners. Provides:

| File | Purpose |
|------|---------|
| `gpu.go` | GPU detection (NVENC/QSV/VAAPI), hardware profile, encoder/decoder enumeration |
| `ffmpeg.go` | FFmpeg command builder for transcode, probe, thumbnail |
| `presets.go` | Preset loader (YAML), validation against GPU capabilities |
| `abr_presets.go` | ABR ladder preset definitions |
| `io.go` | HTTP download (with progress) and pre-signed PUT upload |
| `progress.go` | FFmpeg stderr progress parser (FPS, speed, time position) |
| `filters.go` | FFmpeg filter graph builders (subtitles, watermark, tone map, scale) |
| `hls.go` | HLS manifest generation for ABR output |
| `live.go` | Trickle channel client for live streaming |
| `thumbnail.go` | Thumbnail and sprite sheet extraction helpers |

---

## Codec Support

### Video Codecs

| Codec | GPU Acceleration | Notes |
|-------|-----------------|-------|
| H.264 / AVC | NVIDIA NVENC, Intel QSV, AMD VAAPI | Universally supported |
| H.265 / HEVC | NVIDIA NVENC, Intel QSV, AMD VAAPI | ~40% smaller than H.264 |
| AV1 | NVIDIA Ada+ (RTX 4000+), Intel Arc/12th+, AMD RDNA3+ | Best compression; newer GPUs only |

### Audio Codecs

| Codec | Notes |
|-------|-------|
| AAC | Primary audio codec; encoded by FFmpeg (CPU) |
| Opus | Supported via libopus |
| Copy | Passthrough (no re-encode) |

### FFmpeg Build

FFmpeg is compiled from source in each Docker image to ensure the correct GPU-accelerated encoders/decoders are available:

| Component | Version | Source |
|-----------|---------|--------|
| FFmpeg | n7.1.3 | github.com/FFmpeg/FFmpeg |
| nv-codec-headers | n12.2.72.0 | github.com/FFmpeg/nv-codec-headers |
| x264 | stable | github.com/mirror/x264 (built from source) |
| x265 | 3.5 | Ubuntu 24.04 package `libx265-dev` / `libx265-199` |
| SVT-AV1 | v2.3.0 | gitlab.com/AOMediaCodec/SVT-AV1 (built from source) |
| libopus | v1.5.2 | github.com/xiph/opus (built from source) |
| libvpx | v1.15.2 | github.com/webmproject/libvpx (built from source) |
| libzimg | release-3.0.6 | github.com/sekrit-twc/zimg (built from source) |

---

## GPU Hardware Support

### NVIDIA

| Architecture | Examples | H.264 | HEVC | AV1 | Max Sessions |
|-------------|----------|:-----:|:----:|:---:|:---:|
| Pascal (2016) | GTX 1080, P4000 | ✓ | ✓ | — | 3 |
| Turing (2018) | RTX 2080, T4 | ✓ | ✓ | — | 3 |
| Ampere (2020) | RTX 3090, A4000 | ✓ | ✓ | — | 5 |
| Ada Lovelace (2022) | RTX 4090, L40 | ✓ | ✓ | ✓ | 5 |
| Blackwell (2025) | RTX 5090, B200 | ✓ | ✓ | ✓ | 5 |

> **Note:** A100/H100 data center GPUs have **no NVENC encoder** and are not supported. Consumer session limits (3–5) can be removed with [nvidia-patch](https://github.com/keylase/nvidia-patch) or by using Quadro/Pro/L-series GPUs.

### Intel

| Generation | Examples | H.264 | HEVC | AV1 |
|-----------|----------|:-----:|:----:|:---:|
| Coffee Lake (8th–9th) | i7-9700K | ✓ | ✓ | — |
| Tiger Lake (11th+) | i7-1185G7 | ✓ | ✓ | — |
| Alder Lake (12th+) | i7-12700K | ✓ | ✓ | ✓ |
| Arc (discrete) | Arc A770, A380 | ✓ | ✓ | ✓ |

### AMD

| Architecture | Examples | H.264 | HEVC | AV1 |
|-------------|----------|:-----:|:----:|:---:|
| RDNA 2 (2020) | RX 6800 | ✓ | ✓ | — |
| RDNA 3 (2022) | RX 7900 | ✓ | ✓ | ✓ |
| RDNA 4 (2025) | RX 9070 | ✓ | ✓ | ✓ |

> **Note:** Intel and AMD live transcoding (`live-transcode-runner`) is not currently supported — NVIDIA only.

---

## Build System

### Prerequisites

- Docker with BuildKit enabled
- A container registry to push images to

### Build all images

```bash
REGISTRY=your.registry.com TAG=v1.0.0 ./build.sh
```

To also push images after building:

```bash
REGISTRY=your.registry.com TAG=v1.0.0 PUSH=true ./build.sh
```

### Build order

1. **`codecs-builder`** — shared base image with all codec libraries compiled from source
2. **Runner images** — each runner Dockerfile pulls compiled codecs from `codecs-builder`

### Images produced

| Image | Dockerfile | Target |
|-------|-----------|--------|
| `livepeer-byoc-codecs-builder` | `codecs-builder/Dockerfile` | _(single stage)_ |
| `livepeer-byoc-transcode-runner` | `transcode-runner/Dockerfile` | `runtime-nvidia` |
| `livepeer-byoc-transcode-runner-intel` | `transcode-runner/Dockerfile` | `runtime-intel` |
| `livepeer-byoc-transcode-runner-amd` | `transcode-runner/Dockerfile` | `runtime-amd` |
| `livepeer-byoc-abr-runner` | `abr-runner/Dockerfile` | `runtime-nvidia` |
| `livepeer-byoc-abr-runner-intel` | `abr-runner/Dockerfile` | `runtime-intel` |
| `livepeer-byoc-abr-runner-amd` | `abr-runner/Dockerfile` | `runtime-amd` |
| `livepeer-byoc-live-transcode-runner` | `live-transcode-runner/Dockerfile` | `runtime-nvidia` |

### Build a single image manually

```bash
# Build codecs-builder first
docker build -t your.registry.com/livepeer-byoc-codecs-builder:latest \
  -f codecs-builder/Dockerfile .

# Build a specific runner + GPU variant
docker build -t your.registry.com/livepeer-byoc-transcode-runner:latest \
  -f transcode-runner/Dockerfile \
  --target runtime-nvidia \
  --build-arg REGISTRY=your.registry.com \
  --build-arg TAG=latest \
  .
```

---

## Deployment

### Docker Compose

A `docker-compose.yml` is provided for running the full stack. It requires an orchestrator and gateway already running (or use the included service definitions).

```bash
# Set required variables
export REGISTRY=your.registry.com
export TAG=v1.0.0

# Run NVIDIA GPU services (default)
docker compose up -d

# Run Intel GPU services
docker compose --profile intel up -d

# Run AMD GPU services
docker compose --profile amd up -d
```

### Compose services

| Service | GPU | Description |
|---------|-----|-------------|
| `byoc_transcode_runner` | NVIDIA | VOD transcode runner |
| `byoc_abr_runner` | NVIDIA | ABR ladder runner |
| `byoc_live_transcode_runner` | NVIDIA | Live transcode runner |
| `byoc_transcode_runner_intel` | Intel | VOD transcode runner (Intel profile) |
| `byoc_abr_runner_intel` | Intel | ABR runner (Intel profile) |
| `byoc_transcode_runner_amd` | AMD | VOD transcode runner (AMD profile) |
| `byoc_abr_runner_amd` | AMD | ABR runner (AMD profile) |
| `register_transcode_capability` | — | Registers `video-transcode` capability |
| `register_abr_capability` | — | Registers `transcode-abr` capability |
| `register_live_transcode_capability` | — | Registers `transcode-live` capability |
| `orchestrator` | — | Livepeer orchestrator node |
| `gateway` | — | Livepeer gateway node |

### Capability registration

The `register-capability` sidecar (image: `livepeer-byoc-register-capability`) announces each runner to the orchestrator with:

- **`CAPABILITY_NAME`** — capability identifier used for job routing
- **`CAPABILITY_URL`** — internal URL of the runner
- **`PRICE_PER_UNIT`** / **`CAPACITY`** — pricing and concurrency limits
- **`PERIODIC_REGISTRATION_ENABLED`** — keeps the registration alive
- **`UNREGISTER_ON_SHUTDOWN`** — clean deregistration on container stop

### Data directories

```
data/
  orchestrator/    # Orchestrator persistent data (keys, DB)
  gateway/         # Gateway persistent data
```

---

## Configuration

### Preset file

Presets are defined in YAML and embedded in the binary at compile time. To use a custom preset file at runtime:

```bash
docker run -e PRESETS_FILE=/config/presets.yaml \
  -v ./my-presets.yaml:/config/presets.yaml \
  your.registry.com/livepeer-byoc-transcode-runner:latest
```

### GPU session limits

Runners respect GPU encoder session limits automatically:

- **NVIDIA consumer GPUs:** typically 3–5 concurrent encode sessions
- `MAX_QUEUE_SIZE` should be set to match the GPU session limit
- `MAX_STREAMS` (live runner) defaults to GPU session limit minus headroom

---

## Presets

Presets are named encoding profiles loaded from YAML at startup and validated against the detected GPU capabilities. Presets requiring unsupported GPU features (e.g., AV1 on an Ampere GPU) are automatically disabled.

### transcode-runner presets

#### Streaming — H.264

| Preset | Resolution | Bitrate | Profile |
|--------|-----------|---------|---------|
| `h264-4k` | 3840×2160 | 15 Mbps VBR | High L5.1 |
| `h264-1080p` | 1920×1080 | 5 Mbps VBR | High L4.1 |
| `h264-720p` | 1280×720 | 2.5 Mbps VBR | High L3.1 |
| `h264-480p` | 854×480 | 1 Mbps VBR | Main L3.0 |
| `h264-360p` | 640×360 | 600 kbps VBR | Main L3.0 |

#### Streaming — HEVC & AV1

| Preset | Codec | Resolution | Bitrate |
|--------|-------|-----------|---------|
| `hevc-1080p` | HEVC | 1920×1080 | 3 Mbps VBR |
| `hevc-720p` | HEVC | 1280×720 | 1.5 Mbps VBR |
| `av1-1080p` | AV1 | 1920×1080 | 2.5 Mbps VBR |
| `av1-720p` | AV1 | 1280×720 | 1.2 Mbps VBR |

#### Social media

| Preset | Resolution | Aspect | Notes |
|--------|-----------|--------|-------|
| `social-landscape` | 1920×1080 | 16:9 | YouTube, Twitter/X |
| `social-vertical` | 1080×1920 | 9:16 | TikTok, Reels, Shorts |
| `social-square` | 1080×1080 | 1:1 | Instagram feed |

#### Archive & utility

| Preset | Notes |
|--------|-------|
| `archive-hevc` | CRF 18, source resolution, HEVC Main10 |
| `archive-av1` | CRF 24, source resolution, best compression |
| `proxy-edit` | CRF 23, 720p, lightweight editing proxy |
| `thumbnail` | Extract JPEG at 10% mark |
| `thumbnails-grid` | 4×4 sprite sheet (16 thumbnails) |
| `audio-extract` | Audio-only extraction |
| `remux-mp4` | Container change only, no re-encode |

### abr-runner presets

| Preset | Renditions | Notes |
|--------|-----------|-------|
| `abr-standard` | 1080p / 720p / 480p / 360p | Standard 4-rung HLS ladder |
| `abr-premium` | 4K / 1080p / 720p / 480p / 360p + audio-only | Premium 5+1 rung ladder |
| `abr-mobile` | 720p / 480p / 360p | Bandwidth-constrained |
| `abr-hevc` | 4K-HEVC / 1080p-HEVC / 720p-H264 / 480p-H264 | HEVC top rungs, H.264 fallback |
| `abr-av1` | 4K-AV1 / 1080p-AV1 / 720p-AV1 / 480p-H264 | AV1 top rungs, H.264 fallback |

All ABR presets output fMP4 byte-range HLS (single `.mp4` file per rendition with byte-range playlist).

---

## API Reference

### transcode-runner

#### Submit job

```http
POST /v1/video/transcode
Content-Type: application/json

{
  "input_url": "https://cdn.example.com/source.mp4",
  "output_url": "https://s3.example.com/bucket/output.mp4?X-Amz-Signature=...",
  "preset": "h264-1080p",
  "webhook_url": "https://client.example.com/hooks/transcode",
  "webhook_secret": "whsec_abc123",

  // Optional
  "subtitle_url": "https://cdn.example.com/subs.srt",
  "watermark_url": "https://cdn.example.com/logo.png",
  "watermark_position": "top-right",
  "watermark_scale": 0.1,
  "thumbnail_url": "https://s3.example.com/thumb.jpg?sig=...",
  "thumbnail_seek": 10.0,
  "tone_map": false
}
```

Response `202 Accepted`:
```json
{
  "job_id": "tc-1709467530000-a1b2",
  "status": "queued"
}
```

#### Poll status

```http
POST /v1/video/transcode/status
Content-Type: application/json

{"job_id": "tc-1709467530000-a1b2"}
```

Response (complete):
```json
{
  "job_id": "tc-1709467530000-a1b2",
  "status": "complete",
  "phase": "complete",
  "progress": 100.0,
  "input": {
    "duration": 120.5,
    "width": 3840, "height": 2160,
    "video_codec": "h264", "fps": 24.0,
    "bitrate": 45000
  },
  "output": {
    "width": 1920, "height": 1080,
    "video_codec": "h264",
    "bitrate": 4850, "file_size": 72940000
  },
  "processing_time_seconds": 20.4,
  "gpu": "NVIDIA GeForce RTX 5090",
  "created_at": "2026-03-03T12:00:00Z",
  "completed_at": "2026-03-03T12:00:20Z"
}
```

#### Job lifecycle phases

```
queued → downloading → probing → encoding → uploading → complete
                                                       → error (any phase)
```

### abr-runner

#### Submit ABR job

```http
POST /v1/video/transcode/abr
Content-Type: application/json

{
  "input_url": "https://cdn.example.com/source.mp4",
  "output_urls": {
    "manifest": "https://s3.example.com/video/master.m3u8?sig=...",
    "renditions": {
      "1080p": {
        "playlist": "https://s3.example.com/video/1080p/playlist.m3u8?sig=...",
        "stream":   "https://s3.example.com/video/1080p/stream.mp4?sig=..."
      },
      "720p": { "playlist": "...", "stream": "..." },
      "480p": { "playlist": "...", "stream": "..." },
      "360p": { "playlist": "...", "stream": "..." }
    }
  },
  "preset": "abr-standard"
}
```

### Error codes

| Code | HTTP | Description |
|------|------|-------------|
| `INPUT_FETCH_FAILED` | 400 | Cannot download input URL |
| `INPUT_UNSUPPORTED_FORMAT` | 400 | Unrecognized or undecodable format |
| `INPUT_CORRUPT` | 400 | Corrupt or truncated input |
| `OUTPUT_UPLOAD_FAILED` | 500 | Pre-signed PUT rejected or expired |
| `OUTPUT_URL_EXPIRY_TOO_SHORT` | 400 | Pre-signed URL expires before job completes |
| `PRESET_NOT_FOUND` | 400 | Unknown preset name |
| `PRESET_GPU_INCOMPATIBLE` | 400 | Preset requires GPU feature not available |
| `CAPACITY_EXCEEDED` | 503 | All GPU sessions in use |
| `GPU_ENCODER_ERROR` | 500 | GPU encoder error (driver crash, OOM) |
| `ENCODING_FAILED` | 500 | FFmpeg exited non-zero |
| `JOB_TIMEOUT` | 504 | Job exceeded max processing time |
| `JOB_NOT_FOUND` | 404 | Job ID expired or never existed |

### Health check

```http
GET /healthz
```

```json
{
  "status": "ok",
  "gpu": "NVIDIA GeForce RTX 5090",
  "vram_mb": 32768,
  "active_jobs": 1,
  "max_jobs": 5,
  "presets": 18
}
```

---

## Webhooks

Runners support optional webhook callbacks for job lifecycle events. The `webhook_url` and `webhook_secret` fields are set per-job in the submit request.

### Events

| Event | When |
|-------|------|
| `job.started` | Job picked up from queue, input probed |
| `job.encoding` | Encoding begins |
| `job.progress` | Periodic during encoding |
| `job.rendition.complete` | ABR: one rendition finished and uploaded |
| `job.complete` | All outputs uploaded |
| `job.error` | Job failed |

### Security

Each webhook delivery includes:
- `X-Webhook-Signature`: `sha256=HMAC-SHA256(secret, timestamp + "." + body)`
- `X-Webhook-Timestamp`: Unix timestamp (for replay prevention)
- `X-Webhook-Event`: Event name
- `X-Webhook-Job-Id`: Job ID

Verify on the receiving end:
```
expected = HMAC-SHA256(webhook_secret, timestamp + "." + body)
valid = X-Webhook-Signature == expected AND abs(now - timestamp) < 300s
```

Webhook delivery failures (non-2xx, timeout) are retried 3 times with backoff (1s, 5s, 25s). Failure is **non-blocking** — the job completes regardless, and status remains available via polling.

---

## Project Structure

```
transcode-runners/
│
├── codecs-builder/
│   └── Dockerfile          # Shared base: x264, SVT-AV1, libopus, libvpx, libzimg
│
├── transcode-core/         # Shared Go library
│   ├── gpu.go              # GPU detection & hardware profiles
│   ├── ffmpeg.go           # FFmpeg command builder
│   ├── presets.go          # Preset loader & validator
│   ├── abr_presets.go      # ABR ladder preset types
│   ├── io.go               # HTTP download & upload
│   ├── progress.go         # FFmpeg stderr progress parser
│   ├── filters.go          # FFmpeg filter graph builders
│   ├── hls.go              # HLS manifest generation
│   ├── live.go             # Trickle channel client
│   └── thumbnail.go        # Thumbnail extraction
│
├── transcode-runner/       # VOD transcode runner
│   ├── Dockerfile          # Stages: ffmpeg-nvidia/intel/amd, go-builder, runtime-*
│   ├── main.go             # HTTP server, job manager, webhook sender
│   ├── presets.yaml        # Default preset catalog
│   └── go.mod
│
├── abr-runner/             # ABR ladder runner
│   ├── Dockerfile
│   ├── main.go
│   ├── presets.yaml
│   └── go.mod
│
├── live-transcode-runner/  # Live transcode runner (NVIDIA only)
│   ├── Dockerfile
│   ├── main.go             # Stream lifecycle, Trickle I/O
│   ├── trickle.go          # Trickle channel client
│   └── go.mod
│
├── data/                   # Runtime data (gitignored)
│   ├── orchestrator/
│   └── gateway/
│
├── docs/
│   └── transcode-runner-architecture.md  # Full architecture decisions doc
│
├── docker-compose.yml      # Full stack deployment
└── build.sh                # Build & push all images
```

---

## Design Decisions

Key architectural decisions are documented in detail in [`docs/transcode-runner-architecture.md`](docs/transcode-runner-architecture.md). Summary:

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Architecture | 3 runners (transcode, abr, live) | Balance between isolation and operational overhead |
| GPU vendor support | Same Go code, per-vendor Docker images | Vendor differences are FFmpeg flags only |
| I/O model | HTTP pull + pre-signed PUT | Zero-trust, no credential passing, works with any S3-compatible store |
| Configuration | Presets (YAML) | Ensures valid FFmpeg configurations; simpler client API |
| FFmpeg | Built from source | Control over codecs, versions, GPU features |
| Language | Go | Fast startup, low memory, strong concurrency model |
| CPU fallback | None | Runners refuse to start without GPU — enforces hardware requirements |
| ABR output | fMP4 byte-range HLS | ~9 files for 4-rung ladder vs thousands of segments; works with existing pre-signed URL model |
| Notifications | Polling + optional webhooks | Polling is stateless and universal; webhooks reduce latency |
