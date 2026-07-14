import { ApiClient, ApiError, publicOrigin } from "./api";
import { parseDesktopHandoffPageURL, submitDesktopHandoff } from "./desktopHandoff";
import type { WorkspaceSummary } from "./types";
import "./desktopHandoffSpike.css";

const tokenStorageKey = "codesk.auth.token";

const loadingView = requiredElement<HTMLElement>("loading-view");
const signedOutView = requiredElement<HTMLElement>("signed-out-view");
const errorView = requiredElement<HTMLElement>("error-view");
const errorMessage = requiredElement<HTMLElement>("error-message");
const statusChip = requiredElement<HTMLElement>("status-chip");
const retryButton = requiredElement<HTMLButtonElement>("retry-button");
const connectForm = requiredElement<HTMLFormElement>("connect-form");
const workspaceSelect = requiredElement<HTMLSelectElement>("workspace-select");
const daemonName = requiredElement<HTMLInputElement>("daemon-name");
const connectButton = requiredElement<HTMLButtonElement>("connect-button");
const accountLabel = requiredElement<HTMLElement>("account-label");

let callback = "";
let workspaces: WorkspaceSummary[] = [];
let api: ApiClient | null = null;

retryButton.addEventListener("click", () => void loadSession());
workspaceSelect.addEventListener("change", updateConnectButton);
daemonName.addEventListener("input", updateConnectButton);
connectForm.addEventListener("submit", (event) => void connect(event));

void initialize();

async function initialize() {
  try {
    callback = parseDesktopHandoffPageURL(window.location.href).href;
  } catch {
    showError("The desktop callback is missing or invalid.");
    return;
  }
  await loadSession();
}

async function loadSession() {
  showOnly(loadingView);
  setStatus("loading", "Checking session");
  const humanToken = window.localStorage.getItem(tokenStorageKey) ?? "";
  if (!humanToken) {
    api = null;
    showOnly(signedOutView);
    setStatus("error", "Sign in required");
    return;
  }

  try {
    const nextAPI = new ApiClient(humanToken);
    const response = await nextAPI.me();
    api = nextAPI;
    workspaces = Array.isArray(response.workspaces) ? response.workspaces : [];
    renderWorkspaces(workspaces);
    accountLabel.textContent = `Signed in as ${response.account.email}`;
    showOnly(connectForm);
    setStatus("ready", "Ready");
  } catch {
    api = null;
    showOnly(signedOutView);
    setStatus("error", "Session expired");
  }
}

async function connect(event: SubmitEvent) {
  event.preventDefault();
  if (!api) {
    showError("Your Codesk session is not available.");
    return;
  }
  const workspace = workspaces.find((candidate) => candidate.id === workspaceSelect.value);
  const name = daemonName.value.trim();
  if (!workspace || !name) {
    updateConnectButton();
    return;
  }

  connectButton.disabled = true;
  workspaceSelect.disabled = true;
  daemonName.disabled = true;
  setStatus("loading", "Creating desktop");
  try {
    const response = await api.createDaemon(workspace.id, name);
    setStatus("loading", "Opening desktop");
    submitDesktopHandoff(callback, {
      daemonId: response.daemon.id,
      token: response.token,
      workspaceId: workspace.id,
      workspaceName: workspace.name,
      workspaceSlug: workspace.slug,
      workspaceUrl: new URL(`/w/${encodeURIComponent(workspace.slug)}`, publicOrigin).href,
    });
  } catch (error) {
    workspaceSelect.disabled = false;
    daemonName.disabled = false;
    showError(
      error instanceof ApiError && error.status === 403
        ? "You do not have permission to create a desktop for this workspace."
        : "Codesk could not create the desktop connection.",
    );
  }
}

function renderWorkspaces(items: WorkspaceSummary[]) {
  workspaceSelect.replaceChildren(new Option("Choose a workspace", ""));
  for (const workspace of items) {
    workspaceSelect.add(new Option(workspace.name, workspace.id));
  }
  if (items.length === 0) {
    workspaceSelect.options[0].text = "No workspaces available";
    workspaceSelect.disabled = true;
  } else {
    workspaceSelect.disabled = false;
  }
  updateConnectButton();
}

function updateConnectButton() {
  connectButton.disabled = !api || !workspaceSelect.value || !daemonName.value.trim();
}

function showError(message: string) {
  errorMessage.textContent = message;
  showOnly(errorView);
  setStatus("error", "Blocked");
}

function showOnly(visible: HTMLElement) {
  for (const view of [loadingView, signedOutView, connectForm, errorView]) {
    view.hidden = view !== visible;
  }
}

function setStatus(state: "loading" | "ready" | "error", label: string) {
  statusChip.dataset.state = state;
  statusChip.textContent = label;
}

function requiredElement<T extends HTMLElement>(id: string): T {
  const element = document.getElementById(id);
  if (!element) {
    throw new Error(`Missing desktop handoff element: ${id}`);
  }
  return element as T;
}
