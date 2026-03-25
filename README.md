# Echoserver with battery-included features

This service provides a highly configurable EchoServer and a background worker simulator. It is designed to validate graceful shutdown behavior, simulate infrastructure failures, and test Kubernetes networking components like Istio and Gateway API.

## Features

- **HTTP Endpoints:**
  - `ANY /`: The main Echo endpoint. It supports all HTTP verbs and returns a JSON response containing request headers, method, and an optional message.
    - **Request ID:** Automatically generates a UUID for each request if `X-Request-ID` header is missing.
    - **Failure Simulation:** Can simulate complex infrastructure/network failures based on a configurable rate and mode.
    - **Delay Simulation:** Use `?delay=X` (seconds) to simulate a long-running request.
  - `GET /status/{code}`: Returns the specified HTTP status code on demand (e.g., `/status/418`).
  - `GET /healthz`: Health check endpoint for Liveness and Readiness probes.
- **Background Worker:** Optional background process (e.g., a queue consumer) that performs periodic tasks. Toggleable via `ENABLE_BACKGROUND_WORKER`.
- **Graceful Shutdown:** Handles `SIGTERM` and `SIGINT` signals with a structured sequence:
  1. **Shutdown Delay:** Waits for `SHUTDOWN_DELAY` before starting the actual shutdown (useful for Istio sidecar drain coordination).
  2. **Server Shutdown:** Stops accepting new connections and waits for active requests to complete.
  3. **Worker Cleanup:** Triggers the background worker (if enabled) to stop and perform its final cleanup tasks.
  4. **Final Exit:** Terminates once all components are finished or the `DRAIN_TIMEOUT` is reached.

## Configuration

| Environment Variable | Default | Description |
|----------------------|---------|-------------|
| `ENABLE_BACKGROUND_WORKER` | `false` | Whether to enable the periodic background worker. |
| `SHUTDOWN_DELAY`     | `0s`    | Delay after receiving SIGTERM before starting the shutdown sequence. |
| `DRAIN_TIMEOUT`      | `30s`   | Maximum time to wait for active requests and worker cleanup during shutdown. |
| `ERROR_RATE`         | `0.0`   | Probability (0.0 to 1.0) of triggering a failure for root `/` requests. |
| `FAILURE_MODE`       | `500`   | Type of failure to simulate. (Options: `500`, `503`, `504`, `5xx`, `hang`, `reset`, `close`, `partial`, `slow-body`, `random`, or a comma-separated list). |
| `MAX_ERROR_DELAY_SECONDS` | `5` | Maximum random delay (in seconds) applied before returning `503` or `504` errors. |

### Failure Modes Detail

- `500`: Returns 500 Internal Server Error.
- `503`: Returns 503 Service Unavailable (with random delay up to `MAX_ERROR_DELAY_SECONDS`).
- `504`: Returns 504 Gateway Timeout (with random delay up to `MAX_ERROR_DELAY_SECONDS`).
- `5xx`: Randomly picks between `500`, `503`, or `504`.
- `hang`: Request hangs indefinitely (simulates a dead/silent backend).
- `reset`: Hijacks the connection and sends a TCP RST (simulates a connection reset).
- `close`: Hijacks and closes the connection abruptly without a response (simulates an abrupt server crash).
- `partial`: Sends headers and a partial chunked response body, then abruptly closes (simulates a mid-stream failure).
- `slow-body`: Sends the response body very slowly using chunked encoding with 1s delays between chunks.
- `random`: Randomly selects one of all above failure modes for each error.
- `Comma-separated list`: If the value contains a comma (e.g., `500,hang,close`), the server will randomly pick one from the list for each failure.

## Deployment

### Docker

The project includes a multi-stage `Dockerfile` based on **Wolfi** for a secure, minimal runtime environment.

```bash
docker build -t ghcr.io/ardikabs/echoserver:latest .
```

### Kubernetes & Istio

Configurations are available in the `config/` directory:

- `config/kubernetes/`: Standard Deployment and Service manifests.
- `config/istio/`: `VirtualService`, `DestinationRule`, and `Sidecar` configurations.
- `config/gateway-api/`: Modern Kubernetes `Gateway` and `HTTPRoute` manifests.

The `Deployment` is pre-configured with Istio annotations to optimize sidecar lifecycle during shutdown:
- `proxy.istio.io/config`: Sets `terminationDrainDuration` to ensure the sidecar waits for active connections.
- `proxy.istio.io/holdApplicationUntilProxyReceivesConfig`: Ensures the sidecar is fully ready before the application starts.

## Usage

### Running Locally

```bash
# Build the service
go build -o echoserver main.go

# Run as a simple echoserver
./echoserver

# Run with a 10% random failure rate and background worker enabled
ENABLE_BACKGROUND_WORKER=true ERROR_RATE=0.1 FAILURE_MODE=random ./echoserver
```

### Testing Scenarios

1. **Verify Echo functionality:**
   - `curl -X POST -d "hello" "http://localhost:8080/"`

2. **Verify On-demand Status Codes:**
   - `curl -i "http://localhost:8080/status/418"`

3. **Verify In-flight Request Handling during Shutdown:**
   - Start a 15s request: `curl -v "http://localhost:8080/?delay=15"`
   - Trigger shutdown: `kill -TERM <pid>`
   - Observe logs: The server should wait for the request to complete before exiting.
