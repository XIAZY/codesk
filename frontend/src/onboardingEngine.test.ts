import { describe, it, expect } from "vitest";
import {
  NODES,
  CHECKLIST_ITEMS,
  ONBOARDING_EVENT_KEYS,
  eligibleNodes,
  guideSteps,
  activeNode,
  activeTip,
  activeChapter,
  chapterSteps,
  nextChapterStep,
  hasChapter,
  nextIncomplete,
  isComplete,
  checklistProgress,
  acknowledgeFlag,
  type OnboardingCondition,
  type OnboardingContext,
  type OnboardingLiveSignals,
  type OnboardingRole,
} from "./onboardingEngine";

const emptySignals: OnboardingLiveSignals = {
  documentCount: 0,
  threadCount: 0,
  liveEnvironmentCount: 0,
  agentCount: 0,
};

function ctx(overrides: Partial<OnboardingContext> = {}): OnboardingContext {
  const { signals, ...rest } = overrides;
  return {
    roles: ["owner"] as OnboardingRole[],
    events: new Set<string>(),
    route: "workspace",
    selectionActive: false,
    ...rest,
    signals: { ...emptySignals, ...(signals ?? {}) },
  };
}

const node = (id: string) => NODES.find((n) => n.id === id)!;
const seen = (id: string) => acknowledgeFlag(node(id));

describe("onboarding event keys (§4.2 contract)", () => {
  it("has the 11 stable keys and never a step_N key", () => {
    expect(ONBOARDING_EVENT_KEYS).toHaveLength(11);
    expect(ONBOARDING_EVENT_KEYS).toContain("first_document_created");
    expect(ONBOARDING_EVENT_KEYS).toContain("local_environment_connected");
    for (const key of ONBOARDING_EVENT_KEYS) {
      expect(key).not.toMatch(/step[_ ]?\d/i);
    }
  });
});

describe("node config integrity (§4.1)", () => {
  it("every node has a stable id, version, and a valid fallback", () => {
    for (const n of NODES) {
      expect(n.id).toBeTruthy();
      expect(n.version).toBeGreaterThanOrEqual(1);
      expect(["page-card", "skip"]).toContain(n.fallback);
      expect(typeof n.skippable).toBe("boolean");
    }
    // The guided sequence is exactly the 2 ruled spotlight steps (#56: watchers left it).
    expect(guideSteps(ctx()).map((n) => n.id)).toEqual(["create-first-document", "threads-intro"]);
  });

  it("teaching nodes complete on acknowledge; the action node derives", () => {
    // create-first-document is derivable; teaching nodes are not (need a flag).
    expect(isComplete(node("create-first-document"), ctx({ signals: { ...emptySignals, documentCount: 1 } }))).toBe(true);
    expect(isComplete(node("threads-intro"), ctx({ signals: { ...emptySignals, documentCount: 5 } }))).toBe(false);
    expect(isComplete(node("threads-intro"), ctx({ events: new Set([seen("threads-intro")]) }))).toBe(true);
  });

  it("bumping a node's version re-shows it — the acknowledge flag is version-scoped", () => {
    const n = node("threads-intro");
    expect(acknowledgeFlag(n)).toBe(`seen:${n.id}@v${n.version}`);
    // Acknowledged at the current version → complete.
    expect(isComplete(n, ctx({ events: new Set([acknowledgeFlag(n)]) }))).toBe(true);
    // A prior-version flag (as a redesign would leave) does NOT satisfy → re-shows.
    expect(isComplete(n, ctx({ events: new Set([`seen:${n.id}@v${n.version - 1}`]) }))).toBe(false);
    // An unversioned flag likewise does not satisfy.
    expect(isComplete(n, ctx({ events: new Set([`seen:${n.id}`]) }))).toBe(false);
  });

  // Class guard (Tom): every derived signal answers a WORKSPACE-live question, so an
  // account-scoped node that could ONLY complete via a derived leg would re-nag after a
  // workspace switch. An account node must carry an account-durable (acknowledge/flag)
  // leg. This forbids the tip-first-selection class, not just the one instance.
  it("every account-scoped node has an account-durable (non-derived) completion leg", () => {
    const hasNonDerivedLeg = (cond: OnboardingCondition): boolean => {
      switch (cond.via) {
        case "acknowledge":
        case "flag":
          return true;
        case "derived":
          return false;
        case "any":
        case "all":
          return cond.of.some(hasNonDerivedLeg);
      }
    };
    const accountNodes = NODES.filter((n) => n.scope === "account");
    expect(accountNodes.length).toBeGreaterThan(0); // guard: the rule has something to check
    for (const n of accountNodes) {
      expect(hasNonDerivedLeg(n.completion)).toBe(true);
    }
  });

  it("tip-first-selection is account-durable: a recorded first_thread_created completes it", () => {
    const tip = node("tip-first-selection");
    expect(tip.scope).toBe("account");
    // The account-scope flag recorded on thread creation completes the tip across a
    // workspace switch, even where the current workspace has no thread (thread-exists false).
    expect(isComplete(tip, ctx({ events: new Set(["first_thread_created"]) }))).toBe(true);
    // Nothing recorded + no live thread → still open (would recur, which the flag prevents).
    expect(isComplete(tip, ctx())).toBe(false);
  });
});

