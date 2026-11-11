// Integrated agent: conversations, live event feed, prompt / steer / interrupt.
import { h, icon, btn, ago, every, patch } from '../dom.js';
import { api, errText } from '../api.js';
import { card, toast, toastErr, stateChip, isLive, empty } from '../ui.js';

const HARNESSES = [['claude', 'Claude Code'], ['codex', 'Codex'], ['opencode', 'OpenCode']];

export default async function (ctx, root) {
  const id = ctx.id;
  let convId = ctx.query.get('c') || null, convs = [], fresh = false;

  const feed = h('div.feed', { role: 'log', 'aria-live': 'polite', 'aria-label': 'Agent activity' });
  const convList = h('div');
  const ta = h('textarea.textarea', { rows: 2, placeholder: 'Ask the agent to do something in this sandbox…', 'aria-label': 'Prompt' });
  let harness = 'claude';
  try { harness = localStorage.getItem('crowdvm.harness') || harness; } catch { /* storage blocked */ }
  const harnessSel = h('select.select.sm', { 'aria-label': 'Agent harness', style: { width: 'auto' } }, HARNESSES.map(([v, l]) => h('option', { value: v, selected: v === harness }, l)));
  harnessSel.addEventListener('change', () => { harness = harnessSel.value; try { localStorage.setItem('crowdvm.harness', harness); } catch { /* */ } });
  const send = btn('Send', { kind: 'primary', ico: 'send' });
  const interrupt = btn('Interrupt', { kind: 'danger', ico: 'pause', size: 'sm' });
  interrupt.hidden = true;
  const mode = h('span.small.faint');

  const running = () => convs.find((c) => c.id === convId)?.running;

  async function submit() {
    const text = ta.value.trim(); if (!text) return;
    send.disabled = true;
    try {
      if (running() && !fresh) {
        await api.post(`/sandboxes/${id}/steer`, { message: text, conversation: convId });
        toast('Sent into the running turn');
      } else {
        const b = { prompt: text, provider: harness };
        if (fresh || !convId) b.new = true; else b.conversationId = convId;
        const r = await api.post(`/sandboxes/${id}/prompt`, b);
        if (r.conversationId) convId = r.conversationId;
        fresh = false;
      }
      ta.value = '';
      await Promise.all([loadConvs(), loadFeed()]);
    } catch (e) { toastErr(e); } finally { send.disabled = false; ta.focus(); }
  }
  send.addEventListener('click', submit);
  ta.addEventListener('keydown', (e) => { if (e.key === 'Enter' && (e.ctrlKey || e.metaKey || !e.shiftKey) && !e.isComposing) { e.preventDefault(); submit(); } });
  interrupt.addEventListener('click', async () => {
    try { await api.post(`/sandboxes/${id}/interrupt${convId ? `?conversation=${encodeURIComponent(convId)}` : ''}`, {}); toast('Interrupted'); loadConvs(); } catch (e) { toastErr(e); }
  });

  const live = isLive(ctx.sb.state);
  if (!live) { ta.disabled = true; ta.placeholder = 'Start the sandbox to prompt its agent.'; send.disabled = true; }

  root.append(h('div.grid.side',
    h('div.card',
      h('div.card-head', h('div.row', icon('bot'), h('h2', 'Agent'), mode), h('div.row', interrupt)),
      feed,
      h('div.composer', h('div.stack.tight.grow', ta, h('div.row.small.faint', harnessSel, h('span.hide-sm', 'Enter to send · Shift+Enter for a new line'))), send)),
    h('div.stack',
      card({ title: 'Conversations', flush: true, actions: btn('New', { size: 'sm', ico: 'plus', onClick: () => { fresh = true; convId = null; drawConvs(); loadFeed(); ta.focus(); } }), body: convList }),
      card({ title: 'About', body: h('p.small.muted', 'The agent runs inside the sandbox with its own tools, files and desktop. Prompts queue per conversation; while a turn runs, Send steers it. Credentials come from ', h('a', { href: '/ai' }, 'AI credentials'), '.') }))));

  function drawConvs() {
    patch(convList, { convs, convId, fresh }, () => {
      if (!convs.length && !fresh) return h('p.faint.small', { style: { padding: '12px 16px' } }, 'No conversations yet.');
      return h('div', { role: 'listbox', 'aria-label': 'Conversations' },
        fresh ? h('button.fitem', { type: 'button', 'aria-selected': 'true' }, icon('plus', 'sm'), h('span', 'New conversation')) : null,
        convs.map((c) => h('button.fitem', { type: 'button', role: 'option', 'aria-selected': String(!fresh && c.id === convId), style: { alignItems: 'flex-start', padding: '8px 16px' },
          onclick: () => { fresh = false; convId = c.id; history.replaceState(null, '', `/sandboxes/${id}/agent?c=${c.id}`); drawConvs(); loadFeed(); } },
        h('div.stack.tight.grow', { style: { gap: '2px' } },
          h('span.truncate', c.lastPromptPreview || 'Untitled'),
          h('span.faint.small', `${c.lastHarness || 'agent'} · ${c.prompts} prompt${c.prompts === 1 ? '' : 's'} · ${ago(c.lastPromptAt)}`)),
        c.running ? stateChip('running') : null)));
    });
    const r = running() && !fresh;
    interrupt.hidden = !r;
    send.replaceChildren(icon(r ? 'wand' : 'send'), r ? 'Steer' : 'Send');
    mode.textContent = r ? 'working…' : '';
  }

  async function loadConvs() {
    try {
      const r = await api.get(`/sandboxes/${id}/conversations`);
      convs = r.conversations || [];
      if (!convId && !fresh) convId = (convs.find((c) => c.current) || convs[0])?.id || null;
      drawConvs();
    } catch (e) { convList.replaceChildren(h('p.small.faint', { style: { padding: '12px 16px' } }, errText(e))); }
  }

  async function loadFeed() {
    if (fresh || !convId) {
      feed._key = null;
      feed.replaceChildren(empty({ ico: 'bot', title: 'Start a conversation', text: 'Describe a task. The agent works in this sandbox and reports back here.' }));
      return;
    }
    try {
      const r = await api.get(`/sandboxes/${id}/events`, { conversation: convId, limit: 200 });
      const atBottom = feed.scrollHeight - feed.scrollTop - feed.clientHeight < 40;
      patch(feed, r.events || [], renderEvents);
      if (atBottom) feed.scrollTop = feed.scrollHeight;
    } catch (e) { feed.replaceChildren(h('div.notice.err', icon('alert'), h('div', errText(e)))); }
  }

  await loadConvs();
  await loadFeed();
  feed.scrollTop = feed.scrollHeight;
  every(2000, async () => { await loadConvs(); await loadFeed(); }, ctx.signal);
  if (live) ta.focus();
}

