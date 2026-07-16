// Onboarding engine — a pure module (no React), mirroring logic.ts. It owns the
// data-driven node model (plan §4): the node config, and the eligibility / trigger
// / completion derivation. The overlay (Onboarding.tsx) and the hook
// (useOnboarding.ts) consume these; WorkspaceApp supplies the live context.
//
// Completion is DERIVED from live state wherever the data exists (plan §6.1) — we
// never show "create a document" incomplete when a document exists. localStorage
// holds only the non-derivable seen/dismissed flags, delivered here as recorded
// `events`. The stable event keys (§4.2) are a contract — never `step_N`.

// ---- Event keys (stable contract, plan §4.2) ---------------------------------

export const ONBOARDING_EVENT_KEYS = [
  "account_intro_seen",
  "workspace_created",
  "first_document_created",
  "first_document_edited",
  "first_thread_created",
  "first_thread_replied",
  "local_environment_connected",
  "first_agent_created",
  "first_agent_run_started",
  "first_document_watcher_added",
  "member_invited",
] as const;

export type OnboardingEventKey = (typeof ONBOARDING_EVENT_KEYS)[number];

// ---- Schema (plan §4.1) ------------------------------------------------------

export type OnboardingScope = "account" | "workspace";

// The user role a node is scoped to. Empty `eligibleRoles` means "all roles" — the
// node is removed (not disabled) for anyone outside a non-empty list.
export type OnboardingRole = "owner" | "admin" | "member";

export type OnboardingTrigger =
  | { type: "workspace-empty" } // no documents yet
  | { type: "document-open" } // a document view is active (its spotlight target exists)
  | { type: "first-text-select" }; // the editor has a live text selection

// A completion condition. `derived` reads live signals (the source of truth);
// `flag` reads a recorded seen/dismissed/event key; `any` is satisfied if any child
// is (e.g. "dismissed OR a thread exists").
export type OnboardingCondition =
  | { via: "derived"; signal: DerivedSignal }
  | { via: "flag"; key: string }
  | { via: "acknowledge" } // completed once the node's VERSIONED seen flag is recorded
  | { via: "any"; of: OnboardingCondition[] };

export type DerivedSignal =
  | "document-exists"
  | "thread-exists"
  | "live-environment"
  | "agent-exists"
  | "agent-at-work";

// The UI actions a node's buttons can fire (distinct from the §4.2 completion event
// keys). A closed union so a typo dies at compile time and OnboardingNode stays
// structurally assignable to P2's OnboardingStep by construction — no cast/parser.
export type OnboardingActionEvent = "advance" | "back" | "complete" | "dismiss" | "open-thread-draft";

export type OnboardingAction = { label: string; event: OnboardingActionEvent };

// `spotlight` nodes form the guided sequence (step counter); `tip` nodes are
// standalone contextual callouts (no counter, one-time).
export type OnboardingPresentation = "spotlight" | "tip";

export type OnboardingNode = {
  id: string;
  version: number; // bump to re-show after a redesign
  scope: OnboardingScope;
  presentation: OnboardingPresentation;
  eligibleRoles: OnboardingRole[]; // empty = all
  trigger: OnboardingTrigger;
  completion: OnboardingCondition;
  targetOnboardingId?: string; // data-onboarding-id of the control to spotlight
  title: string;
  body: string;
  primaryAction?: OnboardingAction;
  secondaryAction?: OnboardingAction;
  skippable: boolean;
  fallback: "page-card" | "skip"; // when the target element is missing
};

export type OnboardingChecklistItem = {
  id: string;
  label: string;
  eligibleRoles: OnboardingRole[]; // empty = all
  completion: OnboardingCondition;
};

// ---- Live context ------------------------------------------------------------

// Signals derived by the host (WorkspaceApp / useOnboarding) from live data. NOTE:
// `liveEnvironmentCount` MUST be derived via daemonLiveStatus (receipt-elapsed
// liveness), never a raw `status:'online'` field — a stale daemon that still reads
// online must not satisfy `local_environment_connected`.
export type OnboardingLiveSignals = {
  documentCount: number;
  threadCount: number; // any thread created (a reply implies a thread)
  liveEnvironmentCount: number; // daemons live by receipt-elapsed liveness only
  agentCount: number;
  agentRunCount: number;
  agentThreadCount: number; // threads an agent participates in
  watchedDocumentCount: number;
};

export type OnboardingContext = {
  roles: OnboardingRole[];
  events: ReadonlySet<string>; // recorded seen/dismissed/event flags (localStorage)
  route: string; // active view, e.g. "document"
  selectionActive: boolean; // editor has a live text selection
  signals: OnboardingLiveSignals;
};

