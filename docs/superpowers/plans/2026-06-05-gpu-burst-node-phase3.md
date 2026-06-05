# GPU Burst Node Phase 3 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a small production-safety guard before remote GPU resources are created.

**Architecture:** Keep policy enforcement in `pkg/core` so every provider goes through the same launch gate. `LifecycleService.EnsureCreated` validates `WorkloadRequest` before calling `ProviderAdapter.Create`. Phase 3 covers only local policy checks; Kubernetes admission, namespace policy and CRDs remain out of scope.

**Tech Stack:** Go 1.23, standard library only, `go test ./...`.

---

## File Structure

- `pkg/core/launch_guard.go`: launch policy and guard implementation.
- `pkg/core/launch_guard_test.go`: policy unit tests.
- `pkg/core/lifecycle.go`: optional launch guard integration.
- `pkg/core/lifecycle_test.go`: create-blocking test.
- `docs/launch-guard.md`: Phase 3 safety guard documentation.

## Task 1: Launch Guard TDD

**Files:**
- Create: `pkg/core/launch_guard.go`
- Create: `pkg/core/launch_guard_test.go`

- [ ] **Step 1: Write failing tests**

Test that the guard rejects missing max price when required, excessive GPU count, and excessive exposed ports. Test that a valid request passes.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/core -run 'TestLaunchGuard' -v`

Expected: FAIL because launch guard types do not exist.

- [ ] **Step 3: Implement minimal launch guard**

Add `LaunchPolicy`, `LaunchGuard`, `PolicyViolationError`, and stable violation codes.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/core -run 'TestLaunchGuard' -v`

Expected: PASS.

## Task 2: Lifecycle Integration TDD

**Files:**
- Modify: `pkg/core/lifecycle.go`
- Modify: `pkg/core/lifecycle_test.go`

- [ ] **Step 1: Write failing test**

Test `LifecycleService.EnsureCreated` returns a policy violation and does not call provider create when launch guard rejects the workload.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/core -run 'TestLifecycleServiceRejectsPolicyViolationBeforeProviderCreate' -v`

Expected: FAIL because lifecycle service does not support a launch guard.

- [ ] **Step 3: Implement lifecycle integration**

Add `WithLaunchGuard` option and call guard before `ProviderAdapter.Create`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/core -run 'TestLifecycleServiceRejectsPolicyViolationBeforeProviderCreate' -v`

Expected: PASS.

## Task 3: Documentation

**Files:**
- Create: `docs/launch-guard.md`

- [ ] **Step 1: Document policy fields**

Document `RequireMaxPrice`, `MaxGPUCount`, and `MaxPorts`.

- [ ] **Step 2: Document non-goals**

Document that this is not namespace policy, admission, quota, budget accounting, or CRD-based governance.

## Task 4: Final Verification and Commit

**Files:**
- Modify: all Phase 3 files as needed for formatting only.

- [ ] **Step 1: Format**

Run: `gofmt -w pkg/core/*.go`

Expected: no output.

- [ ] **Step 2: Run all tests**

Run: `go test ./...`

Expected: PASS.

- [ ] **Step 3: Commit Phase 3**

Run:

```bash
git add docs/launch-guard.md docs/superpowers/plans/2026-06-05-gpu-burst-node-phase3.md pkg/core
git commit -m "feat: add launch safety guard"
```

Expected: one commit containing only Phase 3 safety guard changes.

