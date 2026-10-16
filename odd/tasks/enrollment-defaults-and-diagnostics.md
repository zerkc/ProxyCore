# enrollment-defaults-and-diagnostics

## Objective
Make primary enrollment usable from the UI and diagnosable when it fails on a real deployment.

## Problem (verified in code)
1. `handleEnrollmentTokenDefaults` builds the default primary URL from `gatewayIngress()`, which reads only `defaultIngress` (never set outside tests) and env `PROXY_INGRESS_IPV4`. The "Advertised IPv4" saved from the Ingress screen (SQLite settings) is ignored, so the default URL is empty. (`apps/api/internal/httpserver/enrollment_tokens.go:259-310`)
2. With an empty default URL, `hostCovered` is false and the UI shows "The default address is not covered by the enrollment certificate" although no default exists. (`apps/ui/src/dashboard/EnrollmentTokenPanel.tsx:264`)
3. Node confirm returns the opaque 409 `enrollment grant denied` from several sites and logs nothing, so the cause cannot be diagnosed. (`apps/api/internal/httpserver/enrollment_workflow.go:421/443/449/587`, `apps/api/internal/enrollment/bootstrap_accept.go:78-131`)
4. e2e gap: `scripts/e2e-docker-endpoints.sh` always sends `primaryUrl` explicitly (line 63) and only asserts the type of `defaults.primaryUrl` (line 196); nodes in e2e are fresh installs.

## Scope
- T1 defaults read the persisted Advertised IPv4/IPv6 (settings) before env fallback.
- T2 UI hides the host warning when `primaryUrl` is empty.
- T3 redacted reason-code logging at every rejection point of the confirm flow and `BootstrapAcceptor.Accept` (no secrets, no raw errors).
- T4 e2e asserts a non-empty default once ingress is configured.

## Constraints
- Artifacts in English. No secrets, tokens, keys or payloads in logs.
- Test-first where a runnable deterministic test exists (Go tests, UI tests).
- Advisory ~400 authored changed lines per task.

## Tasks
- [x] T1 backend defaults from persisted ingress (+ Go test)
- [ ] T2 UI warning guard (+ UI test)
- [ ] T3 diagnostic reason codes (+ Go tests asserting no secret leakage)
- [ ] T4 e2e assertion

## Route
Delegated direct: one writer (2+ non-trivial files across Go, TS, shell).

## Progress / evidence
- T1: RED observed (primaryUrl empty with persisted 198.51.100.7), GREEN after gatewayIngress(ctx); httpserver tests pass; two legacy subtests moved to a no-persisted-ingress backend wrapper.

## Next step
Writer implements T1-T4; focused verification; work-unit commits on this branch.
