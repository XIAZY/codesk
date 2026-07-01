# Align Frontend With The Codesk OXO Design Language

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This plan follows `.agent/PLANS.md`, which requires a self-contained plan with observable outcomes, validation steps, and living progress notes.

## Purpose / Big Picture

The Codesk frontend currently has the right brand assets in the repository, but the login and onboarding screens still look like an older product: they use a boxed app icon beside a capitalized wordmark, Inter/Instrument Serif fonts, pill-heavy buttons, and warmer orange accents that do not match `docs/design/codesk-oxo-system.html`. After this change, a user opening the login, invite, or workspace creation screens should see the OXO design language: the unboxed OXO mark, lowercase `codesk` wordmark, Archivo UI text, Newsreader display type, paper/ink surfaces, subtle borders, and apricot/cyan OXO color roles. The app shell should also inherit the corrected tokens so future frontend work starts from the right visual base.

## Progress

- [x] (2026-07-01) Read `.agent/PLANS.md`, `docs/design/codesk-oxo-system.html`, the provided screenshot, and the current auth-related React/CSS files.
- [x] (2026-07-01) Created this ExecPlan to keep the design alignment work restartable.
- [x] (2026-07-01) Updated shared CSS tokens in `frontend/src/styles.css` to match the OXO palette and font families.
- [x] (2026-07-01) Updated the shared `Logo` component in `frontend/src/App.tsx` so brand lockups use the unboxed OXO mark and lowercase `codesk`.
- [x] (2026-07-01) Updated auth, invite, and workspace onboarding layout styles so they use the OXO card/frame language rather than the old centered utility card look.
- [x] (2026-07-01) Validated with `npm run build` from `frontend/` and browser screenshots of the login page at desktop and 390px mobile width.

## Surprises & Discoveries

- Observation: The design system distinguishes the OXO mark used in wordmark lockups from the boxed app icon. The screenshot’s black rounded-square icon is technically the app icon, but the OXO system’s “mark & lockups” section shows the unboxed OXO mark next to lowercase `codesk`.
  Evidence: `docs/design/codesk-oxo-system.html` contains the title lockup `<svg class="mark" ...><use href="#oxo"/></svg>` followed by lowercase `codesk`, while `frontend/src/App.tsx` currently renders `/app-icon.svg` next to `Codesk`.

## Decision Log

- Decision: Use `/favicon.svg` for the in-app wordmark lockup and reserve `/app-icon.svg` for browser/app icon contexts.
  Rationale: The OXO design system’s lockup shows the unboxed OXO mark; the boxed app icon beside a wordmark is exactly what the provided screenshot calls out as wrong.
  Date/Author: 2026-07-01 / Codex.

- Decision: Keep compatibility variable names such as `--accent`, `--agent`, and `--border` while changing their values to OXO meanings.
  Rationale: The frontend already uses those variables broadly. Preserving names lets the whole shell inherit the design language without a risky class-by-class rewrite.
  Date/Author: 2026-07-01 / Codex.

- Decision: Keep the implementation in CSS/React only and avoid pulling additional font packages.
  Rationale: The existing frontend already imports web fonts from Google Fonts in `frontend/src/styles.css`; swapping to the design system’s families is enough for this pass.
  Date/Author: 2026-07-01 / Codex.

## Outcomes & Retrospective

The frontend now uses the OXO visual language for the auth family of screens and shared tokens. The login screen shows the unboxed OXO mark and lowercase `codesk`, uses Archivo for UI text and Newsreader for display text, and no longer clips the `Welcome back` heading. Desktop browser metrics reported `titleClipped: false`; mobile-width metrics at 390px reported `horizontalOverflow: false` and `titleClipped: false`. `npm run build` completed successfully.

## Context and Orientation

The Vite React frontend lives under `frontend/`. The entry point is `frontend/src/App.tsx`, and most visual styling lives in `frontend/src/styles.css`. The shared brand lockup is the `Logo` function near the end of `frontend/src/App.tsx`. Auth, invite, route-message, and onboarding screens all render inside `.auth-screen` with `.auth-panel` or `.picker-panel`.

