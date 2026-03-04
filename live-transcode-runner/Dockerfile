ARG REGISTRY
ARG TAG=latest
FROM ${REGISTRY}/livepeer-byoc-codecs-builder:${TAG} AS codecs-builder

# ============================================================================
# Stage: ffmpeg-nvidia — ffmpeg with CUDA/NVENC/NVDEC support
# ============================================================================
FROM nvidia/cuda:12.9.1-devel-ubuntu24.04 AS ffmpeg-nvidia

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    git \
    nasm \
    yasm \
    pkg-config \
    wget \
    ca-certificates \
    libssl-dev \
    libx265-dev \
    && rm -rf /var/lib/apt/lists/*

COPY --from=codecs-builder /usr/local /usr/local
RUN ldconfig

WORKDIR /build

# nv-codec-headers (NVENC/NVDEC API)
RUN git clone --depth 1 --branch n12.2.72.0 https://github.com/FFmpeg/nv-codec-headers.git && \
    cd nv-codec-headers && \
    make install PREFIX=/usr/local && \
    cd .. && rm -rf nv-codec-headers

ENV PKG_CONFIG_PATH=/usr/local/lib/pkgconfig:/usr/local/lib/x86_64-linux-gnu/pkgconfig
ENV LD_LIBRARY_PATH=/usr/local/lib:/usr/local/lib/x86_64-linux-gnu

RUN git clone --depth 1 --branch n7.1.3 https://github.com/FFmpeg/FFmpeg.git && \
    cd FFmpeg && \
    ./configure \
        --prefix=/usr/local \
        --enable-gpl \
        --enable-nonfree \
        --enable-cuda-nvcc \
        --enable-openssl \
        --enable-libnpp \
        --enable-nvenc \
        --enable-nvdec \
        --enable-cuvid \
        --enable-libx264 \
        --enable-libx265 \
        --enable-libsvtav1 \
        --enable-libopus \
        --enable-libvpx \
        --enable-shared \
        --extra-cflags="-I/usr/local/include -I/usr/local/cuda/include" \
        --extra-ldflags="-L/usr/local/lib -L/usr/local/cuda/lib64" && \
    make -j$(nproc) && make install && \
    cd .. && rm -rf FFmpeg

RUN ldconfig

# ============================================================================
# Stage: go-builder
# ============================================================================
FROM golang:1.22-alpine AS go-builder

RUN apk add --no-cache git

WORKDIR /app

COPY transcode-core/go.mod transcode-core/go.sum* /app/transcode-core/
COPY live-transcode-runner/go.mod live-transcode-runner/go.sum* /app/live-transcode-runner/

WORKDIR /app/live-transcode-runner
RUN go mod download 2>/dev/null || true

COPY transcode-core/ /app/transcode-core/
COPY live-transcode-runner/ /app/live-transcode-runner/

RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /bin/live-transcode-runner .

# ============================================================================
# Stage: runtime-nvidia
# ============================================================================
FROM nvidia/cuda:12.9.1-runtime-ubuntu24.04 AS runtime-nvidia

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    libx265-199 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=ffmpeg-nvidia /usr/local/bin/ffmpeg /usr/local/bin/ffmpeg
COPY --from=ffmpeg-nvidia /usr/local/bin/ffprobe /usr/local/bin/ffprobe
COPY --from=ffmpeg-nvidia /usr/local/lib/ /usr/local/lib/

RUN ldconfig

COPY --from=go-builder /bin/live-transcode-runner /usr/local/bin/live-transcode-runner

RUN groupadd -r livetranscode && useradd -r -g livetranscode -d /home/livetranscode -s /sbin/nologin livetranscode

USER livetranscode
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/live-transcode-runner", "-healthcheck"] || exit 1
ENTRYPOINT ["/usr/local/bin/live-transcode-runner"]
