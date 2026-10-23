// Minimal static server for the console e2e oracles.
//
// The tests cannot run over file:// — console.html's boot guard is `if(location.protocol!=="file:")`, so a
// file:// load deliberately skips liveAdopt() and the whole live path (the thing under test) never runs.
//
// It binds an EXPLICIT port and FAILS LOUDLY if that port is taken. That is not defensive coding: during
// this work a run of these oracles silently hit an unrelated ssh tunnel squatting on the port, served a
// different build (799,973 bytes instead of 800,422), and produced a completely credible RED that had
// nothing to do with the code under test. A test harness that will happily measure someone else's artifact
// is worse than no harness.
//
// Usage: node serve.mjs <root> <port>   — prints "listening <port>" once ready.
import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { join, normalize, extname } from 'node:path';

const root = process.argv[2] || '.';
const port = Number(process.argv[3] || 8137);
const TYPES = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript', '.css': 'text/css',
                '.json': 'application/json', '.svg': 'image/svg+xml', '.woff2': 'font/woff2' };

const server = createServer(async (req, res) => {
  try {
    const rel = normalize(decodeURIComponent(req.url.split('?')[0])).replace(/^(\.\.[/\\])+/, '');
    const body = await readFile(join(root, rel === '/' ? 'index.html' : rel));
    // No-store: an oracle must never be answered from a cache of the previous build.
    res.writeHead(200, { 'Content-Type': TYPES[extname(rel)] || 'application/octet-stream',
                         'Cache-Control': 'no-store' });
    res.end(body);
  } catch {
    res.writeHead(404, { 'Content-Type': 'text/plain' });
    res.end('not found');
  }
});

server.on('error', (e) => {
  console.error(`serve.mjs: cannot bind 127.0.0.1:${port} — ${e.code}. Refusing to continue: the oracles ` +
                `would silently run against whatever else is on this port.`);
  process.exit(1);
});

server.listen(port, '127.0.0.1', () => console.log(`listening ${port}`));
