# Multi-stage build for the DocsPage operator

# Stage 1: Build
FROM golang:1.24-alpine AS builder

WORKDIR /workspace

# Copy go module files first to leverage Docker layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the operator binary for the target platform (set automatically by buildkit)
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
    -ldflags="-s -w" \
    -o operator \
    .

# Stage 2: Runtime
FROM gcr.io/distroless/static:nonroot

WORKDIR /

# Copy the operator binary from the builder stage
COPY --from=builder /workspace/operator .

# Run as nonroot user (uid 65532)
USER 65532:65532

ENTRYPOINT ["/operator"]
