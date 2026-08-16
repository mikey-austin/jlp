// A2A chat server: holds the @a2a-js/sdk client (a Node package — it
// cannot run in the browser without dragging in Node-only deps and
// needing CORS on the target agent) and serves a small static UI that
// talks to *this* process over plain fetch. See README.md.
import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { randomUUID } from 'node:crypto';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { ClientFactory } from '@a2a-js/sdk/client';
import { Role, roleToJSON, taskStateToJSON } from '@a2a-js/sdk';
import { isJsonRpcError, isRestError } from '@a2a-js/sdk/errors';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PUBLIC_DIR = path.join(__dirname, 'public');

const PORT = Number(process.env.PORT || 3000);

// Defaults to JLP's in-network address (this service is meant to run
// as a compose sidecar next to `app`). Nothing below is JLP-specific:
// point A2A_AGENT_URL (env var, or the "Connect" field in the UI) at
// any A2A agent's base URL and it works the same way.
const DEFAULT_AGENT_URL = process.env.A2A_AGENT_URL || 'http://app:8080/a2a/';

// GOTCHA (paid for once building JLP's own A2A adapter — see
// docs/api/a2a.md): the SDK resolves the agent card with
// `new URL('.well-known/agent-card.json', baseUrl)`, a *relative*
// resolution. Given a base URL with no trailing slash
// (`http://host/a2a`), URL resolution drops the last path segment and
// fetches `http://host/.well-known/agent-card.json` instead of
// `http://host/a2a/.well-known/agent-card.json`. Always normalize to a
// trailing slash before handing a URL to ClientFactory.
function normalizeAgentUrl(url) {
  const trimmed = String(url).trim();
  if (trimmed === '') return trimmed;
  return trimmed.endsWith('/') ? trimmed : `${trimmed}/`;
}

// Single shared connection: this is a personal single-operator tool
// (same posture as the rest of JLP's LAN-dev tooling), not a
// multi-tenant service, so one mutable "current agent" is enough and
// keeps the UI's "point it at a different agent" story simple — no
// session/cookie plumbing.
const state = {
  agentUrl: null,
  client: null,
  card: null,
  error: null,
};

// A failed connect attempt reports its error but deliberately does NOT
// tear down a previously-working connection — pointing the "Connect"
// field at a typo'd or unreachable URL (exactly what step 5's
// wrong-URL check does) should surface an honest error message, not
// silently drop a chat session that was working fine a moment ago.
async function connect(rawUrl) {
  const agentUrl = normalizeAgentUrl(rawUrl);
  try {
    const client = await new ClientFactory().createFromUrl(agentUrl);
    const card = await client.getAgentCard();
    state.agentUrl = agentUrl;
    state.client = client;
    state.card = card;
    state.error = null;
    logRpc('connect', agentUrl, `${card.name} v${card.version}`);
    return card;
  } catch (err) {
    logRpc('connect', agentUrl, `error: ${describeError(err).message}`);
    throw err;
  }
}

// Maps whatever the SDK throws to a short, human string. The SDK
// raises typed errors (JsonRpcTaskNotFoundError, RestTaskNotCancelableError,
// ...) for protocol-level failures, and plain fetch/TypeErrors for
// transport-level ones (DNS, connection refused, non-JSON body).
function describeError(err) {
  if (isJsonRpcError(err) || isRestError(err)) {
    return {
      kind: err.constructor.name,
      message: err.message,
      code: 'envelopeCode' in err ? err.envelopeCode : err.statusCode,
    };
  }
  if (err && err.name === 'AbortError') {
    return { kind: 'Aborted', message: 'Cancelled before the agent responded.' };
  }
  if (err instanceof TypeError) {
    // node's fetch wraps DNS/connection failures as a generic TypeError
    // ("fetch failed") with the real cause nested in `err.cause`.
    const cause = err.cause ? `: ${err.cause.message || err.cause}` : '';
    return { kind: 'NetworkError', message: `Could not reach the agent (${err.message}${cause})` };
  }
  return { kind: err?.constructor?.name || 'Error', message: err?.message || String(err) };
}

// The one place to look when something looks wrong: every JSON-RPC
// call this process makes to the agent is logged here as a single
// compact line (method, a short summary of params, and the outcome).
// `docker compose logs -f a2a-chat` (or plain stdout when run
// standalone) is the "where can I see the request/response" answer —
// see README.md's Debugging section. Message text is truncated so a
// long essay doesn't turn the log into another chat transcript.
function truncate(s, n = 120) {
  return s.length > n ? `${s.slice(0, n)}…` : s;
}

function logRpc(method, summary, outcome) {
  console.log(`[a2a-chat] ${method} ${summary} -> ${outcome}`);
}

function serializePart(part) {
  if (!part || !part.content) return { kind: 'unknown', text: '' };
  switch (part.content.$case) {
    case 'text':
      return { kind: 'text', text: part.content.value };
    case 'data':
      return { kind: 'data', text: JSON.stringify(part.content.value, null, 2) };
    case 'url':
      return { kind: 'url', text: part.content.value };
    case 'raw':
      return { kind: 'raw', text: '[binary content, not rendered]' };
    default:
      return { kind: 'unknown', text: '' };
  }
}

