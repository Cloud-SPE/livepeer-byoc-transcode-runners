#!/usr/bin/env bash
set -euo pipefail

# Build all Docker images for the BYOC Transcoding Stack

REGISTRY="${REGISTRY:?Error: REGISTRY is not set. Usage: REGISTRY=your.registry.com TAG=v1.0.0 ./build.sh}"
TAG="${TAG:-latest}"
PUSH="${PUSH:-false}"

CODECS_IMAGE="${REGISTRY}/livepeer-byoc-codecs-builder:${TAG}"

# Build shared codec base image first (used by all runner builds)
echo "==> Building ${CODECS_IMAGE}"
docker build -t "${CODECS_IMAGE}" -f codecs-builder/Dockerfile .
echo ""

# Fields: context_dir  image_name  dockerfile  target
images=(
  ".  ${REGISTRY}/livepeer-byoc-transcode-runner:${TAG}              transcode-runner/Dockerfile       runtime-nvidia"
  ".  ${REGISTRY}/livepeer-byoc-transcode-runner-intel:${TAG}        transcode-runner/Dockerfile       runtime-intel"
  ".  ${REGISTRY}/livepeer-byoc-transcode-runner-amd:${TAG}          transcode-runner/Dockerfile       runtime-amd"
  ".  ${REGISTRY}/livepeer-byoc-abr-runner:${TAG}                    abr-runner/Dockerfile             runtime-nvidia"
  ".  ${REGISTRY}/livepeer-byoc-abr-runner-intel:${TAG}              abr-runner/Dockerfile             runtime-intel"
  ".  ${REGISTRY}/livepeer-byoc-abr-runner-amd:${TAG}                abr-runner/Dockerfile             runtime-amd"
  ".  ${REGISTRY}/livepeer-byoc-live-transcode-runner:${TAG}         live-transcode-runner/Dockerfile  runtime-nvidia"
)

for entry in "${images[@]}"; do
  context=$(echo    "$entry" | awk '{print $1}')
  image=$(echo      "$entry" | awk '{print $2}')
  dockerfile=$(echo "$entry" | awk '{print $3}')
  target=$(echo     "$entry" | awk '{print $4}')
  echo "==> Building ${image} from ${context}"
  docker build -t "$image" -f "$dockerfile" --target "$target" \
    --build-arg REGISTRY="${REGISTRY}" --build-arg TAG="${TAG}" \
    "$context"
  echo ""
done

echo "All images built successfully."

if [ "$PUSH" = "true" ]; then
  echo ""
  echo "Pushing images..."
  docker push "${CODECS_IMAGE}"
  for entry in "${images[@]}"; do
    image=$(echo "$entry" | awk '{print $2}')
    echo "==> Pushing ${image}"
    docker push "$image"
  done
  echo ""
  echo "All images pushed successfully."
fi
