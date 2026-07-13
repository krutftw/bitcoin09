export const MAX_CONTENT_BYTES = 20 * 1024 * 1024;

const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder("utf-8", { fatal: true });
const MAX_METADATA_BYTES = 64 * 1024;
const NONCE_BYTES = 12;

const safetyFirst = [
  "amber", "apple", "april", "arrow", "atlas", "birch", "blue", "brass",
  "brick", "calm", "cedar", "clay", "clear", "cloud", "cobalt", "coral",
  "crane", "dawn", "delta", "ember", "fern", "field", "flint", "forest",
  "frost", "glass", "gold", "green", "harbor", "hazel", "indigo", "iron",
  "jade", "juniper", "lake", "linen", "maple", "marble", "mint", "navy",
  "north", "oak", "ocean", "olive", "opal", "orange", "paper", "pearl",
  "pine", "plum", "quiet", "rain", "reed", "river", "sage", "silver",
  "slate", "snow", "south", "stone", "teal", "umber", "willow", "winter",
];

const safetySecond = [
  "acorn", "beacon", "bridge", "brook", "cabin", "canyon", "circle", "comet",
  "cove", "drift", "falcon", "feather", "garden", "gate", "grove", "harvest",
  "hill", "island", "kettle", "lantern", "leaf", "meadow", "mesa", "moon",
  "nest", "orbit", "otter", "path", "peak", "pocket", "pond", "quartz",
  "raven", "ridge", "robin", "sail", "shell", "shore", "signal", "sparrow",
  "spring", "star", "stream", "summit", "sun", "thistle", "timber", "trail",
  "valley", "violet", "wave", "wheat", "wheel", "wind", "window", "wing",
  "wood", "wren", "yard", "yarrow", "zephyr", "zinc", "acacia", "anchor",
];

function randomBytes(size) {
  const value = new Uint8Array(size);
  crypto.getRandomValues(value);
  return value;
}

function asBytes(value, size, label) {
  if (!(value instanceof Uint8Array) || value.length !== size) {
    throw new Error(`Invalid ${label}.`);
  }
  return value;
}

function base64urlEncode(value) {
  let binary = "";
  for (const byte of value) binary += String.fromCharCode(byte);
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/u, "");
}

function base64urlDecode(value, label = "pairing code") {
  if (typeof value !== "string" || value.length === 0 || value.length > 4096 ||
      !/^[A-Za-z0-9_-]+$/u.test(value)) {
    throw new Error(`Invalid ${label}.`);
  }
  const padding = "=".repeat((4 - (value.length % 4)) % 4);
  let binary;
  try {
    binary = atob(value.replaceAll("-", "+").replaceAll("_", "/") + padding);
  } catch {
    throw new Error(`Invalid ${label}.`);
  }
  const decoded = Uint8Array.from(binary, (character) => character.charCodeAt(0));
  if (base64urlEncode(decoded) !== value) throw new Error(`Invalid ${label}.`);
  return decoded;
}

function normalizeAPIBase(value) {
  let parsed;
  try {
    parsed = new URL(value);
  } catch {
    throw new Error("A secure Nine Inbox address is required.");
  }
  const loopback = parsed.hostname === "127.0.0.1" || parsed.hostname === "::1" || parsed.hostname === "localhost";
  if ((parsed.protocol !== "https:" && !(parsed.protocol === "http:" && loopback)) ||
      parsed.username || parsed.password || parsed.search || parsed.hash || (parsed.pathname !== "/" && parsed.pathname !== "")) {
    throw new Error("A secure Nine Inbox address is required.");
  }
  return parsed.origin;
}

function validPublicID(value) {
  try {
    return base64urlDecode(value, "item ID").length === 16;
  } catch {
    return false;
  }
}

function normalizePairingBundle(bundle, requireMailbox = true) {
  if (!bundle || bundle.v !== 1) throw new Error("Invalid pairing code.");
  const mailbox = bundle.mailboxId || "";
  if (requireMailbox && !validPublicID(mailbox)) throw new Error("Invalid pairing code.");
  return {
    v: 1,
    apiBase: normalizeAPIBase(bundle.apiBase),
    mailboxId: mailbox,
    key: asBytes(bundle.key, 32, "pairing code").slice(),
    writeToken: asBytes(bundle.writeToken, 32, "pairing code").slice(),
    recoveryToken: asBytes(bundle.recoveryToken, 16, "pairing code").slice(),
  };
}

