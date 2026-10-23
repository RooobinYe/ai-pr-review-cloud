import { createServer } from 'node:http';
createServer((req, res) => {
  console.log(`${req.method} ${req.url}`);
  const healthy = req.url === '/healthz' || req.url === '/';
  res.writeHead(healthy ? 200 : 404, { 'Content-Type': 'text/plain' });
  res.end(healthy ? 'preview is healthy\n' : 'not found\n');
}).listen(8080, '0.0.0.0');
