import { createServer } from 'node:http';

// Paths answered with 200; everything else is 404.
const routes = new Map([
  ['/', 'preview is healthy\n'],
  ['/health', 'preview is healthy\n'],
  ['/version', `${process.env.APP_VERSION ?? 'dev'}\n`],
]);

createServer((req, res) => {
  console.log(`${req.method} ${req.url}`);
  const body = routes.get(req.url);
  res.writeHead(body ? 200 : 404, { 'Content-Type': 'text/plain' });
  res.end(body ?? 'not found\n');
}).listen(8080, '0.0.0.0');
