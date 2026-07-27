// @vitest-environment jsdom
//
// Interaction matrix for the CreateAgentModal model/effort selectors (daemon-model-selection
// task #4). Renders ModelProfileFields directly and drives the transitions Thomas required: the
// four honest capability states, the stale-selection rendering (retained displayName, never the
// opaque id), the atomic reset, the present-but-empty `error: ""` field-presence row, and the
// ambiguous-default effort gate. These are lane-local proofs; producer/consumer compatibility is
// the Phase 2 composition proof with real daemon bytes.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { CreateAgentModal, ModelProfileFields } from "./App";
import type { Daemon, RuntimeModelCatalog } from "./types";

afterEach(() => cleanup());

const sol = { model: "gpt-5.6-sol", displayName: "GPT-5.6-Sol", isDefault: true, reasoningEfforts: ["low", "medium", "high", "ultra"], defaultReasoningEffort: "low" };
const terra = { model: "gpt-5.6-terra", displayName: "GPT-5.6-Terra", isDefault: false, reasoningEfforts: ["low", "medium", "high"], defaultReasoningEffort: "medium" };
const READY: RuntimeModelCatalog = { models: [sol, terra] };

function daemonWith(catalog?: RuntimeModelCatalog): Daemon {
  return {
    id: "d1",
    name: "box",
    status: "active",
    createdAt: "2026-01-01T00:00:00Z",
    runtimes: [{ kind: "codex", available: true, ...(catalog ? { modelCatalog: catalog } : {}) }],
  } as Daemon;
}

type Props = Parameters<typeof ModelProfileFields>[0];

function renderFields(overrides: Partial<Props> = {}) {
  const props: Props = {
    daemon: daemonWith(READY),
    runtimeKind: "codex",
    model: "",
    modelLabel: "",
    reasoningEffort: "",
    onSelectModel: vi.fn(),
    onEffortChange: vi.fn(),
    onReset: vi.fn(),
    ...overrides,
  };
  const utils = render(<ModelProfileFields {...props} />);
  return { props, ...utils };
}

// The opaque provider id must never appear as VISIBLE TEXT (option value attributes are the submit
// value and are fine). This asserts the text content, matching Thomas's "no opaque id in the DOM".
function expectNoOpaqueIdText(id: string) {
  expect(screen.queryByText(id)).toBeNull();
  expect(document.body.textContent).not.toContain(id);
}