describe("eligibility by role (§4.1 — removed, not disabled)", () => {
  it("guide + tip nodes are role-agnostic (chapter nodes are role-scoped — see chapter tests)", () => {
    const allGuideTips = NODES.filter((n) => n.presentation !== "chapter").map((n) => n.id);
    const memberGuideTips = eligibleNodes(ctx({ roles: ["member"] }))
      .filter((n) => n.presentation !== "chapter")
      .map((n) => n.id);
    // A member sees every non-chapter node (all eligibleRoles []); chapter role-gating
    // is asserted separately.
    expect(memberGuideTips).toEqual(allGuideTips);
  });

  it("checklist folds the agent rows into ONE counted owner/admin chapter entry; members get NO AI entry (#90)", () => {
    const owner = checklistProgress(ctx({ roles: ["owner"] })).map((c) => c.item.id);
    // Owner/admin: the two passive tasks + the "Add an AI teammate" entry + invite-team (4).
    expect(owner).toEqual(["create-document", "start-discussion", "add-teammate-entry", "invite-team"]);
    // The old per-action rows are gone (folded into the chapter).
    expect(owner).not.toContain("connect-environment");
    expect(owner).not.toContain("create-agent");
    expect(owner).not.toContain("agent-at-work");

    // Member with no agents: no AI entry at all, denominator is 2.
    expect(checklistProgress(ctx({ roles: ["member"] })).map((c) => c.item.id)).toEqual([
      "create-document",
      "start-discussion",
    ]);
    // #90 member regression: a member NEVER gets an AI onboarding item, even once an agent
    // exists — work-with-agent-entry was removed (an incomplete row is a completion demand,
    // not an offer). The denominator stays 2.
    expect(
      checklistProgress(ctx({ roles: ["member"], signals: { ...emptySignals, agentCount: 1 } })).map((c) => c.item.id),
    ).toEqual(["create-document", "start-discussion"]);
    // The removed entry id is gone from the config entirely.
    expect(CHECKLIST_ITEMS.some((i) => i.id === "work-with-agent-entry")).toBe(false);
  });
});

