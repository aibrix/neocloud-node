# GPU Burst Node Phase 0 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the first testable core of AIBrix GPU Burst Node: provider-neutral workload types, in-memory binding store, transformer validation, fake provider adapter, and an idempotent lifecycle service.

**Architecture:** Start with a pure Go core package that does not require a Kubernetes cluster or real cloud provider. Kubernetes and Virtual Kubelet integration will wrap these interfaces later. The initial implementation uses Pod UID style strings, provider-neutral requests, durable binding semantics, and a fake provider to verify lifecycle invariants.

**Tech Stack:** Go 1.23, standard library only for Phase 0 core, `go test ./...`.

---

## File Structure

- `go.mod`: module metadata for `github.com/vllm-project/aibrix/pkg/gpu-burst-node`.
- `pkg/core/types.go`: provider-neutral workload, binding, status and error types.
- `pkg/core/binding_store.go`: in-memory `BindingStore` implementation with idempotent create/update/delete semantics.
- `pkg/core/transformer.go`: minimal Pod-like input validation and conversion to `WorkloadRequest`.
- `pkg/core/fake_provider.go`: fake `ProviderAdapter` used by tests.
- `pkg/core/lifecycle.go`: idempotent create/delete/status orchestration using `BindingStore` and `ProviderAdapter`.
- `pkg/core/*_test.go`: table and lifecycle tests.

## Task 1: Initialize Go Module

**Files:**
- Create: `go.mod`

- [ ] **Step 1: Create module file**

```go
module github.com/vllm-project/aibrix/pkg/gpu-burst-node

go 1.23
```

- [ ] **Step 2: Verify module**

Run: `go test ./...`

Expected: no packages matched or no test files yet; command exits successfully once packages exist.

## Task 2: Binding Store TDD

**Files:**
- Create: `pkg/core/types.go`
- Create: `pkg/core/binding_store.go`
- Create: `pkg/core/binding_store_test.go`

- [ ] **Step 1: Write failing tests**

Test `CreateOrGet` creates one binding per workload UID, returns the existing binding on repeat calls, and `MarkDeleting` preserves the remote resource until cleanup is confirmed.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/core -run 'TestMemoryBindingStore' -v`

Expected: FAIL because types and store do not exist.

- [ ] **Step 3: Implement minimal store**

Add `Binding`, `BindingPhase`, `Placement`, `Attempt`, `MemoryBindingStore`, and methods `CreateOrGet`, `Update`, `GetByWorkloadUID`, `MarkDeleting`, `CompleteDelete`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/core -run 'TestMemoryBindingStore' -v`

Expected: PASS.

## Task 3: Transformer TDD

**Files:**
- Create: `pkg/core/transformer.go`
- Create: `pkg/core/transformer_test.go`

- [ ] **Step 1: Write failing tests**

Test a single-container GPU workload transforms into `WorkloadRequest`, multi-container workloads are rejected, missing image is rejected, and zero GPU is rejected.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/core -run 'TestTransformer' -v`

Expected: FAIL because transformer does not exist.

- [ ] **Step 3: Implement minimal transformer**

Add `PodSpec`, `ContainerSpec`, `ResourceSpec`, `UnsupportedSpecError`, and `DefaultTransformer.Transform`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/core -run 'TestTransformer' -v`

Expected: PASS.

## Task 4: Fake Provider TDD

**Files:**
- Create: `pkg/core/fake_provider.go`
- Create: `pkg/core/fake_provider_test.go`

- [ ] **Step 1: Write failing tests**

Test fake provider creates exactly one remote resource per idempotency key, returns status, supports delete, and treats delete of a missing remote resource as success.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/core -run 'TestFakeProvider' -v`

Expected: FAIL because fake provider does not exist.

- [ ] **Step 3: Implement minimal fake provider**

Add `ProviderAdapter`, `RemoteResource`, `RemotePhase`, `FakeProvider`, and idempotent `Create`, `Get`, `Delete`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/core -run 'TestFakeProvider' -v`

Expected: PASS.

## Task 5: Lifecycle Service TDD

**Files:**
- Create: `pkg/core/lifecycle.go`
- Create: `pkg/core/lifecycle_test.go`

- [ ] **Step 1: Write failing tests**

Test `EnsureCreated` is idempotent across repeated calls, stores the remote ID in the binding, and `EnsureDeleted` keeps deleting state until remote cleanup is complete.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/core -run 'TestLifecycleService' -v`

Expected: FAIL because lifecycle service does not exist.

- [ ] **Step 3: Implement minimal lifecycle service**

Add `LifecycleService` with `EnsureCreated`, `SyncStatus`, and `EnsureDeleted`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/core -run 'TestLifecycleService' -v`

Expected: PASS.

## Task 6: Final Verification

**Files:**
- Modify: all Go files as needed for formatting only.

- [ ] **Step 1: Format**

Run: `gofmt -w pkg/core/*.go`

Expected: no output.

- [ ] **Step 2: Run all tests**

Run: `go test ./...`

Expected: PASS.

- [ ] **Step 3: Review git status**

Run: `git status --short`

Expected: shows new Go core files and existing docs.

