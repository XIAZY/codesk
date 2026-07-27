#!/usr/bin/env node

import fs from "node:fs";
import path from "node:path";
import readline from "node:readline";
import { fileURLToPath } from "node:url";

const args = process.argv.slice(2);
if (args.length === 1 && args[0] === "--version") {
  process.stdout.write("codex-cli 0.144.5\n");
  process.exit(0);
}
if (args.length === 2 && args[0] === "app-server" && args[1] === "--help") {
  process.exit(0);
}
if (args.length !== 1 || args[0] !== "app-server") {
  process.exit(2);
}

const fixtureDir = path.dirname(fileURLToPath(import.meta.url));
const baseModels = JSON.parse(
  fs.readFileSync(path.join(fixtureDir, "model-profile-models.json"), "utf8"),
);
const modePath = "/workspace/model-profile-catalog-mode";
const eventPath = "/workspace/model-profile-fixture-events.jsonl";
const modelListDelayMs = Number.parseInt(
  process.env.NOTTY_TEST_MODEL_PROFILE_DELAY_MS ?? "0",
  10,
);
if (!Number.isFinite(modelListDelayMs) || modelListDelayMs < 0) {
  throw new Error("NOTTY_TEST_MODEL_PROFILE_DELAY_MS must be a non-negative integer");
}

function currentModels() {
  const models = structuredClone(baseModels);
  let mode = "base";
  try {
    mode = fs.readFileSync(modePath, "utf8").trim() || "base";
  } catch (error) {
    if (error.code !== "ENOENT") {
      throw error;
    }
  }
  if (mode === "base") {
    return models;
  }
  if (mode === "vanished") {
    return models
      .filter((model) => model.model !== "gpt-5.6-sol")
      .map((model) => ({...model, isDefault: model.model === "gpt-5.6-luna"}));
  }
  if (mode === "default-moved") {
    return models.map((model) => ({
      ...model,
      isDefault: model.model === "gpt-5.6-luna",
    }));
  }
  throw new Error(`unsupported model-profile fixture mode ${JSON.stringify(mode)}`);
}

function emit(payload) {
  process.stdout.write(`${JSON.stringify(payload)}\n`);
}

function record(request) {
  fs.appendFileSync(
    eventPath,
    `${JSON.stringify({
      pid: process.pid,
      processCwd: process.cwd(),
      method: request.method,
      params: request.params ?? null,
    })}\n`,
  );
}

const input = readline.createInterface({
  input: process.stdin,
  crlfDelay: Infinity,
});

for await (const line of input) {
  const request = JSON.parse(line);
  record(request);
  switch (request.method) {
    case "initialize":
      emit({id: request.id, result: {}});
      break;
    case "initialized":
      break;
    case "model/list":
      if (modelListDelayMs > 0) {
        await new Promise((resolve) => setTimeout(resolve, modelListDelayMs));
      }
      emit({id: request.id, result: {data: currentModels(), nextCursor: null}});
      break;
    case "thread/start":
      emit({
        id: request.id,
        result: {thread: {id: `thread_${path.basename(process.cwd())}`}},
      });
      break;
    case "thread/resume":
      emit({id: request.id, result: {}});
      break;
    case "turn/start":
      emit({
        id: request.id,
        result: {turn: {id: `turn_${path.basename(process.cwd())}`}},
      });
      break;
    default:
      if (request.id !== undefined && request.id !== null) {
        emit({
          id: request.id,
          error: {code: -32601, message: `unsupported fixture method ${request.method}`},
        });
      }
  }
}
