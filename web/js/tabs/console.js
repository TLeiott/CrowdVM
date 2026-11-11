// Command console: streamed commands (NDJSON) plus the sandbox's command history.
// Not a PTY: each line runs as its own `bash -lc` command in the guest.
import { h, icon, btn, ago, every, patch, duration, bytes, copy } from '../dom.js';
import { api, stream, errText } from '../api.js';
import { card, table, stateChip, modal, toast, toastErr, isLive, empty, menuButton } from '../ui.js';

export default async function (ctx, root) {
  const id = ctx.id;
  const out = h('div.console', { role: 'log', 'aria-live': 'polite', 'aria-label': 'Command output', tabindex: '0' });
  const input = h('input', { 'aria-label': 'Command', autocomplete: 'off', spellcheck: 'false', autocapitalize: 'none', placeholder: 'Type a command and press Enter' });
  const cwd = h('input.input.sm.mono', { value: '', placeholder: '~', 'aria-label': 'Working directory', style: { width: '160px', background: 'transparent', color: 'var(--console-fg)', borderColor: '#ffffff26' } });
  const timeout = h('select.select.sm', { 'aria-label': 'Timeout', style: { width: 'auto', background: 'transparent', color: 'var(--console-fg)', borderColor: '#ffffff26' } },
    [['30', '30 s'], ['120', '2 min'], ['600', '10 min']].map(([v, l]) => h('option', { value: v }, l)));
  const bg = h('input', { type: 'checkbox' });
  const status = h('span', { style: { marginLeft: 'auto' } });
  const hist = [];
  let hi = 0, running = null;

  const line = (cls, text) => { out.append(h('span', { class: cls }, text)); out.scrollTop = out.scrollHeight; };
  const prompt = h('span', `user@${id}:${cwd.value || '~'}$`);
  cwd.addEventListener('input', () => { prompt.textContent = `user@${id}:${cwd.value || '~'}$`; });

  out.append(h('span.dim', `Connected to ${ctx.sb.name}. Each line runs as its own command (no TTY). Ctrl+C stops a running command.\n`));

  async function run(cmd) {
    line('cmd', cmd + '\n');
    hist.push(cmd); hi = hist.length;
    const body = { command: cmd, timeoutSeconds: Number(timeout.value) };
    if (cwd.value.trim()) body.cwd = cwd.value.trim();
    if (bg.checked) {
      try {
        const r = await api.post(`/sandboxes/${id}/commands`, { ...body, detached: true });
        line('dim', `started in background: pid ${r.pid ?? r.processId}${r.commandId ? `, ${r.commandId}` : ''}\n`);
      } catch (e) { line('err', errText(e) + '\n'); }
      loadHistory();
      return;
    }
    const ctl = new AbortController();
    running = ctl;
    status.replaceChildren(h('span.spinner', { style: { width: '12px', height: '12px' } }), ' running');
    const t0 = performance.now();
    try {
      await stream(`/sandboxes/${id}/commands`, { ...body, stream: true }, (f) => {
        if (f.type === 'stdout') line('', f.data);
        else if (f.type === 'stderr') line('err', f.data);
        else if (f.type === 'exit') line(f.exitCode === 0 ? 'exit-ok' : 'exit-bad', `${out.lastChild?.textContent?.endsWith('\n') ? '' : '\n'}[exit ${f.exitCode ?? '?'}${f.timedOut ? ', timed out' : ''} · ${duration((performance.now() - t0) / 1000)}]\n`);
        else if (f.type === 'error') line('err', `${f.message || f.error}\n`);
        else if (f.type === '_result') { // server answered without streaming
          if (f.stdout) line('', f.stdout); if (f.stderr) line('err', f.stderr);
          line(f.exitCode === 0 ? 'exit-ok' : 'exit-bad', `[exit ${f.exitCode}]\n`);
        }
      }, ctl.signal);
    } catch (e) {
      line('err', e.name === 'AbortError' ? '^C\n' : `${errText(e)}\n`);
    } finally {
      running = null; status.replaceChildren(); loadHistory();
    }
  }

  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !running) {
      const cmd = input.value.trim(); if (!cmd) return;
      input.value = '';
      if (cmd === 'clear') { out.replaceChildren(); return; }
      run(cmd);
    } else if (e.key === 'c' && e.ctrlKey && running) { e.preventDefault(); running.abort(); }
    else if (e.key === 'l' && e.ctrlKey) { e.preventDefault(); out.replaceChildren(); }
    else if (e.key === 'ArrowUp' && hist.length) { e.preventDefault(); hi = Math.max(0, hi - 1); input.value = hist[hi]; }
    else if (e.key === 'ArrowDown' && hist.length) { e.preventDefault(); hi = Math.min(hist.length, hi + 1); input.value = hist[hi] || ''; }
  });

  const live = isLive(ctx.sb.state);
  const term = h('div.term',
    h('div.term-bar', icon('terminal', 'sm'), h('label', { class: 'sr-only', for: 'cwd' }, 'Working directory'), Object.assign(cwd, { id: 'cwd' }), timeout,
      h('label.check', { style: { alignItems: 'center', gap: '6px' } }, bg, h('span', 'Background')), status,
      btn('Clear', { size: 'sm', onClick: () => out.replaceChildren() })),
    out,
    h('div.console-input', prompt, input));
  if (!live) { input.disabled = true; input.placeholder = 'Start the sandbox to run commands'; }

  const histEl = h('div');
  root.append(h('div.stack.loose', term, card({ title: 'Command history', sub: 'Commands run through the API, CLI or this console. Kept 90 days.', flush: true, body: histEl })));
  input.focus();

  async function loadHistory() {
    try {
      const r = await api.get(`/sandboxes/${id}/command-history`, { limit: 25 });
      patch(histEl, r.commands || [], (rows) => table({
        rows,
        onRow: (c) => showLogs(id, c),
        emptyEl: empty({ ico: 'history', title: 'No commands yet', text: 'Commands you run show up here with their output.' }),
        cols: [
          { label: 'Command', render: (c) => h('code.truncate', { style: { display: 'block', maxWidth: '420px' }, title: c.command }, c.command) },
          { label: 'Status', render: (c) => c.status === 'exited' ? h('span.chip', { class: c.exitCode === 0 ? 'ok' : 'err' }, `exit ${c.exitCode}`) : stateChip(c.status === 'running' ? 'running' : c.status, c.status.replace('_', ' ')) },
          { label: 'Mode', cls: 'hide-xs', render: (c) => h('span.tag', c.mode) },
          { label: 'Output', cls: 'num hide-md', render: (c) => bytes((c.stdoutBytes || 0) + (c.stderrBytes || 0)) },
          { label: 'Started', cls: 'hide-xs', render: (c) => h('span.muted', ago(c.startedAt)) },
          { label: 'Took', cls: 'num hide-md', render: (c) => c.finishedAt ? duration((new Date(c.finishedAt) - new Date(c.startedAt)) / 1000) : '—' },
          { label: h('span.sr-only', 'Actions'), cls: 'act', render: (c) => menuButton(() => [
            { label: c.status === 'running' ? 'Follow output' : 'Show output', ico: 'terminal', onClick: () => showLogs(id, c) },
            { label: 'Copy command', ico: 'copy', onClick: () => copy(c.command).then(() => toast('Copied')) },
            { label: 'Run again', ico: 'play', onClick: () => { input.value = c.command; input.focus(); } },
            c.status === 'running' && c.processId ? '-' : null,
            c.status === 'running' && c.processId ? { label: 'Stop', ico: 'stop', danger: true, onClick: async () => {
              try { await api.del(`/sandboxes/${id}/commands/${c.processId}`); toast('Stopped'); loadHistory(); } catch (e) { toastErr(e); }
            } } : null,
          ], { title: `Actions for ${c.command.slice(0, 40)}` }) },
        ],
      }));
    } catch (e) { histEl.replaceChildren(h('div.card-body', h('span.faint', errText(e)))); }
  }
  await loadHistory();
  every(8000, loadHistory, ctx.signal);
}

