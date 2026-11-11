// Telemetry: export lifecycle logs and sandbox metrics to an OpenTelemetry collector (OTLP/HTTP JSON).
import { h, icon, btn, time } from '../dom.js';
import { api, isMissing } from '../api.js';
import { pageHead, card, field, loading, errorBox, empty, toast, toastErr, confirmDialog } from '../ui.js';

const METRICS = [
  ['boat.account.sandboxes.active', 'Active sandboxes (and .limit when the server caps them)'],
  ['boat.account.credit.balance', 'Credit balance in seconds of a default sandbox, when credits are enforced'],
  ['boat.sandbox.cpu.utilization', 'CPU busy share, 0–1'],
  ['boat.sandbox.memory.usage', 'Memory used in bytes, plus .utilization'],
  ['boat.sandbox.io.pressure', 'IO pressure, 60 s average'],
  ['boat.sandbox.filesystem.usage', 'Disk used in bytes, plus .utilization, when the guest reports it'],
];

export default async function (ctx) {
  ctx.title('Telemetry');
  const body = h('div', loading());
  ctx.root.append(pageHead({
    title: 'Telemetry',
    lede: 'Send sandbox metrics and lifecycle events to your own OpenTelemetry collector over OTLP/HTTP (JSON).',
  }), body);

  async function load() {
    try {
      const r = await api.get('/telemetry', undefined, { signal: ctx.signal });
      draw(r.telemetry);
    } catch (e) {
      if (e.name === 'AbortError') return;
      body.replaceChildren(isMissing(e) ? h('div.card', empty({ ico: 'activity', title: 'Telemetry export is not available', text: 'This server does not have the OpenTelemetry feature.' })) : errorBox(e, load));
    }
  }

  function draw(t, test) {
    body.replaceChildren(h('div.grid.side',
      h('div.stack.loose', statusCard(t), formCard(t, test)),
      h('div.stack.loose', exportedCard())));
  }

  function statusCard(t) {
    if (!t) return h('div.card', empty({ ico: 'activity', title: 'Not connected', text: 'Add your collector’s endpoint below. Nothing is exported until you do.' }));
    const err = t.lastError;
    const state = err ? ['err', 'failing'] : t.lastExportAt ? ['ok', 'exporting'] : ['info busy', 'waiting'];
    return card({
      title: 'Connection',
      actions: btn('Disconnect', { size: 'sm', kind: 'danger', ico: 'trash', onClick: () => disconnect(t) }),
      body: h('div.stack',
        h('dl.kv',
          h('dt', 'Status'), h('dd', h('span.chip', { class: state[0] }, state[1])),
          h('dt', 'Endpoint'), h('dd', h('code', t.endpoint)),
          h('dt', 'Last export'), h('dd', t.lastExportAt ? time(t.lastExportAt) : h('span.faint', 'none yet')),
          h('dt', 'Headers'), h('dd', t.headers?.length ? t.headers.map((n) => h('span.tag', n)) : h('span.faint', 'none')),
          h('dt', 'Connected'), h('dd', time(t.createdAt))),
        err ? h('div.notice.err', { role: 'alert' }, icon('alert'), h('div',
          h('b', err.status && !err.message?.startsWith('HTTP') ? `HTTP ${err.status}` : 'Export failed'), err.at ? [' · ', time(err.at)] : null,
          err.message ? h('div', { style: { overflowWrap: 'anywhere' } }, err.message) : null)) : null),
    });
  }

  function formCard(t, test) {
    const endpoint = h('input.input.mono', { type: 'url', value: t?.endpoint || '', placeholder: 'http://otel-collector.lan:4318', autocomplete: 'off', required: true });
    const rows = h('div.stack.tight');
    const addRow = (name = '', existing = false) => {
      const n = h('input.input.mono', { value: name, placeholder: 'Header name', 'aria-label': 'Header name', autocomplete: 'off', maxlength: 128 });
      const v = h('input.input.mono', { type: 'password', placeholder: existing ? 'Re-enter value' : 'Value', 'aria-label': `Value of ${name || 'header'}`, autocomplete: 'new-password', maxlength: 8192 });
      n.addEventListener('input', () => v.setAttribute('aria-label', `Value of ${n.value || 'header'}`));
      const row = h('div.row.wrap', { style: { gap: '6px' } }, h('div', { style: { minWidth: 0, flex: '1 1 140px' } }, n), h('div.row', { style: { gap: '6px', minWidth: 0, flex: '2 1 180px' } }, h('div.grow', { style: { minWidth: 0 } }, v),
        btn('', { kind: 'ghost', size: 'sm', ico: 'x', title: 'Remove header', onClick: () => { row.remove(); if (!rows.children.length) hint.hidden = true; } })));
      row._n = n; row._v = v;
      rows.append(row);
      return n;
    };
    for (const n of t?.headers || []) addRow(n, true);
    const hint = h('div.hint', { hidden: !t?.headers?.length }, 'Header values are stored encrypted and never shown again. Re-enter every value when you save.');
    const result = h('div', { role: 'status' }, test ? h('div.notice', { class: test.ok ? 'ok' : 'err' }, icon(test.ok ? 'check' : 'alert'), h('div',
      h('b', test.ok ? 'Test export succeeded' : 'Saved, but the test export failed'), test.status && !test.message?.startsWith('HTTP') ? ` · HTTP ${test.status}` : '',
      test.message ? h('div', { style: { overflowWrap: 'anywhere' } }, test.message) : null)) : null);
    const save = btn(t ? 'Save and test' : 'Connect and test', { kind: 'primary', type: 'submit' });
    const form = h('form.stack', { novalidate: true },
      field('Endpoint', endpoint, h('span', 'The collector’s base URL. The server appends ', h('code', '/v1/metrics'), ' and ', h('code', '/v1/logs'), '. http:// and LAN addresses work.')),
      h('div.field', h('span.label', 'Headers'), rows, hint,
        h('div', btn('Add header', { size: 'sm', ico: 'plus', onClick: () => { addRow().focus(); } })),
        h('div.hint', 'Sent with every export, e.g. an authorization token for your collector.')),
      result,
      h('div.row.end', save));
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      result.replaceChildren();
      const headers = {};
      try {
        if (!/^https?:\/\/.+/.test(endpoint.value.trim())) { endpoint.focus(); throw new Error('Enter an http:// or https:// URL.'); }
        for (const r of rows.children) {
          const n = r._n.value.trim(), v = r._v.value;
          if (!n && !v) continue;
          if (!n) { r._n.focus(); throw new Error('Every header needs a name.'); }
          if (!v) { r._v.focus(); throw new Error(`Enter the value of ${n}.`); }
          if (n in headers) { r._n.focus(); throw new Error(`${n} is listed twice.`); }
          headers[n] = v;
        }
      } catch (err) { result.append(h('div.notice.err', icon('alert'), h('div', err.message))); return; }
      save.disabled = true; save.prepend(h('span.spinner'));
      try {
        const r = await api.put('/telemetry', { endpoint: endpoint.value.trim(), headers });
        draw(r.telemetry, r.test || {});
      } catch (err) { toastErr(err); save.disabled = false; save.querySelector('.spinner')?.remove(); }
    });
    return card({ title: t ? 'Change destination' : 'Connect a collector', sub: 'Saving sends one empty metrics request to check the endpoint.', body: form });
  }

  function exportedCard() {
    return card({ title: 'What is exported', body: h('div.stack.small',
      h('div.stack.tight', h('b', 'Metrics'), h('p.muted', 'Every 60 seconds to ', h('code', '/v1/metrics'), '. Sandbox gauges come from running sandboxes with monitoring on. If the collector is down, up to 10 minutes are kept and sent in order.'),
        h('ul.stack.tight', { style: { margin: 0, paddingLeft: '18px' } }, METRICS.map(([n, d]) => h('li', h('code', n), h('div.faint', d))))),
      h('div.stack.tight', h('b', 'Logs'), h('p.muted', 'One record per sandbox lifecycle event to ', h('code', '/v1/logs'), ': ready, hydrated, stopped, recovered (INFO), degraded (WARN), error (ERROR). Failed sends are retried like webhooks.')),
      h('p.muted', 'Resources carry ', h('code', 'service.name=crowdvm'), ' and the sandbox id, name and size. No traces, commands, files or AI conversations are sent.')) });
  }

  async function disconnect(t) {
    if (!(await confirmDialog({ title: 'Disconnect telemetry?', desc: `Stops exporting to ${t.endpoint} and drops anything not yet sent.`, confirm: 'Disconnect', danger: true }))) return;
    try { await api.del('/telemetry'); toast('Telemetry disconnected'); draw(null); } catch (e) { toastErr(e); }
  }

  await load();
}