// ---- Node config (authoritative copy from onboarding-nodes-spec.md — locked) --

const created = (key: OnboardingEventKey, signal: DerivedSignal): OnboardingCondition => ({
  via: "any",
  of: [
    { via: "derived", signal }, // live state is the source of truth
    { via: "flag", key }, // belt-and-suspenders: the recorded event
  ],
});

const ACKNOWLEDGE: OnboardingCondition = { via: "acknowledge" };

// The versioned seen flag the host records when a node is acknowledged (Next/Done)
// or dismissed. The version is embedded so bumping OnboardingNode.version re-shows
// the node after a redesign — a v1 flag no longer satisfies the v2 node.
export function acknowledgeFlag(node: OnboardingNode): string {
  return `seen:${node.id}@v${node.version}`;
}

export const NODES: OnboardingNode[] = [
  // A. Guided spotlight sequence — scope workspace, all roles, 3 steps.
  {
    id: "create-first-document",
    version: 1,
    scope: "workspace",
    presentation: "spotlight",
    eligibleRoles: [],
    trigger: { type: "workspace-empty" },
    completion: created("first_document_created", "document-exists"),
    targetOnboardingId: "create-document",
    title: "These are real files",
    body: "What you write syncs to your computer as an actual file — open the same document in the browser or your local editor.",
    primaryAction: { label: "Next", event: "advance" },
    skippable: true,
    fallback: "page-card",
  },
  {
    id: "threads-intro",
    version: 1,
    scope: "workspace",
    presentation: "spotlight",
    eligibleRoles: [],
    trigger: { type: "document-open" },
    completion: ACKNOWLEDGE,
    targetOnboardingId: "document-threads",
    title: "Every discussion has a home",
    body: "Select any text to open a thread anchored to it. Discussion stays out of the document — open threads here anytime.",
    primaryAction: { label: "Next", event: "advance" },
    secondaryAction: { label: "Back", event: "back" },
    skippable: true,
    fallback: "skip",
  },
  {
    id: "watchers-intro",
    version: 1,
    scope: "workspace",
    presentation: "spotlight",
    eligibleRoles: [],
    trigger: { type: "document-open" },
    completion: ACKNOWLEDGE,
    targetOnboardingId: "document-watchers",
    title: "Let an agent keep watch",
    body: "Watchers are the agents following this document. When something relevant changes, it lands in their work queue — no need to ping them.",
    primaryAction: { label: "Done", event: "complete" },
    secondaryAction: { label: "Back", event: "back" },
    skippable: true,
    fallback: "skip",
  },
  // B. Contextual tip — standalone, account-scoped, not in the sequence.
  {
    id: "tip-first-selection",
    version: 1,
    scope: "account",
    presentation: "tip",
    eligibleRoles: [],
    trigger: { type: "first-text-select" },
    completion: {
      via: "any",
      of: [
        ACKNOWLEDGE, // dismissed with "Got it" (versioned)
        // Account-durable: recorded at ACCOUNT scope on thread creation, so once the
        // user has started a thread anywhere the tip never nags again after a switch
        // (plan §4.3 `has-used-thread` is an account-level fact). This is the account
        // leg that makes an account-scoped node honestly account-complete.
        { via: "flag", key: "first_thread_created" },
        // Convenience leg for workspaces that already had a thread before this shipped.
        { via: "derived", signal: "thread-exists" },
      ],
    },
    targetOnboardingId: "selection-thread",
    title: "Talk about this exact line",
    body: "Select any text to open a thread anchored right here — no need to write feedback into the document.",
    primaryAction: { label: "Start thread", event: "open-thread-draft" },
    secondaryAction: { label: "Got it", event: "dismiss" },
    skippable: true,
    fallback: "skip",
  },
];

// C. Getting-started checklist — scope workspace. Completion is derived from live
// data (never a stored "done") for every item EXCEPT `invite-team`: an invitation
// sent is not derivable from the member list until the invitee accepts, so deriving
// it would falsely read incomplete. It is the one flag-based item, by design.
export const CHECKLIST_ITEMS: OnboardingChecklistItem[] = [
  { id: "create-document", label: "Create your first document", eligibleRoles: [], completion: { via: "derived", signal: "document-exists" } },
  { id: "start-discussion", label: "Start a discussion", eligibleRoles: [], completion: { via: "derived", signal: "thread-exists" } },
  // Owner/admin only: connect/create actions require ManageDaemons / ManageAgents
  // (backend role.go), which members lack — showing them to a member is a task the
  // backend 403s. eligibleRoles here must mirror the authz matrix; the config↔authz
  // test in onboardingEngine.test.ts enforces it.
  { id: "connect-environment", label: "Connect a local environment", eligibleRoles: ["owner", "admin"], completion: { via: "derived", signal: "live-environment" } },
  { id: "create-agent", label: "Create your first agent", eligibleRoles: ["owner", "admin"], completion: { via: "derived", signal: "agent-exists" } },
  // All roles: "put an agent to work" is a start-run action, and handleStartAgentRunRequest
  // (backend server_agents.go) gates only requireHumanPrincipal, NOT ManageAgents — a member
  // CAN start a run on an existing agent, so this is an honest task for them (not a 403).
  { id: "agent-at-work", label: "Put an agent to work", eligibleRoles: [], completion: { via: "derived", signal: "agent-at-work" } },
  { id: "invite-team", label: "Invite your team", eligibleRoles: ["owner", "admin"], completion: { via: "flag", key: "member_invited" } },
];

