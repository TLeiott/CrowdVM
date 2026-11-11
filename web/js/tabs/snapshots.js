// Snapshots of one sandbox (the disk chain), a read-only file browser for any snapshot
// (also used by the Files tab while the sandbox is stopped), save as named snapshot.
import { h, icon, btn, bytes, ago, num } from '../dom.js';
import { api, errText } from '../api.js';
import { card, table, modal, empty, loading, idText, menuButton, confirmDialog, toast, toastErr } from '../ui.js';
import { saveSnapshotDialog } from '../sbx.js';

export default async function (ctx, root) {
  const id = ctx.id;
  const body = h('div', loading());
  root.append(h('div.stack.loose',
    h('div.notice', icon('info'), h('div', 'Each stop saves the disk as a new snapshot in a chain; running sandboxes also take periodic live snapshots. ',
      h('b', 'Save as snapshot'), ' freezes the current state under a name you can start new sandboxes from.')),
    card({ title: 'Snapshot chain', flush: true, actions: h('div.row', { style: { gap: '6px' } },
      btn('Delete all…', { size: 'sm', ico: 'trash', onClick: () => removeAll() }),
      btn('Save as snapshot', { size: 'sm', kind: 'primary', ico: 'camera', onClick: () => saveSnapshotDialog(ctx.sb) })), body })));
  async function load() {
    try {
      const r = await api.get(`/sandboxes/${id}/snapshots`, { limit: 100 });
      const rows = r.snapshots || [];
      body.replaceChildren(table({
        rows,
        emptyEl: empty({ ico: 'camera', title: 'No snapshots yet', text: 'The first one is taken when the sandbox stops, or at the next periodic save.' }),
        cols: [
          { label: 'Snapshot', render: (s) => idText(s.id) },
          { label: 'Kind', cls: 'hide-xs', render: (s) => h('span.tag', s.kind || 'snapshot', s.generation != null ? ` #${s.generation}` : '') },
          { label: 'Taken', render: (s) => h('span.muted', { title: new Date(s.createdAt).toLocaleString() }, ago(s.createdAt)) },
          { label: 'Added', cls: 'num hide-xs', render: (s) => bytes(s.sizeBytes) },
          { label: 'Files', cls: 'num hide-md', render: (s) => num(s.contentFileCount ?? s.fileCount) },
          { label: h('span.sr-only', 'Actions'), cls: 'act', render: (s) => h('div.row.end',
            btn('Browse', { size: 'sm', ico: 'folder', onClick: () => browse(s) }),
            menuButton([
              { label: 'Download files (.tar)', ico: 'download', href: filesUrl(s.id, '') },
              { label: 'Disk image layers…', ico: 'disk', onClick: () => layers(s) },
              '-',
              { label: 'Delete snapshot…', ico: 'trash', danger: true, onClick: () => remove(s) },
            ], { title: `Actions for ${s.id}` })) },
        ],
      }));
    } catch (e) { body.replaceChildren(h('div.card-body', h('div.notice.err', icon('alert'), h('div', errText(e))))); }
  }
  async function remove(s) {
    if (!(await confirmDialog({ title: 'Delete this snapshot?', desc: 'Later snapshots in the chain that build on it keep working; its own state is gone.', confirm: 'Delete', danger: true, typeToConfirm: s.id }))) return;
    try { await api.del(`/snapshots/${s.id}`, undefined, { headers: { 'X-CrowdVM-Confirm-Delete': s.id } }); toast('Snapshot deleted'); load(); } catch (e) { toastErr(e); }
  }
  async function removeAll() {
    if (!(await confirmDialog({ title: 'Delete all snapshots of this sandbox?', desc: 'Every automatic snapshot of the chain is deleted. Named (saved) snapshots and shares keep the state they need.', confirm: 'Delete all', danger: true, typeToConfirm: id }))) return;
    try {
      const r = await api.del(`/sandboxes/${id}/snapshots`, undefined, { headers: { 'X-CrowdVM-Confirm-Delete': id } });
      toast(`Deleting ${(r.operations || []).length} snapshot(s)`); load();
    } catch (e) { toastErr(e); }
  }
  await load();
}

const filesUrl = (sid, path) => `/api/v1/snapshots/${encodeURIComponent(sid)}/files?path=${encodeURIComponent(path)}`;