The OXO design system lives at `docs/design/codesk-oxo-system.html`. In this document, “OXO” means a human dot on the left, a neutral connector in the middle, and an agent dot on the right. The human dot is apricot, the agent dot is cyan, and the connector is ink on light surfaces or paper on dark surfaces.

The static assets now available to the app are:

- `frontend/public/favicon.svg`: unboxed OXO mark suitable for browser favicon and wordmark lockups.
- `frontend/public/app-icon.svg`: boxed app icon suitable for app icons, touch icons, and standalone product-icon contexts.

## Plan of Work

First, update the CSS font import and root tokens in `frontend/src/styles.css` to match the OXO system: Newsreader for display/wordmark, Archivo for UI, JetBrains Mono for labels, `#FCFBF7` paper, `#1B1A17` ink, OXO apricot/cyan roles, and neutral borders. Keep compatibility aliases such as `--border` and `--accent` so the wider shell does not require a risky rewrite.

Second, update `Logo` in `frontend/src/App.tsx` to render `/favicon.svg` and lowercase `codesk`. Adjust `.logo-row`, `.logo-mark`, and `.logo-type` so the auth lockup resembles the OXO system: unboxed mark, lowercase wordmark, and no cramped black-square app icon.

Third, update auth and onboarding CSS classes in `frontend/src/styles.css`: `.auth-screen`, `.auth-panel`, `.picker-panel`, `.auth-title`, `.auth-copy`, `.btn`, `.field input`, `.create-workspace-card`, and `.invite-preview`. The goal is to make these screens feel like the OXO frames: warm paper background, subtle border, smaller radii than the old large card, Newsreader display headings, and Archivo controls.

Fourth, verify by running `npm run build` from `frontend/`. If possible, start or use the Vite dev server and inspect the login screen. The login page should show the OXO mark plus lowercase `codesk`, a non-clipped heading, restrained frame styling, and controls that match the OXO palette.

## Concrete Steps

Run commands from `/Users/zhongyangxia/Documents/dev/notty` unless noted.

1. Edit `frontend/src/styles.css` and `frontend/src/App.tsx` as described above.
2. Run:

       cd frontend
       npm run build

   This was run successfully on 2026-07-01. The relevant output was:

       ✓ 103 modules transformed.
       ✓ built in 974ms

3. If visual verification is available, run:

       cd frontend
       npm run dev

   This was run during implementation. Browser verification opened `http://localhost:5173/login` at the default desktop viewport and at `390x844`. The auth screen followed the OXO lockup and surface language in both screenshots.

## Validation and Acceptance

The change is accepted when:

- `npm run build` succeeds from `frontend/`. This passed.
- The login screen uses the unboxed OXO mark and lowercase `codesk`, not the boxed app icon plus `Codesk`. Browser screenshots confirmed this.
- The login heading and form controls are not clipped at common desktop viewport sizes. Browser metrics confirmed `titleClipped: false`.
- Auth, invite, and workspace creation surfaces use the OXO paper/ink/card language and the updated Archivo/Newsreader typography. CSS tokens and auth surface styles were updated accordingly.

## Idempotence and Recovery

The edits are ordinary source edits and can be repeated safely. If a styling change has unintended blast radius, inspect `git diff -- frontend/src/styles.css frontend/src/App.tsx` and revert only the affected hunk, preserving unrelated user changes. Build output under `frontend/dist/` is ignored and can be regenerated with `npm run build`.

## Artifacts and Notes

The screenshot supplied by the user shows the old mismatch: a black rounded-square app icon beside a capitalized `Codesk` wordmark, followed by a clipped “Welcome back” heading. This plan treats that as the first acceptance target.

## Interfaces and Dependencies

No new libraries are introduced. The implementation relies on React in `frontend/src/App.tsx`, CSS in `frontend/src/styles.css`, and static SVG files in `frontend/public/`.
