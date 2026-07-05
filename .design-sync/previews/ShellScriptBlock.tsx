import { ShellScriptBlock } from "codesk-frontend";

export const Default = () => (
  <div style={{ maxWidth: 520, padding: 8 }}>
    <ShellScriptBlock
      title="Install daemon"
      badge="Host native"
      command={'curl -fsSL https://codesk.app/install.sh | sh -s -- \\\n  --workspace acme \\\n  --token nottyd_9f3ab21c'}
    >
      <p className="small muted">
        This downloads the release bundle, installs the daemon and agent helper, writes daemon
        config, and starts a local service.
      </p>
    </ShellScriptBlock>
  </div>
);

export const NoBadge = () => (
  <div style={{ maxWidth: 520, padding: 8 }}>
    <ShellScriptBlock
      title="Update the Codex CLI"
      command="npm install -g @openai/codex@latest"
    />
  </div>
);
