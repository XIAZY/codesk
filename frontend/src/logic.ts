import * as Y from "yjs";
import type { ActivityEvent, Agent, AgentEvent, AgentRun, Daemon, DocumentItem, RuntimeModel, RuntimeModelCatalog, ThreadAnchor, ThreadItem, WorkspaceEvent, WorkspaceState } from "./types";

export const identifierPattern = "[a-z0-9_-]+";
export const identifierHelpText = "Only lowercase letters, numbers, underscores, and dashes.";
export const workspaceSlugMinLength = 2;
export const workspaceSlugMaxLength = 64;
export const handleMinLength = 2;
export const handleMaxLength = 32;

export type ReplaceOp = {
  start: number;
  end: number;
  text: string;
};

export type LineThreadGroup<T> = {
  line: number;
  threads: T[];
};

export type ResolvedThreadAnchor = ThreadAnchor & {
  start: number;
  end: number;
  line: number;
  excerpt: string;
  resolved: boolean;
};

export type DaemonInstallPlatform = "unix" | "windows";

export function shellQuote(value: string) {
  if (value.length === 0) {
    return "''";
  }
  if (/^[A-Za-z0-9_@%+=:,./-]+$/.test(value)) {
    return value;
  }
  return `'${value.replace(/'/g, "'\\''")}'`;
}

export function powershellQuote(value: string) {
  return `'${value.replace(/'/g, "''")}'`;
}

function powershellDownloadScript(variable: string, url: string) {
  return [
    `$${variable}Response = Invoke-WebRequest -UseBasicParsing ${powershellQuote(url)}`,
    `$${variable}Source = if ($${variable}Response.Content -is [byte[]]) { [System.Text.Encoding]::UTF8.GetString($${variable}Response.Content) } else { [string]$${variable}Response.Content }`,
  ];
}

export function defaultDaemonInstallPlatform(osHint = "", userAgent?: string): DaemonInstallPlatform {
  const normalizedOS = osHint.trim().toLowerCase();
  if (normalizedOS === "windows") {
    return "windows";
  }
  if (normalizedOS === "darwin" || normalizedOS === "linux") {
    return "unix";
  }
  const browserIdentity = userAgent ?? (
    typeof navigator === "undefined" ? "" : `${navigator.userAgent} ${navigator.platform}`
  );
  return /windows|win32|win64/i.test(browserIdentity) ? "windows" : "unix";
}

export function isMarkdownDocumentPath(path: string) {
  return /\.(md|markdown)$/i.test(path);
}

// ---- Desktop install (task #62) ----------------------------------------------
// The daemon-install redesign detects the user's platform to DEFAULT the download card,
// but a UA is a hint, not a lock — the machine you connect is often not the one you're
// browsing on. The GUI chooser offers only the platforms with desktop apps (Mac/Windows);
// Linux remains available in the explicitly entered command-line setup. An unrecognized
// UA falls to a neutral GUI chooser rather than a guessed default (AlphaToad/Eva).
export type DesktopPlatform = "mac" | "windows" | "linux" | "unknown";

export function detectDesktopPlatform(userAgent?: string): DesktopPlatform {
  const ua = (
    userAgent ?? (typeof navigator === "undefined" ? "" : `${navigator.userAgent} ${navigator.platform}`)
  ).toLowerCase();
  if (/windows|win32|win64/.test(ua)) return "windows";
  // iOS carries "like Mac OS X" but is not a Mac desktop and has no app — chooser, not Mac.
  if (/iphone|ipad|ipod/.test(ua)) return "unknown";
  if (/macintosh|mac os x/.test(ua)) return "mac";
  // Linux desktop, but NOT Android or ChromeOS — both carry linux/X11 tokens yet are not a
  // "connect your computer" target, so they fall through to the neutral chooser.
  if (/linux|x11/.test(ua) && !/android/.test(ua) && !/cros/.test(ua)) return "linux";
  // iOS, Android, ChromeOS, bots, anything unrecognized → neutral chooser, no false default.
  return "unknown";
}

// Only macOS & Windows ship a real desktop app (no Linux desktop build) — Linux connects
// via the terminal as its honest primary path, never a fake download.
export function desktopPlatformHasApp(platform: DesktopPlatform): boolean {
  return platform === "mac" || platform === "windows";
}

// A connected daemon reports its OS (darwin/windows/linux); map it to the desktop-platform
// class the UNINSTALL flow keys on (#63). This answers which OS, NOT how it was installed —
// the record never stores install method, so mac/windows still ask "app or terminal?". Linux
// has no desktop app (terminal-only); an unrecognized/absent OS also stays terminal-only,
// because we can't give correct OS-native app steps we can't verify.
export function daemonDesktopPlatform(osHint = ""): DesktopPlatform {
  const normalized = osHint.trim().toLowerCase();
  if (normalized === "windows") return "windows";
  if (normalized === "darwin") return "mac";
  if (normalized === "linux") return "linux";
  return "unknown";
}

// The terminal install-command platform for a desktop platform: Windows → PowerShell,
// mac/linux/unknown → the unix shell script.
export function desktopPlatformInstallTarget(platform: DesktopPlatform): DaemonInstallPlatform {
  return platform === "windows" ? "windows" : "unix";
}

// The download is by TARGET, not platform (Vitaliy's #61 contract): macOS is one universal
// build, but Windows publishes x64 AND ARM64 MSIs separately and a browser UA cannot
// authoritatively pick between them — so both Windows arches are shown, with detection only
// choosing a default (same hint-not-lock rule as platform). Linux/unknown have no app target.
export type DesktopDownloadTarget = "macos-universal" | "windows-amd64" | "windows-arm64";

export function desktopDownloadTargets(platform: DesktopPlatform): DesktopDownloadTarget[] {
  if (platform === "mac") return ["macos-universal"];
  if (platform === "windows") return ["windows-amd64", "windows-arm64"];
  return []; // linux/unknown → terminal, no download
}

// The target to preselect for a platform — a recommendation, not a lock. Windows defaults to
// x64 unless the UA clearly reads ARM (arm64/aarch64); the user can always switch arches.
export function defaultDesktopDownloadTarget(platform: DesktopPlatform, userAgent?: string): DesktopDownloadTarget | null {
  const targets = desktopDownloadTargets(platform);
  if (targets.length === 0) return null;
  if (platform === "windows") {
    const ua = (userAgent ?? (typeof navigator === "undefined" ? "" : navigator.userAgent)).toLowerCase();
    return /arm64|aarch64/.test(ua) ? "windows-arm64" : "windows-amd64";
  }
  return targets[0];
}

