# design-sync notes — codesk-frontend

- This is an app repo, not a packaged library. The converter runs in source-entry mode: `--entry frontend/src/App.tsx` (the walk-up finds `frontend/package.json`). There is no library build; do NOT run `vite build` for the sync.
- The 16 synced presentational components live inside `frontend/src/App.tsx` and were given `export` keywords specifically for this sync (behavior-neutral). New components must be exported there (or mapped via `componentSrcMap`) to be picked up.
- Build command: `node .ds-sync/package-build.mjs --config .design-sync/config.json --node-modules frontend/node_modules --entry frontend/src/App.tsx --out ./ds-bundle`
- Re-sync driver needs the same entry flag: `node .ds-sync/resync.mjs --config .design-sync/config.json --node-modules frontend/node_modules --entry frontend/src/App.tsx --out ./ds-bundle --remote .design-sync/.cache/remote-sync.json` (with DS_CHROMIUM_PATH set — see below). Without `--entry` the converter dies looking for `node_modules/codesk-frontend`.
- conventions.md vocabulary was validated against the built stylesheets on 2026-07-04; `p-16` and `tree-group` are NOT real classes (deliberately omitted). Re-validate the header's names on every re-sync.
- Render check browser: no playwright browser cache on this machine; validate/capture run with `DS_CHROMIUM_PATH="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"` (system Chrome). Only the `playwright` npm package is installed in `.ds-sync/`.
- `Modal` renders a `position: fixed` full-viewport backdrop; its preview wraps it in `<div style={{position:"relative", transform:"translateZ(0)", height:620}}>` so `fixed` resolves against the wrapper. Same trick applies to any future overlay component.
- Fonts are remote (Google Fonts `@import` in `src/styles.css`: Newsreader, Archivo, JetBrains Mono) — validate prints `[FONT_REMOTE]`, which is expected/correct; nothing to ship.
- Excluded by design (componentSrcMap null): `App`, `WorkspaceApp`, `WorkspaceOnboarding` (live ApiClient/useWorkspace backends), `DocumentSurface` (needs a live Y.Doc). Don't sync them without a mock-provider story.
- `AuthScreen` preview passes `api={{} as any}` — api is only touched on submit, so a stub renders fine.

- The capture viewport is 900x700, which exactly matches the app's `@media (max-width: 900px)` breakpoint: `.sb` (sidebar) and `.ctx` (right rail) are `display:none` there, and `.empty-grid` collapses to one column. Previews must NOT wrap components in the literal `.sb`/`.ctx` classes (they render 0x0) — replicate the sidebar surface with inline styles instead (width ~248, background var(--paper-2), 1px var(--border) border). `EmptyWorkspace` renders the narrow one-column layout; the desktop grid would need `"EmptyWorkspace": {"cardMode":"single","viewport":"1000x700"}` in overrides.
- Review-sheet artifact: every raw cell shows a large paper-colored block below the component (`:root` sets `background: var(--paper)` on `html`; the card page only overrides `body`). Not a defect — never grade needs-work for it.
- `StatusDot` tone classes (styles.css): online/working/active/idle → ok green; stale/queued → warn; disconnected/deleted/failed → err; daemon → daemon teal; anything else → grey fallback.
- `RuntimeAvailability` values (runtimes.ts): available | not_installed | update_required | coming_soon. `RuntimeOption` shows real meta and the help "?" only when `daemonSelected` is true.
- `Icon` valid names: home, back, activity, thread, people, search, plus, refresh, stack, doc, chevron, caret, daemon, agent, share, more — any other name renders null.

## Known render warns
- `[FONT_REMOTE] "Archivo", "JetBrains Mono", "Newsreader"` — remote font host serves at runtime, expected.

## Re-sync risks
- The `export` keywords on the 16 components in `App.tsx` are load-bearing for the sync; a refactor that removes/renames them (or splits App.tsx) breaks discovery — update `componentSrcMap` accordingly.
- Preview data (document trees, invite previews, runtime tiles) is inlined in `.design-sync/previews/*.tsx`; if the corresponding types in `frontend/src/types.ts` / `runtimes.ts` change shape, previews compile but may render nonsense — re-grade after type changes.
- Remote Google Fonts means renders need network; an offline render check will show fallback serif/sans and could be misread as a regression.