// Output of one command. While it runs, polls the logs from the last offset (live follow).
async function showLogs(id, c) {
  const pre = h('div.console', { style: { maxHeight: '55vh' }, 'aria-live': 'off' }, h('span.dim', 'Loading…'));
  const status = h('span.small.muted', { role: 'status' });
  const ctl = new AbortController();
  modal({ title: 'Command output', desc: h('code', c.command), wide: true, body: h('div.stack.tight', pre, status), actions: [{ label: 'Close', onClick: () => true }], onClose: () => ctl.abort() });
  const off = { stdout: 0, stderr: 0 }, dec = (r) => (!r ? '' : r.encoding === 'base64' ? atob(r.data) : r.data || '');
  let first = true, any = false;
  try {
    for (;;) {
      const rs = await Promise.all(['stdout', 'stderr'].map((st) => api.get(`/sandboxes/${id}/command-history/${c.commandId}/logs`, { stream: st, offset: off[st], limit: 1048576 }, { signal: ctl.signal }).catch((e) => { if (e.name === 'AbortError') throw e; return null; })));
      if (first) { pre.replaceChildren(); first = false; }
      const stick = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 8;
      rs.forEach((r, i) => {
        const t = dec(r);
        if (t) { pre.append(h('span', { class: i ? 'err' : '' }, t)); any = true; }
        if (r?.nextOffset != null) off[i ? 'stderr' : 'stdout'] = r.nextOffset;
      });
      if (stick) pre.scrollTop = pre.scrollHeight;
      const done = rs.every((r) => !r || r.done !== false);
      if (done) break;
      status.textContent = 'Running — following output…';
      await new Promise((res) => setTimeout(res, 1000));
      if (ctl.signal.aborted) return;
    }
    const fin = c.status === 'running' ? (await api.get(`/sandboxes/${id}/command-history`, { limit: 50 }).catch(() => null))?.commands?.find((x) => x.commandId === c.commandId) || c : c;
    if (!any) pre.append(h('span.dim', '(no output)'));
    pre.append(h('span', { class: fin.exitCode === 0 ? 'exit-ok' : 'exit-bad' }, `\n[${fin.status}${fin.exitCode != null ? ` ${fin.exitCode}` : ''}]`));
    status.textContent = '';
  } catch (err) { if (err.name !== 'AbortError') pre.replaceChildren(h('span.err', errText(err))); }
}
