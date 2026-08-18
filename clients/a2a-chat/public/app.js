import { render as renderWidget, canRender } from '/widgets.js';

// Vanilla JS — no framework, no build step. This is a small sidecar UI;
// see server.js for the API this talks to and README.md for the
// architecture rationale (SDK stays server-side).
'use strict';

const els = {
  connDot: document.getElementById('conn-dot'),
  agentName: document.getElementById('agent-name'),
  toggleAgentPanel: document.getElementById('toggle-agent-panel'),
  agentPanel: document.getElementById('agent-panel'),
  connectForm: document.getElementById('connect-form'),
  agentUrlInput: document.getElementById('agent-url'),
  connectError: document.getElementById('connect-error'),
  cardDetails: document.getElementById('card-details'),
  cardDescription: document.getElementById('card-description'),
  cardVersion: document.getElementById('card-version'),
  cardProtocol: document.getElementById('card-protocol'),
  cardTransport: document.getElementById('card-transport'),
  cardCapabilities: document.getElementById('card-capabilities'),
  cardSkills: document.getElementById('card-skills'),
  skillsList: document.getElementById('skills-list'),
  thread: document.getElementById('thread'),
  composer: document.getElementById('composer'),
  composerInput: document.getElementById('composer-input'),
  skillSelect: document.getElementById('skill-select'),
  sendBtn: document.getElementById('send-btn'),
  cancelBtn: document.getElementById('cancel-btn'),
  newConversation: document.getElementById('new-conversation'),
  contextIdLabel: document.getElementById('context-id'),
};

// Session state, deliberately in-memory only (see README's "history"
// section): a page reload starts a fresh conversation, same as
// dropping a plain browser tab would for any other single-page chat.
let contextId = null;
let inFlightController = null;

function setConnected(connected) {
  els.connDot.classList.toggle('connected', connected);
  els.connDot.classList.toggle('disconnected', !connected);
}

function renderCard(card) {
  if (!card) {
    els.agentName.textContent = 'A2A Chat — not connected';
    els.cardDetails.hidden = true;
    els.cardSkills.hidden = true;
    setConnected(false);
    els.skillSelect.innerHTML = '<option value="">(default)</option>';
    return;
  }
  setConnected(true);
  els.agentName.textContent = `${card.name} v${card.version}`;
  els.cardDescription.textContent = card.description || '(no description)';
  els.cardVersion.textContent = card.version || '—';
  els.cardProtocol.textContent = card.protocolVersion || '—';
  els.cardTransport.textContent = card.transport || '—';
  const caps = [];
  caps.push(card.capabilities.streaming ? 'streaming: yes' : 'streaming: no (progress state shown instead)');
  caps.push(card.capabilities.pushNotifications ? 'push notifications: yes' : 'push notifications: no');
  els.cardCapabilities.textContent = caps.join(' · ');
  els.cardDetails.hidden = false;

  els.skillsList.innerHTML = '';
  els.skillSelect.innerHTML = '<option value="">(default — agent picks)</option>';
  for (const skill of card.skills || []) {
    const li = document.createElement('li');
    li.className = 'skill-item';
    const strong = document.createElement('strong');
    strong.textContent = skill.name || skill.id;
    const p = document.createElement('p');
    p.textContent = skill.description || '';
    li.append(strong, p);
    els.skillsList.appendChild(li);

    const opt = document.createElement('option');
    opt.value = skill.id;
    opt.textContent = skill.name || skill.id;
    els.skillSelect.appendChild(opt);
  }
  els.cardSkills.hidden = (card.skills || []).length === 0;
}

async function refreshState() {
  const res = await fetch('/api/state');
  const data = await res.json();
  els.agentUrlInput.value = data.agentUrl || data.defaultAgentUrl || '';
  renderCard(data.card);
  if (!data.card && data.error) {
    showConnectError(`${data.error.kind}: ${data.error.message}`);
  }
  return data;
}

