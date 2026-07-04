# ExecPlans

When writing complex features or significant refactors, use an ExecPlan (as described in .agent/PLANS.md) from design to implementation.

# Codesk Brand And Frontend Design

Before making frontend UI or visual design changes, review `docs/design/codesk-oxo-system.html` and follow the OXO design language there.

- Use `frontend/public/favicon.svg` as the browser favicon and as the unboxed OXO mark in wordmark lockups. The static homepage has the matching copy at `homepage/favicon.svg`.
- Use `frontend/public/app-icon.svg` only as the boxed product/app icon for standalone app-icon contexts such as installers, launchers, touch icons, or compact product tiles. The static homepage has the matching copy at `homepage/app-icon.svg`.
- Do not redraw the OXO mark from memory. If an inline SVG is needed, copy the geometry from the design system or the SVG source files.
- Preserve the OXO meaning and color roles: apricot human dot on the left, cyan agent dot on the right, neutral connector in ink on light surfaces or paper on dark surfaces.
- Keep the Codesk visual language warm, restrained, and work-focused: paper/ink surfaces, subtle borders, small radii, Archivo for UI text, Newsreader for expressive display type, and JetBrains Mono for labels or technical metadata.
- When updating deployment-facing static assets, keep the Vite app (`frontend/public`) and homepage (`homepage`) asset copies in sync.

# Identity And Migration Contract

All product and document IDs are bare native UUIDs (Postgres `uuid` columns). Never generate prefixed IDs (`ws_`, `doc_`, `agent_`, ...) for anything that reaches the backend — this includes daemon-side generators. Local-only keys (idempotency, SQLite lock owners) may keep prefixes; classify which one you are touching before changing it.

Schema migrations run at backend startup, in order: schema tables → legacy cleanup → UUID migrations. They are fail-closed, single-transaction, advisory-locked, and idempotent. Verification is two-tier: boot-shape checks on every startup; deep referential verification only during actual migration, in CI, and via explicit tooling. Do not add boot-blocking deep checks — a stale data bug must not become a production restart failure.

# Engineering Standards

- One source of truth: derive column lists, sweeps, and verifiers from the shared inventory. Never hand-maintain a parallel list that must agree with another — that is how two production-blocking gaps happened.
- After the second fix of the same failure class, stop and generalize. Piling special cases on top of existing code is a review-blocking smell.
- Migration/compat code is scaffolding: it must have a planned deletion milestone, not live forever.
- Historical-format decoders (e.g. the migration root-namespace codec) stay migration-private and frozen. Do not couple them to live codecs.

# Testing Bar

- Postgres-affecting changes require live-Postgres tests (`scripts/test-postgres.sh`, Docker via sudo on this host). "No test environment" is not an accepted hand-off state.
- Tests must enter through production paths (`initPostgresSchema`, real routes/stores) — a test that calls an internal helper directly does not prove the path users hit.
- Cross-component behavior (daemon ↔ backend) needs regressions at the boundary; unit-green on both sides proves nothing about the seam.
- Every dangerous path fixed in review gets a committed regression. Temporary QA probes must be promoted into the suite before merge.
- Hand-offs state exactly what was validated (compile/unit vs live-Postgres vs cross-component) with the commands run.
- Major changes (migrations, identity/schema changes, anything startup-blocking) run the ENTIRE suite — backend, daemon, and frontend (`npm test`/`npm run build`) — regardless of which surfaces the diff touches. Diff-scoped validation is for minor changes only.

# Process

- Significant designs need a written artifact and explicit sign-off before implementation starts.
- Only AlphaToad merges PRs. Reviews end in a merge recommendation, never a merge.
- Tracked debt (do not "fix" ad hoc; scoped work exists): `column::text = $1` index-defeating casts; FK constraints with explicit ON DELETE once all environments are migrated (then delete the deep verifier and migration scaffolding); test-postgres.sh isolation; uuidverify harness wiring.
