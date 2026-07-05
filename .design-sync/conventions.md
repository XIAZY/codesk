# Codesk design system — conventions

Codesk is a warm, editorial, paper-and-ink interface: serif display headings (Newsreader), Archivo for UI text, JetBrains Mono for code. Fonts load via the Google Fonts `@import` already in `styles.css` — no setup needed.

## Setup

No provider or wrapper is required — every component styles itself from the global stylesheet. The page background should come from the system: `:root`/`body` rules in `styles.css` set the paper gradient and base typography automatically. `Modal` renders its own `position: fixed` full-viewport backdrop — mount it at the top level of the screen, conditionally, with an `onClose` handler.

## Styling idiom

Class-based CSS on plain elements: a small utility vocabulary plus semantic component classes, colored exclusively through CSS custom properties. Never invent new class names and never hard-code colors — compose from these (all verified against the shipped stylesheet):

| Family | Classes |
|---|---|
| Surfaces | `card`, `card lifted`, `p-12`, `p-20`, `p-24` |
| Layout | `row`, `col`, `between`, `gap-2`, `gap-6`, `gap-8`, `gap-12`, `min-0`, `truncate` |
| Type | `display` (serif headings), `small`, `tiny`, `muted`, `label`, `code` (mono block) |
| Buttons | `btn` + modifiers `accent`, `ghost`, `sm`, `lg`, `full`, `icon`; inline links: `btn-link` |
| Forms | `form-stack`, `field`, `lab`, `error-text` |
| Status | `chip`, `chip sm`, `chip sm accent`, `status-dot <tone>` (tones: `online`/`working`/`idle` green, `stale`/`queued` amber, `disconnected`/`failed` red, `daemon` teal), `avi` (avatar tile) |

Key tokens (`var(--…)`): ink scale `--ink`, `--ink-2`…`--ink-4`; paper scale `--paper`, `--paper-2`, `--paper-3`; `--border`, `--border-strong`; brand accents `--accent` (amber), `--agent` (sky), `--iris` (violet), `--daemon` (teal), each with light variants (`--accent-50` etc.); semantic `--ok`, `--warn`, `--err`; fonts `--display`, `--sans`, `--mono`; radii `--r-1`…`--r-4`; elevation `--shadow`, `--shadow-2`.

## Where the truth lives

Read `styles.css` (and its `@import`ed `_ds_bundle.css`) before styling anything — it is the complete, authoritative class and token index. Each component's `components/general/<Name>/<Name>.prompt.md` shows its props and a working composition.

## Idiomatic example

```jsx
const { Modal, MetricCard, StatusDot } = window.Codesk;

function DaemonHealth({ onClose }) {
  return (
    <Modal title="Daemon health" onClose={onClose}>
      <div className="col gap-8">
        <div className="row gap-8">
          <MetricCard label="Synced docs" value={128} tone="ok" />
          <MetricCard label="Pending runs" value={3} tone="warn" />
        </div>
        <div className="row gap-6">
          <StatusDot tone="online" />
          <span className="small muted">build-daemon · online</span>
        </div>
        <button className="btn accent full" type="button">Restart daemon</button>
      </div>
    </Modal>
  );
}
```