describe("ModelProfileFields — four honest states", () => {
  it("unsupported (no catalog) renders calm default copy, no selectors", () => {
    renderFields({ daemon: daemonWith(undefined) });
    expect(screen.getByText(/use this runtime's default/i)).toBeTruthy();
    expect(screen.queryByRole("combobox")).toBeNull();
  });

  it("error (probe failure) says so and promises a retry, not 'empty'", () => {
    renderFields({ daemon: daemonWith({ models: [], error: "probe timed out" }) });
    expect(screen.getByText(/couldn't load this runtime's models/i)).toBeTruthy();
  });

  it("present-but-empty error:\"\" still classifies as error (field presence, not truthiness)", () => {
    renderFields({ daemon: daemonWith({ models: [], error: "" }) });
    expect(screen.getByText(/couldn't load this runtime's models/i)).toBeTruthy();
    expect(screen.queryByText(/reports no models yet/i)).toBeNull();
  });

  it("empty (capable, no models) says exactly that, not 'not found'", () => {
    renderFields({ daemon: daemonWith({ models: [] }) });
    expect(screen.getByText(/reports no models yet/i)).toBeTruthy();
  });

  it("ready shows selectors: Runtime default + displayName options, opaque id not visible", () => {
    renderFields();
    expect(screen.getByText("Runtime default")).toBeTruthy();
    expect(screen.getByRole("option", { name: "GPT-5.6-Sol" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "GPT-5.6-Terra" })).toBeTruthy();
    // "Runtime default" is the guarantee; the resolved name is secondary observation.
    expect(screen.getByText(/currently GPT-5.6-Sol\. That default can change\./i)).toBeTruthy();
    expectNoOpaqueIdText("gpt-5.6-sol");
  });
});

describe("ModelProfileFields — selection + effort gating", () => {
  it("selecting a model reports (opaque id, displayName) so the label can be retained", () => {
    const onSelectModel = vi.fn();
    renderFields({ onSelectModel });
    fireEvent.change(screen.getByRole("combobox", { name: /model/i }), { target: { value: "gpt-5.6-terra" } });
    expect(onSelectModel).toHaveBeenCalledWith("gpt-5.6-terra", "GPT-5.6-Terra");
  });

  it("runtime-default effort is disabled when no unique default resolves (ambiguous)", () => {
    const ambiguous: RuntimeModelCatalog = { models: [sol, { ...terra, isDefault: true }] };
    renderFields({ daemon: daemonWith(ambiguous), model: "" });
    const effort = screen.getByRole("combobox", { name: /reasoning effort/i }) as HTMLSelectElement;
    expect(effort.disabled).toBe(true);
    expect(screen.getByText(/choose a specific model to set an explicit effort/i)).toBeTruthy();
  });

  it("an explicit model still derives its own efforts even when the default is ambiguous", () => {
    const ambiguous: RuntimeModelCatalog = { models: [sol, { ...terra, isDefault: true }] };
    renderFields({ daemon: daemonWith(ambiguous), model: "gpt-5.6-terra", modelLabel: "GPT-5.6-Terra" });
    const effort = screen.getByRole("combobox", { name: /reasoning effort/i }) as HTMLSelectElement;
    expect(effort.disabled).toBe(false);
  });

  it("provider-wide efforts work without an inferred default and explain provenance", () => {
    const claude: RuntimeModelCatalog = {
      models: [
        { model: "fable", displayName: "Fable", isDefault: false, reasoningEfforts: [] },
        { model: "opus", displayName: "Opus", isDefault: false, reasoningEfforts: [] },
        { model: "sonnet", displayName: "Sonnet", isDefault: false, reasoningEfforts: [] },
      ],
      modelProvenance: "curated",
      reasoningEfforts: ["low", "medium", "high", "xhigh", "max"],
      reasoningEffortProvenance: "detected",
    };
    renderFields({ daemon: daemonWith(claude), model: "" });
    const effort = screen.getByRole("combobox", { name: /reasoning effort/i }) as HTMLSelectElement;
    expect(effort.disabled).toBe(false);
    expect(screen.getByRole("option", { name: "xhigh" })).toBeTruthy();
    expect(screen.getByText(/model choices are curated; the provider verifies account access on first use/i)).toBeTruthy();
    expect(screen.getByText(/effort choices are detected from the installed CLI/i)).toBeTruthy();
  });

  it("a partial Claude catalog disables effort after explicit model selection and reports pending choices", () => {
    const partialClaude: RuntimeModelCatalog = {
      models: [
        { model: "fable", displayName: "Fable", isDefault: false, reasoningEfforts: [] },
        { model: "opus", displayName: "Opus", isDefault: false, reasoningEfforts: [] },
        { model: "sonnet", displayName: "Sonnet", isDefault: false, reasoningEfforts: [] },
      ],
      modelProvenance: "curated",
    };
    renderFields({ daemon: daemonWith(partialClaude), model: "fable", modelLabel: "Fable" });

    const effort = screen.getByRole("combobox", { name: /reasoning effort/i }) as HTMLSelectElement;
    expect(effort.disabled).toBe(true);
    expect(screen.getByText(/reasoning effort choices aren't available yet/i)).toBeTruthy();
    expect(screen.queryByText(/pin a specific effort/i)).toBeNull();
  });

  // The model-change dependent-reset branch: switching model drops an effort the new model doesn't
  // advertise, but keeps one it does. This is a distinct production path from the daemon/runtime
  // effect (it lives in the model <select> onChange), so it needs its own two rows.
  it("changing model clears an effort the newly selected model does not support", () => {
    const onEffortChange = vi.fn();
    renderFields({ model: "gpt-5.6-sol", modelLabel: "GPT-5.6-Sol", reasoningEffort: "ultra", onEffortChange });
    fireEvent.change(screen.getByRole("combobox", { name: /model/i }), { target: { value: "gpt-5.6-terra" } });
    expect(onEffortChange).toHaveBeenCalledWith(""); // terra has no "ultra"
  });

  it("changing model retains an effort the newly selected model still supports", () => {
    const onEffortChange = vi.fn();
    renderFields({ model: "gpt-5.6-sol", modelLabel: "GPT-5.6-Sol", reasoningEffort: "high", onEffortChange });
    fireEvent.change(screen.getByRole("combobox", { name: /model/i }), { target: { value: "gpt-5.6-terra" } });
    expect(onEffortChange).not.toHaveBeenCalled(); // terra still advertises "high"
  });
});

describe("ModelProfileFields — stale selection (background invalidation)", () => {
  it("a removed model renders the retained displayName disabled, never the opaque id, with a reset", () => {
    // Preserved explicit profile whose model vanished from a refreshed catalog.
    renderFields({
      daemon: daemonWith({ models: [sol] }),
      model: "gpt-5.6-terra",
      modelLabel: "GPT-5.6-Terra",
      reasoningEffort: "high",
    });
    expect(screen.getByText(/GPT-5.6-Terra/)).toBeTruthy();
    expectNoOpaqueIdText("gpt-5.6-terra");
    expect(screen.getByRole("button", { name: /use runtime default/i })).toBeTruthy();
    // No calm "using its default" copy while an explicit selection is preserved.
    expect(screen.queryByText(/using its default/i)).toBeNull();
  });

  it("a ready->error transition with a preserved profile is stale (not the calm error copy)", () => {
    renderFields({
      daemon: daemonWith({ models: [], error: "probe timed out" }),
      model: "gpt-5.6-sol",
      modelLabel: "GPT-5.6-Sol",
    });
    expect(screen.getByText(/GPT-5.6-Sol/)).toBeTruthy();
    expect(screen.getByText(/can't be confirmed right now/i)).toBeTruthy();
    expect(screen.getByRole("button", { name: /use runtime default/i })).toBeTruthy();
    expectNoOpaqueIdText("gpt-5.6-sol");
  });

  it("a dropped effort on a still-present model is stale", () => {
    const solNoUltra: RuntimeModelCatalog = { models: [{ ...sol, reasoningEfforts: ["low", "medium", "high"] }] };
    renderFields({ daemon: daemonWith(solNoUltra), model: "gpt-5.6-sol", modelLabel: "GPT-5.6-Sol", reasoningEffort: "ultra" });
    expect(screen.getByRole("button", { name: /use runtime default/i })).toBeTruthy();
    expect(screen.getByText(/GPT-5.6-Sol · ultra/)).toBeTruthy();
  });

  it("a stale runtime-default profile (empty model + effort) shows 'Runtime default', never 'Selected model'", () => {
    // Partial inheritance {model:"", effort:"ultra"}: the user chose Runtime default and pinned an
    // explicit effort. When the background default moves to a model without `ultra` the profile goes
    // stale, but the label must stay "Runtime default · ultra" — the user never picked a model, so
    // "Selected model · ultra" would misrepresent the choice. This pins the model==="" distinction.
    const defaultNoUltra: RuntimeModelCatalog = { models: [{ ...sol, reasoningEfforts: ["low", "medium", "high"] }] };
    renderFields({ daemon: daemonWith(defaultNoUltra), model: "", modelLabel: "", reasoningEffort: "ultra" });
    expect(screen.getByText(/Runtime default · ultra/)).toBeTruthy();
    expect(screen.queryByText(/Selected model/)).toBeNull();
    expect(screen.getByRole("button", { name: /use runtime default/i })).toBeTruthy();
  });

  it("a ready->unsupported transition with a preserved profile is stale (retains label, not the calm copy)", () => {
    // Background daemon.updated drops the whole catalog (old/incapable daemon). An explicit profile
    // is preserved+blocked, NOT collapsed to the calm no-explicit "uses its default" message.
    renderFields({ daemon: daemonWith(undefined), model: "gpt-5.6-sol", modelLabel: "GPT-5.6-Sol", reasoningEffort: "high" });
    expect(screen.getByText(/GPT-5.6-Sol/)).toBeTruthy();
    expect(screen.getByText(/no longer offers model selection/i)).toBeTruthy();
    expect(screen.getByRole("button", { name: /use runtime default/i })).toBeTruthy(); // calm state has no reset
    expectNoOpaqueIdText("gpt-5.6-sol");
  });

  it("a ready->empty transition with a preserved profile is stale", () => {
    renderFields({ daemon: daemonWith({ models: [] }), model: "gpt-5.6-sol", modelLabel: "GPT-5.6-Sol" });
    expect(screen.getByText(/GPT-5.6-Sol/)).toBeTruthy();
    expect(screen.getByText(/no longer reports any models/i)).toBeTruthy();
    expect(screen.getByRole("button", { name: /use runtime default/i })).toBeTruthy();
    expectNoOpaqueIdText("gpt-5.6-sol");
  });

  it("the reset button fires onReset (parent clears model + effort + label atomically)", () => {
    const onReset = vi.fn();
    renderFields({ daemon: daemonWith({ models: [sol] }), model: "gpt-5.6-terra", modelLabel: "GPT-5.6-Terra", onReset });
    fireEvent.click(screen.getByRole("button", { name: /use runtime default/i }));
    expect(onReset).toHaveBeenCalledTimes(1);
  });
});

// Modal-level rows Thomas required: the component onReset assertion only proves delegation, so the
// modal must prove that (a) create stays BLOCKED on a background-invalidated profile, and (b) one
// "Use runtime default" action clears the persisted model + effort + retained label and immediately
// RESTORES a valid create state — modal-owned atomic clearing, not just delegation.
describe("CreateAgentModal — model profile create-gating (modal-level)", () => {
  const onlineDaemon = (catalog: RuntimeModelCatalog, id = "d1", name = "Local"): Daemon =>
    ({
      id,
      workspaceId: "ws",
      name,
      status: "active",
      connectionStatus: "online",
      createdAt: "2026-01-01T00:00:00Z",
      runtimes: [{ kind: "codex", available: true, version: "1.0.0", modelCatalog: catalog }],
    }) as Daemon;

  const createButton = () => screen.getByRole("button", { name: /create agent/i }) as HTMLButtonElement;

  function fillRequired() {
    // The field <label> wraps span+input+hint, so the accessible name includes the hint — match by
    // role + a leading-anchored regex rather than an exact label string.
    fireEvent.change(screen.getByRole("textbox", { name: /^display name/i }), { target: { value: "Rev" } });
    fireEvent.change(screen.getByRole("textbox", { name: /^role/i }), { target: { value: "Reviewer" } });
  }

  it("blocks create on a background-invalidated profile; reset atomically restores a valid create state", () => {
    const api = { createAgent: vi.fn().mockResolvedValue({}) };
    const props = { api: api as never, workspaceId: "ws", onClose: vi.fn(), onDone: vi.fn() };
    const { rerender } = render(<CreateAgentModal {...props} daemons={[onlineDaemon(READY)]} />);
    fillRequired();
    // Explicitly pin a model the refreshed catalog will drop — valid while it exists.
    fireEvent.change(screen.getByRole("combobox", { name: /model/i }), { target: { value: "gpt-5.6-terra" } });
    expect(createButton().disabled).toBe(false);

    // Background daemon.updated drops terra (same daemonId → the selection is preserved, not reset).
    rerender(<CreateAgentModal {...props} daemons={[onlineDaemon({ models: [sol] })]} />);
    expect(createButton().disabled).toBe(true); // create blocked on the now-invalid profile
    expectNoOpaqueIdText("gpt-5.6-terra"); // stale display shows the retained label, never the id

    // One intentional reset clears model + effort + label and restores a valid create state.
    fireEvent.click(screen.getByRole("button", { name: /use runtime default/i }));
    expect(createButton().disabled).toBe(false);
    expectNoOpaqueIdText("gpt-5.6-terra");
  });

  const lastCreatePayload = (api: { createAgent: ReturnType<typeof vi.fn> }) => {
    const calls = api.createAgent.mock.calls;
    return calls[calls.length - 1]?.[2];
  };

  it("create submits the empty profile as model:'' + reasoningEffort:'' (full inheritance)", async () => {
    const api = { createAgent: vi.fn().mockResolvedValue({}) };
    render(<CreateAgentModal api={api as never} workspaceId="ws" onClose={vi.fn()} onDone={vi.fn()} daemons={[onlineDaemon(READY)]} />);
    fillRequired();
    fireEvent.click(createButton());
    await Promise.resolve();
    expect(api.createAgent).toHaveBeenCalledWith("ws", "d1", expect.objectContaining({ kind: "codex", model: "", reasoningEffort: "" }));
  });

  it("create submits a partial profile (runtime default model + explicit effort) as model:'' + the effort", async () => {
    const api = { createAgent: vi.fn().mockResolvedValue({}) };
    render(<CreateAgentModal api={api as never} workspaceId="ws" onClose={vi.fn()} onDone={vi.fn()} daemons={[onlineDaemon(READY)]} />);
    fillRequired();
    // Model left at Runtime default (""); pin an effort the unique default (sol) advertises.
    fireEvent.change(screen.getByRole("combobox", { name: /reasoning effort/i }), { target: { value: "low" } });
    fireEvent.click(createButton());
    await Promise.resolve();
    expect(lastCreatePayload(api)).toMatchObject({ model: "", reasoningEffort: "low" });
  });

  it("create submits a fully pinned profile as the opaque model id + effort", async () => {
    const api = { createAgent: vi.fn().mockResolvedValue({}) };
    render(<CreateAgentModal api={api as never} workspaceId="ws" onClose={vi.fn()} onDone={vi.fn()} daemons={[onlineDaemon(READY)]} />);
    fillRequired();
    fireEvent.change(screen.getByRole("combobox", { name: /model/i }), { target: { value: "gpt-5.6-terra" } });
    fireEvent.change(screen.getByRole("combobox", { name: /reasoning effort/i }), { target: { value: "high" } });
    fireEvent.click(createButton());
    await Promise.resolve();
    // The opaque id is the submitted value; the displayName is never sent.
    expect(lastCreatePayload(api)).toMatchObject({ model: "gpt-5.6-terra", reasoningEffort: "high" });
  });

  it("a user daemon change clears ONLY the model profile, keeping name/role", () => {
    const api = { createAgent: vi.fn().mockResolvedValue({}) };
    const props = { api: api as never, workspaceId: "ws", onClose: vi.fn(), onDone: vi.fn() };
    render(<CreateAgentModal {...props} daemons={[onlineDaemon(READY, "d1", "Local"), onlineDaemon(READY, "d2", "Build server")]} />);
    fillRequired();
    fireEvent.change(screen.getByRole("combobox", { name: /model/i }), { target: { value: "gpt-5.6-terra" } });
    expect((screen.getByRole("combobox", { name: /model/i }) as HTMLSelectElement).value).toBe("gpt-5.6-terra");

    // User picks a different daemon (a real daemonId change, not a background update) → dependent
    // model profile resets, but the name/role the user typed are independent and must survive.
    fireEvent.click(screen.getByRole("radio", { name: /build server/i }));
    expect((screen.getByRole("combobox", { name: /model/i }) as HTMLSelectElement).value).toBe("");
    expect((screen.getByRole("textbox", { name: /^display name/i }) as HTMLInputElement).value).toBe("Rev");
    expect((screen.getByRole("textbox", { name: /^role/i }) as HTMLInputElement).value).toBe("Reviewer");
  });

  it("a user runtime change clears the dependent model profile (distinct runtimeKind branch)", () => {
    const api = { createAgent: vi.fn().mockResolvedValue({}) };
    const twoRuntimes = {
      id: "d1",
      workspaceId: "ws",
      name: "Local",
      status: "active",
      connectionStatus: "online",
      createdAt: "2026-01-01T00:00:00Z",
      runtimes: [
        { kind: "codex", available: true, version: "1.0.0", modelCatalog: READY },
        { kind: "claude-code", available: true, version: "1.0.0", modelCatalog: READY },
      ],
    } as Daemon;
    render(<CreateAgentModal api={api as never} workspaceId="ws" onClose={vi.fn()} onDone={vi.fn()} daemons={[twoRuntimes]} />);
    fillRequired();
    fireEvent.change(screen.getByRole("combobox", { name: /model/i }), { target: { value: "gpt-5.6-terra" } });
    expect((screen.getByRole("combobox", { name: /model/i }) as HTMLSelectElement).value).toBe("gpt-5.6-terra");

    // Switch runtime codex -> claude-code (a real runtimeKind change): the reset effect's runtimeKind
    // dependency clears the dependent model profile, independently of the daemonId path proven above.
    const claudeRadio = screen.getByText("Claude Code").closest("label")!.querySelector('input[type="radio"]') as HTMLInputElement;
    fireEvent.click(claudeRadio);
    expect((screen.getByRole("combobox", { name: /model/i }) as HTMLSelectElement).value).toBe("");
  });
});