// The download target for an EXISTING daemon's re-download (#63 app reinstall). Unlike the
// install page — where the browser UA is the only hint — the daemon reports its real `arch`, so
// we must use it: a Windows ARM64 daemon managed from an x64/mac browser needs the ARM64 MSI, not
// the browser's arch. macOS is one universal build; an unrecognized Windows arch FAILS CLOSED
// (null) rather than shipping a wrong-architecture installer.
export function daemonDownloadTarget(platform: DesktopPlatform, arch?: string): DesktopDownloadTarget | null {
  if (platform === "mac") return "macos-universal";
  if (platform === "windows") {
    const normalized = (arch ?? "").trim().toLowerCase();
    if (normalized === "amd64" || normalized === "x86_64" || normalized === "x64") return "windows-amd64";
    if (normalized === "arm64" || normalized === "aarch64") return "windows-arm64";
    return null; // unknown arch → no guessed installer
  }
  return null;
}

// Per-TARGET desktop-app download URLs, keyed by the frozen asset keys. EMPTY until lane A
// (#69 publish → #61 validating resolver) lands real GitHub Releases — until then every
// target resolves to null, the CTA stays DISABLED ("Download unavailable"), and nothing ships
// pointing at a URL that isn't there (a button pointing at nothing is a lying control).
// @Vitaliy's #61 resolver (manifest.json → per-asset URL, fail-closed) replaces this.
export const DESKTOP_APP_DOWNLOAD_URLS: Partial<Record<DesktopDownloadTarget, string>> = {};

export function desktopAppDownloadUrl(target: DesktopDownloadTarget): string | null {
  return DESKTOP_APP_DOWNLOAD_URLS[target] ?? null;
}

// ---- Desktop download resolver (#61 consumer half) ---------------------------
// Parses the live R2 manifests into per-target download URLs. The two manifests are NOT one
// schema (Vitaliy): macOS carries {schema:1, version, signed_and_notarized, disk_image:{path,
// sha256,size}}; Windows carries {version, artifacts[]:{os,arch,file,sha256}} with no schema/
// size/signing. We FAIL CLOSED with an exact allowlist, never arbitrary URL joining: the
// declared filename must EQUAL the name computed from the validated version, sha must be 64
// lowercase hex, and anything unsafe (absolute URL, `..`, query/fragment, backslash, leading
// slash, duplicate/missing arch, malformed version) drops THAT target only — a corrupt Windows
// arm64 entry must not take amd64 down with it. We never invent size/signing the manifest can't
// prove; macOS's notarization state is surfaced only from its explicit boolean field.
export type DesktopManifestResult = {
  urls: Partial<Record<DesktopDownloadTarget, string>>;
  // true/false from the macOS manifest's explicit field; null when unknown / not a mac manifest.
  macNotarized: boolean | null;
};

const DESKTOP_SHA256_RE = /^[0-9a-f]{64}$/;
// A safe semver-ish version that is also path-safe (no slash, no `..`, no dangerous chars).
const DESKTOP_VERSION_RE = /^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$/;

