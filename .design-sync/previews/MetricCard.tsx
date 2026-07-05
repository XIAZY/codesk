import { MetricCard } from "codesk-frontend";

export const Default = () => (
  <div style={{ maxWidth: 180 }}>
    <MetricCard label="Documents" value={24} />
  </div>
);

export const Tones = () => (
  <div style={{ display: "flex", gap: 12 }}>
    <MetricCard label="Synced docs" value={128} tone="ok" />
    <MetricCard label="Pending runs" value={3} tone="warn" />
    <MetricCard label="Failed runs" value={1} tone="err" />
  </div>
);
