import assert from "node:assert/strict";
import { test } from "node:test";
import { buildDiff, commentableLines, splitDiff } from "./diff.ts";

const DIFF = `diff --git a/src/a.ts b/src/a.ts
--- a/src/a.ts
+++ b/src/a.ts
@@ -10,4 +10,5 @@ function f() {
 const a = 1;
-const b = 2;
+const b = 3;
+const c = 4;
 return a;
\\ No newline at end of file
@@ -40,2 +41,2 @@
-old
+new
 tail
diff --git a/gone.ts b/gone.ts
--- a/gone.ts
+++ /dev/null
@@ -1,2 +0,0 @@
-x
-y
diff --git a/new.md b/new.md
--- /dev/null
+++ b/new.md
@@ -0,0 +1,2 @@
++++ looks like a header
+second`;

test("commentableLines keeps added and context lines on the new side", () => {
  const lines = commentableLines(DIFF);
  assert.deepEqual([...lines.keys()], ["src/a.ts", "new.md"]);
  assert.deepEqual([...lines.get("src/a.ts")!], [10, 11, 12, 13, 41, 42]);
  assert.deepEqual([...lines.get("new.md")!], [1, 2]);
});

test("buildDiff adds file headers to GitHub patches", () => {
  const diff = buildDiff([
    { path: "a.go", status: "modified", patch: "@@ -1 +1 @@\n-a\n+b" },
    { path: "b.go", previousPath: "old/b.go", status: "renamed", patch: "@@ -2 +2 @@\n-c\n+d" },
    { path: "c.go", status: "added", patch: "@@ -0,0 +1 @@\n+e" },
    { path: "logo.png", status: "added" },
  ]);
  assert.match(diff, /^diff --git a\/old\/b\.go b\/b\.go\n--- a\/old\/b\.go\n\+\+\+ b\/b\.go$/m);
  assert.match(diff, /^--- \/dev\/null\n\+\+\+ b\/c\.go$/m);
  assert.match(diff, /diff not shown/);
  assert.deepEqual(
    [...commentableLines(diff)].map(([p, l]) => [p, [...l]]),
    [["a.go", [1]], ["b.go", [2]], ["c.go", [1]], ["logo.png", []]],
  );
});

test("splitDiff returns one section per file", () => {
  assert.deepEqual(
    splitDiff(DIFF).map((s) => s.path),
    ["src/a.ts", "gone.ts", "new.md"],
  );
  assert.ok(splitDiff(DIFF)[1]!.text.endsWith("-y"));
});
