# GPU Burst Node Phase 5 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the first provider-neutral placement/fallback planner.

**Architecture:** Placement is a pure core function. Providers later feed offers into the planner; the planner returns one selected offer plus rejected candidate reasons for Events and binding status. Kubernetes default scheduler remains unchanged.

**Tech Stack:** Go 1.23, standard library only, `go test ./...`.

---

## File Structure

- `pkg/core/placement.go`: offer, policy, decision, and planner.
- `pkg/core/placement_test.go`: deterministic selection and rejection tests.
- `docs/placement.md`: mechanism and current limitations.

## Task 1: Placement Planner TDD

**Files:**
- Create: `pkg/core/placement.go`
- Create: `pkg/core/placement_test.go`

- [ ] **Step 1: Write failing tests**

Test provider order, region order, unavailable offers, max price, and no-offer errors with rejected reasons.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/core -run 'TestPlacement' -v`

Expected: FAIL because placement types do not exist.

- [ ] **Step 3: Implement minimal planner**

Add `Offer`, `PlacementPolicy`, `PlacementDecision`, `RejectedOffer`, `PlacementUnavailableError`, and `SelectOffer`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/core -run 'TestPlacement' -v`

Expected: PASS.

## Task 2: Documentation

**Files:**
- Create: `docs/placement.md`

- [ ] **Step 1: Document selection order**

Document filter and sort order: GPU count/type, availability, max price, provider order, region order, then price.

- [ ] **Step 2: Document rejected reasons**

Document rejected reason codes used by the planner.

## Task 3: Final Verification and Commit

**Files:**
- Modify: all Phase 5 files as needed for formatting only.

- [ ] **Step 1: Format**

Run: `gofmt -w pkg/core/*.go`

Expected: no output.

- [ ] **Step 2: Run all tests**

Run: `go test ./...`

Expected: PASS.

- [ ] **Step 3: Commit Phase 5**

Run:

```bash
git add docs/placement.md docs/superpowers/plans/2026-06-05-gpu-burst-node-phase5.md pkg/core
git commit -m "feat: add placement planner"
```

Expected: one commit containing only Phase 5 placement changes.

