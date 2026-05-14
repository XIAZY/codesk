import { Awareness, applyAwarenessUpdate, encodeAwarenessUpdate } from "y-protocols/awareness.js";
import * as syncProtocol from "y-protocols/sync.js";
import * as decoding from "lib0/decoding";
import * as encoding from "lib0/encoding";
import * as Y from "yjs";

export const messageSync = 0;
export const messageAwareness = 1;

export function encodeSyncStep1(doc: Y.Doc) {
  const encoder = encoding.createEncoder();
  encoding.writeVarUint(encoder, messageSync);
  syncProtocol.writeSyncStep1(encoder, doc);
  return encoding.toUint8Array(encoder);
}

export function encodeSyncUpdate(update: Uint8Array) {
  const encoder = encoding.createEncoder();
  encoding.writeVarUint(encoder, messageSync);
  syncProtocol.writeUpdate(encoder, update);
  return encoding.toUint8Array(encoder);
}

export function encodeSyncReply(reply: Uint8Array) {
  const encoder = encoding.createEncoder();
  encoding.writeVarUint(encoder, messageSync);
  encoding.writeUint8Array(encoder, reply);
  return encoding.toUint8Array(encoder);
}

export function encodeAwarenessMessage(awareness: Awareness, clients: number[]) {
  const update = encodeAwarenessUpdate(awareness, clients);
  const encoder = encoding.createEncoder();
  encoding.writeVarUint(encoder, messageAwareness);
  encoding.writeUint8Array(encoder, update);
  return encoding.toUint8Array(encoder);
}

export function readProtocolMessage(bytes: Uint8Array) {
  const { value, offset } = decodeVarUint(bytes);
  return { messageType: value, payload: bytes.slice(offset) };
}

export function handleSyncPayload(doc: Y.Doc, payload: Uint8Array) {
  const decoder = decoding.createDecoder(payload);
  const encoder = encoding.createEncoder();
  syncProtocol.readSyncMessage(decoder, encoder, doc, "remote");
  const reply = encoding.toUint8Array(encoder);
  if (isEmptySyncStep2(reply)) {
    return null;
  }
  return reply.length > 0 ? encodeSyncReply(reply) : null;
}

export function handleAwarenessPayload(awareness: Awareness, payload: Uint8Array) {
  applyAwarenessUpdate(awareness, payload, "remote");
}

function decodeVarUint(bytes: Uint8Array) {
  let value = 0;
  let shift = 0;
  let offset = 0;
  while (offset < bytes.length) {
    const current = bytes[offset];
    value |= (current & 0x7f) << shift;
    offset += 1;
    if ((current & 0x80) === 0) {
      return { value, offset };
    }
    shift += 7;
  }
  return { value: 0, offset: 0 };
}

function isEmptySyncStep2(bytes: Uint8Array) {
  return bytes.length === 4 && bytes[0] === 1 && bytes[1] === 2 && bytes[2] === 0 && bytes[3] === 0;
}
