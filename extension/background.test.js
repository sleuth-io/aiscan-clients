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

const { buildTar, gzip, plan, upload } = require("./background.js");

const td = new TextDecoder();
const field = (tar, off, len) => td.decode(tar.subarray(off, off + len));

// A default upload span; the server hands these back from the sync plan and the
// client tags each evidence upload with the one it's fulfilling.
const SPAN = {
  start: "2026-06-01T00:00:00.000Z",
  end: "2026-07-01T00:00:00.000Z",
};

// Wire up global.chrome + global.fetch for a plan()/upload() call; returns the
// captured request and registers teardown. A cached, non-expired token skips the
// device flow. `respond` overrides the fetch reply (e.g. a GraphQL plan body).
function mockEnv(t, { instanceUrl = "https://app.skills.new", respond } = {}) {
  const store = {
    config: { instanceUrl },
    auth: {
      instanceUrl,
      accessToken: "tok",
      expiresAt: Date.now() + 3_600_000,
    },
  };
  const cap = {};
  global.chrome = {
    runtime: {},
    storage: {
      local: {
        get: async (key) =>
          typeof key === "string" ? { [key]: store[key] } : { ...store },
        set: async (obj) => Object.assign(store, obj),
        remove: async (key) => delete store[key],
      },
    },
  };
  global.fetch = async (url, opts) => {
    cap.url = url;
    cap.opts = opts;
    if (respond) return respond(url, opts);
    return {
      ok: true,
      status: 200,
      text: async () => JSON.stringify({ evidence: "AR_1" }),
    };
  };
  t.after(() => {
    delete global.chrome;
    delete global.fetch;
  });
  return { cap, instanceUrl, store };
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
  assert.equal(
    zlib.gunzipSync(Buffer.from(gz)).toString(),
    "hello tar payload",
  );
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

test("upload marks a re-send with force so the server re-parses it", async (t) => {
  // Ingest is idempotent on window + content, so a re-send of the same conversations
  // is accepted and ignored unless it says force. Without this the "send them again"
  // escape hatch reports success and changes nothing.
  const { cap } = mockEnv(t);
  const conv = { uuid: "u1", chat_messages: [{ sender: "human", text: "hi", content: [] }] };

  await upload({ provider: "claude-ai", conversations: [conv], span: SPAN, force: true }, 1);
  assert.equal(new URL(cap.url).searchParams.get("force"), "1");
});

test("upload omits force on an ordinary sync", async (t) => {
  const { cap } = mockEnv(t);
  const conv = { uuid: "u1", chat_messages: [{ sender: "human", text: "hi", content: [] }] };

  await upload({ provider: "claude-ai", conversations: [conv], span: SPAN }, 1);
  assert.equal(new URL(cap.url).searchParams.get("force"), null);
});

test("upload files claude.ai conversations as raw JSON under claude-web/", async (t) => {
  const { cap, instanceUrl } = mockEnv(t);
  const conv = {
    uuid: "u1",
    model: "claude-sonnet-4-6",
    chat_messages: [{ sender: "human", text: "hi", content: [] }],
  };

  const res = await upload(
    { provider: "claude-ai", conversations: [conv], span: SPAN },
    1,
  );

  assert.deepEqual(res, {
    ok: true,
    sessions: 1,
    evidence: "AR_1",
    reportsUrl: instanceUrl + "/aiscan",
  });
  assert.ok(cap.url.includes("source=claude-web"));
  // Span-based ingest contract: the upload is tagged with the span it fulfills,
  // plus the v1 schema version.
  const u = new URL(cap.url);
  assert.equal(u.searchParams.get("captured_start"), SPAN.start);
  assert.equal(u.searchParams.get("captured_end"), SPAN.end);
  assert.equal(u.searchParams.get("schema_version"), "1");
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

  // No span supplied → the all-time fallback [epoch, now].
  const res = await upload({ provider: "chatgpt", conversations: [conv] }, 1);

  assert.equal(res.sessions, 1);
  assert.ok(cap.url.includes("source=chatgpt-web"));
  const u = new URL(cap.url);
  assert.equal(u.searchParams.get("captured_start"), new Date(0).toISOString());
  assert.ok(u.searchParams.get("captured_end"), "captured_end present");
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
    payload: [
      [
        [
          ["c_1", "r_1"],
          null,
          [["hi"], 4],
          [[["rc_1", ["hello"]]]],
          [1769009299, 0],
        ],
      ],
    ],
  };

  const res = await upload(
    { provider: "gemini", conversations: [conv], span: SPAN },
    1,
  );

  assert.equal(res.sessions, 1);
  assert.ok(cap.url.includes("source=gemini-web"));
  const { name, json } = firstMember(cap.opts.body);
  assert.equal(name, "gemini/c_1.json");
  assert.deepEqual(json, conv);
});