// A manifest-declared filename segment must be exactly a plain relative path — reject anything
// that could escape the versioned prefix or smuggle a different origin.
function isSafeAssetPath(value: unknown): value is string {
  return (
    typeof value === "string" &&
    value.length > 0 &&
    !value.startsWith("/") &&
    !value.includes("..") &&
    !/[?#\\]/.test(value) &&
    !/^[a-z][a-z0-9+.-]*:\/\//i.test(value)
  );
}

export function resolveDesktopManifest(
  platform: DesktopPlatform,
  raw: unknown,
  staticBase: string,
): DesktopManifestResult {
  const empty: DesktopManifestResult = { urls: {}, macNotarized: null };
  if (!raw || typeof raw !== "object") return empty;
  const base = staticBase.trim().replace(/\/+$/, "");
  if (!base) return empty;
  const manifest = raw as Record<string, unknown>;
  const version = typeof manifest.version === "string" ? manifest.version : "";
  if (!DESKTOP_VERSION_RE.test(version)) {
    // Version is unusable for the whole manifest — but still surface macOS notarization state,
    // which is honest regardless of whether the download URL resolves.
    const notarized = platform === "mac" && typeof manifest.signed_and_notarized === "boolean"
      ? manifest.signed_and_notarized
      : null;
    return { urls: {}, macNotarized: notarized };
  }

  if (platform === "mac") {
    const notarized = typeof manifest.signed_and_notarized === "boolean" ? manifest.signed_and_notarized : null;
    if (manifest.schema !== 1) return { urls: {}, macNotarized: notarized };
    const diskImage = manifest.disk_image;
    if (!diskImage || typeof diskImage !== "object") return { urls: {}, macNotarized: notarized };
    const image = diskImage as Record<string, unknown>;
    const path = image.path;
    const sha = image.sha256;
    const size = image.size;
    const expected = `Codesk_${version}_macos_universal.dmg`;
    // The frozen manifest contract carries a positive size; reject zero/negative/non-integer/
    // string/missing so a malformed disk_image can't resolve to a "valid" object.
    const sizeValid = typeof size === "number" && Number.isInteger(size) && size > 0;
    if (!isSafeAssetPath(path) || path !== expected || typeof sha !== "string" || !DESKTOP_SHA256_RE.test(sha) || !sizeValid) {
      return { urls: {}, macNotarized: notarized };
    }
    return { urls: { "macos-universal": `${base}/macos/${version}/${path}` }, macNotarized: notarized };
  }

  if (platform === "windows") {
    const artifacts = Array.isArray(manifest.artifacts) ? manifest.artifacts : null;
    if (!artifacts) return empty;
    const urls: Partial<Record<DesktopDownloadTarget, string>> = {};
    const archTarget = { amd64: "windows-amd64", arm64: "windows-arm64" } as const;
    for (const arch of ["amd64", "arm64"] as const) {
      const matches = artifacts.filter(
        (a): a is Record<string, unknown> =>
          !!a && typeof a === "object" &&
          (a as Record<string, unknown>).os === "windows" &&
          (a as Record<string, unknown>).arch === arch,
      );
      if (matches.length !== 1) continue; // missing OR duplicate arch → drop only this target
      const artifact = matches[0];
      const file = artifact.file;
      const sha = artifact.sha256;
      const expected = `${arch}/Codesk_${version}_windows_${arch}.msi`;
      if (!isSafeAssetPath(file) || file !== expected || typeof sha !== "string" || !DESKTOP_SHA256_RE.test(sha)) {
        continue;
      }
      urls[archTarget[arch]] = `${base}/windows/${version}/${file}`;
    }
    return { urls, macNotarized: null };
  }

  return empty;
}

export function buildDaemonInstallCommand(input: {
  backendUrl: string;
  workspaceId: string;
  daemonToken: string;
  staticBaseUrl?: string;
  platform?: DaemonInstallPlatform;
}) {
  const backendUrl = input.backendUrl.trim().replace(/\/+$/, "");
  const staticBaseUrl = (input.staticBaseUrl?.trim() || `${backendUrl}/daemons`).replace(/\/+$/, "");
  if (input.platform === "windows") {
    return [
      "$ErrorActionPreference = 'Stop'",
      ...powershellDownloadScript("codeskInstaller", `${staticBaseUrl}/install.ps1`),
      "& ([ScriptBlock]::Create($codeskInstallerSource)) `",
      `  -BackendUrl ${powershellQuote(backendUrl)} \``,
      `  -WorkspaceId ${powershellQuote(input.workspaceId)} \``,
      `  -DaemonToken ${powershellQuote(input.daemonToken)} \``,
      `  -StaticBase ${powershellQuote(staticBaseUrl)}`,
    ].join("\n");
  }
  return [
    `curl -fsSL ${shellQuote(`${staticBaseUrl}/install.sh`)} | sh -s -- \\`,
    `  --backend-url ${shellQuote(backendUrl)} \\`,
    `  --workspace-id ${shellQuote(input.workspaceId)} \\`,
    `  --daemon-token ${shellQuote(input.daemonToken)} \\`,
    `  --static-base ${shellQuote(staticBaseUrl)}`,
  ].join("\n");
}

export function buildDaemonReinstallCommand(input: {
  backendUrl: string;
  workspaceId: string;
  daemonToken: string;
  staticBaseUrl?: string;
  platform?: DaemonInstallPlatform;
}) {
  const backendUrl = input.backendUrl.trim().replace(/\/+$/, "");
  const staticBaseUrl = (input.staticBaseUrl?.trim() || `${backendUrl}/daemons`).replace(/\/+$/, "");
  if (input.platform === "windows") {
    return [
      "$ErrorActionPreference = 'Stop'",
      ...powershellDownloadScript("codeskUninstaller", `${staticBaseUrl}/uninstall.ps1`),
      "& ([ScriptBlock]::Create($codeskUninstallerSource)) -All",
      ...powershellDownloadScript("codeskInstaller", `${staticBaseUrl}/install.ps1`),
      "& ([ScriptBlock]::Create($codeskInstallerSource)) `",
      `  -BackendUrl ${powershellQuote(backendUrl)} \``,
      `  -WorkspaceId ${powershellQuote(input.workspaceId)} \``,
      `  -DaemonToken ${powershellQuote(input.daemonToken)} \``,
      `  -StaticBase ${powershellQuote(staticBaseUrl)}`,
    ].join("\n");
  }
  return [
    "set -e",
    `curl -fsSL ${shellQuote(`${staticBaseUrl}/uninstall.sh`)} | sh -s -- \\`,
    "  --all",
    `curl -fsSL ${shellQuote(`${staticBaseUrl}/install.sh`)} | sh -s -- \\`,
    `  --backend-url ${shellQuote(backendUrl)} \\`,
    `  --workspace-id ${shellQuote(input.workspaceId)} \\`,
    `  --daemon-token ${shellQuote(input.daemonToken)} \\`,
    `  --static-base ${shellQuote(staticBaseUrl)}`,
  ].join("\n");
}

export function buildDaemonUninstallCommand(input: { staticBaseUrl: string; platform?: DaemonInstallPlatform }) {
  const staticBaseUrl = input.staticBaseUrl.trim().replace(/\/+$/, "");
  if (input.platform === "windows") {
    return [
      "$ErrorActionPreference = 'Stop'",
      ...powershellDownloadScript("codeskUninstaller", `${staticBaseUrl}/uninstall.ps1`),
      "& ([ScriptBlock]::Create($codeskUninstallerSource)) -All",
    ].join("\n");
  }
  const lines = [
    `curl -fsSL ${shellQuote(`${staticBaseUrl}/uninstall.sh`)} | sh -s -- \\`,
    "  --all",
  ];
  return lines.join("\n");
}

export function emptyWorkspace(): WorkspaceState {
  return {
    workspaceId: "",
    rootDocumentId: "",
    currentMembershipRole: "",
    name: "",
    users: [],
    daemons: [],
    agents: [],
    agentRuns: [],
    threads: [],
    agentEvents: [],
    presences: {},
  };
}

export const workspaceNameAdjectives = [
  "Crimson",
  "Azure",
  "Golden",
  "Silver",
  "Amber",
  "Violet",
  "Coral",
  "Jade",
  "Scarlet",
  "Cobalt",
  "Ivory",
  "Onyx",
  "Emerald",
  "Copper",
  "Indigo",
  "Hazel",
];

export const workspaceNameNouns = [
  "Otter",
  "Falcon",
  "Willow",
  "Harbor",
  "Meadow",
  "Cedar",
  "Lantern",
  "Comet",
  "Canyon",
  "Ember",
  "Ridge",
  "Beacon",
  "Maple",
  "Quartz",
  "Summit",
  "Heron",
];

export function randomWorkspaceName(random: () => number = Math.random): string {
  const adjective = workspaceNameAdjectives[Math.floor(random() * workspaceNameAdjectives.length)];
  const noun = workspaceNameNouns[Math.floor(random() * workspaceNameNouns.length)];
  return `${adjective} ${noun}`;
}

export function identifierFromName(value: string, maxLength: number) {
  return value
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9_-]+/g, "-")
    .replace(/-+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, maxLength);
}

export function computeReplace(before: string, after: string): ReplaceOp {
  let start = 0;
  while (start < before.length && start < after.length && before[start] === after[start]) {
    start += 1;
  }
  let beforeEnd = before.length;
  let afterEnd = after.length;
  while (beforeEnd > start && afterEnd > start && before[beforeEnd - 1] === after[afterEnd - 1]) {
    beforeEnd -= 1;
    afterEnd -= 1;
  }
  return { start, end: beforeEnd, text: after.slice(start, afterEnd) };
}

export function applyReplaceToYText(text: Y.Text, replace: ReplaceOp) {
  const deleteLength = Math.max(0, replace.end - replace.start);
  if (deleteLength > 0) {
    text.delete(replace.start, deleteLength);
  }
  if (replace.text.length > 0) {
    text.insert(replace.start, replace.text);
  }
}

