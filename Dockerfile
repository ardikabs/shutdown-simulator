# Build stage
FROM golang:1.24-alpine AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /app

# Copy go.mod and main.go
COPY . .
# Build the binary
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o echoserver main.go

# Final stage
FROM cgr.dev/chainguard/wolfi-base

WORKDIR /app

# Copy the binary from the builder stage
COPY --from=builder /app/echoserver .

# Run the service
ENTRYPOINT ["/app/echoserver"]
