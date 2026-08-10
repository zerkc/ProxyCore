## Summary

<!-- What changed and why? Link the issue: Fixes #NNN -->

## Review path

1. <!-- First file or area to read -->
2. <!-- Second area / riskiest change -->

## Out of scope

<!-- Explicit non-goals for this PR -->

## Test plan

- [ ] `bun run typecheck`
- [ ] `bun run test`
- [ ] `bun run lint`
- [ ] `cd apps/api && go test ./...` (set `DATABASE_URL` for DB-backed tests)
- [ ] Manual check (describe):

## Checklist

- [ ] Change matches an issue or a clear bug
- [ ] Docs/runbooks updated when behavior or install steps change
- [ ] No secrets committed (`.env`, keys, PEMs, Cloudflare tokens)
- [ ] Version bump only in a dedicated release PR (`VERSION`, `package.json`, Go `version` default)