function showConnectError(text) {
  els.connectError.textContent = text;
  els.connectError.hidden = false;
}

function clearConnectError() {
  els.connectError.hidden = true;
  els.connectError.textContent = '';
}

els.toggleAgentPanel.addEventListener('click', () => {
  const willShow = els.agentPanel.hidden;
  els.agentPanel.hidden = !willShow;
  els.toggleAgentPanel.setAttribute('aria-expanded', String(willShow));
});

els.connectForm.addEventListener('submit', async (ev) => {
  ev.preventDefault();
  clearConnectError();
  const target = els.agentUrlInput.value.trim();
  if (!target) return;
  const submitBtn = els.connectForm.querySelector('button[type="submit"]');
  submitBtn.disabled = true;
  try {
    const res = await fetch('/api/connect', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ agentUrl: target }),
    });
    const data = await res.json();
    if (data.ok) {
      renderCard(data.card);
      addSystemNote(`Connected to ${data.card.name} (${data.card.agentUrl}).`);
    } else {
      showConnectError(`${data.error.kind}: ${data.error.message}`);
      // The previous working connection (if any) is untouched server-side
      // — refresh to show it's still there and still usable.
      await refreshState();
    }
  } catch (err) {
    showConnectError(String(err));
  } finally {
    submitBtn.disabled = false;
  }
});

els.newConversation.addEventListener('click', () => {
  if (inFlightController) inFlightController.abort();
  contextId = null;
  els.contextIdLabel.textContent = '';
  els.thread.innerHTML = '';
  renderEmptyState();
});

function renderEmptyState() {
  if (els.thread.children.length > 0) return;
  const p = document.createElement('p');
  p.className = 'empty-state';
  p.textContent = 'Send a message to start a conversation with the connected agent.';
  els.thread.appendChild(p);
}

function clearEmptyState() {
  const existing = els.thread.querySelector('.empty-state');
  if (existing) existing.remove();
}

function scrollToBottom() {
  els.thread.scrollTop = els.thread.scrollHeight;
}

function addUserBubble(text) {
  clearEmptyState();
  const row = document.createElement('div');
  row.className = 'bubble-row user';
  const bubble = document.createElement('div');
  bubble.className = 'bubble user';
  bubble.textContent = text;
  row.appendChild(bubble);
  els.thread.appendChild(row);
  scrollToBottom();
}

function addSystemNote(text) {
  clearEmptyState();
  const row = document.createElement('div');
  row.className = 'bubble-row system';
  const bubble = document.createElement('div');
  bubble.className = 'bubble cancelled';
  bubble.textContent = text;
  row.appendChild(bubble);
  els.thread.appendChild(row);
  scrollToBottom();
}

function addProgressBubble() {
  clearEmptyState();
  const row = document.createElement('div');
  row.className = 'bubble-row agent';
  const bubble = document.createElement('div');
  bubble.className = 'bubble progress';
  const spinner = document.createElement('span');
  spinner.className = 'spinner';
  const label = document.createElement('span');
  label.textContent = 'Waiting for the agent — this can take 5–30s behind a local model…';
  bubble.append(spinner, label);
  row.appendChild(bubble);
  els.thread.appendChild(row);
  scrollToBottom();
  return row;
}

const STATE_LABELS = {
  TASK_STATE_UNSPECIFIED: 'unspecified',
  TASK_STATE_SUBMITTED: 'submitted',
  TASK_STATE_WORKING: 'working',
  TASK_STATE_COMPLETED: 'completed',
  TASK_STATE_FAILED: 'failed',
  TASK_STATE_CANCELED: 'canceled',
  TASK_STATE_INPUT_REQUIRED: 'input required',
  TASK_STATE_REJECTED: 'rejected',
};

function stateBadgeClass(state) {
  const short = (state || '').replace('TASK_STATE_', '').toLowerCase();
  return short || 'unspecified';
}