function renderEvents(events) {
  // keep only the latest streaming partial per task; drop it once its final arrives
  const out = [], partial = new Map();
  for (const ev of events) {
    const d = ev.data || {};
    if (ev.type === 'response' && d.is_streaming) { partial.set(ev.taskId || 'x', ev); continue; }
    if (ev.type === 'response') partial.delete(ev.taskId || 'x');
    out.push(ev);
  }
  out.push(...partial.values());
  if (!out.length) return h('p.faint', { style: { margin: 'auto' } }, 'Nothing here yet.');
  return out.map((ev) => {
    const d = ev.data || {}, when = ev.timestamp ? ago(ev.timestamp) : '';
    if (ev.type === 'prompt') return h('div.msg.user', h('div', h('div.bubble', d.prompt), h('div.meta', stateChip(d.status), when, d.error ? h('span', { style: { color: 'var(--err-text)' } }, d.error) : null)));
    if (ev.type === 'steer') return h('div.msg.user', h('div', h('div.bubble', d.message), h('div.meta', icon('wand', 'sm'), `steer · ${d.mode || ''}`, when)));
    if (ev.type === 'response') {
      const tools = (d.tools || []).map((t) => h('div.tool', h('b', t.name || t.tool || 'tool'), ' ', String(t.input?.command || t.input?.path || t.summary || (t.input ? JSON.stringify(t.input) : '')).slice(0, 240)));
      return h('div.msg', h('span.avatar', { style: { background: 'var(--surface-3)', color: 'var(--text-2)' }, 'aria-hidden': 'true' }, icon('bot', 'sm')),
        h('div.stack.tight', { style: { minWidth: 0 } }, tools, d.content ? h('div.bubble', d.content) : null,
          h('div.meta', d.model ? h('span', `${d.model} ·`) : null, d.is_streaming ? [h('span.spinner', { style: { width: '10px', height: '10px' } }), h('span', 'typing')] : h('span', when))));
    }
    if (ev.type === 'usage_limit') return h('div.notice.warn', icon('alert'), h('div', d.message || 'The provider usage limit was reached.'));
    return h('div.sysline', icon('info', 'sm'), ev.type.replace(/_/g, ' '), when ? ` · ${when}` : '');
  });
}
