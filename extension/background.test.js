// Unit tests for the pure "service" functions in background.js — the transcoders,
// the tar/gzip packaging, and the upload orchestration. No build step and no
// dependencies: run with `node --test` (or `npm test`) from this directory.
//
// background.js guards its chrome.* listener registration behind a runtime check
// and exports these functions under CommonJS, so requiring it here is safe.

const test = require("node:test");
const assert = require("node:assert/strict");
const zlib = require("node:zlib");

const {
  transcodeClaudeConversation,
  transcodeChatGPTConversation,
  chatgptActiveBranch,
  epochToIso,
  buildTar,
  gzip,
  upload,
} = require("./background.js");

const CAPTURED_AT = "2026-06-24T00:00:00.000Z";
const parseJsonl = (s) =>
  s
    .trim()
    .split("\n")
    .map((l) => JSON.parse(l));

// ---------------------------------------------------------------------------
// claude.ai transcode
// ---------------------------------------------------------------------------

test("transcodeClaudeConversation maps turns and drops tool_result", () => {
  const conv = {
    uuid: "c-1",
    model: "claude-web",
    created_at: "2026-06-01T00:00:00Z",
    chat_messages: [
      { sender: "human", created_at: "2026-06-01T00:00:01Z", text: "fix bug" },
      {
        sender: "assistant",
        created_at: "2026-06-01T00:00:02Z",
        content: [
          { type: "thinking", thinking: "let me think" },
          { type: "text", text: "done" },
          { type: "tool_use", name: "web_search", input: { q: "x" } },
          { type: "tool_result", content: "ignored" },
        ],
      },
    ],
  };

  const lines = parseJsonl(transcodeClaudeConversation(conv, CAPTURED_AT));
  assert.equal(lines.length, 2);

  assert.deepEqual(lines[0], {
    timestamp: "2026-06-01T00:00:01Z",
    sessionId: "c-1",
    cwd: "claude.ai",
    type: "user",
    message: { role: "user", content: "fix bug" },
  });

  const asst = lines[1];
  assert.equal(asst.type, "assistant");
  assert.equal(asst.message.model, "claude-web");
  // thinking + text + tool_use survive, in order; tool_result is dropped.
  assert.deepEqual(asst.message.content, [
    { type: "thinking", text: "let me think" },
    { type: "text", text: "done" },
    { type: "tool_use", name: "web_search", input: { q: "x" } },
  ]);
});

test("transcodeClaudeConversation returns empty string without a uuid", () => {
  assert.equal(transcodeClaudeConversation({ chat_messages: [] }, CAPTURED_AT), "");
});

// ---------------------------------------------------------------------------
// chatgpt.com transcode (tree -> active branch -> JSONL)
// ---------------------------------------------------------------------------

// A tree whose active branch is root->n0->n1->n2->n3->n4, with a sibling branch
// (nX) hanging off n1 that must be ignored because it isn't under current_node.
function chatgptTree() {
  const node = (id, parent, children, message) => ({ id, parent, children, message });
  return {
    conversation_id: "g-1",
    default_model_slug: "gpt-5-5",
    current_node: "n4",
    mapping: {
      root: node("root", null, ["n0"], null),
      n0: node("n0", "root", ["n1"], {
        id: "n0",
        author: { role: "system" },
        create_time: 1781767100,
        content: { content_type: "text", parts: [""] },
        metadata: { is_visually_hidden_from_conversation: true },
      }),
      n1: node("n1", "n0", ["n2", "nX"], {
        id: "n1",
        author: { role: "user" },
        create_time: 1781767171,
        content: { content_type: "text", parts: ["list my assets"] },
      }),
      n2: node("n2", "n1", ["n3"], {
        id: "n2",
        author: { role: "assistant" },
        create_time: 1781767172,
        recipient: "api_tool.call_tool",
        content: { content_type: "code", text: '{"path":"/x","args":{}}' },
        metadata: { model_slug: "gpt-5-5" },
      }),
      n3: node("n3", "n2", ["n4"], {
        id: "n3",
        author: { role: "tool", name: "api_tool.call_tool" },
        create_time: 1781767173,
        content: { content_type: "code", text: "{result}" },
      }),
      n4: node("n4", "n3", [], {
        id: "n4",
        author: { role: "assistant" },
        create_time: 1781767174,
        content: { content_type: "text", parts: ["Your assets:"] },
        metadata: { model_slug: "gpt-5-5" },
      }),
      nX: node("nX", "n1", [], {
        id: "nX",
        author: { role: "assistant" },
        create_time: 1781767180,
        content: { content_type: "text", parts: ["WRONG BRANCH"] },
      }),
    },
  };
}

test("chatgptActiveBranch follows current_node and ignores sibling branches", () => {
  const msgs = chatgptActiveBranch(chatgptTree());
  // root has no message; n0..n4 do. nX is off-branch and excluded.
  assert.deepEqual(
    msgs.map((m) => m.id),
    ["n0", "n1", "n2", "n3", "n4"],
  );
});

