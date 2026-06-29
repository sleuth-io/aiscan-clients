// Unit tests for the pure "service" functions in background.js — the tar/gzip
// packaging and the upload orchestration. No build step and no dependencies:
// run with `node --test` (or `npm test`) from this directory.
//
// background.js guards its chrome.* listener registration behind a runtime check
// and exports these functions under CommonJS, so requiring it here is safe.
// Per-provider parsing now lives server-side (pulse parsers/), so there are no
// client-side transcoders to test here — the client only packs raw captures.

const test = require("node:test");
const assert = require("node:assert/strict");
const zlib = require("node:zlib");

const { buildTar, gzip, upload } = require("./background.js");

const td = new TextDecoder();
const field = (tar, off, len) => td.decode(tar.subarray(off, off + len));

// Wire up global.chrome + global.fetch for an upload() call; returns the captured
// request and registers teardown. A cached, non-expired token skips the device flow.
function mockEnv(t, { instanceUrl = "https://app.skills.new", runGid = "AR_1" } = {}) {
  const store = {
    config: { instanceUrl },
    auth: { instanceUrl, accessToken: "tok", expiresAt: Date.now() + 3_600_000 },
  };
  const cap = {};
  global.chrome = {
    runtime: {},
    storage: {
      local: {
        get: async (key) => (typeof key === "string" ? { [key]: store[key] } : { ...store }),
        set: async (obj) => Object.assign(store, obj),
        remove: async (key) => delete store[key],
      },
    },
  };
  global.fetch = async (url, opts) => {
    cap.url = url;
    cap.opts = opts;
    return { ok: true, status: 200, text: async () => JSON.stringify({ run: runGid }) };
  };
  t.after(() => {
    delete global.chrome;
    delete global.fetch;
  });
  return { cap, instanceUrl };
}

// ---------------------------------------------------------------------------
// tar + gzip packaging
// ---------------------------------------------------------------------------

test("buildTar writes a valid ustar header and pads to 512-byte blocks", () => {
  const data = "hello\n";
  const tar = buildTar([{ name: "claude-web/u1.json", data }], 1700000000);

  assert.equal(field(tar, 0, 18), "claude-web/u1.json");
  assert.equal(parseInt(field(tar, 124, 11), 8), data.length); // size octal
  assert.equal(field(tar, 257, 5), "ustar"); // magic
  assert.equal(field(tar, 512, data.length), data); // data follows the header

  // Whole archive is block-aligned and ends with two zero terminator blocks.
  assert.equal(tar.length % 512, 0);
  assert.ok(tar.subarray(tar.length - 1024).every((b) => b === 0));

  // The checksum field equals the sum of all header bytes with chksum as spaces.
  const hdr = Uint8Array.from(tar.subarray(0, 512));
  for (let i = 148; i < 156; i++) hdr[i] = 0x20;
  const sum = hdr.reduce((a, b) => a + b, 0);
  assert.equal(parseInt(field(tar, 148, 6), 8), sum);
});

test("gzip output round-trips through gunzip", async () => {
  const bytes = new TextEncoder().encode("hello tar payload");
  const gz = await gzip(bytes);
  assert.equal(zlib.gunzipSync(Buffer.from(gz)).toString(), "hello tar payload");
});

// ---------------------------------------------------------------------------
// upload orchestration: every provider ships its RAW capture as JSON, filed
// under its own dir, with the matching source. Parsing happens server-side.
// ---------------------------------------------------------------------------

// Extract the first tar member's name and parse its JSON body.
function firstMember(body) {
  const tar = zlib.gunzipSync(Buffer.from(body));
  const name = field(tar, 0, 100).replace(/\0.*/s, "");
  const size = parseInt(field(tar, 124, 11), 8);
  return { name, json: JSON.parse(field(tar, 512, size)) };
}

test("upload files claude.ai conversations as raw JSON under claude-web/", async (t) => {
  const { cap, instanceUrl } = mockEnv(t);
  const conv = {
    uuid: "u1",
    model: "claude-sonnet-4-6",
    chat_messages: [{ sender: "human", text: "hi", content: [] }],
  };

  const res = await upload({ provider: "claude-ai", conversations: [conv], windowDays: 7 }, 1);

  assert.deepEqual(res, { ok: true, reportUrl: instanceUrl + "/aiscan/AR_1", sessions: 1 });
  assert.ok(cap.url.includes("source=claude-web"));
  assert.ok(cap.url.includes("window_days=7"));
  assert.equal(cap.opts.headers.authorization, "Bearer tok");
  const { name, json } = firstMember(cap.opts.body);
  assert.equal(name, "claude-web/u1.json"); // raw, server parses
  assert.deepEqual(json, conv);
});

test("upload files chatgpt conversations as raw JSON under chatgpt/", async (t) => {
  const { cap } = mockEnv(t);
  const conv = {
    conversation_id: "g1",
    default_model_slug: "gpt-5-5",
    current_node: "n1",
    mapping: { n1: { id: "n1", parent: null, children: [], message: null } },
  };

  const res = await upload({ provider: "chatgpt", conversations: [conv], windowDays: 0 }, 1);

  assert.equal(res.sessions, 1);
  assert.ok(cap.url.includes("source=chatgpt-web"));
  const { name, json } = firstMember(cap.opts.body);
  assert.equal(name, "chatgpt/g1.json");
  assert.deepEqual(json, conv);
});

test("upload ships gemini conversations as raw JSON under gemini/", async (t) => {
  const { cap } = mockEnv(t);
  // Gemini arrives already transport-unwrapped: { conversation_id, …, payload }.
  const conv = {
    conversation_id: "c_1",
    title: "Weight Reduction",
    updated_at: "2026-01-21T00:00:00.000Z",
    payload: [[[["c_1", "r_1"], null, [["hi"], 4], [[["rc_1", ["hello"]]]], [1769009299, 0]]]],
  };

  const res = await upload({ provider: "gemini", conversations: [conv], windowDays: 0 }, 1);

  assert.equal(res.sessions, 1);
  assert.ok(cap.url.includes("source=gemini-web"));
  const { name, json } = firstMember(cap.opts.body);
  assert.equal(name, "gemini/c_1.json");
  assert.deepEqual(json, conv);
});

test("upload skips gemini conversations whose payload failed to load", async (t) => {
  mockEnv(t);
  await assert.rejects(
    upload({ provider: "gemini", conversations: [{ conversation_id: "c_1", payload: null }] }, 1),
    /nothing to upload/,
  );
});

test("upload throws when there is nothing capturable", async (t) => {
  mockEnv(t);
  await assert.rejects(
    upload({ provider: "chatgpt", conversations: [null] }, 1),
    /nothing to upload/,
  );
});
