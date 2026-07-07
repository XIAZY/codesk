// Backend-pinned workspace contract goldens, imported directly from the Go contract tier
// (backend/internal/notty/testdata/contract/*.json, produced by the TestContractWorkspace* rows under
// NOTTY_UPDATE_GOLDEN=1). Importing the real canonicalized bytes means a backend response-shape change
// breaks these fixtures — and the frontend tests built on them — instead of letting the frontend drift
// silently against a hand-copied shape. This is the frontend half of the contract loop #80 opened.
//
// Ids/timestamps/volatile numerics are canonical placeholders (<id-N>, <ts>, <n>), so these are SHAPE
// fixtures: assert structure and wiring, not literal values.
//
// Opted into the build exactly like daemonFixtures.ts: frontend/tsconfig.json "include" lists the files
// and resolveJsonModule (already on) types the import.
import workspaceGetEmpty from "../../backend/internal/notty/testdata/contract/workspace_get_empty.json";
import workspaceGetPopulated from "../../backend/internal/notty/testdata/contract/workspace_get_populated.json";

export const contractGoldens = {
  workspaceGetEmpty,
  workspaceGetPopulated,
};
