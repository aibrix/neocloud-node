# Contributor Workflow Handoff Context

## Issue in plain language

Improve the GitHub contribution workflow without adding heavy process:

- provide focused forms for bugs, provider integrations, and Pod compatibility;
- make issue reporters include the environment and expected behavior needed for triage;
- make pull requests state what was tested, what documentation or manifests changed,
  how provider behavior is affected, and whether paid cloud resources could be left behind;
- add one simple default ownership rule that can be refined by path later.

## Acceptance criteria

- Relevant issue forms ask for the provider, Kubernetes version, manifest version,
  and expected behavior.
- The pull request template explicitly asks for tests run and paid-resource cleanup risk.
- `CODEOWNERS` supplies a default reviewer without restricting who may contribute.

## Repository state found during review

- `.github/ISSUE_TEMPLATE/bug_report.yml` already exists, but Kubernetes is only
  mentioned inside a free-form environment block, there is no explicit manifest
  version field, and Vast.ai is missing from the provider choices.
- `.github/ISSUE_TEMPLATE/provider_request.yml` already exists and covers provider
  API/lifecycle details, but does not explicitly ask for expected Kubernetes-facing
  behavior.
- There is no dedicated Pod compatibility request form.
- `.github/PULL_REQUEST_TEMPLATE.md` already has a useful safety checklist, but tests
  run and paid-resource cleanup risk should be explicit response fields rather than
  being implied only by checklist items.
- There is no `CODEOWNERS` file.
- The repository currently supports fake, RunPod, Lambda Cloud, and Vast.ai providers.

## Implementation approach and rationale

1. Refine the existing bug form instead of adding a duplicate. Use separate required
   fields for Kubernetes version and manifest version so reports are consistently
   searchable and complete.
2. Refine the existing provider request form instead of replacing its useful API and
   lifecycle questions. Add optional compatibility-baseline fields and a required
   expected-behavior prompt focused on the Kubernetes-to-provider contract.
3. Add a dedicated Pod compatibility form containing a sanitized minimal Pod manifest,
   provider, Kubernetes version, manifest version, current behavior, expected behavior,
   and workload rationale.
4. Reshape the PR template into short answer sections plus a compact checklist covering
   tests, documentation/manifests, and provider behavior. Include a dedicated
   paid-resource cleanup-risk section that accepts `None` for changes with no cloud cost.
5. Add a broad `.github/CODEOWNERS` default. Keep comments clear that narrower rules can
   be appended later; CODEOWNERS requests review and does not prevent external PRs.

This document records decisions and rationale for handoff. It intentionally does not
contain private chain-of-thought.

## Validation plan

- Parse every issue-form YAML file to catch syntax errors.
- Inspect the rendered/diffed templates for all acceptance-criteria fields.
- Run repository checks appropriate for documentation and GitHub metadata changes if
  the local toolchain is available.

## Progress

- [x] Read the complete requested scope and acceptance criteria.
- [x] Inspected all existing issue forms, the PR template, contribution guidance,
  supported providers, CI configuration, and repository status.
- [x] Updated the bug report form with explicit provider, Kubernetes version,
  manifest version, expected behavior, reproduction, and optional Pod manifest fields.
- [x] Updated the provider integration request with compatibility-baseline and
  expected Kubernetes/provider behavior prompts.
- [x] Added the Pod compatibility request form.
- [x] Updated the pull request template with explicit tests, docs/manifests,
  provider behavior, and paid-resource cleanup sections plus matching checkboxes.
- [x] Added `.github/CODEOWNERS` with one broad default reviewer rule.
- [x] Added `.github/workflows/assign.yml` so commenting `/assign` on an issue or
  pull request assigns it to the commenter, and documented it in CONTRIBUTING.md.
- [x] Validated the result.

## Validation results

- All issue forms parse as YAML.
- A local structural check confirmed each issue form has the required top-level
  fields, supported body item types, unique valid IDs, labels, and options.
- The CODEOWNERS check confirmed one wildcard default rule with an owner.
- `git diff --check` completed without errors.
- `actionlint` passed on `.github/workflows/assign.yml` (when the local toolchain
  and network are available).
- No application tests were run because this change only touches GitHub metadata,
  workflows, and handoff documentation; no Go code, runtime manifests, or workflows
  changed.

## Current handoff state

All requested deliverables are complete: the three issue forms, the pull request
template, `.github/CODEOWNERS`, and the `/assign` self-assign workflow are under
`.github/`, with contributor guidance in `CONTRIBUTING.md` and this `context.md`
handoff file. A future agent should review `git diff` and repository status before
making additional changes.
