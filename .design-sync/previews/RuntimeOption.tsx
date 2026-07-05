import { RuntimeOption } from "codesk-frontend";

const noop = () => {};

const codexEntry = { kind: "codex", label: "Codex", monogram: "Cx", tile: "codex", supported: true } as any;
const claudeEntry = { kind: "claude-code", label: "Claude Code", monogram: "CC", tile: "claude", supported: false } as any;
const piEntry = { kind: "pi", label: "pi", monogram: "π", tile: "pi", supported: false } as any;
const opencodeEntry = { kind: "opencode", label: "opencode", monogram: "oc", supported: false } as any;

const tile = (entry: any, availability: string, meta: string) => ({ entry, availability, meta }) as any;

const Grid = ({ children }: any) => (
  <div className="runtime-grid" style={{ maxWidth: 440, padding: 8 }}>
    {children}
  </div>
);

export const AvailabilitySweep = () => (
  <Grid>
    <RuntimeOption
      tile={tile(codexEntry, "available", "codex 0.24.0")}
      selected
      daemonSelected
      onSelect={noop}
      onExplain={noop}
    />
    <RuntimeOption
      tile={tile(claudeEntry, "coming_soon", "Coming soon to Codesk")}
      selected={false}
      daemonSelected
      onSelect={noop}
      onExplain={noop}
    />
    <RuntimeOption
      tile={tile(piEntry, "coming_soon", "Coming soon to Codesk")}
      selected={false}
      daemonSelected
      onSelect={noop}
      onExplain={noop}
    />
    <RuntimeOption
      tile={tile(opencodeEntry, "coming_soon", "Coming soon to Codesk")}
      selected={false}
      daemonSelected
      onSelect={noop}
      onExplain={noop}
    />
  </Grid>
);

export const AvailableUnselected = () => (
  <Grid>
    <RuntimeOption
      tile={tile(codexEntry, "available", "codex 0.24.0")}
      selected={false}
      daemonSelected
      onSelect={noop}
      onExplain={noop}
    />
  </Grid>
);

export const Unavailable = () => (
  <Grid>
    <RuntimeOption
      tile={tile(codexEntry, "not_installed", "Not installed on host")}
      selected={false}
      daemonSelected
      onSelect={noop}
      onExplain={noop}
    />
    <RuntimeOption
      tile={tile(codexEntry, "update_required", "Requires codex >= 0.20 (found 0.13.2)")}
      selected={false}
      daemonSelected
      onSelect={noop}
      onExplain={noop}
    />
    <RuntimeOption
      tile={tile(codexEntry, "not_installed", "Not installed on host")}
      selected={false}
      daemonSelected={false}
      onSelect={noop}
      onExplain={noop}
    />
  </Grid>
);
