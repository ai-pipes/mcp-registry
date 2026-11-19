// Reverse proxy of the compat suite's self-check image (compat/selfcheck/Dockerfile).
//
// It listens on SELFCHECK_LISTEN_PORT and forwards every request to the application on
// SELFCHECK_TARGET_PORT, streaming bodies and keeping status and headers (names, order and repeats
// as received) except for the one distortion selected by BREAK (names below). BREAK=passthrough
// runs the proxy with no distortion. When BREAK is unset entrypoint.sh does not start the proxy at
// all: the app listens on the public port itself.
//
// Known defect, not a limit of HTTP proxies: the 10 MiB body-limit scenario (the app answers 413
// without reading the upload and closes) is relayed wrongly by this proxy in a minority of runs
// (a 502 or an EOF instead of the 413). Observed: about 1 in 10 runs with a plain pipe of the
// request, about 1 in 30 with the forwarding below (one chunk per event-loop turn, nothing written
// once the app has answered, a request-side error after an answer ignored). The proxy log of the
// failing runs says `write EPIPE` before any answer was seen, i.e. the write hit the reset before
// Node had read the answer that the app sent first; Node destroys the socket on that error and the
// answer is lost. The cause fits the evidence but a fix was not found; a client that reads while
// it writes (the Go test client) is not affected (15 of 15 direct runs). Hence the passthrough
// control skips that one subtest and no distortion case depends on an early refusal.
//
// No dependencies: only node:http, so the image needs nothing beyond its Node runtime.
import http from 'node:http';

const listenPort = Number(process.env.SELFCHECK_LISTEN_PORT);
const targetPort = Number(process.env.SELFCHECK_TARGET_PORT);
const mode = process.env.BREAK ?? '';
const publicOrigin = process.env.PUBLIC_URL ? new URL(process.env.PUBLIC_URL).origin : '';

// Headers that describe one hop, not the message: Node writes its own for each side.
const hopByHop = new Set(['connection', 'keep-alive', 'proxy-connection']);

/** @typedef {string[]} RawHeaders flat list: name, value, name, value... as received */

/** Pairs of a flat header list. */
function pairsOf(raw) {
  const out = [];
  for (let i = 0; i + 1 < raw.length; i += 2) out.push([raw[i], raw[i + 1]]);
  return out;
}

function flat(pairs) {
  return pairs.flat();
}

function mapJson(fn) {
  return (text) => JSON.stringify(fn(JSON.parse(text)));
}

const bearer = 'sk_' + 'A'.repeat(43);

// Distortions of the request headers: (pairs, request path) -> pairs.
const requestDistortions = {
  // Every request claims to come from the page of the gateway itself, so the CSRF check passes.
  origin: (pairs) => [
    ...pairs.filter(([n]) => n.toLowerCase() !== 'origin'),
    ['Origin', publicOrigin],
  ],
};

// Distortions of the response status: (status, request) -> status.
const statusDistortions = {
  status: (status) => (status === 409 ? 200 : status),
};

// Distortions of the response headers: (pairs, request, status) -> pairs.
const headerDistortions = {
  csp: (pairs) =>
    pairs.map(([n, v]) =>
      n.toLowerCase() === 'content-security-policy'
        ? [n, v.replace(/;?\s*frame-ancestors 'none'/, '')]
        : [n, v],
    ),
  'www-auth': (pairs, req, status) =>
    status === 401 && req.path === '/mcp'
      ? pairs.filter(([n]) => n.toLowerCase() !== 'www-authenticate')
      : pairs,
};

/** Adds a description suffix to search_tools in every tools/list result of one JSON-RPC message. */
function distortToolsList(message) {
  const list = Array.isArray(message) ? message : [message];
  for (const m of list) {
    for (const tool of m?.result?.tools ?? []) {
      if (tool.name === 'search_tools') tool.description = `${tool.description} (distorted)`;
    }
  }
  return message;
}

/** The answer to one /mcp POST is one JSON message or a short event stream of them. */
function distortMcpBody(text, contentType) {
  if (contentType.includes('text/event-stream')) {
    return text
      .split('\n')
      .map((line) => {
        if (!line.startsWith('data:')) return line;
        const data = line.slice(5).trim();
        return data.startsWith('{') || data.startsWith('[')
          ? `data: ${JSON.stringify(distortToolsList(JSON.parse(data)))}`
          : line;
      })
      .join('\n');
  }
  return JSON.stringify(distortToolsList(JSON.parse(text)));
}

// Distortions of the response body: (req, status, contentType) -> (text -> text) | null. The body
// is buffered only when a distortion applies to the response; every other response streams.
const bodyDistortions = {
  'extra-field': (req, status) =>
    req.method === 'GET' && req.path === '/api/servers' && status === 200
      ? mapJson((servers) => servers.map((s) => ({ ...s, secret: 'leaked-by-the-self-check' })))
      : null,
  'audit-drop': (req, status) =>
    req.method === 'GET' && req.path === '/api/audit' && status === 200
      ? mapJson((rows) => rows.filter((r) => !String(r.action).startsWith('connection.')))
      : null,
  'token-in-list': (req, status) =>
    req.method === 'GET' && req.path === '/api/tokens' && status === 200
      ? mapJson((tokens) => tokens.map((t) => ({ ...t, token: bearer })))
      : null,
  'mcp-desc': (req, status, contentType) =>
    req.method === 'POST' && req.path === '/mcp' && status === 200
      ? (text) => distortMcpBody(text, contentType)
      : null,
};