test("upload skips gemini conversations whose payload failed to load", async (t) => {
  mockEnv(t);
  await assert.rejects(
    upload(
      {
        provider: "gemini",
        conversations: [{ conversation_id: "c_1", payload: null }],
      },
      1,
    ),
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

test("upload posts an empty body to record a confirmed-empty span", async (t) => {
  const { cap } = mockEnv(t);
  // An empty conversation set is a deliberate confirmed-empty window, not an
  // error: the server records the span as scanned so it never re-asks for it.
  const res = await upload(
    { provider: "claude-ai", conversations: [], span: SPAN },
    1,
  );

  assert.equal(res.sessions, 0);
  assert.equal(cap.opts.body.length, 0); // zero-length body
  const u = new URL(cap.url);
  assert.equal(u.searchParams.get("captured_start"), SPAN.start);
  assert.equal(u.searchParams.get("captured_end"), SPAN.end);
  assert.equal(u.searchParams.get("schema_version"), "1");
});

// ---------------------------------------------------------------------------
// sync plan: the client offers its available span; the server returns just the
// spans it still needs, scoped to the provider's source + the v1 schema version.
// ---------------------------------------------------------------------------

test("plan requests needed spans for the provider's source", async (t) => {
  const { cap, instanceUrl } = mockEnv(t, {
    respond: async () => ({
      ok: true,
      status: 200,
      text: async () =>
        JSON.stringify({
          data: {
            aiscanSyncPlan: {
              neededSpans: [
                { start: "2026-06-10T00:00:00Z", end: "2026-07-01T00:00:00Z" },
              ],
            },
          },
        }),
    }),
  });
  const available = [
    { start: "2026-01-01T00:00:00Z", end: "2026-07-01T00:00:00Z" },
  ];
  const res = await plan({ provider: "claude-ai", available }, 1);

  assert.ok(cap.url.endsWith("/graphql"));
  assert.equal(cap.opts.headers.authorization, "Bearer tok");
  const reqBody = JSON.parse(cap.opts.body);
  assert.equal(reqBody.variables.source, "claude-web");
  assert.equal(reqBody.variables.schemaVersion, 1);
  assert.deepEqual(reqBody.variables.available, available);
  assert.equal(res.reportsUrl, instanceUrl + "/aiscan");
  assert.equal(res.neededSpans.length, 1);
  assert.equal(res.neededSpans[0].start, "2026-06-10T00:00:00Z");
});

test("plan surfaces graphql errors", async (t) => {
  mockEnv(t, {
    respond: async () => ({
      ok: true,
      status: 200,
      text: async () =>
        JSON.stringify({ errors: [{ message: "aiscan not enabled" }] }),
    }),
  });
  await assert.rejects(
    plan({ provider: "claude-ai", available: [] }, 1),
    /aiscan not enabled/,
  );
});

test("plan clears the cached token and reports a rejected token", async (t) => {
  const { store } = mockEnv(t, {
    respond: async () => ({ ok: false, status: 401, text: async () => "" }),
  });
  await assert.rejects(
    plan({ provider: "claude-ai", available: [] }, 1),
    /unauthorized/,
  );
  assert.equal(store.auth, undefined); // cache cleared so the next scan re-auths
});
