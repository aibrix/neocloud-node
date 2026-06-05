# GPU Burst Node Phase 2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a RunPod provider adapter that implements the core `ProviderAdapter` contract against the RunPod REST Pods API.

**Architecture:** Keep RunPod code in `pkg/providers/runpod`, isolated from core lifecycle logic. The adapter maps `core.WorkloadRequest` into RunPod `POST /pods` JSON, maps RunPod pod status back to `core.RemoteResource`, and treats `DELETE` 404 as successful cleanup. Tests use `httptest.Server`; no real RunPod API key is required.

**Tech Stack:** Go 1.23, standard library `net/http`, `encoding/json`, `httptest`, `go test ./...`.

---

## Official API References

- RunPod create Pod: `POST https://rest.runpod.io/v1/pods`
- RunPod find Pod by ID: `GET https://rest.runpod.io/v1/pods/{podId}`
- RunPod delete Pod: `DELETE https://rest.runpod.io/v1/pods/{podId}`

## File Structure

- `pkg/providers/runpod/client.go`: RunPod adapter implementation.
- `pkg/providers/runpod/client_test.go`: HTTP contract tests.
- `docs/runpod-adapter.md`: RunPod adapter field mapping and current limitations.

## Task 1: Create Payload Contract

**Files:**
- Create: `pkg/providers/runpod/client_test.go`
- Create: `pkg/providers/runpod/client.go`

- [ ] **Step 1: Write failing test**

Test `Client.Create` sends `POST /pods`, bearer auth, JSON content type, and maps `WorkloadRequest` into `imageName`, `dockerEntrypoint`, `dockerStartCmd`, `env`, `gpuCount`, `gpuTypeIds`, `ports`, `computeType`, and `name`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/providers/runpod -run 'TestClientCreateSendsRunPodPayload' -v`

Expected: FAIL because the RunPod package does not exist.

- [ ] **Step 3: Implement minimal create**

Add `Config`, `Client`, `NewClient`, `Create`, request/response structs, and payload mapping.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/providers/runpod -run 'TestClientCreateSendsRunPodPayload' -v`

Expected: PASS.

## Task 2: Get/Delete/Status Contract

**Files:**
- Modify: `pkg/providers/runpod/client_test.go`
- Modify: `pkg/providers/runpod/client.go`

- [ ] **Step 1: Write failing tests**

Test `Get` maps RunPod `desiredStatus` values to core phases, and `Delete` treats HTTP 204 and 404 as successful cleanup.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/providers/runpod -run 'TestClient(Get|Delete)' -v`

Expected: FAIL because `Get` and `Delete` do not exist or status mapping is missing.

- [ ] **Step 3: Implement get/delete/status mapping**

Add `Get`, `Delete`, HTTP error handling, and status mapping for `RUNNING`, `EXITED`, `TERMINATED`, and unknown statuses.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/providers/runpod -run 'TestClient(Get|Delete)' -v`

Expected: PASS.

## Task 3: Adapter Documentation

**Files:**
- Create: `docs/runpod-adapter.md`

- [ ] **Step 1: Document field mapping**

Document the RunPod REST fields used by Phase 2 and how they map from `core.WorkloadRequest`.

- [ ] **Step 2: Document limitations**

Document that Phase 2 does not yet provide remote create de-duplication after ambiguous HTTP failures, real integration tests, logs, registry auth ID mapping, or RunPod catalog lookup.

## Task 4: Final Verification and Commit

**Files:**
- Modify: all Phase 2 files as needed for formatting only.

- [ ] **Step 1: Format**

Run: `gofmt -w pkg/providers/runpod/*.go`

Expected: no output.

- [ ] **Step 2: Run all tests**

Run: `go test ./...`

Expected: PASS.

- [ ] **Step 3: Commit Phase 2**

Run:

```bash
git add docs/runpod-adapter.md docs/superpowers/plans/2026-06-05-gpu-burst-node-phase2.md pkg/providers/runpod
git commit -m "feat: add runpod provider adapter"
```

Expected: one commit containing only Phase 2 RunPod adapter changes.

