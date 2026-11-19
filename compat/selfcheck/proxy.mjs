// Reverse proxy of the compat suite's self-check image (compat/selfcheck/Dockerfile).
//
// It listens on SELFCHECK_LISTEN_PORT and forwards every request to the application on
// SELFCHECK_TARGET_PORT, streaming bodies and keeping status and headers (names, order and repeats
// as received) except for the one distortion selected by BREAK (names below). BREAK=passthrough
// runs the proxy with no distortion. When BREAK is unset entrypoint.sh does not start the proxy at
// all: the app listens on the public port itself.
//
// Known limit of any HTTP proxy: an app that answers early and closes while the client is still
// uploading (the 10 MiB body limit scenario) resets the connection, and Node loses the answer to
// that reset in about one run in ten, where a client that reads while it writes (Go) keeps it.
// Distortion runs therefore name the subtest that must fail instead of any failure of the test.
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

  upstream.on('response', (answer) => {
    const status = (statusDistortions[mode] ?? ((s) => s))(answer.statusCode, info);
    const received = pairsOf(answer.rawHeaders).filter(([n]) => !hopByHop.has(n.toLowerCase()));
    let pairs = (headerDistortions[mode] ?? ((p) => p))(received, info, answer.statusCode);
    const contentType = answer.headers['content-type'] ?? '';
    const rewrite = bodyDistortions[mode]?.(info, answer.statusCode, contentType) ?? null;

    if (rewrite === null || answer.headers['content-encoding'] !== undefined) {
      clientRes.writeHead(status, answer.statusMessage, flat(pairs));
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
  // The application may answer before it has read the whole body (early refusals): once it has
  // answered, drain the rest of the upload instead of writing into a finished request.
  upstream.on('response', () => {
    clientReq.unpipe(upstream);
    clientReq.resume();
  });
  clientReq.pipe(upstream);
});

// The app's own limits decide how long a request may take, not the proxy.
server.requestTimeout = 0;
server.headersTimeout = 0;
server.listen(listenPort, '0.0.0.0');
