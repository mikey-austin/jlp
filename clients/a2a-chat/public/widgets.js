// Rich content renderers, keyed by media type.
//
// This file is the ONLY JLP-specific thing in this client, and it is
// deliberately shaped so that stays true: a registry of media types the
// client happens to recognise. Point this client at a different A2A
// agent and nothing here matches, so every part falls back to prose —
// exactly as it behaved before widgets existed.
//
// See docs/superpowers/specs/2026-08-18-a2a-rich-content-design.md in
// the JLP repo. Two rules from it are load-bearing here:
//
//   1. The prose part is always rendered, first, whatever else arrives.
//      A widget is enrichment, never the only copy of something.
//   2. A gated correction arrives WITHOUT a replacement and WITHOUT
//      spans. That is not missing data to paper over — it means the
//      learner has not earned the answer yet, and the widget must say
//      so rather than render an empty diff, which would read as "no
//      correction needed".

const MEDIA = {
  correction: 'application/vnd.jlp.correction+json',
  vocabulary: 'application/vnd.jlp.vocabulary+json',
  priorities: 'application/vnd.jlp.priorities+json',
  lesson: 'application/vnd.jlp.lesson+json',
};

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined && text !== null) node.textContent = String(text);
  return node;
}

// --- correction diff --------------------------------------------------

function renderCorrections(rows) {
  if (!Array.isArray(rows) || rows.length === 0) return null;
  const wrap = el('div', 'widget widget--corrections');
  wrap.appendChild(el('h3', 'widget__title', '添削'));

  for (const c of rows) {
    const card = el('div', 'correction-card');

    const body = el('div', 'correction-card__body');
    if (Array.isArray(c.spans) && c.spans.length > 0) {
      // Ungated: show what changed.
      for (const span of c.spans) {
        const cls =
          span.op === 'insert' ? 'd-ins' : span.op === 'delete' ? 'd-del' : 'd-eq';
        body.appendChild(el('span', cls, span.text));
      }
    } else {
      // Gated, or nothing to diff. Mark WHERE the problem is and say the
      // answer is still open — never an empty diff.
      body.appendChild(el('span', 'd-eq', c.original || ''));
      card.classList.add('correction-card--gated');
    }
    card.appendChild(body);

    const meta = el('div', 'correction-card__meta');
    if (c.type) meta.appendChild(el('span', 'badge', c.type));
    if (c.severity) meta.appendChild(el('span', 'badge', c.severity));
    if (typeof c.attempts === 'number' && c.attempts > 0) {
      meta.appendChild(el('span', 'correction-card__attempts', `${c.attempts}回挑戦中`));
    }
    card.appendChild(meta);

    if (c.explanation_en) {
      card.appendChild(el('p', 'correction-card__why', c.explanation_en));
    } else if (!c.spans || c.spans.length === 0) {
      card.appendChild(el('p', 'correction-card__pending', 'まだ答えは出ていません。'));
    }

    wrap.appendChild(card);
  }
  return wrap;
}

// --- vocabulary -------------------------------------------------------

function renderVocabulary(rows) {
  if (!Array.isArray(rows) || rows.length === 0) return null;
  const wrap = el('div', 'widget widget--vocabulary');
  wrap.appendChild(el('h3', 'widget__title', '語彙'));

  for (const w of rows) {
    const item = el('div', 'word-card');
    const head = el('div', 'word-card__head');
    head.appendChild(el('span', 'word-card__expression', w.expression || ''));
    if (w.reading) head.appendChild(el('span', 'word-card__reading', w.reading));
    item.appendChild(head);
    if (w.meaning) item.appendChild(el('div', 'word-card__meaning', w.meaning));

    const meta = el('div', 'word-card__meta');
    if (w.kind) meta.appendChild(el('span', 'badge', w.kind));
    if (typeof w.lookups === 'number') meta.appendChild(el('span', null, `検索 ${w.lookups}`));
    if (typeof w.productions === 'number') meta.appendChild(el('span', null, `使用 ${w.productions}`));
    item.appendChild(meta);

    wrap.appendChild(item);
  }
  return wrap;
}

