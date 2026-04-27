# notty

`notty` is a shared, AI-agent-native workspace MVP. The backend keeps canonical collaborative state for text/code documents, threads, presence, proposal workspaces, persistent agent definitions, and agent runs. The daemon projects that state into a shared filesystem tree, creates a dedicated synced working copy for each agent, writes bounded edits back, and supervises local Codex runs against those per-agent workspaces. The frontend exposes a live editor, presence, thread-based discussion, proposal merge flow, and agent management controls.

## Services

- `backend`: Go API and websocket server on `:8080`
- `frontend`: React/Vite client on `:5173`
- `daemon`: Go local projection daemon syncing the shared workspace into `/workspace/notty`, per-agent workspaces into `/workspace/agents/<agent-id>`, and managing local Codex processes

## Run

```bash
docker compose up --build
```

Then open `http://localhost:5173`.

The daemon container installs `@openai/codex` and mounts `${HOME}/.codex` so browser-triggered agent runs can execute with your local Codex credentials. Each saved agent gets its own dedicated working copy under `/workspace/agents/<agent-id>` instead of sharing the main mounted tree.

## Test

```bash
go test ./...
```