describe("completion derivation (§6.1 — live state is the source of truth)", () => {
  it("first_document_created derives from documentCount and also honors the recorded event", () => {
    const n = node("create-first-document");
    expect(isComplete(n, ctx())).toBe(false); // no doc, no event
    expect(isComplete(n, ctx({ signals: { ...emptySignals, documentCount: 1 } }))).toBe(true); // derived
    expect(isComplete(n, ctx({ events: new Set(["first_document_created"]) }))).toBe(true); // belt-and-suspenders
  });

  it("local_environment is satisfied only by a receipt-live daemon count, never raw status", () => {
    // The chapter's connect step completes on liveEnvironmentCount (host derives it via
    // daemonLiveStatus). A stale-but-'online' daemon contributes 0 here by contract.
    const connect = node("add-teammate-connect");
    expect(isComplete(connect, ctx({ signals: { ...emptySignals, liveEnvironmentCount: 0 } }))).toBe(false);
    expect(isComplete(connect, ctx({ signals: { ...emptySignals, liveEnvironmentCount: 1 } }))).toBe(true);
  });

  it("the teammate Done card completes on env AND agent (the #90 oracle — no manufactured run)", () => {
    // The terminal Done card's completion is the AND of a live environment and an agent
    // existing — the banned agent-at-work signal is gone entirely.
    const done = node("add-teammate-done");
    expect(isComplete(done, ctx())).toBe(false);
    expect(isComplete(done, ctx({ signals: { ...emptySignals, liveEnvironmentCount: 1 } }))).toBe(false); // env only
    expect(isComplete(done, ctx({ signals: { ...emptySignals, agentCount: 1 } }))).toBe(false); // agent only
    expect(isComplete(done, ctx({ signals: { ...emptySignals, liveEnvironmentCount: 1, agentCount: 1 } }))).toBe(true);
  });

  it("the checklist entry completes at agent-exists (agent ready) — NOT any run/at-work signal (#90)", () => {
    const entry = CHECKLIST_ITEMS.find((i) => i.id === "add-teammate-entry")!;
    expect(isComplete(entry, ctx())).toBe(false);
    // An agent existing (created) is the whole bar — no run to kick off.
    expect(isComplete(entry, ctx({ signals: { ...emptySignals, agentCount: 1 } }))).toBe(true);
    // The banned agent-at-work signal (and its inputs) are gone — grep guard.
    expect(JSON.stringify(entry.completion)).not.toContain("agent-at-work");
  });
});

describe("guided sequence: activeNode / nextIncomplete", () => {
  it("empty workspace spotlights create-first-document", () => {
    const c = ctx(); // documentCount 0 → workspace-empty triggered
    expect(activeNode(c)?.id).toBe("create-first-document");
  });

  it("after a document exists, waits (null) until a document is open, then shows threads-intro", () => {
    const withDoc = ctx({ signals: { ...emptySignals, documentCount: 1 } });
    // create-first-document is complete; threads-intro's trigger (document-open) not met yet.
    expect(nextIncomplete(withDoc)?.id).toBe("threads-intro");
    expect(activeNode(withDoc)).toBeNull();

    const onDoc = ctx({ signals: { ...emptySignals, documentCount: 1 }, route: "document" });
    expect(activeNode(onDoc)?.id).toBe("threads-intro");
  });

  it("finishes after threads-intro is acknowledged — the guide is 2 steps, watchers no longer follows", () => {
    const c = ctx({
      signals: { ...emptySignals, documentCount: 1 },
      route: "document",
      events: new Set([seen("threads-intro")]),
    });
    // create-first-document complete (doc exists) + threads-intro acknowledged → done.
    expect(nextIncomplete(c)).toBeNull();
    expect(activeNode(c)).toBeNull();
  });

  it("never jumps past an earlier incomplete step to a later triggered one", () => {
    // threads-intro not seen but a document is open (its trigger). create-first-document
    // is still incomplete only when no doc exists; here a doc exists so step 1 is done and
    // activeNode is threads-intro — never a later/other node.
    const c = ctx({ signals: { ...emptySignals, documentCount: 1 }, route: "document" });
    expect(activeNode(c)?.id).toBe("threads-intro");
  });
});

describe("contextual tip (standalone, not in the sequence)", () => {
  it("shows on first text selection, hides once dismissed or a thread exists", () => {
    expect(activeTip(ctx({ selectionActive: true }))?.id).toBe("tip-first-selection");
    expect(activeTip(ctx({ selectionActive: false }))).toBeNull();
    expect(activeTip(ctx({ selectionActive: true, events: new Set([seen("tip-first-selection")]) }))).toBeNull();
    expect(activeTip(ctx({ selectionActive: true, signals: { ...emptySignals, threadCount: 1 } }))).toBeNull();
  });

  it("the tip is not part of the guided step sequence", () => {
    expect(guideSteps(ctx()).some((n) => n.id === "tip-first-selection")).toBe(false);
  });
});