export function createPairingBundle(apiBase) {
  return {
    v: 1,
    apiBase: normalizeAPIBase(apiBase),
    mailboxId: "",
    key: randomBytes(32),
    writeToken: randomBytes(32),
    recoveryToken: randomBytes(16),
  };
}

export function createItemId() {
  return base64urlEncode(randomBytes(16));
}

export function encodePairingFragment(bundle) {
  const value = normalizePairingBundle(bundle);
  return base64urlEncode(textEncoder.encode(JSON.stringify({
    v: value.v,
    apiBase: value.apiBase,
    mailboxId: value.mailboxId,
    key: base64urlEncode(value.key),
    writeToken: base64urlEncode(value.writeToken),
    recoveryToken: base64urlEncode(value.recoveryToken),
  })));
}

export function decodePairingFragment(value) {
  try {
    const parsed = JSON.parse(textDecoder.decode(base64urlDecode(value)));
    if (!parsed || Object.keys(parsed).sort().join(",") !== "apiBase,key,mailboxId,recoveryToken,v,writeToken") {
      throw new Error("shape");
    }
    return normalizePairingBundle({
      ...parsed,
      key: base64urlDecode(parsed.key),
      writeToken: base64urlDecode(parsed.writeToken),
      recoveryToken: base64urlDecode(parsed.recoveryToken),
    });
  } catch {
    throw new Error("Invalid pairing code.");
  }
}

export async function hashToken(value) {
  if (!(value instanceof Uint8Array) || (value.length !== 16 && value.length !== 32)) {
    throw new Error("Invalid inbox token.");
  }
  return new Uint8Array(await crypto.subtle.digest("SHA-256", value));
}

export async function safetyWords(bundle, localChallenge, remoteChallenge) {
  const value = normalizePairingBundle(bundle);
  if (typeof localChallenge !== "string" || typeof remoteChallenge !== "string" ||
      localChallenge.length < 8 || remoteChallenge.length < 8 ||
      localChallenge.length > 128 || remoteChallenge.length > 128) {
    throw new Error("Invalid pairing challenge.");
  }
  const challenges = [localChallenge, remoteChallenge].sort();
  const material = new Uint8Array(textEncoder.encode("nine-inbox:safety:v1\0" + challenges.join("\0")).length + value.key.length);
  material.set(textEncoder.encode("nine-inbox:safety:v1\0" + challenges.join("\0")), 0);
  material.set(value.key, material.length - value.key.length);
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", material));
  return `${safetyFirst[digest[0] % safetyFirst.length]} ${safetySecond[digest[1] % safetySecond.length]}`;
}

function normalizeDate(value, label) {
  if (typeof value !== "string") throw new Error(`Invalid item ${label}.`);
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) throw new Error(`Invalid item ${label}.`);
  return date.toISOString();
}

function normalizeItem(input) {
  if (!input || !["text", "link", "file", "photo"].includes(input.kind)) throw new Error("Invalid item kind.");
  if (typeof input.device !== "string" || input.device.trim().length < 1 || input.device.length > 64) throw new Error("Invalid item device.");
  if (typeof input.name !== "string" || input.name.length > 255 || /[\u0000-\u001f]/u.test(input.name)) throw new Error("Invalid item name.");
  if (typeof input.type !== "string" || input.type.length > 128 || /[\r\n]/u.test(input.type)) throw new Error("Invalid item type.");
  const createdAt = normalizeDate(input.createdAt, "created time");
  const expiresAt = normalizeDate(input.expiresAt, "expiry");
  if (new Date(expiresAt) <= new Date(createdAt)) throw new Error("Invalid item expiry.");

  let text = "";
  let data = new Uint8Array();
  if (input.kind === "text" || input.kind === "link") {
    if (typeof input.text !== "string" || input.text.length < 1) throw new Error("Invalid item text.");
    if (input.data && (!(input.data instanceof Uint8Array) || input.data.length !== 0)) throw new Error("Invalid item data.");
    text = input.text;
    data = new Uint8Array();
    if (input.kind === "link") {
      let link;
      try { link = new URL(text); } catch { throw new Error("Invalid item link."); }
      if (link.protocol !== "https:" && link.protocol !== "http:") throw new Error("Invalid item link.");
    }
  } else {
    if (!input.name || !(input.data instanceof Uint8Array) || (input.text && input.text !== "")) throw new Error("Invalid item file.");
    data = input.data.slice();
  }
  const size = input.kind === "text" || input.kind === "link" ? textEncoder.encode(text).length : data.length;
  if (size > MAX_CONTENT_BYTES) throw new Error("Items must be 20 MiB or smaller.");
  return {
    v: 1,
    kind: input.kind,
    name: input.name,
    type: input.type,
    size,
    device: input.device.trim(),
    createdAt,
    expiresAt,
    text,
    data,
  };
}