test("transcodeChatGPTConversation maps user/tool_use/text and drops hidden+tool", () => {
  const lines = parseJsonl(transcodeChatGPTConversation(chatgptTree(), CAPTURED_AT));
  // n0 (hidden system) and n3 (tool role) dropped; nX off-branch.
  assert.equal(lines.length, 3);

  assert.deepEqual(lines[0], {
    timestamp: epochToIso(1781767171, CAPTURED_AT),
    sessionId: "g-1",
    cwd: "chatgpt.com",
    type: "user",
    message: { role: "user", content: "list my assets" },
  });

  assert.equal(lines[1].type, "assistant");
  assert.deepEqual(lines[1].message.content, [
    { type: "tool_use", name: "api_tool.call_tool", input: { path: "/x", args: {} } },
  ]);
  assert.equal(lines[1].message.model, "gpt-5-5");

  assert.deepEqual(lines[2].message.content, [{ type: "text", text: "Your assets:" }]);
});

test("transcodeChatGPTConversation maps reasoning thoughts to a thinking block", () => {
  const conv = {
    conversation_id: "g-2",
    current_node: "a",
    mapping: {
      a: {
        id: "a",
        parent: null,
        children: [],
        message: {
          id: "a",
          author: { role: "assistant" },
          create_time: 1781767200,
          content: {
            content_type: "thoughts",
            parts: [{ summary: "short", content: "deep reasoning" }],
          },
        },
      },
    },
  };
  const lines = parseJsonl(transcodeChatGPTConversation(conv, CAPTURED_AT));
  // content is preferred over summary.
  assert.deepEqual(lines[0].message.content, [
    { type: "thinking", text: "deep reasoning" },
  ]);
});

test("transcodeChatGPTConversation falls back to raw on unparseable tool args", () => {
  const conv = {
    conversation_id: "g-3",
    current_node: "a",
    mapping: {
      a: {
        id: "a",
        parent: null,
        children: [],
        message: {
          id: "a",
          author: { role: "assistant" },
          create_time: 1781767300,
          recipient: "python",
          content: { content_type: "code", text: "print('not json')" },
        },
      },
    },
  };
  const block = parseJsonl(transcodeChatGPTConversation(conv, CAPTURED_AT))[0]
    .message.content[0];
  assert.deepEqual(block, {
    type: "tool_use",
    name: "python",
    input: { raw: "print('not json')" },
  });
});

test("epochToIso converts seconds and falls back on non-numbers", () => {
  assert.equal(epochToIso(1781767171, "fb"), "2026-06-18T07:19:31.000Z");
  assert.equal(epochToIso(undefined, "fb"), "fb");
  assert.equal(epochToIso(NaN, "fb"), "fb");
});

// ---------------------------------------------------------------------------
// tar + gzip packaging
// ---------------------------------------------------------------------------

const td = new TextDecoder();
const field = (tar, off, len) => td.decode(tar.subarray(off, off + len));

test("buildTar writes a valid ustar header and pads to 512-byte blocks", () => {
  const data = "hello\n";
  const tar = buildTar([{ name: "projects/chatgpt/g-1.jsonl", data }], 1700000000);

  assert.equal(field(tar, 0, 26), "projects/chatgpt/g-1.jsonl");
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
// upload orchestration (transcode -> tar.gz -> POST), with chrome + fetch mocked
// ---------------------------------------------------------------------------

test("upload packs the batch and POSTs gzip to the ingest endpoint", async (t) => {
  const instanceUrl = "https://app.skills.new";
  // Seed a non-expired cached token so ensureToken skips the device flow.
  const store = {
    config: { instanceUrl },
    auth: { instanceUrl, accessToken: "tok", expiresAt: Date.now() + 3_600_000 },
  };
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
  let captured = null;
  global.fetch = async (url, opts) => {
    captured = { url, opts };
    return { ok: true, status: 200, text: async () => JSON.stringify({ run: "AR_9" }) };
  };
  t.after(() => {
    delete global.chrome;
    delete global.fetch;
  });

  const res = await upload(
    { provider: "chatgpt", conversations: [chatgptTree()], windowDays: 7 },
    1,
  );

  assert.deepEqual(res, {
    ok: true,
    reportUrl: instanceUrl + "/aiscan/AR_9",
    sessions: 1,
  });

  // Right endpoint, source, window, and bearer token.
  assert.ok(captured.url.startsWith(instanceUrl + "/api/aiscan/ingest?"));
  assert.ok(captured.url.includes("source=chatgpt-web"));
  assert.ok(captured.url.includes("window_days=7"));
  assert.equal(captured.opts.headers.authorization, "Bearer tok");
  assert.equal(captured.opts.headers["content-type"], "application/gzip");

  // Body is a gzipped tar whose first member is the chatgpt session file.
  const tar = zlib.gunzipSync(Buffer.from(captured.opts.body));
  assert.equal(field(tar, 0, 24), "projects/chatgpt/g-1.jsonl".slice(0, 24));
});

test("upload throws when nothing is transcodable", async (t) => {
  global.chrome = {
    runtime: {},
    storage: { local: { get: async () => ({}), set: async () => {}, remove: async () => {} } },
  };
  t.after(() => delete global.chrome);
  await assert.rejects(
    upload({ provider: "chatgpt", conversations: [{}] }, 1),
    /nothing to upload/,
  );
});
