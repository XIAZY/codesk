# e2e — browser core-flow smoke (task #14)

Playwright (chromium) drives the **production** frontend build (`vite preview`) against the **real**
docker-compose backend + Postgres. It walks the one path no automation covered: load → authenticate →
switch workspace A→B (B idle, the white-screen incident shape) → switch back → open the seeded document.

## Run
```sh
e2e/run-smoke.sh            # brings the stack up, runs, tears down
NOTTY_E2E_KEEP=1 e2e/run-smoke.sh   # leave the stack up to debug
```
Playwright/chromium must be installed once: `cd e2e && npm ci && npx playwright install chromium`
(the browser cache lives in `~/.cache/ms-playwright`, out of the repo tree).

## Discipline (Tom's spec)
- **retries=0** — three assertions against a local stack are deterministic or broken. Nondeterminism is
  answered by the readiness waits in `global-setup.ts` (backend `/healthz` + preview up), **never a retry**.
- **`page.on('pageerror')` fails the run on any uncaught exception** — the stand-in for the error boundary
  the app lacks; the white-screen class fails even where explicit assertions are thin.
- **Production build, not the dev server** — the smoke tests what deploys.
- Traces retained on failure; ≤3min total budget.

## Seeding (Tom's (c) ruling)
The compose stack runs fake Mailgun (no inbox), so `global-setup.ts` registers a throwaway account and
marks it verified directly in Postgres before the browser logs in — the only DB touch; every flow
assertion goes through the browser against real endpoints. Workspace B is left fully idle so the switch
reproduces the incident by construction.