// ---- Derivation --------------------------------------------------------------

function derivedSignal(signal: DerivedSignal, s: OnboardingLiveSignals): boolean {
  switch (signal) {
    case "document-exists":
      return s.documentCount > 0;
    case "thread-exists":
      return s.threadCount > 0;
    case "live-environment":
      return s.liveEnvironmentCount > 0;
    case "agent-exists":
      return s.agentCount > 0;
    case "agent-at-work":
      return s.watchedDocumentCount > 0 || s.agentRunCount > 0 || s.agentThreadCount > 0;
  }
}

function isSatisfied(
  cond: OnboardingCondition,
  ctx: OnboardingContext,
  node: OnboardingNode | OnboardingChecklistItem,
): boolean {
  switch (cond.via) {
    case "flag":
      return ctx.events.has(cond.key);
    case "acknowledge":
      // Only nodes (which carry a version) use acknowledge; checklist items never do.
      return "version" in node && ctx.events.has(acknowledgeFlag(node));
    case "derived":
      return derivedSignal(cond.signal, ctx.signals);
    case "any":
      return cond.of.some((c) => isSatisfied(c, ctx, node));
  }
}

function roleEligible(eligibleRoles: OnboardingRole[], roles: OnboardingRole[]): boolean {
  return eligibleRoles.length === 0 || eligibleRoles.some((r) => roles.includes(r));
}

function isTriggered(node: OnboardingNode, ctx: OnboardingContext): boolean {
  switch (node.trigger.type) {
    case "workspace-empty":
      return ctx.signals.documentCount === 0;
    case "document-open":
      return ctx.route === "document";
    case "first-text-select":
      return ctx.selectionActive;
  }
}

// ---- Public API (plan §5.1) --------------------------------------------------

// Nodes the user's role can ever see (removed, not disabled, for others).
export function eligibleNodes(ctx: OnboardingContext): OnboardingNode[] {
  return NODES.filter((n) => roleEligible(n.eligibleRoles, ctx.roles));
}

// The ordered guided-spotlight sequence for this role.
export function guideSteps(ctx: OnboardingContext): OnboardingNode[] {
  return eligibleNodes(ctx).filter((n) => n.presentation === "spotlight");
}

export function isComplete(node: OnboardingNode | OnboardingChecklistItem, ctx: OnboardingContext): boolean {
  return isSatisfied(node.completion, ctx, node);
}

// The next incomplete step in the guided sequence (drives the step counter), or
// null when the guide is finished. Does not consider triggers — it is "what's left".
export function nextIncomplete(ctx: OnboardingContext): OnboardingNode | null {
  return guideSteps(ctx).find((n) => !isComplete(n, ctx)) ?? null;
}

// The spotlight step to show right now: the first incomplete step, but only once
// its own trigger is met — so we wait (return null) for the document to open rather
// than skipping past step 2. Never jumps to a later step over an earlier incomplete
// one (the sequence is ordered).
export function activeNode(ctx: OnboardingContext): OnboardingNode | null {
  const step = nextIncomplete(ctx);
  if (!step) return null;
  return isTriggered(step, ctx) ? step : null;
}

// The standalone contextual tip to show, if any (independent of the guided
// sequence): the first eligible tip that is triggered and not yet complete.
export function activeTip(ctx: OnboardingContext): OnboardingNode | null {
  return (
    eligibleNodes(ctx).find((n) => n.presentation === "tip" && isTriggered(n, ctx) && !isComplete(n, ctx)) ?? null
  );
}

// The checklist for this role, each item with its live-derived done state.
export function checklistProgress(ctx: OnboardingContext): Array<{ item: OnboardingChecklistItem; done: boolean }> {
  return CHECKLIST_ITEMS.filter((i) => roleEligible(i.eligibleRoles, ctx.roles)).map((item) => ({
    item,
    done: isComplete(item, ctx),
  }));
}