function additionalData(mailboxId, itemId) {
  if (!validPublicID(mailboxId) || !validPublicID(itemId)) throw new Error("Invalid item binding.");
  return textEncoder.encode(`nine-inbox:v1:${mailboxId}:${itemId}`);
}

async function importInboxKey(bundle, mailboxId) {
  const value = normalizePairingBundle(bundle);
  if (value.mailboxId !== mailboxId) throw new Error("Pairing mailbox mismatch.");
  return crypto.subtle.importKey("raw", value.key, { name: "AES-GCM" }, false, ["encrypt", "decrypt"]);
}

export async function encryptItem(input, bundle, mailboxId, itemId) {
  const item = normalizeItem(input);
  const metadata = textEncoder.encode(JSON.stringify({
    v: item.v,
    kind: item.kind,
    name: item.name,
    type: item.type,
    size: item.size,
    device: item.device,
    createdAt: item.createdAt,
    expiresAt: item.expiresAt,
    text: item.text,
  }));
  if (metadata.length > MAX_METADATA_BYTES) throw new Error("Invalid item metadata.");
  const plaintext = new Uint8Array(4 + metadata.length + item.data.length);
  new DataView(plaintext.buffer).setUint32(0, metadata.length, false);
  plaintext.set(metadata, 4);
  plaintext.set(item.data, 4 + metadata.length);
  const nonce = randomBytes(NONCE_BYTES);
  const key = await importInboxKey(bundle, mailboxId);
  const encrypted = new Uint8Array(await crypto.subtle.encrypt({
    name: "AES-GCM",
    iv: nonce,
    additionalData: additionalData(mailboxId, itemId),
    tagLength: 128,
  }, key, plaintext));
  const output = new Uint8Array(nonce.length + encrypted.length);
  output.set(nonce, 0);
  output.set(encrypted, nonce.length);
  return output;
}

export async function decryptItem(ciphertext, bundle, mailboxId, itemId) {
  try {
    if (!(ciphertext instanceof Uint8Array) || ciphertext.length < NONCE_BYTES + 16 + 4) throw new Error("short");
    const key = await importInboxKey(bundle, mailboxId);
    const plaintext = new Uint8Array(await crypto.subtle.decrypt({
      name: "AES-GCM",
      iv: ciphertext.slice(0, NONCE_BYTES),
      additionalData: additionalData(mailboxId, itemId),
      tagLength: 128,
    }, key, ciphertext.slice(NONCE_BYTES)));
    const metadataLength = new DataView(plaintext.buffer, plaintext.byteOffset, plaintext.byteLength).getUint32(0, false);
    if (metadataLength < 2 || metadataLength > MAX_METADATA_BYTES || 4 + metadataLength > plaintext.length) throw new Error("metadata");
    const metadata = JSON.parse(textDecoder.decode(plaintext.slice(4, 4 + metadataLength)));
    const data = plaintext.slice(4 + metadataLength);
    const item = normalizeItem({ ...metadata, data });
    if (metadata.v !== 1 || metadata.size !== item.size || Object.keys(metadata).sort().join(",") !== "createdAt,device,expiresAt,kind,name,size,text,type,v") {
      throw new Error("shape");
    }
    return item;
  } catch {
    throw new Error("Unable to decrypt item.");
  }
}