function serializeMessage(m) {
  if (!m) return null;
  return {
    messageId: m.messageId,
    role: roleToJSON(m.role),
    parts: (m.parts || []).map(serializePart),
  };
}

function serializeTask(task) {
  return {
    id: task.id,
    contextId: task.contextId,
    status: {
      state: taskStateToJSON(task.status?.state),
      message: serializeMessage(task.status?.message),
      timestamp: task.status?.timestamp ?? null,
    },
    artifacts: (task.artifacts || []).map((a) => ({
      artifactId: a.artifactId,
      name: a.name,
      description: a.description || null,
      parts: (a.parts || []).map(serializePart),
    })),
    history: (task.history || []).map(serializeMessage),
    metadata: task.metadata ?? null,
  };
}

function serializeCard(card, agentUrl) {
  const iface = card.supportedInterfaces?.[0];
  return {
    agentUrl,
    name: card.name,
    description: card.description,
    version: card.version,
    protocolVersion: iface?.protocolVersion ?? null,
    transport: iface?.protocolBinding ?? null,
    endpointUrl: iface?.url ?? null,
    capabilities: {
      streaming: !!card.capabilities?.streaming,
      pushNotifications: !!card.capabilities?.pushNotifications,
    },
    skills: (card.skills || []).map((s) => ({
      id: s.id,
      name: s.name,
      description: s.description,
      tags: s.tags || [],
      examples: s.examples || [],
    })),
  };
}

// --- tiny JSON HTTP plumbing (no framework: one dependency, the SDK
// itself, is enough for a sidecar this small) ---

function sendJson(res, status, body) {
  const payload = JSON.stringify(body);
  res.writeHead(status, { 'Content-Type': 'application/json; charset=utf-8' });
  res.end(payload);
}

const MAX_BODY_BYTES = 1024 * 1024; // 1 MiB — mirrors JLP's own request cap

