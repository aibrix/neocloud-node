# GPU Burst Node Phase 4 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a provider-neutral VM runner bootstrap generator for Lambda-style VM-backed GPU providers.

**Architecture:** VM-backed providers launch an instance, pass cloud-init, run one container, then report completion to a controller callback URL. Phase 4 implements only deterministic cloud-init generation and shell-safe argument rendering. It does not call Lambda Cloud yet.

**Tech Stack:** Go 1.23, standard library only, `go test ./...`.

---

## File Structure

- `pkg/runner/cloudinit.go`: cloud-init generator.
- `pkg/runner/cloudinit_test.go`: bootstrap rendering tests.
- `docs/vm-runner.md`: VM runner contract and Lambda integration notes.

## Task 1: Cloud-Init Bootstrap TDD

**Files:**
- Create: `pkg/runner/cloudinit.go`
- Create: `pkg/runner/cloudinit_test.go`

- [ ] **Step 1: Write failing test**

Test that cloud-init includes Docker installation, image pull, GPU-enabled `docker run`, env variables, command/args, and callback URL.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/runner -run 'TestBuildCloudInit' -v`

Expected: FAIL because runner package does not exist.

- [ ] **Step 3: Implement minimal cloud-init builder**

Add `CloudInitConfig`, `BuildCloudInit`, and shell quoting helpers.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/runner -run 'TestBuildCloudInit' -v`

Expected: PASS.

## Task 2: Validation TDD

**Files:**
- Modify: `pkg/runner/cloudinit.go`
- Modify: `pkg/runner/cloudinit_test.go`

- [ ] **Step 1: Write failing test**

Test missing image, workload ID, or callback URL returns a validation error.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/runner -run 'TestBuildCloudInitValidation' -v`

Expected: FAIL because validation does not exist.

- [ ] **Step 3: Implement validation**

Return stable `ConfigError` codes for required fields.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/runner -run 'TestBuildCloudInitValidation' -v`

Expected: PASS.

## Task 3: Documentation

**Files:**
- Create: `docs/vm-runner.md`

- [ ] **Step 1: Document runner model**

Document one VM per workload, Docker container execution, and callback-based status reporting.

- [ ] **Step 2: Document limitations**

Document that Phase 4 does not implement real Lambda API calls, SSH fallback, logs streaming, secret delivery, or checkpointing.

## Task 4: Final Verification and Commit

**Files:**
- Modify: all Phase 4 files as needed for formatting only.

- [ ] **Step 1: Format**

Run: `gofmt -w pkg/runner/*.go`

Expected: no output.

- [ ] **Step 2: Run all tests**

Run: `go test ./...`

Expected: PASS.

- [ ] **Step 3: Commit Phase 4**

Run:

```bash
git add docs/vm-runner.md docs/superpowers/plans/2026-06-05-gpu-burst-node-phase4.md pkg/runner
git commit -m "feat: add vm runner bootstrap"
```

Expected: one commit containing only Phase 4 VM runner bootstrap changes.

