// Runner entry point. Two modes share one review pipeline:
//   - cluster mode: RUN_ID is set; inputs come from the fetch init container's
//     /workspace and results go back to the controller.
//   - local mode: --pr <url>; fetches the PR itself and prints the result,
//     for development and CI use without a cluster.
import { parseArgs } from "node:util";
import { parsePRUrl } from "./prurl.ts";

const { values } = parseArgs({
  options: {
    pr: { type: "string" },
    format: { type: "string", default: "markdown" },
  },
});

if (process.env.RUN_ID) {
  console.error(`runner: cluster mode for run ${process.env.RUN_ID} is not implemented yet (milestone M1)`);
} else if (values.pr) {
  const pr = parsePRUrl(values.pr);
  console.error(`runner: local review of ${pr.owner}/${pr.repo}#${pr.number} is not implemented yet (milestone M0)`);
} else {
  console.error("usage: runner --pr <github-pr-url> [--format markdown|json]");
}
process.exit(2);