export function lineStartsForText(text: string) {
  const starts = [0];
  for (let index = 0; index < text.length; index += 1) {
    if (text[index] === "\n") {
      starts.push(index + 1);
    }
  }
  return starts;
}

export function lineForOffset(lineStarts: number[], offset: number) {
  let line = 1;
  for (let index = 0; index < lineStarts.length; index += 1) {
    if (lineStarts[index] > offset) {
      break;
    }
    line = index + 1;
  }
  return line;
}

export function selectionLabel(start: number, end: number, lineStarts: number[]) {
  const startLine = lineForOffset(lineStarts, start);
  const endLine = lineForOffset(lineStarts, Math.max(start, end - 1));
  if (start === end) {
    return `Cursor on line ${startLine}`;
  }
  if (startLine === endLine) {
    return `Selection on line ${startLine}`;
  }
  return `Selection across lines ${startLine}-${endLine}`;
}

export function buildLineThreads<T extends { anchor: { line: number } }>(threads: T[]): Array<LineThreadGroup<T>> {
  const groups = new Map<number, T[]>();
  for (const thread of threads) {
    const line = Math.max(1, thread.anchor.line || 1);
    groups.set(line, [...(groups.get(line) ?? []), thread]);
  }
  return Array.from(groups.entries())
    .sort((left, right) => left[0] - right[0])
    .map(([line, threads]) => ({ line, threads }));
}

export function daemonStatus(daemon: Daemon) {
  if (daemon.status === "deleted") {
    return "deleted";
  }
  return daemon.connectionStatus || "disconnected";
}

// A daemon has genuinely checked in only if it carries a real lastSeenAt. Never-seen daemons carry
// a zero time (year 0001) — the same idiomatic guard the daemon table uses for "Last check-in".
export function hasGenuineCheckIn(daemon: Daemon): boolean {
  return daemon.lastSeenAt != null && new Date(daemon.lastSeenAt).getUTCFullYear() >= 2020;
}

// Liveness decay windows — mirror the backend's daemonOnlineWindow / daemonStaleWindow in
// backend/internal/notty/store.go (applyDaemonLiveness). A daemon that stops checking in emits
// no event, so the client must decay its status on a timer instead of trusting the last payload.
export const DAEMON_ONLINE_WINDOW_MS = 30_000;
export const DAEMON_STALE_WINDOW_MS = 120_000;

// Live daemon status accounting for time elapsed since the last payload landed. We add the
// server-computed lastSeenAgeSeconds to elapsed-since-receipt rather than comparing lastSeenAt to
// the client clock, which would be wrong under clock skew. Windows use <= to match the backend.
export function daemonLiveStatus(daemon: Daemon, nowMs: number): string {
  if (daemon.status === "deleted") {
    return "deleted";
  }
  // A daemon that has never checked in serializes lastSeenAgeSeconds: 0 (no omitempty) with a zero
  // lastSeenAt, so the null-guard alone would fabricate a fresh "online". Gate on a genuine check-in
  // — the same zero-time idiom the table uses — and otherwise trust the server's status.
  if (!hasGenuineCheckIn(daemon) || daemon.lastSeenAgeSeconds == null || daemon.receivedAtMs == null) {
    return daemon.connectionStatus || "disconnected";
  }
  const ageMs = daemon.lastSeenAgeSeconds * 1000 + Math.max(0, nowMs - daemon.receivedAtMs);
  if (ageMs <= DAEMON_ONLINE_WINDOW_MS) {
    return "online";
  }
  if (ageMs <= DAEMON_STALE_WINDOW_MS) {
    return "stale";
  }
  return "disconnected";
}

// ---- daemon-model-selection task #4: catalog resolution (pure) --------------------------------
// The projected model catalog for a runtime lives on its RuntimeDetection (daemon.runtimes[]). All
// consumers branch on the *shape*, never on convention, so a decode-drop can only fail conservative.

export function runtimeModelCatalog(daemon: Daemon | undefined, kind: string): RuntimeModelCatalog | undefined {
  return daemon?.runtimes?.find((runtime) => runtime.kind === kind)?.modelCatalog;
}

// The honest capability states. Order matters: a missing catalog reads as the conservative
// "unsupported" rather than a false "ready", and a transient error is distinct from a genuine empty.
//   "unsupported" → old/incapable daemon (field absent): inherited defaults only, calm — not an error
//   "error"       → capable daemon, transient probe failure: "couldn't load models, retrying"
//   "empty"       → capable daemon, genuinely no models: first-class, not a misleading "not found"
//   "ready"       → offer the selectors
export type ModelCatalogState = "unsupported" | "error" | "empty" | "ready";

export function modelCatalogState(catalog: RuntimeModelCatalog | undefined): ModelCatalogState {
  if (!catalog) {
    return "unsupported";
  }
  // Field PRESENCE, not truthiness: the wire omits `error` when there is none (omitempty), so a
  // present `error` — even "" from a malformed/drifted payload — must fail closed as a probe error
  // rather than masquerade as a genuine empty catalog. (Thomas, architecture seat.)
  if (catalog.error !== undefined) {
    return "error";
  }
  if (catalog.models.length === 0) {
    return "empty";
  }
  return "ready";
}

// Return the one visible default row, when the runtime reports one. Runtimes
// with provider-wide efforts can still offer "runtime default + explicit
// effort" without this row.
export function uniqueDefaultModel(catalog: RuntimeModelCatalog | undefined): RuntimeModel | null {
  if (!catalog) {
    return null;
  }
  const defaults = catalog.models.filter((model) => model.isDefault);
  return defaults.length === 1 ? defaults[0] : null;
}

// Look up a model row by its opaque id (never the displayName).
export function findRuntimeModel(catalog: RuntimeModelCatalog | undefined, model: string): RuntimeModel | undefined {
  return catalog?.models.find((entry) => entry.model === model);
}

// Efforts offered for the current profile: prefer the selected/default model's
// row, then fall back to provider-wide choices. Unknown explicit models never
// inherit the provider-wide choices.
export function availableReasoningEfforts(catalog: RuntimeModelCatalog | undefined, model: string): string[] {
  if (model) {
    const selected = findRuntimeModel(catalog, model);
    if (!selected) return [];
    return selected.reasoningEfforts.length ? selected.reasoningEfforts : catalog?.reasoningEfforts ?? [];
  }
  const defaultModel = uniqueDefaultModel(catalog);
  return defaultModel?.reasoningEfforts.length ? defaultModel.reasoningEfforts : catalog?.reasoningEfforts ?? [];
}