function partsToText(parts) {
  if (!parts || parts.length === 0) return '(no content)';
  return parts
    .map((p) => {
      if (p.kind === 'text') return p.text;
      // A data part this client can render contributes nothing to the
      // prose — it becomes a widget below. One it cannot render still
      // falls back to its JSON, so an unknown payload is visible rather
      // than silently missing.
      if (p.kind === 'data') return renderableMedia(p) ? '' : `[structured data]\n${p.text}`;
      if (p.kind === 'url') return `[file] ${p.text}`;
      return '[unsupported content]';
    })
    .filter((s) => s !== '')
    .join('\n\n');
}

function replaceRow(row, newBubbleEl, metaEls) {
  row.innerHTML = '';
  row.appendChild(newBubbleEl);
  if (metaEls) {
    const meta = document.createElement('div');
    meta.className = 'meta-row';
    for (const el of metaEls) meta.appendChild(el);
    row.after(meta);
  }
  scrollToBottom();
}

function renderTaskResult(progressRow, task) {
  const stateName = task.status.state;
  const isFailed = stateName === 'TASK_STATE_FAILED' || stateName === 'TASK_STATE_REJECTED';
  const isTerminal = !['TASK_STATE_SUBMITTED', 'TASK_STATE_WORKING', 'TASK_STATE_INPUT_REQUIRED'].includes(
    stateName,
  );

  const bubble = document.createElement('div');
  bubble.className = `bubble agent${isFailed ? ' error' : ''}`;

  if (isFailed) {
    const reason =
      (task.status.message && partsToText(task.status.message.parts)) || 'The agent reported a failure with no message.';
    bubble.textContent = `Task failed.\n\n${reason}`;
  } else if (task.artifacts && task.artifacts.length > 0) {
    // Prose first and unconditionally; widgets append under it. A client
    // may ignore parts it does not understand, so the text has to carry
    // the whole answer on its own — see widgets.js.
    bubble.textContent = task.artifacts.map((a) => partsToText(a.parts)).join('\n\n---\n\n');
    for (const artifact of task.artifacts) {
      appendWidgets(bubble, artifact.parts);
    }
  } else if (task.status.message) {
    bubble.textContent = partsToText(task.status.message.parts);
  } else {
    bubble.textContent = `(no output yet — task is ${STATE_LABELS[stateName] || stateName})`;
  }

  const meta = document.createElement('div');
  meta.className = 'meta-row';
  const badge = document.createElement('span');
  badge.className = `badge ${stateBadgeClass(stateName)}`;
  badge.textContent = STATE_LABELS[stateName] || stateName;
  const idSpan = document.createElement('span');
  idSpan.className = 'task-id';
  idSpan.textContent = `task ${task.id.slice(0, 8)}…`;
  idSpan.title = task.id;
  meta.append(badge, idSpan);

  if (!isTerminal) {
    const cancelLink = document.createElement('button');
    cancelLink.type = 'button';
    cancelLink.className = 'inline-cancel';
    cancelLink.textContent = 'Cancel task';
    cancelLink.addEventListener('click', () => cancelTaskById(task.id, meta, bubble));
    meta.appendChild(cancelLink);
  }

  progressRow.innerHTML = '';
  progressRow.appendChild(bubble);
  progressRow.after(meta);
  scrollToBottom();

  if (task.contextId) {
    contextId = task.contextId;
    els.contextIdLabel.textContent = `context ${contextId.slice(0, 8)}…`;
    els.contextIdLabel.title = contextId;
  }
}

async function cancelTaskById(taskId, metaEl, bubbleEl) {
  try {
    const res = await fetch('/api/cancel', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ taskId }),
    });
    const data = await res.json();
    if (data.ok) {
      bubbleEl.textContent += `\n\n(cancelled: ${STATE_LABELS[data.task.status.state] || data.task.status.state})`;
    } else {
      bubbleEl.textContent += `\n\n(cancel failed — ${data.error.kind}: ${data.error.message})`;
    }
  } catch (err) {
    bubbleEl.textContent += `\n\n(cancel request failed: ${err})`;
  }
  metaEl.querySelector('.inline-cancel')?.remove();
}

