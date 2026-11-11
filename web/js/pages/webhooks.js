// Webhooks: signed POSTs on sandbox lifecycle events.
import { h, icon, btn, time } from '../dom.js';
import { api } from '../api.js';
import { pageHead, table, loading, errorBox, modal, field, toast, toastErr, confirmDialog, secretDialog, menuButton, empty, card, copyBtn } from '../ui.js';

const EVENTS = [
  ['sandbox.ready', 'Ready', 'Booted and accepting commands.'],
  ['sandbox.archived', 'Stopped', 'Disk saved, machine off.'],
  ['sandbox.error', 'Error', 'Failed to start or crashed.'],
  ['sandbox.hydrated', 'Hydrated', 'Every file of a resumed snapshot is on disk.'],
  ['sandbox.degraded', 'Degraded', 'Its node lost contact.'],
  ['sandbox.recovered', 'Recovered', 'Reachable again.'],
];

export default async function (ctx) {
  ctx.title('Webhooks');
  const body = h('div', loading());
  ctx.root.append(pageHead({
    title: 'Webhooks',
    lede: 'The server POSTs a JSON event to your URL when a sandbox changes state. Each request is signed with the webhook’s secret.',
    actions: btn('Add webhook', { kind: 'primary', ico: 'plus', onClick: () => edit(), attrs: { 'data-create': '' } }),
  }), body);

  async function load() {
    try {
      const r = await api.get('/webhooks', undefined, { signal: ctx.signal });
      body.replaceChildren(h('div.stack.loose', h('div.card', table({
        rows: r.webhooks || [],
        emptyEl: empty({ ico: 'webhook', title: 'No webhooks', text: 'Add one to react to sandboxes starting, stopping or failing.', action: btn('Add webhook', { kind: 'primary', ico: 'plus', onClick: () => edit() }) }),
        cols: [
          { label: 'Endpoint', render: (w) => h('div.stack.tight', { style: { gap: 0 } }, h('b', w.name || 'Webhook'), h('code.faint.small.truncate', { style: { display: 'block', maxWidth: '360px' } }, w.url)) },
          { label: 'Events', cls: 'hide-xs', render: (w) => h('div.row.wrap', { style: { gap: '4px' } }, (w.events || []).map((e) => h('span.tag', e.replace('sandbox.', '')))) },
          { label: 'Updated', cls: 'hide-md', render: (w) => h('span.muted', time(w.updatedAt)) },
          { label: h('span.sr-only', 'Actions'), cls: 'act', render: (w) => menuButton([
            { label: 'Deliveries…', ico: 'history', onClick: () => deliveries(w) },
            { label: 'Send test event', ico: 'send', onClick: () => test(w) },
            { label: 'Edit…', ico: 'edit', onClick: () => edit(w) },
            { label: 'Rotate secret…', ico: 'refresh', onClick: () => rotate(w) },
            '-',
            { label: 'Delete…', ico: 'trash', danger: true, onClick: () => remove(w) },
          ], { title: `Actions for ${w.name || w.url}` }) },
        ],
      })), card({ title: 'Verifying requests', body: h('div.stack.tight.small.muted',
        h('p', 'Each request carries ', h('code', 'X-CrowdVM-Delivery'), ' (the event id), ', h('code', 'X-CrowdVM-Timestamp'), ' (unix seconds) and ', h('code', 'X-CrowdVM-Signature: v1=<hex>'),
          '. Compute HMAC-SHA256 with the secret over ', h('code', '<delivery>.<timestamp>.<raw body>'), ' and compare. Reject old timestamps and repeated delivery ids.'),
        h('p', 'Respond 2xx within 5 seconds; redirects are not followed. Failed deliveries are retried with backoff, up to 8 attempts over about 3 hours.')) })));
    } catch (e) { if (e.name !== 'AbortError') body.replaceChildren(errorBox(e, load)); }
  }

  function edit(w) {
    const name = h('input.input', { value: w?.name || '', placeholder: 'e.g. Slack relay', maxlength: 100 });
    const url = h('input.input.mono', { value: w?.url || '', placeholder: 'http://ci.lan:8080/hooks/crowdvm', type: 'url' });
    const evs = h('div.checks', EVENTS.map(([v, l, d]) => h('label.check', h('input', { type: 'checkbox', value: v, checked: w ? w.events.includes(v) : ['sandbox.ready', 'sandbox.archived', 'sandbox.error'].includes(v) }), h('div', h('div.t', l), h('div.d', d)))));
    modal({
      title: w ? 'Edit webhook' : 'Add webhook', wide: true, submit: true,
      body: h('div.stack', h('div.fields', field('Name', name), field('URL', url, 'Any http(s) URL the server can reach on your LAN or tailnet.')), h('div.field', h('span.label', 'Events'), evs)),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: w ? 'Save' : 'Add webhook', kind: 'primary', submit: true, onClick: async () => {
        const events = [...evs.querySelectorAll('input:checked')].map((i) => i.value);
        if (!/^https?:\/\/.+/.test(url.value.trim())) throw new Error('Enter an http:// or https:// URL.');
        if (!events.length) throw new Error('Pick at least one event.');
        const b = { name: name.value.trim() || null, url: url.value.trim(), events };
        if (w) { await api.patch(`/webhooks/${w.id}`, b); toast('Webhook saved'); }
        else { const r = await api.post('/webhooks', b); secretDialog({ title: 'Webhook added', desc: 'Use this secret to verify signatures.', secret: r.secret }); }
        load();
      } }],
    });
  }
  async function rotate(w) {
    if (!(await confirmDialog({ title: 'Rotate signing secret?', desc: 'Requests are signed with the new secret from now on.', confirm: 'Rotate' }))) return;
    try { const r = await api.post(`/webhooks/${w.id}/rotate`, {}); secretDialog({ title: 'New signing secret', secret: r.secret }); } catch (e) { toastErr(e); }
  }
  async function remove(w) {
    if (!(await confirmDialog({ title: 'Delete webhook?', desc: w.url, confirm: 'Delete', danger: true }))) return;
    try { await api.del(`/webhooks/${w.id}`); toast('Webhook deleted'); load(); } catch (e) { toastErr(e); }
  }
  async function test(w) {
    try {
      const { test: t = {} } = await api.post(`/webhooks/${w.id}/test`, {});
      modal({
        title: t.ok ? 'Test event delivered' : 'Test event failed', desc: w.url,
        body: h('div.stack',
          h('div.notice', { class: t.ok ? 'ok' : 'err' }, icon(t.ok ? 'check' : 'alert'), h('div', h('b', t.message?.startsWith('HTTP') ? t.message : t.status ? `HTTP ${t.status}` : 'No answer'), t.message && !t.message.startsWith('HTTP') ? h('div', { style: { overflowWrap: 'anywhere' } }, t.message) : null)),
          h('p.small.muted', 'A signed ', h('code', 'webhook.test'), ' event was posted once (no retries). It shows up in the delivery log as ', h('code', t.eventId || '—'), '.')),
        actions: [{ label: 'View deliveries', onClick: () => { deliveries(w); return true; } }, { label: 'Close', kind: 'primary', onClick: () => true }],
      });
    } catch (e) { toastErr(e); }
  }

  function deliveries(w) {
    const box = h('div', loading());
    const reload = btn('Refresh', { size: 'sm', ico: 'refresh', onClick: () => draw() });
    modal({ title: 'Deliveries', desc: w.name ? `${w.name} · ${w.url}` : w.url, wide: true, body: h('div.stack', h('div.row.between', h('span.small.muted', 'Newest first, kept for 30 days.'), reload), box),
      actions: [{ label: 'Close', onClick: () => true }] });
    let sel = null;
    async function draw() {
      try {
        const r = await api.get(`/webhooks/${w.id}/deliveries`, { limit: 100 });
        const rows = r.deliveries || [];
        sel = rows.find((d) => d.id === sel?.id) || rows[0] || null;
        const detail = h('div');
        const show = (d) => { sel = d; detail.replaceChildren(payload(d)); for (const tr of box.querySelectorAll('tbody tr')) { const on = tr._d === d; tr.toggleAttribute('aria-current', on); tr.style.background = on ? 'var(--surface-2)' : ''; } };
        box.replaceChildren(h('div.stack', table({
          rows, onRow: show, rowLabel: (d) => `${d.event}, ${d.status}`,
          emptyEl: empty({ ico: 'history', title: 'No deliveries yet', text: 'Events show up here once a subscribed sandbox changes state. Send a test event to try the endpoint.' }),
          cols: [
            { label: 'Event', render: (d) => h('div.stack.tight', { style: { gap: 0, alignItems: 'flex-start' } }, h('span.tag', d.event), h('span.faint.small', time(d.createdAt))) },
            { label: 'Status', render: statusChip },
            { label: 'HTTP', cls: 'num hide-xs', render: (d) => d.httpStatus ?? h('span.faint', '—') },
            { label: 'Last attempt', cls: 'hide-md', render: (d) => h('span.muted', time(d.lastAttemptAt)) },
            { label: h('span.sr-only', 'Payload'), cls: 'act', render: (d) => btn('', { kind: 'ghost', size: 'sm', ico: 'eye', title: `Show payload of ${d.event}`, onClick: () => show(d) }) },
          ],
        }), detail));
        box.querySelectorAll('tbody tr').forEach((tr, i) => { tr._d = rows[i]; });
        if (sel) show(sel);
      } catch (e) { box.replaceChildren(errorBox(e, draw)); }
    }
    draw();
  }

  await load();
}

function statusChip(d) {
  const retry = d.status === 'pending' && d.attempts > 0;
  const [cls, text] = d.status === 'delivered' ? ['ok', 'delivered'] : d.status === 'failed' ? ['err', 'failed'] : retry ? ['warn', 'retrying'] : ['info busy', 'pending'];
  return h('div.stack.tight', { style: { gap: 0, alignItems: 'flex-start' } }, h('span.chip', { class: cls }, text),
    h('span.faint.small', retry && d.nextRetryAt ? ['retry ', time(d.nextRetryAt)] : `${d.attempts}/${d.maxAttempts} attempts`));
}

function payload(d) {
  const json = JSON.stringify(d.payload, null, 2);
  return h('div.stack.tight',
    h('div.row.between', h('b.small', 'Payload of ', h('code', d.eventId)), copyBtn(json, 'Copy payload')),
    d.error ? h('div.notice.err', icon('alert'), h('div', { style: { overflowWrap: 'anywhere' } }, d.error)) : null,
    h('pre.code', { tabindex: '0', 'aria-label': 'Payload JSON', style: { maxHeight: '280px' } }, json));
}
