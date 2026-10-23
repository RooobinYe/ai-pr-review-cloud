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