const known = new Set([
  'passthrough',
  ...Object.keys(requestDistortions),
  ...Object.keys(statusDistortions),
  ...Object.keys(headerDistortions),
  ...Object.keys(bodyDistortions),
]);
if (mode !== '' && !known.has(mode)) {
  console.error(
    `selfcheck proxy: unknown BREAK ${JSON.stringify(mode)}; known: ${[...known].toSorted().join(', ')}`,
  );
  process.exit(2);
}
if (mode === 'origin' && publicOrigin === '') {
  console.error('selfcheck proxy: BREAK=origin needs PUBLIC_URL');
  process.exit(2);
}
if (!Number.isInteger(listenPort) || !Number.isInteger(targetPort)) {
  console.error('selfcheck proxy: SELFCHECK_LISTEN_PORT and SELFCHECK_TARGET_PORT are required');
  process.exit(2);
}

const server = http.createServer((clientReq, clientRes) => {
  const info = {
    method: clientReq.method,
    path: new URL(clientReq.url, 'http://localhost').pathname,
  };
  let requestPairs = pairsOf(clientReq.rawHeaders).filter(([n]) => !hopByHop.has(n.toLowerCase()));
  requestPairs = (requestDistortions[mode] ?? ((p) => p))(requestPairs, info);

  const upstream = http.request({
    host: '127.0.0.1',
    port: targetPort,
    method: clientReq.method,
    path: clientReq.url,
    headers: flat(requestPairs),
    agent: false,
  });

  // Set when the app has answered. An early refusal (the app answers 413 and closes without
  // reading the body) resets the connection while the client's body is still being forwarded; a
  // write error from then on is not a failure of the exchange, the answer is relayed.
  let answered = false;

  upstream.on('response', (answer) => {
    answered = true;
    const status = (statusDistortions[mode] ?? ((s) => s))(answer.statusCode, info);
    const received = pairsOf(answer.rawHeaders).filter(([n]) => !hopByHop.has(n.toLowerCase()));
    let pairs = (headerDistortions[mode] ?? ((p) => p))(received, info, answer.statusCode);
    const contentType = answer.headers['content-type'] ?? '';
    const rewrite = bodyDistortions[mode]?.(info, answer.statusCode, contentType) ?? null;

    const compressed = answer.headers['content-encoding'] !== undefined;
    if (rewrite !== null && compressed) {
      console.error(
        `selfcheck proxy: ${info.method} ${info.path}: BREAK=${mode} not applied, the response is ${answer.headers['content-encoding']}-encoded`,
      );
    }
    if (rewrite === null || compressed) {
      clientRes.writeHead(status, answer.statusMessage, flat(pairs));
      answer.on('error', () => clientRes.destroy());
      answer.pipe(clientRes);
      return;
    }
    const chunks = [];
    answer.on('data', (c) => chunks.push(c));
    answer.on('end', () => {
      let body;
      try {
        body = Buffer.from(rewrite(Buffer.concat(chunks).toString('utf8')), 'utf8');
      } catch (err) {
        console.error(
          `selfcheck proxy: cannot distort ${info.method} ${info.path}: ${err.message}`,
        );
        clientRes.writeHead(502).end();
        return;
      }
      pairs = pairs.filter(
        ([n]) => !['content-length', 'transfer-encoding'].includes(n.toLowerCase()),
      );
      pairs.push(['Content-Length', String(body.length)]);
      clientRes.writeHead(status, answer.statusMessage, flat(pairs));
      clientRes.end(body);
    });
    answer.on('error', () => clientRes.destroy());
  });

  upstream.on('error', (err) => {
    // After an answer the request side may fail (see `answered`); the answer's own stream reports
    // a broken answer.
    if (answered) return;
    if (clientRes.headersSent) {
      clientRes.destroy();
      return;
    }
    // The app is not listening yet while it starts: that is expected and stays silent.
    if (err.code !== 'ECONNREFUSED') {
      console.error(`selfcheck proxy: ${info.method} ${info.path}: ${err.message}`);
    }
    clientRes.writeHead(502, { 'Content-Type': 'text/plain' }).end('bad gateway');
  });
  // The client went away: stop the upstream request too.
  clientRes.on('close', () => upstream.destroy());
  clientReq.on('error', () => upstream.destroy());
  // Forward the client's body one chunk per event-loop turn, in the check phase: every socket that
  // is ready has then been read first, so an answer that is already queued from the app is seen
  // before the next write can hit the reset. Once the app has answered the rest of the upload is
  // drained, not forwarded.
  clientReq.on('data', (chunk) => {
    if (answered) return;
    clientReq.pause();
    setImmediate(() => {
      if (answered || upstream.destroyed) {
        clientReq.resume();
        return;
      }
      upstream.write(chunk, () => clientReq.resume());
    });
  });
  clientReq.on('end', () => {
    setImmediate(() => {
      if (!answered && !upstream.destroyed) upstream.end();
    });
  });
});

// The app's own limits decide how long a request may take, not the proxy.
server.requestTimeout = 0;
server.headersTimeout = 0;
server.listen(listenPort, '0.0.0.0');
