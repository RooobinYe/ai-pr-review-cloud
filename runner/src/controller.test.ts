import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import { join } from "node:path";
import { test } from "node:test";
import { postResult, ResultRejectedError } from "./controller.ts";
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
      seen.push(`${req.method} ${req.url}`);
      assert.deepEqual(JSON.parse(body), RESULT);
      res.writeHead(seen.length < 3 ? 503 : 204).end();
    },
    (url) => postResult(url, "run-1", RESULT, { retryDelayMs: 1 }),
  );
  assert.deepEqual(seen, Array(3).fill("POST /runs/run-1/result"));
});

test("postResult does not retry a rejection", async () => {
  let calls = 0;
  await withServer(
    (_req, _body, res) => {
      calls++;
      res.writeHead(422).end("result is for another commit");
    },
    (url) => assert.rejects(postResult(url, "run-1", RESULT, { retryDelayMs: 1 }), (err: Error) => err instanceof ResultRejectedError && /another commit/.test(err.message)),
  );
  assert.equal(calls, 1);
});

test("postResult gives up after the last attempt", async () => {
  await withServer(
    (_req, _body, res) => res.writeHead(500).end("down"),
    (url) => assert.rejects(postResult(url, "run-1", RESULT, { attempts: 2, retryDelayMs: 1 }), /after 2 attempts: 500 down/),
  );
});
