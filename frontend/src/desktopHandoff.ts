export type DesktopHandoffPayload = {
  daemonId: string;
  token: string;
  workspaceId: string;
  workspaceName: string;
  workspaceSlug: string;
  workspaceUrl: string;
};

const canonicalBase64UrlTail = "AEIMQUYcgkosw048";
const callbackPattern = new RegExp(
  `^http://127\\.0\\.0\\.1:([1-9][0-9]{0,4})(/desktop/connect/[A-Za-z0-9_-]{42}[${canonicalBase64UrlTail}])$`,
);
const controlCharacters = /[\u0000-\u001f\u007f-\u009f]/;
const maxTokenBytes = 4 << 10;

const fields: ReadonlyArray<readonly [string, keyof DesktopHandoffPayload]> = [
  ["daemon_id", "daemonId"],
  ["token", "token"],
  ["workspace_id", "workspaceId"],
  ["workspace_name", "workspaceName"],
  ["workspace_slug", "workspaceSlug"],
  ["workspace_url", "workspaceUrl"],
];

export function parseDesktopCallback(value: string): URL {
  const match = callbackPattern.exec(value);
  if (!match) {
    throw new Error("Invalid Codesk Desktop callback URL.");
  }
  const port = Number(match[1]);
  if (!Number.isSafeInteger(port) || port < 1 || port > 65535) {
    throw new Error("Invalid Codesk Desktop callback URL.");
  }

  let callback: URL;
  try {
    callback = new URL(value);
  } catch {
    throw new Error("Invalid Codesk Desktop callback URL.");
  }
  if (callback.href !== value) {
    throw new Error("Invalid Codesk Desktop callback URL.");
  }
  return callback;
}

export function parseDesktopHandoffPageURL(value: string): URL {
  let pageURL: URL;
  try {
    pageURL = new URL(value);
  } catch {
    throw new Error("Invalid Codesk Desktop handoff page URL.");
  }
  const callbackValues = pageURL.searchParams.getAll("callback");
  if (
    (pageURL.protocol !== "https:" && pageURL.protocol !== "http:") ||
    pageURL.username ||
    pageURL.password ||
    value.includes("#") ||
    pageURL.hash ||
    callbackValues.length !== 1 ||
    Array.from(pageURL.searchParams.keys()).some((key) => key !== "callback")
  ) {
    throw new Error("Invalid Codesk Desktop handoff page URL.");
  }
  return parseDesktopCallback(callbackValues[0]);
}

export function createDesktopHandoffForm(
  doc: Document,
  callback: string,
  payload: DesktopHandoffPayload,
): HTMLFormElement {
  const callbackURL = parseDesktopCallback(callback);
  validatePayload(payload);

  const form = doc.createElement("form");
  form.method = "POST";
  form.action = callbackURL.href;
  form.enctype = "application/x-www-form-urlencoded";
  form.acceptCharset = "UTF-8";
  form.noValidate = true;
  form.hidden = true;

  for (const [fieldName, payloadKey] of fields) {
    const input = doc.createElement("input");
    input.type = "hidden";
    input.name = fieldName;
    input.value = payload[payloadKey];
    form.append(input);
  }
  return form;
}

export function submitDesktopHandoff(callback: string, payload: DesktopHandoffPayload): void {
  const form = createDesktopHandoffForm(document, callback, payload);
  document.body.append(form);
  try {
    form.submit();
  } finally {
    queueMicrotask(() => {
      for (const input of form.querySelectorAll("input")) {
        input.value = "";
      }
      form.remove();
    });
  }
}

function validatePayload(payload: DesktopHandoffPayload) {
  assertText(payload.daemonId, 128);
  assertOpaqueToken(payload.token);
  assertText(payload.workspaceId, 128);
  assertText(payload.workspaceName, 256);
  assertText(payload.workspaceSlug, 128);
  assertWorkspaceURL(payload.workspaceUrl);
}

function assertText(value: string, maxLength: number) {
  if (
    !value ||
    !isWellFormedUTF16(value) ||
    new TextEncoder().encode(value).byteLength > maxLength ||
    value.trim() !== value ||
    controlCharacters.test(value)
  ) {
    throw new Error("Invalid Codesk Desktop handoff payload.");
  }
}

function assertOpaqueToken(value: string) {
  if (
    !value ||
    !isWellFormedUTF16(value) ||
    new TextEncoder().encode(value).byteLength > maxTokenBytes ||
    controlCharacters.test(value)
  ) {
    throw new Error("Invalid Codesk Desktop handoff payload.");
  }
}

function isWellFormedUTF16(value: string) {
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index);
    if (code >= 0xd800 && code <= 0xdbff) {
      const next = value.charCodeAt(index + 1);
      if (next < 0xdc00 || next > 0xdfff) {
        return false;
      }
      index += 1;
    } else if (code >= 0xdc00 && code <= 0xdfff) {
      return false;
    }
  }
  return true;
}

function assertWorkspaceURL(value: string) {
  assertText(value, 2048);
  let workspaceURL: URL;
  try {
    workspaceURL = new URL(value);
  } catch {
    throw new Error("Invalid Codesk Desktop handoff payload.");
  }
  if (
    (workspaceURL.protocol !== "https:" && workspaceURL.protocol !== "http:") ||
    !workspaceURL.host ||
    workspaceURL.username ||
    workspaceURL.password ||
    value.includes("?") ||
    value.includes("#") ||
    workspaceURL.search ||
    workspaceURL.hash
  ) {
    throw new Error("Invalid Codesk Desktop handoff payload.");
  }
}
