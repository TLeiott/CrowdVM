// Saved snapshots (named templates) and their share codes.
import { h, btn, bytes, time, ago, every, patch } from '../dom.js';
import { api } from '../api.js';
import { pageHead, card, table, stateChip, menuButton, empty, loading, errorBox, modal, toast, toastErr, confirmDialog, secretDialog, copyable } from '../ui.js';
import { createSandboxDialog } from '../sbx.js';
import { browse } from '../tabs/snapshots.js';

export default async function (ctx) {
  ctx.title('Saved snapshots');
  const body = h('div', loading()), allowance = h('div');
  const sel = new Set();
  let names = [];
  const bulk = h('div.row', { hidden: true, style: { marginBottom: '12px' } });
  // Select-all and the "Delete selected" bar follow the checkboxes.
  const sync = () => {
    for (const n of [...sel]) if (!names.includes(n)) sel.delete(n);
    body.querySelectorAll('input[data-snap]').forEach((c) => { c.checked = sel.has(c.dataset.snap); });
    const all = body.querySelector('input[data-all]');
    if (all) { all.checked = names.length > 0 && sel.size === names.length; all.indeterminate = sel.size > 0 && sel.size < names.length; }
    bulk.hidden = !sel.size;
    bulk.replaceChildren(h('span.small.muted', `${sel.size} selected`), btn('Delete selected…', { size: 'sm', kind: 'danger', ico: 'trash', onClick: removeSelected }),
      btn('Clear', { size: 'sm', kind: 'ghost', onClick: () => { sel.clear(); sync(); } }));
  };
  ctx.root.append(
    pageHead({
      title: 'Saved snapshots',
      lede: 'Frozen sandbox disks under a name. Start any number of sandboxes from one, or hand a share code to another account.',
      actions: [btn('Use a share code', { ico: 'link', onClick: () => createSandboxDialog(ctx, { code: true }) })],
    }),
    allowance, bulk, body);

  async function load() {
    try {
      const r = await api.get('/saved-snapshots', undefined, { signal: ctx.signal });
      const a = r.allowance;
      patch(allowance, a || null, (x) => x && x.included ? h('p.small.faint', { style: { margin: '-8px 0 12px' } }, `${x.used} of ${x.included} included snapshots used${x.orgName ? ` in ${x.orgName}` : ''}.`) : null);
      names = (r.snapshots || []).map((s) => s.name);
      patch(body, r.snapshots || [], (rows) => h('div.card', table({
        rows,
        emptyEl: empty({ ico: 'camera', title: 'No saved snapshots', text: 'Open a sandbox and pick “Save as snapshot”, or run the CLI command below.',
          action: h('div', { style: { width: 'min(360px, 80vw)' } }, copyable('crowdvm snapshot save <sandbox-id> my-template')) }),
        cols: [
          { label: h('input', { type: 'checkbox', 'data-all': '', 'aria-label': 'Select all saved snapshots', onchange: (e) => { names.forEach((n) => (e.target.checked ? sel.add(n) : sel.delete(n))); sync(); } }), cls: 'act',
            render: (s) => h('input', { type: 'checkbox', 'data-snap': s.name, 'aria-label': `Select ${s.name}`, onchange: (e) => { e.target.checked ? sel.add(s.name) : sel.delete(s.name); sync(); } }) },
          { label: 'Name', render: (s) => h('div.stack.tight', { style: { gap: 0 } }, h('b.mono', s.name), s.error ? h('span.small', { style: { color: 'var(--err-text)' } }, s.error) : null) },
          { label: 'State', render: (s) => stateChip(s.status === 'ready' ? 'active' : s.status, s.status) },
          { label: 'From', cls: 'hide-xs', render: (s) => h('a.idlink', { href: `/sandboxes/${s.sourceSandboxId}` }, s.sourceSandboxId) },
          { label: 'Size', cls: 'hide-md', render: (s) => h('span', { style: { textTransform: 'capitalize' } }, s.type || '—') },
          { label: 'Disk', cls: 'num hide-md', render: (s) => bytes(s.sizeBytes) },
          { label: 'Saved', cls: 'hide-xs', render: (s) => h('span.muted', time(s.createdAt)) },
          { label: h('span.sr-only', 'Actions'), cls: 'act', render: (s) => h('div.row.end',
            btn('Deploy', { size: 'sm', ico: 'play', disabled: s.status !== 'ready', onClick: () => createSandboxDialog(ctx, { from: s.name, type: s.type }) }),
            menuButton([
              { label: 'Share codes…', ico: 'share', onClick: () => shares(s) },
              s.snapshotId ? { label: 'Browse files', ico: 'folder', onClick: () => browse({ id: s.snapshotId }) } : null,
              '-',
              { label: 'Delete…', ico: 'trash', danger: true, onClick: () => remove(s) },
            ], { title: `Actions for ${s.name}` })) },
        ],
      })));
      sync();
    } catch (e) { if (e.name !== 'AbortError') body.replaceChildren(errorBox(e, load)); }
  }

  async function removeSelected() {
    const list = [...sel];
    if (!(await confirmDialog({ title: `Delete ${list.length} saved snapshot${list.length > 1 ? 's' : ''}?`, danger: true, confirm: 'Delete',
      desc: 'Sandboxes already made from them keep working. Their share codes stop working.', body: h('p.small', list.map((n, i) => [i ? ', ' : '', h('code', n)])) }))) return;
    const res = await Promise.allSettled(list.map((n) => api.del(`/saved-snapshots/${encodeURIComponent(n)}`)));
    const failed = res.filter((x) => x.status === 'rejected');
    list.forEach((n, i) => res[i].status === 'fulfilled' && sel.delete(n));
    failed.length ? toastErr(failed[0].reason) : toast(`Deleted ${list.length} saved snapshot${list.length > 1 ? 's' : ''}`);
    load();
  }

  async function remove(s) {
    if (!(await confirmDialog({ title: `Delete ${s.name}?`, desc: 'Sandboxes already made from it keep working. Its share codes stop working.', confirm: 'Delete', danger: true }))) return;
    try { await api.del(`/saved-snapshots/${encodeURIComponent(s.name)}`); toast(`Deleted ${s.name}`); load(); } catch (e) { toastErr(e); }
  }

  function shares(s) {
    const list = h('div', loading());
    const path = `/saved-snapshots/${encodeURIComponent(s.name)}/shares`;
    const draw = async () => {
      try {
        const r = await api.get(path);
        list.replaceChildren(table({
          rows: r.shares || [],
          emptyEl: h('p.faint', 'No share codes yet.'),
          cols: [
            { label: 'Code', render: (x) => h('code', `${x.codePrefix}…`) },
            { label: 'State', render: (x) => stateChip(x.status) },
            { label: 'Used', cls: 'num', render: (x) => `${x.useCount}×` },
            { label: 'Last used', cls: 'hide-xs', render: (x) => h('span.muted', x.lastUsedAt ? ago(x.lastUsedAt) : 'never') },
            { label: h('span.sr-only', 'Actions'), cls: 'act', render: (x) => x.status === 'active' ? btn('Revoke', { size: 'sm', kind: 'danger', onClick: async () => {
              try { await api.del(`${path}/${x.id}`); toast('Code revoked'); draw(); } catch (e) { toastErr(e); }
            } }) : null },
          ],
        }));
      } catch (e) { list.replaceChildren(errorBox(e)); }
    };
    modal({
      title: `Share ${s.name}`, wide: true,
      desc: 'Anyone on this server holding a code can create a sandbox from this snapshot. It runs on their account; your per-sandbox env is not passed.',
      body: list,
      actions: [{ label: 'Close', onClick: () => true }, { label: 'New share code', kind: 'primary', ico: 'plus', onClick: async () => {
        const r = await api.post(path, {});
        secretDialog({ title: 'Share code created', desc: `Deploys ${s.name}.`, secret: r.shareCode, extra: h('p.small.muted', 'They run: ', h('code', `crowdvm create --from ${r.shareCode}`)) });
        draw(); return false;
      } }],
    });
    draw();
  }

  await load();
  every(5000, load, ctx.signal);
}
