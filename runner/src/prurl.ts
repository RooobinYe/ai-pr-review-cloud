export interface PRRef {
  owner: string;
  repo: string;
  number: number;
}

const PR_URL = /^https:\/\/github\.com\/([\w.-]+)\/([\w.-]+)\/pull\/(\d+)\/?$/;

/** Parses a GitHub pull request URL such as https://github.com/octo/demo/pull/7. */
export function parsePRUrl(url: string): PRRef {
  const m = PR_URL.exec(url.trim());
  const number = Number(m?.[3]);
  if (!m || !Number.isSafeInteger(number) || number <= 0) {
    throw new Error(`not a GitHub pull request URL: ${url}`);
  }
  return { owner: m[1]!, repo: m[2]!, number };
}