// Whether a model/effort profile is still valid against the current catalog. Full inheritance
// (empty/empty) is always valid. Used to gate submit AND — critically — to detect when a background
// daemon.updated has invalidated a live selection, which must then be preserved+blocked (never
// silently reset behind the user's click).
export function isModelProfileValid(
  catalog: RuntimeModelCatalog | undefined,
  model: string,
  reasoningEffort: string,
): boolean {
  if (!model && !reasoningEffort) {
    return true; // full inheritance is always valid
  }
  if (modelCatalogState(catalog) !== "ready") {
    return false; // any explicit choice requires a ready catalog
  }
  if (model && !findRuntimeModel(catalog, model)) {
    return false; // an explicit model must exist in the catalog
  }
  if (reasoningEffort && !availableReasoningEfforts(catalog, model).includes(reasoningEffort)) {
    return false; // an explicit effort must be in the effective model's advertised efforts
  }
  return true;
}

// Read-only label for a persisted agent model profile (agent surface, task #4). The agent record
// stores only the opaque `model` id, so an unresolvable stored model — no catalog (old/offline
// daemon) or a model since removed from it — renders an explicit "Unavailable" rather than the
// opaque id. "" is the runtime default.
export function agentModelDisplay(daemon: Daemon | undefined, kind: string, model: string): string {
  if (!model) {
    return "Runtime default";
  }
  const found = findRuntimeModel(runtimeModelCatalog(daemon, kind), model);
  return found ? found.displayName : "Unavailable";
}

// Stamp the client receipt time onto daemon payloads as they land, so daemonLiveStatus can decay
// them later. Pure: the caller passes nowMs (Date.now at the socket/snapshot boundary).
export function stampDaemonReceipt(event: WorkspaceEvent, nowMs: number): WorkspaceEvent {
  if (event.type === "daemon.created" || event.type === "daemon.updated" || event.type === "daemon.deleted") {
    return { ...event, data: { ...(event.data as Daemon), receivedAtMs: nowMs } };
  }
  if (event.type === "workspace.snapshot") {
    const data = event.data as Partial<WorkspaceState> & { daemons?: Daemon[] };
    if (data.daemons) {
      return { ...event, data: { ...data, daemons: data.daemons.map((daemon) => ({ ...daemon, receivedAtMs: nowMs })) } };
    }
  }
  return event;
}

export type AgentDisplayStatusKey = "running" | "queued" | "waiting-env" | "failed" | "stalled" | "idle";

export type AgentDisplayStatus = {
  key: AgentDisplayStatusKey;
  tone: AgentDisplayStatusKey;
  label: string;
  detailLabel?: string;
  title: string;
  action?: string;
  reason?: string;
  run?: AgentRun;
};

const terminalAgentRunStatuses = new Set(["completed", "failed", "stopped", "cancelled", "canceled"]);

function runUpdatedAtMs(run: AgentRun) {
  const value = Date.parse(run.updatedAt);
  return Number.isNaN(value) ? 0 : value;
}

function currentAgentRun(agent: Agent, runs: AgentRun[]) {
  const exact = agent.currentRunId ? runs.find((run) => run.id === agent.currentRunId) : undefined;
  if (exact) {
    return exact;
  }
  const agentRuns = runs
    .filter((run) => run.agentId === agent.id)
    .sort((left, right) => runUpdatedAtMs(right) - runUpdatedAtMs(left));
  return agentRuns.find((run) => !terminalAgentRunStatuses.has(run.status)) ?? agentRuns[0];
}

function textOrUndefined(value?: string) {
  const text = (value ?? "").trim().replace(/\s+/g, " ");
  return text || undefined;
}

function runningAction(agent: Agent, run?: AgentRun) {
  const candidates = [run?.lastMessage, agent.currentActivity, agent.currentTask];
  for (const candidate of candidates) {
    const text = textOrUndefined(candidate);
    if (!text) {
      continue;
    }
    const normalized = text.toLowerCase();
    if (normalized === "running" || normalized === "working" || normalized === "queued" || normalized === "queued in daemon") {
      continue;
    }
    return text;
  }
  return "Working";
}

function failureReason(agent: Agent, run?: AgentRun) {
  for (const candidate of [run?.error, run?.lastMessage, agent.currentActivity]) {
    const text = textOrUndefined(candidate);
    if (!text || text.toLowerCase() === "failed") {
      continue;
    }
    return text;
  }
  return "No failure reason was provided.";
}

function hasQueuedWork(agent: Agent, run?: AgentRun) {
  return !!run && !terminalAgentRunStatuses.has(run.status) && (run.status === "queued" || agent.status === "queued");
}

function status(key: AgentDisplayStatusKey, label: string, title = label, extra: Omit<AgentDisplayStatus, "key" | "tone" | "label" | "title"> = {}): AgentDisplayStatus {
  return { key, tone: key, label, title, ...extra };
}

export function agentDisplayStatus(
  agent: Agent,
  runs: AgentRun[],
  daemons?: Daemon[],
  nowMs = Date.now(),
): AgentDisplayStatus {
  const daemon = daemons?.find((item) => item.id === agent.daemonId);
  const daemonState = daemon ? daemonLiveStatus(daemon, nowMs) : daemons ? "disconnected" : "";
  if (daemonState === "disconnected" || daemonState === "deleted") {
    return status("waiting-env", "Waiting for local environment", "Waiting for local environment", { run: currentAgentRun(agent, runs) });
  }

  const run = currentAgentRun(agent, runs);
  if (agent.status === "failed" || run?.status === "failed") {
    const reason = failureReason(agent, run);
    return status("failed", "Failed — view reason", `Failed — view reason: ${reason}`, { reason, run });
  }

  // A wedged working turn the daemon has surfaced as `stalled`: show it as its own
  // visible status carrying the daemon's diagnostic (currentActivity), never let it
  // fall through to Idle/Standing by — that is the human-facing half of item #5.
  if (agent.status === "stalled") {
    const detail = agent.currentActivity?.trim() || "Stalled — no recent runtime activity";
    return status("stalled", "Stalled", detail, { detailLabel: detail, run });
  }

  if (agent.status === "working" || run?.status === "running" || (run?.desiredStatus === "running" && run && !terminalAgentRunStatuses.has(run.status) && run.status !== "queued")) {
    const action = runningAction(agent, run);
    return status("running", `Running · ${action}`, `Running · ${action}`, { action, run });
  }

  if (hasQueuedWork(agent, run)) {
    return status("queued", "Queued", "Queued", { run });
  }

  return status("idle", "Idle", "Standing by", { detailLabel: "Standing by", run });
}