// --- priorities -------------------------------------------------------

function renderPriorities(rows) {
  if (!Array.isArray(rows) || rows.length === 0) return null;
  const wrap = el('div', 'widget widget--priorities');
  wrap.appendChild(el('h3', 'widget__title', '学習の優先度'));

  // Scale bars against the largest score present rather than an assumed
  // maximum: the score's range is the planner's business, not this
  // client's, and hardcoding one here would silently mis-draw the day it
  // changes.
  const top = rows.reduce((max, r) => Math.max(max, Number(r.score) || 0), 0) || 1;

  for (const p of rows) {
    const row = el('div', 'priority-row');
    row.appendChild(el('span', 'priority-row__subject', p.subject || ''));

    const track = el('span', 'priority-row__track');
    const fill = el('span', 'priority-row__fill');
    fill.style.width = `${Math.round(((Number(p.score) || 0) / top) * 100)}%`;
    track.appendChild(fill);
    row.appendChild(track);

    if (p.reason) row.appendChild(el('span', 'priority-row__reason', p.reason));
    wrap.appendChild(row);
  }
  return wrap;
}

// --- lesson plan ------------------------------------------------------

function renderLesson(value) {
  if (!value || typeof value !== 'object') return null;
  const plan = value.plan && typeof value.plan === 'object' ? value.plan : value;

  const wrap = el('div', 'widget widget--lesson');
  wrap.appendChild(el('h3', 'widget__title', 'レッスンガイド'));
  if (value.status) wrap.appendChild(el('span', 'badge', value.status));

  // The plan is lesson_plan.v1, whose exact shape belongs to the schema
  // rather than to this client. Render the fields we know and list the
  // rest generically, so a schema that grows a field degrades to "shown
  // plainly" instead of "silently dropped".
  // Labelled rather than showing the schema's field names: everything
  // else in these widgets is Japanese, and a raw "objective" heading
  // next to a finished correction card reads as debug output.
  const known = {
    objective: '目標',
    objectives: '目標',
    activities: '活動',
    materials: '教材',
    notes: 'メモ',
  };
  for (const [key, label] of Object.entries(known)) {
    const v = plan[key];
    if (!v) continue;
    wrap.appendChild(el('h4', 'widget__subtitle', label));
    if (Array.isArray(v)) {
      const list = el('ul');
      for (const entry of v) {
        list.appendChild(el('li', null, typeof entry === 'string' ? entry : JSON.stringify(entry)));
      }
      wrap.appendChild(list);
    } else {
      wrap.appendChild(el('p', null, typeof v === 'string' ? v : JSON.stringify(v)));
    }
  }
  if (wrap.childElementCount <= 1) return null;
  return wrap;
}

// --- registry ---------------------------------------------------------

const RENDERERS = {
  [MEDIA.correction]: renderCorrections,
  [MEDIA.vocabulary]: renderVocabulary,
  [MEDIA.priorities]: renderPriorities,
  [MEDIA.lesson]: renderLesson,
};

// render returns an element for a recognised media type, or null for
// anything else — an unknown type, a payload that does not match, or a
// renderer that throws. Null means "the caller shows its text fallback",
// which is why a bad widget costs a nicer presentation and never the
// answer itself.
export function render(mediaType, data) {
  const renderer = RENDERERS[mediaType];
  if (!renderer) return null;
  try {
    return renderer(data);
  } catch (err) {
    console.warn(`[a2a-chat] widget ${mediaType} failed to render`, err);
    return null;
  }
}

// canRender reports whether a media type has a renderer, without
// building anything — the caller needs the answer before deciding
// whether the part still owes the reader a text fallback.
export function canRender(mediaType) {
  return Object.prototype.hasOwnProperty.call(RENDERERS, mediaType);
}

export const MEDIA_TYPES = MEDIA;