describe("watchers-intro relocated as a contextual tip (#56)", () => {
  it("left the guided sequence and is now a tip", () => {
    expect(guideSteps(ctx()).some((n) => n.id === "watchers-intro")).toBe(false);
    expect(node("watchers-intro").presentation).toBe("tip");
  });

  it("keeps id + version 1 so the existing ack carries — zero migration, no re-show", () => {
    const n = node("watchers-intro");
    expect(n.version).toBe(1);
    expect(acknowledgeFlag(n)).toBe("seen:watchers-intro@v1");
    // A user who acknowledged the OLD spotlight form has exactly this flag → still complete,
    // so the relocated tip never re-appears for them.
    expect(isComplete(n, ctx({ events: new Set(["seen:watchers-intro@v1"]) }))).toBe(true);
  });

  it("surfaces only with a document open AND an agent — never before an agent exists", () => {
    const withAgent = { ...emptySignals, agentCount: 1 };
    // document open but no agent → inert (watchers make no sense without an agent).
    expect(activeTip(ctx({ route: "document" }))).toBeNull();
    // an agent but no document open → inert.
    expect(activeTip(ctx({ signals: withAgent }))).toBeNull();
    // document open + agent + not yet acknowledged → shows.
    expect(activeTip(ctx({ route: "document", signals: withAgent }))?.id).toBe("watchers-intro");
    // once acknowledged → never again.
    expect(
      activeTip(ctx({ route: "document", signals: withAgent, events: new Set([seen("watchers-intro")]) })),
    ).toBeNull();
  });
});

