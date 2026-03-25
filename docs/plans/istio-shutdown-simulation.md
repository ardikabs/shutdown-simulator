# Plan: Simulating Non-Graceful Shutdown in Istio v1.28+ (Native Sidecars)

This document outlines scenarios to simulate and validate "non-graceful" shutdown behavior using the `echoserver` in a Kubernetes environment with Istio v1.28+ native sidecars.

## Context: Istio v1.28 & Native Sidecars
With Kubernetes Native Sidecars (SidecarContainers), the sidecar container is guaranteed to start before the application and **terminate after** the application containers have exited. However, "graceful shutdown" is still a coordination between the Load Balancer (Istio/Gateway), the Proxy (Envoy), and the Application.

## Objectives
1.  Identify configuration mismatches that lead to 5xx errors during pod termination.
2.  Simulate "Zombies" or hung processes that ignore termination signals.
3.  Observe the impact of insufficient grace periods on in-flight requests.
4.  Validate the lifecycle interaction between the main container and the native sidecar.

---

## Scenario 1: The "503 Race" (Missing Shutdown Delay)
**Goal:** Prove that an application exiting immediately upon `SIGTERM` causes 503 errors for clients, even with Istio.

### Configuration
- **Echoserver:** `SHUTDOWN_DELAY=0s`
- **Kubernetes:** `terminationGracePeriodSeconds: 30`
- **Istio:** Default `terminationDrainDuration`

### Simulation Steps
1.  Start a continuous load test against the `echoserver`.
2.  Delete the pod: `kubectl delete pod <pod-name>`.
3.  **Expected Failure:** The app exits immediately. There is a "propagation window" where other proxies in the mesh still have this pod in their endpoint list. Clients hitting those proxies will receive `503 Service Unavailable` or `Connection Reset` because the target container is gone.

---

## Scenario 2: Termination Grace Period Timeout (The `SIGKILL` event)
**Goal:** Simulate a hard kill when the application takes longer to clean up than the cluster allows.

### Configuration
- **Echoserver:** `SHUTDOWN_DELAY=10s`, `DRAIN_TIMEOUT=60s`. (Total potential shutdown: 70s).
- **Kubernetes:** `terminationGracePeriodSeconds: 30`.
- **Simulation Logic:** Send a request with `?delay=50`.

### Simulation Steps
1.  Client sends: `curl "http://echoserver/?delay=50"`.
2.  Immediately delete the pod.
3.  **Expected Failure:** The app receives `SIGTERM`, waits 10s (`SHUTDOWN_DELAY`), then tries to drain the 50s request. At the 30s mark, Kubernetes sends a `SIGKILL` to the entire pod.
4.  **Client Result:** The connection is abruptly closed mid-stream (likely a `502` or `504` at the gateway, or a TCP reset for mesh clients).

---

## Scenario 3: Proxy Drain Mismatch
**Goal:** Simulate the proxy stopping its listeners before the application has finished its "Shutdown Delay".

### Configuration
- **Istio Annotation:** `proxy.istio.io/config: '{"terminationDrainDuration": "5s"}'`
- **Echoserver:** `SHUTDOWN_DELAY=15s`
- **Kubernetes:** `terminationGracePeriodSeconds: 60`

### Simulation Steps
1.  Clients send traffic.
2.  Delete the pod.
3.  **Expected Failure:** The proxy starts draining and closes its inbound listeners after 5s. However, the application is still in its 15s `SHUTDOWN_DELAY` and expects to keep receiving/processing for a bit. Any "late" packets arriving at the proxy between 5s and 15s will be rejected by the proxy, even though the app is "ready".

---

## Scenario 4: The Zombie Worker (Resource Leak)
**Goal:** Simulate a background process that refuses to stop, preventing the pod from ever exiting gracefully.

### Configuration
- **Echoserver:** `ENABLE_BACKGROUND_WORKER=true`.
- **Echoserver Environment:** `DRAIN_TIMEOUT=5s`.
- **Simulation Logic:** Modify `main.go` or use a "hang" failure mode to simulate a worker that doesn't respect the context cancellation.

### Simulation Steps
1.  Ensure the background worker is running.
2.  Delete the pod.
3.  **Expected Failure:** The HTTP server shuts down in 5s, but the worker ignores `ctx.Done()`. The pod will remain in `Terminating` state until the `terminationGracePeriodSeconds` expires and K8s forced-kills it.
4.  **Observation:** Log `Waiting for background worker to finish...` will hang until the `SIGKILL`.

---

## Scenario 5: The "Phantom" 503 (Mesh-wide Propagation Gap)
**Goal:** Explain why errors appear with "no clues" in application logs during pod replacement in Istio v1.28+.