async function layers(s) {
  const box = h('div', loading());
  modal({ title: 'Disk image layers', desc: 'qcow2 files of the chain, newest last. Links work for an hour without signing in.', wide: true, body: box, actions: [{ label: 'Close', onClick: () => true }] });
  try {
    const r = await api.get(`/snapshots/${s.id}/download`);
    const rows = [...(r.baseImage ? [{ ...r.baseImage, label: 'base image' }] : []), ...(r.chunks || [])];
    box.replaceChildren(table({ rows, emptyEl: h('p.faint', 'No layers.'), cols: [
      { label: 'Layer', render: (c) => h('code.small', c.label || c.snapshotId || c.id) },
      { label: 'Size', cls: 'num', render: (c) => bytes(c.sizeBytes) },
      { label: h('span.sr-only', 'Download'), cls: 'act', render: (c) => c.signedUrl ? h('a.btn.sm', { href: c.signedUrl, download: '' }, icon('download'), 'Download') : null },
    ] }));
  } catch (e) { box.replaceChildren(h('div.notice.err', icon('alert'), h('div', errText(e)))); }
}

export function browse(s) {
  modal({ title: 'Snapshot files', desc: h('code', s.id), wide: true, body: snapshotBrowser(s.id), actions: [{ label: 'Close', onClick: () => true }] });
}

// Read-only browser over GET /snapshots/{id}/tree (paths relative to /home/user).
// Files and folders download through /snapshots/{id}/files (folders as .tar).
export function snapshotBrowser(sid, { height = '55vh' } = {}) {
  const el = h('div.stack.tight', loading('Reading the snapshot… (the first time boots an inspector machine)'));
  let entries = [], cwd = '';
  const pathbar = h('nav.pathbar', { 'aria-label': 'Current folder' });
  const list = h('div', { role: 'list', style: { maxHeight: height, overflow: 'auto', border: '1px solid var(--border)', borderRadius: '6px' } });
  const meta = h('span.small.faint.grow');
  function cd(p) {
    cwd = p;
    const parts = p ? p.split('/') : [];
    pathbar.replaceChildren(h('button', { type: 'button', onclick: () => cd('') }, '~'),
      ...parts.flatMap((n, i) => ['/', h('button', { type: 'button', onclick: () => cd(parts.slice(0, i + 1).join('/')) }, n)]));
    const pre = p ? `${p}/` : '';
    const kids = entries.filter((e) => e.path.startsWith(pre) && !e.path.slice(pre.length).includes('/'))
      .sort((a, b) => (a.kind === 'dir' ? 0 : 1) - (b.kind === 'dir' ? 0 : 1) || a.path.localeCompare(b.path));
    list.replaceChildren(
      p ? h('div.fitem.dir', { role: 'listitem' }, h('button.fitem.dir', { type: 'button', style: { padding: 0 }, onclick: () => cd(parts.slice(0, -1).join('/')) }, icon('folder', 'sm'), '..')) : null,
      ...kids.map((e) => {
        const name = e.path.slice(pre.length);
        return h('div.fitem', { role: 'listitem', class: e.kind === 'dir' ? 'dir' : '' },
          e.kind === 'dir' ? h('button.fitem.dir.truncate', { type: 'button', style: { padding: 0, flex: 1 }, onclick: () => cd(e.path) }, icon('folder', 'sm'), name)
            : h('span.row.truncate', { style: { flex: 1, gap: '8px' } }, icon('file', 'sm'), h('span.truncate', name)),
          e.kind === 'file' && e.size != null ? h('span.sz', bytes(e.size, 0)) : null,
          h('a.btn.ghost.sm.icon', { href: filesUrl(sid, e.path), download: '', title: `Download ${name}${e.kind === 'dir' ? ' (.tar)' : ''}`, 'aria-label': `Download ${name}` }, icon('download')));
      }));
    if (!kids.length) list.append(h('p.faint.small', { style: { padding: '10px 12px' } }, 'Empty folder.'));
  }
  (async () => {
    try {
      const r = await api.get(`/snapshots/${encodeURIComponent(sid)}/tree`);
      if (r.treeAvailable === false) { el.replaceChildren(h('p.muted', `File list unavailable${r.reason ? ` (${r.reason})` : ''}.`)); return; }
      entries = r.entries || [];
      meta.textContent = `${num(r.fileCount)} files · ${bytes(r.totalSizeBytes)}${r.truncated ? ' · list truncated' : ''}`;
      el.replaceChildren(h('div.row.wrap', pathbar, meta,
        h('a.btn.sm', { href: filesUrl(sid, ''), download: '' }, icon('download'), 'Download all (.tar)')), list);
      cd('');
    } catch (e) { el.replaceChildren(h('div.notice.err', icon('alert'), h('div', errText(e)))); }
  })();
  return el;
}