function readJsonBody(req) {
  return new Promise((resolve, reject) => {
    let size = 0;
    const chunks = [];
    req.on('data', (chunk) => {
      size += chunk.length;
      if (size > MAX_BODY_BYTES) {
        reject(new Error('request body too large'));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => {
      if (chunks.length === 0) {
        resolve({});
        return;
      }
      try {
        resolve(JSON.parse(Buffer.concat(chunks).toString('utf8')));
      } catch (err) {
        reject(err);
      }
    });
    req.on('error', reject);
  });
}

const CONTENT_TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.svg': 'image/svg+xml',
};

async function serveStatic(req, res, urlPath) {
  const relative = urlPath === '/' ? '/index.html' : urlPath;
  const filePath = path.join(PUBLIC_DIR, relative);
  // Guard against path traversal even though this only ever serves a
  // fixed, small set of files we wrote ourselves.
  if (!filePath.startsWith(PUBLIC_DIR)) {
    res.writeHead(403);
    res.end('forbidden');
    return;
  }
  try {
    const data = await readFile(filePath);
    const ext = path.extname(filePath);
    res.writeHead(200, { 'Content-Type': CONTENT_TYPES[ext] || 'application/octet-stream' });
    res.end(data);
  } catch {
    res.writeHead(404);
    res.end('not found');
  }
}

function buildUserMessage(text, contextId, skill) {
  return {
    messageId: randomUUID(),
    contextId: contextId || '',
    taskId: '',
    role: Role.ROLE_USER,
    parts: [{ content: { $case: 'text', value: text } }],
    metadata: skill ? { skill } : undefined,
    extensions: [],
    referenceTaskIds: [],
  };
}

const server = createServer(async (req, res) => {
  const url = new URL(req.url, `http://${req.headers.host}`);

  try {
    if (req.method === 'GET' && url.pathname === '/api/state') {
      sendJson(res, 200, {
        connected: !!state.client,
        agentUrl: state.agentUrl ?? DEFAULT_AGENT_URL,
        defaultAgentUrl: DEFAULT_AGENT_URL,
        card: state.card ? serializeCard(state.card, state.agentUrl) : null,
        error: state.error,
      });
      return;
    }

    if (req.method === 'POST' && url.pathname === '/api/connect') {
      const body = await readJsonBody(req);
      const target = body.agentUrl || DEFAULT_AGENT_URL;
      try {
        const card = await connect(target);
        sendJson(res, 200, { ok: true, card: serializeCard(card, state.agentUrl) });
      } catch (err) {
        // Deliberately does not touch `state` here — see connect()'s comment.
        sendJson(res, 200, { ok: false, error: describeError(err), attemptedUrl: normalizeAgentUrl(target) });
      }
      return;
    }

    if (req.method === 'POST' && url.pathname === '/api/message') {
      const body = await readJsonBody(req);
      if (!state.client) {
        sendJson(res, 200, { ok: false, error: { kind: 'NotConnected', message: 'Not connected to an agent yet.' } });
        return;
      }
      const text = (body.text || '').trim();
      if (!text) {
        sendJson(res, 400, { ok: false, error: { kind: 'InvalidRequest', message: 'Message text is required.' } });
        return;
      }

      // Propagate a browser-side cancel into the outbound call: if the
      // client disconnects (fetch aborted) before we've written a
      // response, abort the in-flight request to the agent too. This is
      // the only real "cancel while in flight" available for an agent
      // like JLP whose SendMessage blocks until the task is terminal —
      // there is no task id to hand CancelTask until the call returns.
      // See README's "Cancel" section for the honest version of this.
      const controller = new AbortController();
      let clientGone = false;
      res.on('close', () => {
        if (!res.writableEnded) {
          clientGone = true;
          controller.abort();
        }
      });

      try {
        const message = buildUserMessage(text, body.contextId, body.skill);
        const result = await state.client.sendMessage(
          {
            tenant: '',
            message,
            configuration: {
              acceptedOutputModes: ['text/plain'],
              taskPushNotificationConfig: undefined,
              historyLength: undefined,
              returnImmediately: false,
            },
            metadata: undefined,
          },
          { signal: controller.signal },
        );
        if (clientGone) return; // browser already gave up; nothing to write to
        // SendMessage's result is a task|message oneof (Message | Task).
        // JLP always returns a Task; a `status` field is how we tell.
        if (result && 'status' in result) {
          logRpc('SendMessage', `"${truncate(text)}"`, `Task ${result.id} ${taskStateToJSON(result.status?.state)}`);
          sendJson(res, 200, { ok: true, task: serializeTask(result), request: serializeMessage(message) });
        } else {
          logRpc('SendMessage', `"${truncate(text)}"`, 'Message (no task)');
          sendJson(res, 200, {
            ok: true,
            message: serializeMessage(result),
            request: serializeMessage(message),
          });
        }
      } catch (err) {
        if (clientGone) {
          logRpc('SendMessage', `"${truncate(text)}"`, 'client disconnected, aborted');
          return;
        }
        logRpc('SendMessage', `"${truncate(text)}"`, `error: ${describeError(err).message}`);
        sendJson(res, 200, { ok: false, error: describeError(err) });
      }
      return;
    }

    if (req.method === 'POST' && url.pathname === '/api/cancel') {
      const body = await readJsonBody(req);
      if (!state.client) {
        sendJson(res, 200, { ok: false, error: { kind: 'NotConnected', message: 'Not connected to an agent yet.' } });
        return;
      }
      if (!body.taskId) {
        sendJson(res, 400, { ok: false, error: { kind: 'InvalidRequest', message: 'taskId is required.' } });
        return;
      }
      try {
        const task = await state.client.cancelTask({ tenant: '', id: body.taskId, metadata: undefined });
        logRpc('CancelTask', body.taskId, taskStateToJSON(task.status?.state));
        sendJson(res, 200, { ok: true, task: serializeTask(task) });
      } catch (err) {
        logRpc('CancelTask', body.taskId, `error: ${describeError(err).message}`);
        sendJson(res, 200, { ok: false, error: describeError(err) });
      }
      return;
    }

    if (req.method === 'GET' && url.pathname === '/api/task') {
      const id = url.searchParams.get('id');
      if (!state.client) {
        sendJson(res, 200, { ok: false, error: { kind: 'NotConnected', message: 'Not connected to an agent yet.' } });
        return;
      }
      if (!id) {
        sendJson(res, 400, { ok: false, error: { kind: 'InvalidRequest', message: 'id query param is required.' } });
        return;
      }
      try {
        const task = await state.client.getTask({ tenant: '', id, historyLength: undefined });
        logRpc('GetTask', id, taskStateToJSON(task.status?.state));
        sendJson(res, 200, { ok: true, task: serializeTask(task) });
      } catch (err) {
        logRpc('GetTask', id, `error: ${describeError(err).message}`);
        sendJson(res, 200, { ok: false, error: describeError(err) });
      }
      return;
    }

    if (req.method === 'GET' && !url.pathname.startsWith('/api/')) {
      await serveStatic(req, res, url.pathname);
      return;
    }

    res.writeHead(404);
    res.end('not found');
  } catch (err) {
    console.error('unhandled request error:', err);
    if (!res.headersSent) sendJson(res, 500, { ok: false, error: { kind: 'ServerError', message: String(err) } });
  }
});

server.listen(PORT, () => {
  console.log(`a2a-chat listening on :${PORT}`);
  console.log(`default agent: ${DEFAULT_AGENT_URL}`);
  connect(DEFAULT_AGENT_URL)
    .then((card) => console.log(`connected: ${card.name} v${card.version}`))
    .catch((err) => {
      state.error = describeError(err);
      console.warn(`initial connect failed (will retry from the UI): ${state.error.message}`);
    });
});
