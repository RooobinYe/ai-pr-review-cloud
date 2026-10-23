import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import { join } from "node:path";
import { test } from "node:test";
import { getSession, postResult, putSession, putUsage, RejectedError } from "./controller.ts";
import type { ReviewResult } from "./schema.ts";

const RESULT = JSON.parse(readFileSync(join(import.meta.dirname, "../../contract/examples/valid-approve.json"), "utf8")) as ReviewResult;

async function withServer(handler: (req: IncomingMessage, body: string, res: ServerResponse) => void, fn: (url: string) => Promise<void>) {
  const server = createServer((req, res) => {
    let body = "";
    req.on("data", (chunk: Buffer) => (body += chunk.toString()));
    req.on("end", () => handler(req, body, res));
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  try {
    await fn(`http://127.0.0.1:${(server.address() as AddressInfo).port}/`);
  } finally {
    server.close();
  }
}

test("postResult retries server errors, then succeeds", async () => {
  const seen: string[] = [];
  await withServer(
    (req, body, res) => {
      seen.push(`${req.method} ${req.url} ${req.headers.authorization}`);
      assert.deepEqual(JSON.parse(body), RESULT);
      res.writeHead(seen.length < 3 ? 503 : 204).end();
    },
    (url) => postResult(url, "run-1", RESULT, { token: "t0k", retryDelayMs: 1 }),
  );
  assert.deepEqual(seen, Array(3).fill("POST /runs/run-1/result Bearer t0k"));
});

test("postResult does not retry a rejection", async () => {
  let calls = 0;
  await withServer(
    (_req, _body, res) => {
      calls++;
      res.writeHead(422).end("result is for another commit");
    },
    (url) => assert.rejects(postResult(url, "run-1", RESULT, { token: "t", retryDelayMs: 1 }), (err: Error) => err instanceof RejectedError && /another commit/.test(err.message)),
  );
  assert.equal(calls, 1);
});

test("postResult gives up after the last attempt", async () => {
  await withServer(
    (_req, _body, res) => res.writeHead(500).end("down"),
    (url) => assert.rejects(postResult(url, "run-1", RESULT, { token: "t", attempts: 2, retryDelayMs: 1 }), /POST \/runs\/run-1\/result failed after 2 attempts: 500 down/),
  );
});

test("getSession returns the snapshot, or undefined when there is none", async () => {
  const sessions: Record<string, string> = { "/runs/r1/session": '{"type":"session"}\n' };
  await withServer(
    (req, _body, res) => {
      const body = sessions[req.url!];
      if (body) res.writeHead(200, { "X-Session-Turn": "4" }).end(body);
      else res.writeHead(404).end("no session snapshot");
    },
    async (url) => {
      assert.equal(await getSession(url, "r1", { token: "t" }), '{"type":"session"}\n');
      assert.equal(await getSession(url, "r2", { token: "t" }), undefined);
    },
  );
});

test("putSession and putUsage send the turn", async () => {
  const seen: string[] = [];
  await withServer(
    (req, body, res) => {
      seen.push(`${req.method} ${req.url} ${req.headers["content-type"]} ${body}`);
      res.writeHead(204).end();
    },
    async (url) => {
      await putSession(url, "r1", 3, "a\nb\n", { token: "t" });
      await putUsage(url, "r1", { turn: 3, model: "m", input_tokens: 1, output_tokens: 2, cache_read_tokens: 3, cache_write_tokens: 0, cost_usd: 0.5 }, { token: "t" });
    },
  );
  assert.deepEqual(seen, [
    "PUT /runs/r1/session?turn=3 application/x-ndjson a\nb\n",
    'PUT /runs/r1/usage application/json {"turn":3,"model":"m","input_tokens":1,"output_tokens":2,"cache_read_tokens":3,"cache_write_tokens":0,"cost_usd":0.5}',
  ]);
});