export function agentStatus(agent: Agent, runs: AgentRun[]) {
  return agentDisplayStatus(agent, runs).key;
}


export function coworkerCount(workspace: Pick<WorkspaceState, "agents" | "users">) {
  return workspace.agents.length + workspace.users.length;
}

// Matches the backend's 2-minute presence read cutoff (see PR #50), so the
// client-side ring window agrees with what the server would still serve.
export const PRESENCE_ONLINE_WINDOW_MS = 120_000;

// Phase E right-rail resize bounds. The rail clamps to [280, 520]px, and never
// so wide that the center document column drops below 380px — so a drag can't
// crush or overlap the document ("不塌、不错位"). The left rail is 248px open,
// 60px collapsed, which changes how much width the center still needs.
export type WorkspacePerson = {
  id: string;
  handle: string;
  name: string;
  kind: "you" | "agent" | "member";
  // updatedAt of this actor's most recent presence row (any document =
  // workspace-level). Whether it counts as *online* is a freshness decision
  // left to personOnline, so the ring decays instead of lying after the window.
  presentAt?: string;
};

// Everyone in the workspace — humans + agents — for the People panel. The online
// ring is workspace-level: presentAt is the actor's presence row (any document),
// and personOnline decays it on the same 2-minute window. Presence is never
// fabricated: no row means no ring.
export function workspacePeople(
  workspace: Pick<WorkspaceState, "currentUserId" | "agents" | "users" | "presences">,
): WorkspacePerson[] {
  const currentUserId = workspace.currentUserId;
  const presentAt = (id: string) => workspace.presences[id]?.updatedAt;

  const people: WorkspacePerson[] = [];
  for (const user of workspace.users) {
    if (user.kind !== "human") {
      continue;
    }
    people.push({
      id: user.id,
      handle: user.handle,
      name: user.name,
      kind: user.id === currentUserId ? "you" : "member",
      presentAt: presentAt(user.id),
    });
  }
  for (const agent of workspace.agents) {
    people.push({ id: agent.id, handle: agent.handle, name: agent.name, kind: "agent", presentAt: presentAt(agent.id) });
  }

  // Deterministic base order — You first, then by handle. The panel re-orders
  // online-first using a live freshness check (personOnline).
  const rank = (person: WorkspacePerson) => (person.kind === "you" ? 0 : 1);
  people.sort((a, b) => rank(a) - rank(b) || a.handle.localeCompare(b.handle));
  return people;
}

// Online only when the actor has a presence row still within the freshness
// window at nowMs — the same decay daemon liveness uses, so a closed laptop
// drops the ring instead of lying. Workspace-level: any document counts.
export function personOnline(person: Pick<WorkspacePerson, "presentAt">, nowMs: number): boolean {
  if (!person.presentAt) {
    return false;
  }
  const presentMs = Date.parse(person.presentAt);
  return !Number.isNaN(presentMs) && nowMs - presentMs <= PRESENCE_ONLINE_WINDOW_MS;
}

// clampPopoverPosition keeps the thread popover fully inside the viewport (thread-redesign containment fix).
// It clamps the fixed marker coordinates so the card never spills off an edge, and PREFERS flipping above the
// marker when the card can't fit below it (so it stays attached to the line). Horizontally, a card wider than
// the viewport pins to the left margin; vertically, a card that fits neither below nor above (only possible if
// it's taller than the viewport — the CSS height cap prevents that) clamps into view. Pure, so the placement
// math is unit-tested without a browser; a layout-effect feeds it the measured card + viewport.
export function clampPopoverPosition(
  point: { x: number; y: number },
  card: { width: number; height: number },
  viewport: { width: number; height: number },
  margin = 12,
): { left: number; top: number } {
  const maxLeft = viewport.width - card.width - margin;
  const left = maxLeft < margin ? margin : Math.min(Math.max(point.x, margin), maxLeft);

  let top: number;
  if (point.y + card.height <= viewport.height - margin) {
    top = point.y; // fits below the marker
  } else {
    const above = point.y - card.height; // flip: card sits above the marker
    if (above >= margin) {
      top = above;
    } else {
      const maxTop = viewport.height - card.height - margin;
      top = maxTop < margin ? margin : maxTop;
    }
  }
  return { left, top };
}

// The document-scoped Participants panel (task #4). Two disjoint durable/live sets, plus the picker source:
//   hereNow  — humans AND agents with FRESH presence on THIS document (presence rows carry documentId; a
//              stale or other-document row does not count — presence is only ever real activity).
//   watching — agents subscribed to this document (the durable notification relationship), independent of
//              presence. "Watching", never "online": an offline agent still watches.
//   addable  — agents NOT subscribed, the ONLY place unsubscribed agents surface (the add-watcher picker),
//              so the panel never reintroduces the ambient every-agent-watches-everything view.
// subscriberIds comes from GET /documents/{id}/subscribers; presence is workspace state. Pure, so the panel's
// grouping is unit-testable without the fetch.
export type DocumentParticipants = {
  hereNow: WorkspacePerson[];
  watching: WorkspacePerson[];
  addable: WorkspacePerson[];
};

export function documentParticipants(
  workspace: Pick<WorkspaceState, "currentUserId" | "agents" | "users" | "presences">,
  documentId: string | undefined,
  subscriberIds: readonly string[],
  nowMs: number,
): DocumentParticipants {
  const subscribed = new Set(subscriberIds);
  const onThisDoc = (id: string): boolean => {
    if (!documentId) {
      return false;
    }
    const presence = workspace.presences[id];
    if (!presence || presence.documentId !== documentId) {
      return false;
    }
    return personOnline({ presentAt: presence.updatedAt }, nowMs);
  };

  const hereNow: WorkspacePerson[] = [];
  for (const person of workspacePeople(workspace)) {
    if (onThisDoc(person.id)) {
      hereNow.push(person);
    }
  }

  const watching: WorkspacePerson[] = [];
  const addable: WorkspacePerson[] = [];
  const byHandle = (a: WorkspacePerson, b: WorkspacePerson) => a.handle.localeCompare(b.handle);
  for (const agent of workspace.agents) {
    const person: WorkspacePerson = { id: agent.id, handle: agent.handle, name: agent.name, kind: "agent", presentAt: workspace.presences[agent.id]?.updatedAt };
    (subscribed.has(agent.id) ? watching : addable).push(person);
  }
  watching.sort(byHandle);
  addable.sort(byHandle);
  return { hereNow, watching, addable };
}

export type ActivityCategory = "human-edit" | "comment" | "agent-change" | "done" | "neutral";

