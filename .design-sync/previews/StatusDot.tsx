import { StatusDot } from "codesk-frontend";

const Labeled = ({ tone, label }: any) => (
  <span style={{ display: "flex", alignItems: "center", gap: 6 }}>
    <StatusDot tone={tone} />
    <span className="tiny muted">{label ?? tone}</span>
  </span>
);

export const Tones = () => (
  <div style={{ display: "flex", flexWrap: "wrap", gap: 16, padding: 12 }}>
    <Labeled tone="online" />
    <Labeled tone="working" />
    <Labeled tone="idle" />
    <Labeled tone="stale" />
    <Labeled tone="queued" />
    <Labeled tone="disconnected" />
    <Labeled tone="failed" />
    <Labeled tone="daemon" />
    <Labeled tone="unknown" label="unknown (fallback)" />
  </div>
);

export const InContext = () => (
  <div style={{ display: "flex", flexWrap: "wrap", gap: 8, padding: 12 }}>
    <span className="chip"><StatusDot tone="online" />build-daemon · online</span>
    <span className="chip"><StatusDot tone="stale" />Waiting for daemon to check in…</span>
    <span className="chip"><StatusDot tone="failed" />agent run failed</span>
  </div>
);
