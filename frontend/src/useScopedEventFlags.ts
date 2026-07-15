import { useCallback, useMemo, useState } from "react";
import type { OnboardingScope } from "./onboardingEngine";

// The single owner of onboarding event flags (plan §6.1). Both the engine context
// (via useOnboardingController) and useOnboarding read from THIS one store, keyed by
// the current account+workspace scope — so a workspace or account switch rehydrates
// and a recorded flag can never leak across scopes. localStorage is the source of
// truth; a version counter bumped on each write triggers a keyed re-read, so there is
// no second mutable set that can drift out of sync (the defect this store replaces).

function readStored(key: string): ReadonlySet<string> {
  if (!key || typeof window === "undefined") return new Set();
  try {
    const value: unknown = JSON.parse(window.localStorage.getItem(key) ?? "[]");
    return new Set(Array.isArray(value) ? value.filter((event): event is string => typeof event === "string") : []);
  } catch {
    return new Set();
  }
}

function persist(key: string, events: ReadonlySet<string>) {
  if (!key || typeof window === "undefined") return;
  try {
    window.localStorage.setItem(key, JSON.stringify([...events].sort()));
  } catch {
    // ignore storage failures (private mode) — flags just don't persist this session.
  }
}

export type ScopedEventFlags = {
  events: ReadonlySet<string>;
  record: (event: string, scope: OnboardingScope) => void;
};

// accountFlagsKey is keyed by accountId, workspaceFlagsKey by accountId×workspaceId —
// so account-scoped flags survive a workspace switch (same account) but not an account
// switch, and workspace flags are isolated per user × workspace (plan §4.3/§6.1).
export function useScopedEventFlags(accountFlagsKey: string, workspaceFlagsKey: string): ScopedEventFlags {
  const [version, setVersion] = useState(0);
  // Re-read on any key change (scope switch) OR version bump (a record). The merged
  // set is the engine's event view for the CURRENT scope — the engine treats flags as
  // opaque strings, so which scoped key they came from doesn't matter to membership.
  const events = useMemo(
    () => new Set([...readStored(accountFlagsKey), ...readStored(workspaceFlagsKey)]),
    [accountFlagsKey, workspaceFlagsKey, version],
  );
  const record = useCallback(
    (event: string, scope: OnboardingScope) => {
      const key = scope === "account" ? accountFlagsKey : workspaceFlagsKey;
      const current = readStored(key);
      if (current.has(event)) return;
      persist(key, new Set([...current, event]));
      setVersion((current) => current + 1);
    },
    [accountFlagsKey, workspaceFlagsKey],
  );
  return { events, record };
}