### Context: The Istio v1.28+ Native Sidecar Reality
In Istio v1.28+, native sidecars ensure the proxy stays alive until the application exits. However, a "Phantom 503" occurs because of **Endpoint Propagation Latency**:
1.  A Pod is marked for deletion.
2.  Istiod (Control Plane) is notified and must update **every other proxy** in the mesh to stop sending traffic to this Pod.
3.  There is a window (often 100ms - 2s) where other proxies still think the terminating Pod is healthy.

### Why there are "No Clues":
If a request hits a terminating pod during this window:
- The **Client Proxy** sends the request to the **Terminating Pod**.
- The **Terminating Proxy** (Envoy) has already received `SIGTERM` and entered its `terminationDrainDuration`. It may reject the request with a `503 Service Unavailable` immediately to signal "I am closing".
- **Result:** Because Envoy rejected the request at the entry point of the sidecar, it **never reached the echoserver container**. The echoserver logs will show absolutely nothing, making it look like a "phantom" error.

### Simulation Steps
1.  Set `SHUTDOWN_DELAY=0s` and `ERROR_RATE=0.0`.
2.  Start a high-QPS load test using Fortio.
3.  Delete the pod.
4.  Check echoserver logs: You will see zero errors.
5.  Check Fortio results: You will see a spike of 503s.
6.  **The Fix Test:** Update the `VirtualService` with a retry policy:
    ```yaml
    retries:
      attempts: 3
      perTryTimeout: 2s
      retryOn: gateway-error,connect-failure,refused-stream,503
    ```
7.  Repeat the test. The 503s should now be masked by retries to other healthy pods.

---

## Scenario 6: The "Sidecar-App" Lifecycle Gap (Post-App Proxy Termination)
**Goal:** Validate the hypothesis that the proxy terminates prematurely once the main application container exits.

### Context: Native Sidecar vs. Legacy Sidecar
In Istio v1.23 (Legacy), both containers receive `SIGTERM` simultaneously. The proxy continues draining regardless of the app's state until the `terminationGracePeriodSeconds` expires.

In Istio v1.28+ (Native Sidecars), the Kubelet sends `SIGTERM` to the sidecar **only after** the main container has exited.
- **The Risk:** If the main container exits, the Kubelet immediately triggers the sidecar's termination. If there are still in-flight requests that were being buffered or processed by the proxy's network stack (but already acknowledged by the app), they might be dropped because the proxy starts its own shutdown immediately.

### Simulation Steps
1.  **Echoserver:** Set `SHUTDOWN_DELAY=2s` and `DRAIN_TIMEOUT=2s`. (The app exits very quickly).
2.  **Istio:** Set `terminationDrainDuration=30s`.
3.  **Kubernetes:** `terminationGracePeriodSeconds=60`.
4.  **Client:** Use a tool that keeps connections open (Keep-Alive) and send a request just as the pod is deleted.
5.  **Observation:** Monitor if the proxy stays alive for the full `30s` drain duration *after* the echoserver logs "Service gracefully stopped". If the proxy logs "Exit" immediately after the app, it confirms the "Post-App termination" behavior is causing the drop.

---

## Observation Checklist

| Metric/Log | Tool | What to look for |
|------------|------|------------------|
| **HTTP 503/504** | `fortio` / `ghz` | Percentage of errors during the 10-20s after deletion. |
| **Envoy Access Logs** | `kubectl logs -c istio-proxy` | `DC` (Downstream Close) or `UH` (No Healthy Upstream) flags. |
| **App Logs** | `kubectl logs -c echoserver` | Timestamps of `SIGTERM` vs `Service gracefully stopped`. |
| **K8s Events** | `kubectl get events` | `Killing` events with "Exceeded terminationGracePeriodSeconds". |

---

## Appendix: Fortio Commands

[Fortio](https://fortio.org/) is an excellent tool for these simulations as it provides detailed latency and status code histograms.

### 1. Continuous Load (For Scenario 1 & 3 & 5)
Run this command in a separate terminal before deleting the pod. It sends 10 requests per second (qps) for 60 seconds.

```bash
# Basic load test
fortio load -c 4 -qps 10 -t 60s http://echoserver.example.com/
```

### 2. Single Long-Running Request (For Scenario 2 & 6)
To see how a specific long connection is handled during shutdown.

```bash
# A single request that takes 45 seconds to complete
fortio load -c 1 -n 1 "http://echoserver.example.com/?delay=45"
```

### 3. Verification of Status Codes
Useful for checking the `/status` endpoint behavior.

```bash
# Verify the status endpoint returns 418
fortio load -c 1 -n 1 "http://echoserver.example.com/status/418"
```

### 4. Analyzing the Results
Fortio will output a summary of status codes. Look for the following during termination:
- `Code 200`: Success.
- `Code 503`: Service Unavailable (App exited too early or mesh propagation delay).
- `Code 502/504`: Bad Gateway/Gateway Timeout (Proxy closed or App was SIGKILLed).
- `Socket errors`: Connection Reset (App crashed or connection was dropped abruptly).