// Map a Document Activity event to one of 2a-3's semantic categories from its
// type + actor. Only the known shapes get a color; anything else is neutral
// (no guessed color). No completion activity type exists yet, so the "done"
// category stays dormant until one does — we don't fabricate it.
export function activityCategory(type: string, actorType: string): ActivityCategory {
  if (type.startsWith("thread.")) {
    return "comment";
  }
  if (type.startsWith("document.")) {
    return actorType === "agent" ? "agent-change" : "human-edit";
  }
  if (type.startsWith("agent.")) {
    return "agent-change";
  }
  return "neutral";
}

// The current document's activity, newest first. Snapshot-fresh: reflects
// workspace.activities as of the last snapshot — there is no per-event live
// update yet (tracked separately), so the renderer must not imply live.
export function documentActivity(
  workspace: Pick<WorkspaceState, "activities">,
  documentId: string | undefined,
  limit = 12,
): ActivityEvent[] {
  if (!documentId) {
    return [];
  }
  return (workspace.activities ?? [])
    .filter((activity) => activity.documentId === documentId)
    .slice()
    .sort((a, b) => (a.occurredAt < b.occurredAt ? 1 : a.occurredAt > b.occurredAt ? -1 : 0))
    .slice(0, limit);
}

const relativeTimeFormat = new Intl.RelativeTimeFormat("en", { numeric: "auto" });

// Human relative time for an ISO timestamp, e.g. "5 minutes ago". Empty for an
// unparseable timestamp so a bad value never renders a misleading "now".
export function relativeTime(iso: string, nowMs: number): string {
  const ms = Date.parse(iso);
  if (Number.isNaN(ms)) {
    return "";
  }
  const diffSec = Math.round((ms - nowMs) / 1000);
  if (Math.abs(diffSec) < 60) {
    return relativeTimeFormat.format(diffSec, "second");
  }
  const diffMin = Math.round(diffSec / 60);
  if (Math.abs(diffMin) < 60) {
    return relativeTimeFormat.format(diffMin, "minute");
  }
  const diffHour = Math.round(diffSec / 3600);
  if (Math.abs(diffHour) < 24) {
    return relativeTimeFormat.format(diffHour, "hour");
  }
  return relativeTimeFormat.format(Math.round(diffSec / 86400), "day");
}

export function threadReplyCount(thread: { messages: readonly unknown[] }) {
  return Math.max(0, thread.messages.length - 1);
}

export function threadReplyLabel(thread: { messages: readonly unknown[] }) {
  const count = threadReplyCount(thread);
  if (count === 0) {
    return "No replies";
  }
  return `${count} ${count === 1 ? "reply" : "replies"}`;
}

export function agentsByDaemon(agents: Agent[], daemons: Daemon[]) {
  const names = new Map(daemons.map((daemon) => [daemon.id, daemon.name]));
  return agents.reduce<Array<{ daemonId: string; daemonName: string; agents: Agent[] }>>((groups, agent) => {
    const daemonId = agent.daemonId || "unassigned";
    let group = groups.find((item) => item.daemonId === daemonId);
    if (!group) {
      group = { daemonId, daemonName: names.get(daemonId) ?? "Unassigned daemon", agents: [] };
      groups.push(group);
    }
    group.agents.push(agent);
    return groups;
  }, []);
}

// The backend serializes presences as a JSON array (one row per actor), but state.presences is a map
// keyed by actorId: every consumer reads presences[id], and the incremental presence.updated handler
// keys by actorId. A snapshot must key the same way — otherwise presences that arrive in the INITIAL
// snapshot are stored array-shaped and are silently invisible to the online ring until the actor emits
// a live update. A Go nil slice (JSON null) coerces to an empty map, exactly as before.
function presencesById(raw: unknown): WorkspaceState["presences"] {
  if (Array.isArray(raw)) {
    return Object.fromEntries((raw as Array<WorkspaceState["presences"][string]>).map((presence) => [presence.actorId, presence]));
  }
  return (raw as WorkspaceState["presences"]) ?? {};
}

export function reduceWorkspaceEvent(state: WorkspaceState, event: WorkspaceEvent): WorkspaceState {
  if (event.type === "workspace.snapshot") {
    const { documents: _documents, ...data } = event.data as Partial<WorkspaceState> & { documents?: unknown };
    // The backend marshals empty collections as JSON null (Go nil slices) — e.g. an
    // idle workspace's presences after the 2-minute freshness cutoff, or a workspace
    // with no activities. Coerce them to empty at this seam so no consumer (the People
    // online ring, Document Activity, …) dereferences null when switching workspaces.
    return {
      ...state,
      ...data,
      users: data.users ?? [],
      daemons: data.daemons ?? [],
      agents: data.agents ?? [],
      agentRuns: data.agentRuns ?? [],
      threads: data.threads ?? [],
      agentEvents: data.agentEvents ?? [],
      presences: presencesById(data.presences),
      activities: data.activities ?? [],
    };
  }
  if (event.type === "activity.created") {
    const activities = Array.isArray(state.activities) ? state.activities : [];
    // TODO: runtime-validate envelope payloads (unvalidated WS-payload casts are a class, not a one-off)
    return { ...state, activities: [event.data as ActivityEvent, ...activities].slice(0, 50) };
  }
  if (event.type === "workspace.updated") {
    const workspace = event.data as Partial<WorkspaceState> & { id?: string };
    if (workspace.id && workspace.id !== state.workspaceId) {
      return state;
    }
    return {
      ...state,
      workspaceId: workspace.id ?? state.workspaceId,
      rootDocumentId: workspace.rootDocumentId ?? state.rootDocumentId,
      name: workspace.name ?? state.name,
      slug: workspace.slug ?? state.slug,
      defaultRuntime: workspace.defaultRuntime ?? "",
      updatedAt: workspace.updatedAt ?? state.updatedAt,
    };
  }
  if (event.type === "workspace.deleted") {
    const deleted = event.data as { workspaceId?: string };
    return !deleted.workspaceId || deleted.workspaceId === state.workspaceId ? emptyWorkspace() : state;
  }
  if (event.type === "thread.created" || event.type === "thread.updated") {
    return { ...state, threads: upsertById(state.threads, event.data as ThreadItem) };
  }
  if (event.type === "thread.message.created") {
    const message = event.data as ThreadItem["messages"][number];
    return {
      ...state,
      threads: state.threads.map((thread) =>
        thread.id === message.threadId && !thread.messages.some((current) => current.id === message.id)
          ? { ...thread, messages: [...thread.messages, message], updatedAt: message.createdAt }
          : thread
      ),
    };
  }
  if (event.type === "daemon.created" || event.type === "daemon.updated" || event.type === "daemon.deleted") {
    return { ...state, daemons: upsertById(state.daemons, event.data as Daemon) };
  }
  if (event.type === "agent.created" || event.type === "agent.updated") {
    return { ...state, agents: upsertById(state.agents, event.data as Agent) };
  }
  if (event.type === "agent.deleted") {
    const agent = event.data as Agent;
    return { ...state, agents: state.agents.filter((item) => item.id !== agent.id) };
  }
  if (event.type === "agent.run.updated") {
    return { ...state, agentRuns: upsertById(state.agentRuns, event.data as AgentRun) };
  }
  if (event.type === "agent.event.updated") {
    return { ...state, agentEvents: upsertById(state.agentEvents, event.data as WorkspaceState["agentEvents"][number]) };
  }
  if (event.type === "presence.updated") {
    const presence = event.data as WorkspaceState["presences"][string];
    return { ...state, presences: { ...state.presences, [presence.actorId]: presence } };
  }
  return state;
}

