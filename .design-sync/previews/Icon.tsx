import { Icon } from "codesk-frontend";

const NAMES = [
  "home",
  "back",
  "activity",
  "thread",
  "people",
  "search",
  "plus",
  "refresh",
  "stack",
  "doc",
  "chevron",
  "caret",
  "daemon",
  "agent",
  "share",
  "more",
];

export const AllIcons = () => (
  <div style={{ display: "flex", flexWrap: "wrap", gap: 16, padding: 12, maxWidth: 480 }}>
    {NAMES.map((name) => (
      <span
        key={name}
        style={{ display: "flex", flexDirection: "column", alignItems: "center", gap: 4, width: 64 }}
      >
        <Icon name={name} />
        <span className="tiny muted">{name}</span>
      </span>
    ))}
  </div>
);
