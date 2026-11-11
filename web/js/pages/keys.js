// API keys: scoped keys with presets, expiry, rotation, revocation and usage.
import { h, icon, btn, time, ago, num } from '../dom.js';
import { api } from '../api.js';
import { pageHead, table, loading, errorBox, modal, field, toast, toastErr, confirmDialog, secretDialog, menuButton, stateChip, empty, segmented, copyable } from '../ui.js';

const PRESETS = [
  ['read-only', 'Read only', 'Inspect sandboxes, files, snapshots and environments.'],
  ['full-sandbox', 'Full sandbox', 'Operate sandboxes: exec, files, prompt, ssh, desktop. No create or delete.'],
  ['ci', 'CI', 'Full sandbox, plus create, resume, fork and delete the sandboxes it created.'],
  ['admin', 'Admin', 'Everything, including account settings and other keys.'],
];
const TTLS = [['30d', '30 days'], ['90d', '90 days'], ['365d', '1 year'], ['never', 'Never']];

export default async function (ctx) {
  ctx.title('API keys');
  const body = h('div', loading());
  let catalog = null;
  ctx.root.append(pageHead({
    title: 'API keys',
    lede: h('span', 'Keys for the CLI, SDKs and your own automation. Send one as ', h('code', 'Authorization: Bearer cvm_…'), '. A key never reaches more than you can.'),
    actions: btn('Create key', { kind: 'primary', ico: 'plus', onClick: create, attrs: { 'data-create': '' } }),
  }), body);

  async function load() {
    try {
      const r = await api.get('/api-keys', undefined, { signal: ctx.signal });
      catalog = r.catalog || null;
      // The keys of the viewed organization (or the personal account's). New keys go to it too.
      const sel = ctx.session.view?.type === 'org' ? ctx.session.view.id : '';
      const rows = (r.apiKeys || []).filter((k) => !k.sandboxId && (k.org || '') === sel);
      body.replaceChildren(h('div.card', table({
        rows,
        emptyEl: empty({ ico: 'key', title: 'No API keys', text: 'Create one for a script or CI job. The CLI gets its own key when you run crowdvm login.',
          action: btn('Create key', { kind: 'primary', ico: 'plus', onClick: create }) }),
        cols: [
          { label: 'Name', render: (k) => h('div.stack.tight', { style: { gap: 0 } }, h('b', k.name || 'Unnamed'), h('code.faint.small', `${k.keyPrefix || 'cvm'}…${k.keyLastFour}`)) },
          { label: 'Scope', cls: 'hide-xs', render: scopeText },
          { label: 'Last used', cls: 'hide-md', render: (k) => h('span.muted', k.lastUsedAt ? ago(k.lastUsedAt) : 'never') },
          { label: 'Requests', cls: 'num hide-md', render: (k) => h('span', { title: 'Last 30 days' }, num(k.usage?.requests)) },
          { label: 'Expires', render: (k) => k.expired ? stateChip('expired') : k.expiresAt ? h('span', { class: k.expiringSoon ? '' : 'muted', style: k.expiringSoon ? { color: 'var(--warn-text)' } : null }, ago(k.expiresAt)) : h('span.faint', 'never') },
          { label: h('span.sr-only', 'Actions'), cls: 'act', render: (k) => menuButton([
            { label: 'Usage…', ico: 'activity', onClick: () => usage(k) },
            { label: 'Rotate…', ico: 'refresh', onClick: () => rotate(k) },
            '-',
            { label: 'Revoke…', ico: 'trash', danger: true, onClick: () => revoke(k) },
          ], { title: `Actions for ${k.name}` }) },
        ],
      })));
    } catch (e) { if (e.name !== 'AbortError') body.replaceChildren(errorBox(e, load)); }
  }

  function scopeText(k) {
    const a = k.scope?.actions || [];
    const preset = PRESETS.find(([p]) => k.scope?.preset === p || (catalog?.presets?.[p] && sameSet(catalog.presets[p], a)));
    const lim = [];
    if (Array.isArray(k.scope?.sandboxes)) lim.push(`${k.scope.sandboxes.length} sandbox${k.scope.sandboxes.length === 1 ? '' : 'es'}`);
    if (Array.isArray(k.scope?.environments)) lim.push(`${k.scope.environments.length} env`);
    return h('div.row.wrap', { style: { gap: '4px' } },
      preset ? h('span.chip.plain', { class: preset[0] === 'admin' ? 'warn' : '' }, preset[1]) : a.includes('*') || !a.length ? h('span.chip.plain.warn', 'Admin') : h('span.chip.plain', { title: a.join(', ') }, `${a.length} actions`),
      lim.length ? h('span.faint.small', `on ${lim.join(', ')}`) : null);
  }

  async function create() {
    let preset = 'full-sandbox', mode = 'preset', ttl = catalog?.defaultTtl || '90d';
    const name = h('input.input', { placeholder: 'e.g. GitHub Actions', maxlength: 80 });
    const presetBox = h('div.stack.tight', { role: 'radiogroup', 'aria-label': 'Preset' }, PRESETS.map(([v, l, d]) =>
      h('label.check', h('input', { type: 'radio', name: 'preset', value: v, checked: v === preset, onchange: () => { preset = v; } }), h('div', h('div.t', l), h('div.d', d)))));
    const actions = h('div.checks', (catalog?.actions || ['sandbox.create', 'sandbox.read', 'sandbox.update', 'sandbox.stop', 'sandbox.resume', 'sandbox.fork', 'sandbox.delete', 'sandbox.delete-own', 'agent.prompt', 'exec', 'file.read', 'file.write', 'ssh', 'desktop', 'host', 'snapshot.read', 'snapshot.write', 'environment.read', 'environment.write', 'account.read', 'account.admin'])
      .filter((a) => a !== '*').map((a) => h('label.check', h('input', { type: 'checkbox', value: a }), h('code.small', a))));
    const scopeBox = h('div');
    const drawScope = () => scopeBox.replaceChildren(mode === 'preset' ? presetBox : actions);
    const res = { ...catalog?.scopeResources };
    // The catalog's environment list may be empty; fall back to GET /environments.
    if (!res.environments?.length) res.environments = await api.get('/environments').then((r) => r.environments || [], () => []);
    const sbSel = res?.sandboxes?.length ? h('select.select', { multiple: true, size: Math.min(5, res.sandboxes.length) }, res.sandboxes.map((s) => h('option', { value: s.id }, `${s.name} (${s.id})`))) : null;
    const envSel = res?.environments?.length ? h('select.select', { multiple: true, size: Math.min(4, res.environments.length) }, res.environments.map((s) => h('option', { value: s.id }, s.name))) : null;
    drawScope();
    modal({
      title: 'Create API key', wide: true, submit: true,
      body: h('div.stack.loose',
        field('Name', name),
        h('div.field', h('span.label', 'Permissions'), segmented([['preset', 'Preset'], ['custom', 'Custom actions']], mode, (v) => { mode = v; drawScope(); }, 'Permission mode'), scopeBox),
        h('div.field', h('span.label', 'Expires after'), segmented(TTLS, ttl, (v) => { ttl = v; }, 'Expiry')),
        sbSel || envSel ? h('details.more', h('summary', icon('chevron', 'sm'), 'Limit to sandboxes or environments'), h('div.stack',
          sbSel ? field('Sandboxes', sbSel, 'Ctrl/⌘-click to pick several. None = all.') : null,
          envSel ? field('Environments', envSel, 'Sandboxes in these environments.') : null)) : null),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Create key', kind: 'primary', submit: true, onClick: async () => {
        const b = { name: name.value.trim() || 'API key', ttl };
        if (mode === 'preset') b.preset = preset;
        else { b.actions = [...actions.querySelectorAll('input:checked')].map((i) => i.value); if (!b.actions.length) throw new Error('Pick at least one action.'); }
        const sbs = sbSel ? [...sbSel.selectedOptions].map((o) => o.value) : [];
        const envs = envSel ? [...envSel.selectedOptions].map((o) => o.value) : [];
        if (sbs.length) b.sandboxIds = sbs;
        if (envs.length) b.environmentIds = envs;
        const r = await api.post('/api-keys/scoped', b);
        secretDialog({ title: 'API key created', desc: r.apiKey?.name, secret: r.secret,
          extra: h('div.stack.tight', h('span.small.muted', 'Use it from a shell:'), copyable(`export CROWDVM_API_KEY=${r.secret}`), copyable(`export CROWDVM_API_BASE=${location.origin}/api/v1`)) });
        load();
      } }],
    });
    name.focus();
  }

  async function rotate(k) {
    if (!(await confirmDialog({ title: `Rotate ${k.name}?`, desc: 'Issues a new secret. The old one stops working right away.', confirm: 'Rotate' }))) return;
    try { const r = await api.post(`/api-keys/${k.id}/rotate`, {}); secretDialog({ title: 'New secret', desc: k.name, secret: r.secret }); load(); } catch (e) { toastErr(e); }
  }
  async function revoke(k) {
    if (!(await confirmDialog({ title: `Revoke ${k.name}?`, desc: 'Requests with this key fail from now on. This cannot be undone.', confirm: 'Revoke', danger: true }))) return;
    try { await api.del(`/api-keys/${k.id}`); toast(`Revoked ${k.name}`); load(); } catch (e) { toastErr(e); }
  }
  async function usage(k) {
    const box = h('div', loading());
    modal({ title: `Usage of ${k.name}`, wide: true, body: box, actions: [{ label: 'Close', onClick: () => true }] });
    try {
      const r = await api.get(`/api-keys/${k.id}/usage`);
      box.replaceChildren(h('div.stack',
        h('div.grid.cols-3', { style: { gap: '8px' } },
          h('div.card.stat', h('span.k', 'Requests (30 d)'), h('span.v', num(r.usage?.requests))),
          h('div.card.stat', h('span.k', 'Created sandboxes'), h('span.v', num(r.resources?.sandboxes))),
          h('div.card.stat', h('span.k', 'Last used'), h('span.v', { style: { fontSize: '15px' } }, r.lastUsedAt ? ago(r.lastUsedAt) : 'never'))),
        table({ rows: r.createdResources || [], emptyEl: h('p.faint', 'This key has not created anything.'), cols: [
          { label: 'Resource', render: (x) => x.kind === 'sandbox' ? h('a', { href: `/sandboxes/${x.id}` }, x.name || x.id) : x.name || x.id },
          { label: 'State', render: (x) => stateChip(x.state) },
          { label: 'Created', render: (x) => time(x.createdAt) },
        ] })));
    } catch (e) { box.replaceChildren(errorBox(e)); }
  }

  await load();
}

function sameSet(a = [], b = []) { return a.length === b.length && a.every((x) => b.includes(x)); }
