// File browser + editor over the guest files API.
import { h, icon, btn, bytes, ago } from '../dom.js';
import { api, errText } from '../api.js';
import { modal, toast, toastErr, confirmDialog, field, isLive, empty, loading } from '../ui.js';
import { snapshotBrowser } from './snapshots.js';

const HOME = '/home/user';
const MAX_EDIT = 2 * 1024 * 1024;

export default async function (ctx, root) {
  const id = ctx.id;
  if (!isLive(ctx.sb.state)) {
    // Stopped: browse the latest snapshot read-only (no need to resume).
    const snap = await api.get(`/sandboxes/${id}/snapshots/latest`).then((r) => r.snapshot, () => null);
    root.append(snap ? h('div.stack',
      h('div.notice', icon('info'), h('div', 'The sandbox is not running. These are the files of its last snapshot (', ago(snap.createdAt), '), read-only. Resume it to edit.')),
      h('div.card', h('div.card-body', snapshotBrowser(snap.id, { height: '60vh' }))))
      : h('div.card', empty({ ico: 'folder', title: 'The sandbox is not running', text: 'Resume it to browse and edit its files.' })));
    return;
  }
  let cwd = ctx.query.get('path') || HOME, entries = [], sel = null, dirty = false;
  const pathbar = h('nav.pathbar', { 'aria-label': 'Current folder' });
  const list = h('div.list', { role: 'listbox', 'aria-label': 'Files' });
  const viewer = h('div.viewer');
  const fileInput = h('input', { type: 'file', hidden: true, multiple: true });
  const hiddenToggle = h('input', { type: 'checkbox' });

  const toolbar = h('div.card-head.files-bar',
    h('div.row.grow', btn('', { ico: 'chevron', size: 'sm', kind: 'ghost', title: 'Up one folder', onClick: () => cd(parent(cwd)), attrs: { style: 'transform:rotate(180deg)' } }), pathbar),
    h('div.row',
      h('label.check.small.hide-sm', { style: { alignItems: 'center' } }, hiddenToggle, h('span', 'Hidden')),
      btn('', { ico: 'refresh', size: 'sm', title: 'Refresh', onClick: () => cd(cwd) }),
      btn('New', { ico: 'plus', size: 'sm', onClick: newItem }),
      btn('Upload', { ico: 'upload', size: 'sm', onClick: () => fileInput.click() })));
  hiddenToggle.addEventListener('change', drawList);
  root.append(h('div.card', toolbar, h('div.files', list, viewer)), fileInput);

  function parent(p) { const i = p.replace(/\/+$/, '').lastIndexOf('/'); return i <= 0 ? '/' : p.slice(0, i); }
  const join = (dir, name) => (dir.endsWith('/') ? dir : dir + '/') + name;

  function drawPath() {
    const parts = cwd.split('/').filter(Boolean);
    pathbar.replaceChildren(h('button', { type: 'button', onclick: () => cd('/') }, '/'),
      ...parts.flatMap((p, i) => [h('button', { type: 'button', onclick: () => cd('/' + parts.slice(0, i + 1).join('/')) }, p), i < parts.length - 1 ? h('span.faint', '/') : null]).filter(Boolean));
  }

  function drawList() {
    const show = entries.filter((e) => hiddenToggle.checked || !e.name.startsWith('.'))
      .sort((a, b) => (b.isDir - a.isDir) || a.name.localeCompare(b.name));
    if (!show.length) { list.replaceChildren(h('p.faint.small', { style: { padding: '12px' } }, 'Empty folder')); return; }
    list.replaceChildren(...show.map((e) => {
      const b = h('button.fitem', { type: 'button', role: 'option', class: e.isDir ? 'dir' : '', 'aria-selected': String(sel?.path === e.path), title: e.path },
        icon(e.isDir ? 'folder' : 'file', 'sm'), h('span.truncate', e.name, e.isSymlink ? h('span.faint', ' →') : null), e.isDir ? null : h('span.sz', bytes(e.size, 0)));
      b.addEventListener('click', () => (e.isDir ? cd(e.path) : open(e)));
      return b;
    }));
  }

  async function cd(path) {
    if (dirty && !(await confirmDialog({ title: 'Discard changes?', desc: 'The open file has unsaved edits.', confirm: 'Discard', danger: true }))) return;
    list.replaceChildren(loading(''));
    try {
      const r = await api.get(`/sandboxes/${id}/files/list`, { path });
      cwd = r.path || path; entries = r.files || []; sel = null; dirty = false;
      history.replaceState(null, '', `/sandboxes/${id}/files?path=${encodeURIComponent(cwd)}`);
      drawPath(); drawList(); viewer.replaceChildren(empty({ ico: 'file', title: 'No file open', text: `${entries.length} items in ${cwd}${r.truncated ? ' (truncated)' : ''}` }));
    } catch (e) { list.replaceChildren(h('div.notice.err', { style: { margin: '12px' } }, icon('alert'), h('div', errText(e)))); }
  }

  async function open(e) {
    if (dirty && !(await confirmDialog({ title: 'Discard changes?', desc: 'The open file has unsaved edits.', confirm: 'Discard', danger: true }))) return;
    sel = e; dirty = false; drawList();
    const head = h('div.card-head', { style: { borderTop: 0 } },
      h('div', { style: { minWidth: 0 } }, h('h3.truncate', e.name), h('div.faint.small', `${bytes(e.size)} · ${e.mode || ''} · modified ${ago(e.modifiedAt)}`)),
      h('div.row', btn('', { ico: 'download', size: 'sm', title: 'Download', onClick: () => download(e.path) }),
        btn('', { ico: 'edit', size: 'sm', title: 'Rename or move', onClick: () => move(e) }),
        btn('', { ico: 'trash', size: 'sm', kind: 'danger', title: 'Delete', onClick: () => remove(e) })));
    viewer.replaceChildren(head, loading(''));
    if (e.size > MAX_EDIT) { viewer.replaceChildren(head, empty({ ico: 'file', title: 'Too large to edit here', text: `${bytes(e.size)}. Download it instead.`, action: btn('Download', { ico: 'download', onClick: () => download(e.path) }) })); return; }
    try {
      const r = await api.get(`/sandboxes/${id}/files`, { path: e.path });
      if (r.encoding === 'base64') { viewer.replaceChildren(head, empty({ ico: 'file', title: 'Binary file', text: `${bytes(r.size)}. Download it to open it.`, action: btn('Download', { ico: 'download', onClick: () => download(e.path) }) })); return; }
      const ta = h('textarea.textarea', { spellcheck: 'false', 'aria-label': `Contents of ${e.name}`, value: r.content });
      const save = btn('Save', { kind: 'primary', size: 'sm', ico: 'check', disabled: true });
      const saveFn = async () => {
        try { await api.put(`/sandboxes/${id}/files`, { path: e.path, content: ta.value }); dirty = false; save.disabled = true; toast(`Saved ${e.name}`); }
        catch (err) { toastErr(err); }
      };
      save.addEventListener('click', saveFn);
      ta.addEventListener('input', () => { dirty = true; save.disabled = false; });
      ta.addEventListener('keydown', (ev) => { if ((ev.ctrlKey || ev.metaKey) && ev.key === 's') { ev.preventDefault(); saveFn(); } });
      head.lastChild.prepend(save);
      viewer.replaceChildren(head, ta);
    } catch (err) { viewer.replaceChildren(head, h('div.notice.err', { style: { margin: '12px' } }, icon('alert'), h('div', errText(err)))); }
  }

  async function download(path) {
    try {
      const r = await api.post(`/sandboxes/${id}/files/download-url`, { path, ttlSeconds: 300 });
      const a = h('a', { href: r.url, download: path.split('/').pop(), 'data-native': '' }); document.body.append(a); a.click(); a.remove();
    } catch (e) { toastErr(e); }
  }

  async function remove(e) {
    if (!(await confirmDialog({ title: `Delete ${e.name}?`, desc: e.path, confirm: 'Delete', danger: true }))) return;
    try { await api.del(`/sandboxes/${id}/files`, { path: e.path }); dirty = false; toast(`Deleted ${e.name}`); cd(cwd); } catch (err) { toastErr(err); }
  }

  function move(e) {
    const dest = h('input.input.mono', { value: e.path });
    modal({ title: 'Rename or move', submit: true, body: field('New path', dest),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Move', kind: 'primary', submit: true, onClick: async () => {
        await api.post(`/sandboxes/${id}/files/move`, { source: e.path, destination: dest.value.trim() }); dirty = false; cd(cwd);
      } }] });
  }

  function newItem() {
    const name = h('input.input.mono', { placeholder: 'notes.md or src/' });
    modal({ title: `New in ${cwd}`, submit: true, body: field('Name', name, 'End with / to make a folder.'),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Create', kind: 'primary', submit: true, onClick: async () => {
        const n = name.value.trim(); if (!n) throw new Error('Enter a name.');
        const path = n.startsWith('/') ? n : join(cwd, n);
        if (n.endsWith('/')) await api.post(`/sandboxes/${id}/files/folder`, { path: path.replace(/\/+$/, '') });
        else await api.put(`/sandboxes/${id}/files`, { path, content: '' });
        await cd(cwd);
        if (!n.endsWith('/')) { const e = entries.find((x) => x.path === path); if (e) open(e); }
      } }] });
  }

  fileInput.addEventListener('change', async () => {
    for (const f of fileInput.files) {
      try {
        const r = await api.post(`/sandboxes/${id}/files/upload-url`, { path: join(cwd, f.name), ttlSeconds: 600 });
        const res = await fetch(r.url, { method: r.method || 'PUT', body: f });
        if (!res.ok) throw new Error(`Upload of ${f.name} failed (HTTP ${res.status}).`);
        toast(`Uploaded ${f.name}`);
      } catch (e) { toastErr(e); }
    }
    fileInput.value = '';
    cd(cwd);
  });

  addEventListener('beforeunload', guard);
  ctx.signal.addEventListener('abort', () => removeEventListener('beforeunload', guard));
  function guard(e) { if (dirty) { e.preventDefault(); e.returnValue = ''; } }

  drawPath();
  await cd(cwd);
}
