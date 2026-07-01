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
