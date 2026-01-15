ARG BUILD_FROM=alpine:3.23

# Build Stage
# Home Assistant passes BUILD_FROM, but we need a Go builder first.
# We use standard Go alpine image for building.
FROM golang:1.25-alpine AS builder

# Arguments for cross-compilation (passed automatically by docker buildx / HA)
ARG TARGETOS
ARG TARGETARCH

WORKDIR /app

# Copy dependency files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY *.go ./

# Build static binary for the target architecture
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-w -s" -o max2mqtt .

# Final Stage
# Use the base image provided by Home Assistant for consistency, or standard Alpine
FROM ${BUILD_FROM}

# Add Home Assistant Labels
# These help the supervisor identify the add-on
LABEL \
    io.hass.name="MAX! to MQTT Bridge" \
    io.hass.description="Bridge for MAX! Wall Thermostats via CUL-Stick to MQTT" \
    io.hass.arch="aarch64|amd64" \
    io.hass.type="addon" \
    io.hass.version="1.1.0"

WORKDIR /app

# Install runtime dependencies
RUN apk add --no-cache tzdata ca-certificates

# Copy binary from builder
COPY --from=builder /app/max2mqtt /app/max2mqtt
COPY run.sh /app/run.sh

# Ensure execution permissions and create data directory
RUN chmod +x /app/run.sh && \
    mkdir -p /data

ENTRYPOINT ["/app/run.sh"]