describe("Add an AI teammate chapter (#56, reshaped #90)", () => {
  const owner: OnboardingRole[] = ["owner"];
  const member: OnboardingRole[] = ["member"];
  const withEnv = { ...emptySignals, liveEnvironmentCount: 1 };
  const withAgent = { ...withEnv, agentCount: 1 }; // env + agent → activation complete

  it("owner/admin: the chapter walks connect → create → Done on live signals (no work step)", () => {
    // 0 environments → step 1 (connect).
    expect(activeChapter(ctx({ roles: owner }))?.id).toBe("add-teammate-connect");
    // environment, no agent → step 2 (create).
    expect(activeChapter(ctx({ roles: owner, signals: withEnv }))?.id).toBe("add-teammate-create");
    // env + agent → the terminal Done card directly (there is no third "work" step).
    expect(activeChapter(ctx({ roles: owner, signals: withAgent }))?.id).toBe("add-teammate-done");
    // The banned step is gone from the config.
    expect(NODES.some((n) => n.id === "add-teammate-work")).toBe(false);
  });

  it("owner/admin chapter is 2 ordered steps + a terminal Done card", () => {
    expect(chapterSteps(ctx({ roles: owner })).map((n) => n.id)).toEqual([
      "add-teammate-connect",
      "add-teammate-create",
    ]);
  });

  it("member has NO chapter at all — even once an agent exists (#90)", () => {
    // The member chapter cards were removed; a member sees no steps and no card, ever.
    expect(chapterSteps(ctx({ roles: member })).map((n) => n.id)).toEqual([]);
    expect(activeChapter(ctx({ roles: member }))).toBeNull();
    expect(hasChapter(ctx({ roles: member }))).toBe(false);
    // Not even with an agent present (the old "work with an agent" card is gone).
    expect(activeChapter(ctx({ roles: member, signals: withAgent }))).toBeNull();
    expect(hasChapter(ctx({ roles: member, signals: withAgent }))).toBe(false);
    // The orphaned member nodes are deleted from the config.
    expect(NODES.some((n) => n.id === "add-teammate-member")).toBe(false);
    expect(NODES.some((n) => n.id === "add-teammate-member-done")).toBe(false);
  });

  it("Done is live-derived (#90): if the environment regresses, the frontier walks back — no stored done", () => {
    // env + agent → Done. Then the environment goes offline (agent still created): the Done
    // card's preconditions no longer hold, so activeChapter honestly returns the connect
    // frontier again. No stored completion flag defeats live truth (drops the old
    // agent-at-work "history doesn't un-happen" semantics — that signal is banned).
    expect(activeChapter(ctx({ roles: owner, signals: withAgent }))?.id).toBe("add-teammate-done");
    const envRegressed = { ...emptySignals, liveEnvironmentCount: 0, agentCount: 1 };
    expect(activeChapter(ctx({ roles: owner, signals: envRegressed }))?.id).toBe("add-teammate-connect");
  });

  it("eligibleRoles stays authz-pure; the owner/admin audience lives in `audience`", () => {
    const byId = (id: string) => NODES.find((n) => n.id === id)!;
    // Only connect/create are backend-gated (ManageDaemons/ManageAgents).
    expect(byId("add-teammate-connect").eligibleRoles).toEqual(["owner", "admin"]);
    expect(byId("add-teammate-create").eligibleRoles).toEqual(["owner", "admin"]);
    // Closing the Done card is all-roles — eligibleRoles empty; the audience is owner-admin.
    expect(byId("add-teammate-done").eligibleRoles).toEqual([]);
    expect(byId("add-teammate-done").audience).toBe("owner-admin");
    expect(byId("add-teammate-connect").audience).toBe("owner-admin");
  });

  it("only owner/admin have a chapter; a member never does (audience gate, not eligibleRoles)", () => {
    expect(hasChapter(ctx({ roles: owner, signals: withAgent }))).toBe(true);
    expect(hasChapter(ctx({ roles: ["admin"], signals: withAgent }))).toBe(true);
    expect(hasChapter(ctx({ roles: member, signals: withAgent }))).toBe(false);
    // The member exclusion is purely presentation (audience) — eligibleRoles never lies.
    expect(eligibleNodes(ctx({ roles: member })).map((n) => n.id)).not.toContain("add-teammate-connect");
  });

  it("chapter cards never auto-surface as a guide spotlight or a tip", () => {
    // Even with every trigger condition true, activeNode/activeTip must not return a chapter card.
    const full = ctx({
      roles: owner,
      route: "document",
      selectionActive: true,
      signals: { ...withAgent, documentCount: 1 },
    });
    expect(activeNode(full)?.presentation).not.toBe("chapter");
    expect(activeTip(full)?.presentation).not.toBe("chapter");
  });

  it("nextChapterStep is the #90 'activation incomplete' oracle: non-null while incomplete, null when complete", () => {
    expect(nextChapterStep(ctx({ roles: owner }))?.id).toBe("add-teammate-connect"); // no env
    expect(nextChapterStep(ctx({ roles: owner, signals: withEnv }))?.id).toBe("add-teammate-create"); // env, no agent
    expect(nextChapterStep(ctx({ roles: owner, signals: withAgent }))).toBeNull(); // complete → never auto-open
    expect(nextChapterStep(ctx({ roles: member, signals: withAgent }))).toBeNull(); // member has no chapter
  });
});

describe("checklist (§C — derived, never a stored done)", () => {
  it("reflects live completion per item; the chapter entry is done once an agent exists (#90)", () => {
    const c = ctx({
      signals: { ...emptySignals, documentCount: 1, threadCount: 1, agentCount: 1 },
    });
    const done = new Map(checklistProgress(c).map((r) => [r.item.id, r.done]));
    expect(done.get("create-document")).toBe(true);
    expect(done.get("start-discussion")).toBe(true);
    // The chapter entry now completes at agent-exists — an agent existing IS enough (#90).
    expect(done.get("add-teammate-entry")).toBe(true);
    expect(done.get("invite-team")).toBe(false);
  });

  it("the chapter entry is incomplete until an agent exists — same live bar as the chapter", () => {
    const beforeAgent = ctx({ signals: { ...emptySignals, liveEnvironmentCount: 1 } }); // env only
    expect(new Map(checklistProgress(beforeAgent).map((r) => [r.item.id, r.done])).get("add-teammate-entry")).toBe(false);
    const withAgent = ctx({ signals: { ...emptySignals, agentCount: 1 } });
    expect(new Map(checklistProgress(withAgent).map((r) => [r.item.id, r.done])).get("add-teammate-entry")).toBe(true);
  });
});
