// Pure helpers over unified diffs.

const HUNK = /^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@/;

/**
 * Returns, per file, the new-side line numbers a review comment can anchor to:
 * added and context lines inside hunks. GitHub rejects comments elsewhere.
 */
export function commentableLines(diff: string): Map<string, Set<number>> {
  const result = new Map<string, Set<number>>();
  let lines: Set<number> | undefined;
  let next = 0;
  let inHunk = false;
  for (const raw of diff.split("\n")) {
    if (raw.startsWith("diff --git ")) {
      lines = undefined;
      inHunk = false;
      continue;
    }
    if (!inHunk && raw.startsWith("+++ ")) {
      const target = raw.slice(4);
      lines = undefined;
      if (target.startsWith("b/")) {
        lines = new Set();
        result.set(target.slice(2), lines);
      }
      continue;
    }
    const hunk = HUNK.exec(raw);
    if (hunk) {
      next = Number(hunk[1]);
      inHunk = true;
      continue;
    }
    if (!inHunk || !lines) continue;
    if (raw.startsWith("+") || raw.startsWith(" ")) {
      lines.add(next++);
    }
    // "-" lines and "\ No newline at end of file" do not advance the new side.
  }
  return result;
}

/** Builds a unified diff from GitHub's per-file patches (which omit file headers). */
export function buildDiff(files: { path: string; previousPath?: string; status: string; patch?: string }[]): string {
  return files
    .map((f) => {
      const from = f.previousPath ?? f.path;
      const header = [
        `diff --git a/${from} b/${f.path}`,
        f.status === "added" ? "--- /dev/null" : `--- a/${from}`,
        f.status === "removed" ? "+++ /dev/null" : `+++ b/${f.path}`,
      ];
      const body = f.patch ?? "(diff not shown: binary or too large; inspect the file directly)";
      return [...header, body].join("\n");
    })
    .join("\n");
}

/** Splits a unified diff into per-file sections, keyed by new path (or old path for deletions). */
export function splitDiff(diff: string): { path: string; text: string }[] {
  const sections: { path: string; text: string }[] = [];
  for (const text of diff.split(/^(?=diff --git )/m)) {
    if (!text.startsWith("diff --git ")) continue;
    const m = /^diff --git a\/(.+?) b\/(.+)$/m.exec(text);
    sections.push({ path: m?.[2] ?? m?.[1] ?? "", text: text.replace(/\n$/, "") });
  }
  return sections;
}