// Legacy fallback for anchors created before stateAtAnchor existed. Orphaned when
// the resolved span collapsed (the original range is gone) or drifted to text that
// shares no token with the stored excerpt. Public-API only (no Y.js internals).
// Kept as the documented retreat path from the identity walk.
function orphanedByTokenOverlap(excerpt: string, resolvedText: string, start: number, end: number): boolean {
  const excerptNorm = excerpt.trim().toLowerCase();
  const collapsed = start === end && excerptNorm.length > 0;
  const resolvedNorm = resolvedText.trim().toLowerCase();
  const excerptTokens = new Set(excerptNorm.split(/[^a-z0-9]+/).filter(Boolean));
  const resolvedTokens = new Set(resolvedNorm.split(/[^a-z0-9]+/).filter(Boolean));
  const hasOverlap = excerptTokens.size > 0 && resolvedTokens.size > 0
    && [...excerptTokens].some((token) => resolvedTokens.has(token));
  const drifted = !collapsed && start !== end && excerptTokens.size > 0 && resolvedTokens.size > 0 && !hasOverlap;
  return collapsed || drifted;
}

export function resolveThreadAnchorLive(anchor: ThreadAnchor, ydoc: Y.Doc | null, content: string) {
  const fallback: ResolvedThreadAnchor = {
    ...anchor,
    start: 0,
    end: 0,
    line: 1,
    excerpt: (anchor.excerpt || "").slice(0, 140),
    resolved: anchor.kind === "document",
  };
  if (!ydoc || !anchor.relativeStart || !anchor.relativeEnd) {
    return fallback;
  }
  try {
    const startRP = decodeRelativePosition(anchor.relativeStart);
    const endRP = decodeRelativePosition(anchor.relativeEnd);
    const startPosition = Y.createAbsolutePositionFromRelativePosition(startRP, ydoc);
    const endPosition = Y.createAbsolutePositionFromRelativePosition(endRP, ydoc);
    if (!startPosition || !endPosition) {
      return fallback;
    }
    const start = Math.max(0, startPosition.index);
    const end = Math.max(start, endPosition.index);
    // Continuity criterion — ONE branch point, off the encoding's own geometry.
    // The end edge's association is self-describing: the assoc-fixed encoding
    // attaches the end edge LEFT (assoc -1), the old symmetric encoding attaches it
    // RIGHT (assoc 0). Keying on the assoc — NOT stateAtAnchor presence — is correct
    // for every cohort, including anchors created between the #102 deploy and this
    // one that carry a state vector but the OLD geometry: they must take the legacy
    // path, or delete-then-retype resolves to a non-empty span of unrelated text.
    //   • new geometry (end attaches LEFT) -> orphaned iff the resolved span is
    //     EMPTY. Text typed at either boundary lands outside the anchor; a fully
    //     deleted range collapses and can never be re-entered, so "a moment of
    //     nothing" is permanent — AlphaToad's ruled continuity semantics, enforced
    //     by CRDT geometry, no walk, no vector read.
    //   • old geometry -> token-overlap fallback until the population migrates as
    //     threads are created / re-anchored.
    const hadExtent = (anchor.excerpt || "").trim().length > 0;
    const orphaned = (endRP.assoc ?? 0) < 0
      ? hadExtent && end === start
      : orphanedByTokenOverlap(anchor.excerpt || "", content.slice(start, end), start, end);
    const lineStarts = lineStartsForText(content);
    const previewEnd = end === start ? Math.min(content.length, start + 80) : end;
    return {
      ...anchor,
      start,
      end,
      line: lineForOffset(lineStarts, start),
      // Honest preview: an orphaned anchor quotes the STORED original text (the
      // characters that were lost), never a forward-slice of whatever lives at the
      // collapse point now.
      excerpt: (orphaned ? anchor.excerpt || "" : content.slice(start, previewEnd).trim() || anchor.excerpt || "").slice(0, 140),
      resolved: !orphaned,
    };
  } catch {
    return fallback;
  }
}

// Encode a range endpoint as a Y.js relative position. The two edges take OPPOSITE
// associations so the anchor tracks a living range: the start edge attaches to the
// RIGHT (the first anchored character), the end edge attaches to the LEFT (the last
// anchored character). Text typed at either boundary therefore lands OUTSIDE the
// anchor, and a fully-deleted range collapses to an empty span that can never be
// re-entered — the geometric basis of continuity orphan detection.
export function encodeRelativeAnchor(text: Y.Text, index: number, edge: "start" | "end") {
  const safeIndex = Math.max(0, Math.min(index, text.length));
  const assoc = edge === "end" ? -1 : 0;
  return uint8ArrayToBase64(Y.encodeRelativePosition(Y.createRelativePositionFromTypeIndex(text, safeIndex, assoc)));
}

function decodeRelativePosition(value: string) {
  return Y.decodeRelativePosition(base64ToUint8Array(value));
}

function upsertById<T extends { id: string }>(items: T[], next: T) {
  const seen = new Set<string>();
  const result = items.map((item) => {
    if (item.id !== next.id) {
      return item;
    }
    seen.add(item.id);
    return next;
  });
  if (!seen.has(next.id)) {
    result.push(next);
  }
  return result;
}

export function base64ToUint8Array(value: string) {
  const binary = window.atob(value);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) {
    bytes[index] = binary.charCodeAt(index);
  }
  return bytes;
}

export function uint8ArrayToBase64(bytes: Uint8Array) {
  let binary = "";
  for (const byte of bytes) {
    binary += String.fromCharCode(byte);
  }
  return window.btoa(binary);
}
