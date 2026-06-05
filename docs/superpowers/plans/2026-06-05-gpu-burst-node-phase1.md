# GPU Burst Node Phase 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the minimal transformer into a clear Pod compatibility contract for Phase 1.

**Architecture:** Keep the transformer as pure Go logic with provider-neutral types. The Virtual Kubelet wrapper will later convert `corev1.Pod` into these core structs. Unsupported Kubernetes semantics must fail with stable code/field/message values instead of being silently ignored.

**Tech Stack:** Go 1.23, standard library only, `go test ./...`.

---

## File Structure

- `pkg/core/types.go`: extend `WorkloadRequest` with restart policy, ports and image pull secrets.
- `pkg/core/transformer.go`: extend Pod-like input structs and validation.
- `pkg/core/transformer_test.go`: add support and rejection tests.
- `docs/compatibility-matrix.md`: document Phase 1 supported and rejected Pod fields.

## Task 1: Supported Compatibility Fields

**Files:**
- Modify: `pkg/core/types.go`
- Modify: `pkg/core/transformer.go`
- Modify: `pkg/core/transformer_test.go`

- [ ] **Step 1: Write failing test**

Add a transformer test that expects command, args, env, restartPolicy, imagePullSecrets and ports to be preserved in `WorkloadRequest`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/core -run 'TestTransformerMapsSupportedCompatibilityFields' -v`

Expected: FAIL because these fields are not fully represented yet.

- [ ] **Step 3: Implement minimal field mapping**

Extend core structs and `DefaultTransformer.Transform` to copy supported fields.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/core -run 'TestTransformerMapsSupportedCompatibilityFields' -v`

Expected: PASS.

## Task 2: Unsupported Spec Rejection

**Files:**
- Modify: `pkg/core/transformer.go`
- Modify: `pkg/core/transformer_test.go`

- [ ] **Step 1: Write failing test**

Add table tests for unsupported restart policy, init containers, volumes, host networking and privileged containers. Each case must assert stable error code and field path.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/core -run 'TestTransformerRejectsPhase1UnsupportedSpecs' -v`

Expected: FAIL because validation is not implemented.

- [ ] **Step 3: Implement validation**

Return `UnsupportedSpecError` with `Code`, `Field` and `Message` for each unsupported feature.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/core -run 'TestTransformerRejectsPhase1UnsupportedSpecs' -v`

Expected: PASS.

## Task 3: Compatibility Matrix

**Files:**
- Create: `docs/compatibility-matrix.md`

- [ ] **Step 1: Document supported fields**

Document Phase 1 supported Pod fields and their mapping to provider-neutral requests.

- [ ] **Step 2: Document rejected fields**

Document rejected fields and the matching transformer error codes.

## Task 4: Final Verification and Commit

**Files:**
- Modify: all Phase 1 files as needed for formatting only.

- [ ] **Step 1: Format**

Run: `gofmt -w pkg/core/*.go`

Expected: no output.

- [ ] **Step 2: Run all tests**

Run: `go test ./...`

Expected: PASS.

- [ ] **Step 3: Commit Phase 1**

Run:

```bash
git add docs/compatibility-matrix.md docs/superpowers/plans/2026-06-05-gpu-burst-node-phase1.md pkg/core
git commit -m "feat: define transformer compatibility contract"
```

Expected: one commit containing only Phase 1 transformer contract changes.

