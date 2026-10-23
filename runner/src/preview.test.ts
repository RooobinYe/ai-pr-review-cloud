import assert from "node:assert/strict";
import { test } from "node:test";
import { checkKubectlArgs, safePreviewPath } from "./preview.ts";

test("kubectl accepts bounded evidence reads and denies escapes or writes", () => {
  for (const args of [["get", "pods"], ["describe", "pods", "demo-1"], ["get", "events", "--output=json"], ["logs", "demo-1", "--previous"]]) {
    assert.equal(checkKubectlArgs(args), true, args.join(" "));
  }
  for (const args of [["get", "secrets"], ["get", "pods", "--namespace=system"], ["get", "pods", "--kubeconfig=/tmp/x"], ["get", "pods", "--raw=/api/v1/secrets"], ["exec", "demo-1"], ["delete", "pods"], ["logs", "demo-1", "--follow"], ["logs", "demo-1", "--server=https://evil"], ["get", "pods", "--output=jsonpath={..token}"]]) {
    assert.equal(checkKubectlArgs(args), false, args.join(" "));
  }
});

test("preview paths stay relative and normalized", () => {
  for (const p of ["demo/chart", "values.yaml"]) assert.equal(safePreviewPath(p), true);
  for (const p of ["/tmp/chart", "../chart", "a/../../b", "a\\b", "a//b", "a/./b", "", null]) assert.equal(safePreviewPath(p), false);
});

test("kubectl receives reader configuration only through stdin", async () => {
  const { mkdtemp, writeFile, rm } = await import("node:fs/promises");
  const { tmpdir } = await import("node:os");
  const { join } = await import("node:path");
  const { kubectlTool } = await import("./preview.ts");
  const dir = await mkdtemp(join(tmpdir(), "kubectl-test-"));
  const oldPath = process.env.PATH;
  try {
    await writeFile(join(dir, "kubectl"), `#!${process.execPath}\nconst { readFileSync } = require('node:fs');\nconst input = readFileSync('/dev/stdin', 'utf8');\nif (process.argv.some(a => a.includes('reader-test-value')) || JSON.parse(input).token !== 'reader-test-value') process.exit(1);\nconsole.log('pod evidence');\n`, { mode: 0o700 });
    process.env.PATH = `${dir}:${oldPath}`;
    const tool = kubectlTool(() => ({ namespace: "pr-demo-1", status: "ready", kubeconfig: JSON.stringify({ token: "reader-test-value" }) }));
    const result = await tool.execute("id", { args: ["get", "pods"] }, undefined, undefined, undefined as never);
    assert.match(JSON.stringify(result), /pod evidence/);
    assert.doesNotMatch(JSON.stringify(result), /reader-test-value/);
  } finally {
    process.env.PATH = oldPath;
    await rm(dir, { recursive: true, force: true });
  }
});