function renderErrorResult(progressRow, error) {
  const bubble = document.createElement('div');
  bubble.className = 'bubble error';
  bubble.textContent = `${error.kind}: ${error.message}`;
  progressRow.innerHTML = '';
  progressRow.appendChild(bubble);
  scrollToBottom();
}

function renderCancelledResult(progressRow) {
  const bubble = document.createElement('div');
  bubble.className = 'bubble cancelled';
  bubble.textContent =
    'Cancelled before the agent replied. This agent runs SendMessage synchronously, so no task id ' +
    'was available yet to cancel server-side — this only gave up on waiting for the response; the ' +
    "agent may still be finishing the run (check the agent's own trace/log).";
  progressRow.innerHTML = '';
  progressRow.appendChild(bubble);
  scrollToBottom();
}

function setBusy(busy) {
  els.sendBtn.disabled = busy;
  els.composerInput.disabled = busy;
  els.cancelBtn.hidden = !busy;
}

els.composer.addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const text = els.composerInput.value.trim();
  if (!text) return;

  addUserBubble(text);
  els.composerInput.value = '';
  const progressRow = addProgressBubble();
  setBusy(true);

  inFlightController = new AbortController();
  const skill = els.skillSelect.value || undefined;

  try {
    const res = await fetch('/api/message', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ text, contextId, skill }),
      signal: inFlightController.signal,
    });
    const data = await res.json();
    if (data.ok && data.task) {
      renderTaskResult(progressRow, data.task);
    } else if (data.ok && data.message) {
      // Some agents may reply with a bare Message instead of a Task.
      const bubble = document.createElement('div');
      bubble.className = 'bubble agent';
      bubble.textContent = partsToText(data.message.parts);
      progressRow.innerHTML = '';
      progressRow.appendChild(bubble);
    } else {
      renderErrorResult(progressRow, data.error);
    }
  } catch (err) {
    if (err.name === 'AbortError') {
      renderCancelledResult(progressRow);
    } else {
      renderErrorResult(progressRow, { kind: 'ClientError', message: String(err) });
    }
  } finally {
    setBusy(false);
    inFlightController = null;
  }
});

els.cancelBtn.addEventListener('click', () => {
  if (inFlightController) inFlightController.abort();
});

// Grow the textarea a little as text wraps, capped by CSS max-height.
els.composerInput.addEventListener('input', () => {
  els.composerInput.style.height = 'auto';
  els.composerInput.style.height = `${els.composerInput.scrollHeight}px`;
});
els.composerInput.addEventListener('keydown', (ev) => {
  if (ev.key === 'Enter' && !ev.shiftKey) {
    ev.preventDefault();
    els.composer.requestSubmit();
  }
});

renderEmptyState();
refreshState().catch((err) => showConnectError(String(err)));

// renderableMedia reports whether this client has a widget for a part.
// It asks the registry rather than attempting a render, so testing does
// not build a DOM tree that is then thrown away.
function renderableMedia(part) {
  return Boolean(part && part.mediaType && canRender(part.mediaType));
}

// appendWidgets renders every data part it recognises, under the prose.
//
// A widget that fails to render is skipped, never fatal: the answer is
// already in the bubble above it, and losing a nicer presentation is a
// far better outcome than losing the reply.
function appendWidgets(bubble, parts) {
  if (!Array.isArray(parts)) return;
  for (const part of parts) {
    if (!part || part.kind !== 'data' || !part.mediaType) continue;
    let node = null;
    try {
      node = renderWidget(part.mediaType, part.data);
    } catch (err) {
      console.warn('[a2a-chat] widget threw while rendering', part.mediaType, err);
      continue;
    }
    if (node) bubble.appendChild(node);
  }
}
