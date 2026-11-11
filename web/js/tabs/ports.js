// Public URLs: expose a port of the sandbox through the server's reverse proxy.
import { h, icon, btn, copy } from '../dom.js';
import { api, errText, isMissing } from '../api.js';
import { card, table, field, toast, toastErr, confirmDialog, copyBtn, empty, isLive, checkbox, menuButton, modal } from '../ui.js';

export default async function (ctx, root) {
  const id = ctx.id;
  const port = h('input.input.mono', { type: 'number', min: 1, max: 65535, placeholder: '3000', required: true });
  const title = h('input.input', { placeholder: 'Optional, e.g. Storybook' });
  const pub = checkbox('public', 'Public', 'Anyone with the URL can open it. Otherwise the URL carries an access token.');
  const local = checkbox('localhost', 'Service listens on localhost only', 'Set for servers bound to 127.0.0.1 rather than 0.0.0.0.');
  const listEl = h('div');
  let rows = [], listSupported = true;
  const submit = btn('Expose port', { kind: 'primary', ico: 'globe', type: 'submit' });
  const form = h('form.stack', { novalidate: true },
    h('div.fields', field('Port', port), field('Title', title)),
    h('div.checks', pub, local),
    h('div.row.end', submit));
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const p = Number(port.value);
    if (!(p >= 1 && p <= 65535)) { port.setAttribute('aria-invalid', 'true'); port.focus(); return; }
    submit.disabled = true;
    try {
      const r = await api.post(`/sandboxes/${id}/host`, { port: p, public: pub.querySelector('input').checked, localhost: local.querySelector('input').checked, title: title.value.trim() || undefined });
      toast(`Port ${p} is live`);
      rows = [{ ...r, port: p, title: title.value.trim() }, ...rows.filter((x) => x.port !== p)];
      port.value = ''; title.value = '';
      await load();
    } catch (err) { toastErr(err); } finally { submit.disabled = false; }
  });

  const live = isLive(ctx.sb.state);
  root.append(h('div.grid.side',
    card({ title: 'Exposed ports', flush: true, body: listEl }),
    h('div.stack',
      card({ title: 'Expose a port', body: live ? form : h('p.faint', 'Start the sandbox to expose ports.') }),
      card({ title: 'URL shapes', body: h('div.stack.tight.small.muted', { style: { overflowWrap: 'anywhere' } },
        h('p', 'Host routing: ', h('code', `${id}-<port>.<public domain>`)),
        h('p', 'Path routing: ', h('code', `${location.origin}/p/${id}/<port>/`)),
        h('p', 'Or tunnel it to your machine: ', h('code', `crowdvm tunnel ${id} <port>`))) }))));

  // Re-POST a hosted port with changes (access, firewall, un-hide). Needs a running sandbox.
  const rehost = (r, extra) => api.post(`/sandboxes/${id}/host`, { port: r.port, public: r.access === 'public', localhost: !!r.localhost, title: r.title || undefined, ...extra });
  const change = (p, msg) => p.then(() => { toast(msg); load(); }, toastErr);
  function draw() {
    listEl.replaceChildren(table({
      rows,
      emptyEl: empty({ ico: 'globe', title: 'No public URLs', text: 'Expose a port to reach a dev server, preview or API from any browser on your network.' }),
      cols: [
        { label: 'Port', render: (r) => h('div.stack.tight', { style: { gap: 0 } }, h('b.mono', String(r.port)), r.title ? h('span.faint.small', r.title) : null) },
        { label: 'URL', render: (r) => r.hidden ? h('span.faint', 'Hidden') : r.url ? h('div.row', h('a.truncate', { href: r.url, target: '_blank', rel: 'noopener', style: { maxWidth: '340px' } }, r.url.replace(/([?&]_token=)[^&]+/, '$1…')), copyBtn(r.url, 'Copy URL')) : h('span.faint', r.message || r.error || '—') },
        { label: 'Access', cls: 'hide-xs', render: (r) => h('div.row.wrap', { style: { gap: '4px' } },
          r.hidden ? h('span.chip.plain', 'hidden') : h('span.chip.plain', { class: r.access === 'public' ? 'warn' : '' }, r.access === 'public' ? 'public' : 'token'),
          fwCount(r) ? h('span.chip.plain', { title: fwText(r) }, icon('shield', 'sm'), `${fwCount(r)} rule${fwCount(r) > 1 ? 's' : ''}`) : null) },
        { label: h('span.sr-only', 'Actions'), cls: 'act', render: (r) => listSupported ? menuButton(() => [
          r.hidden ? { label: 'Show again', ico: 'eye', disabled: !live, onClick: () => change(rehost(r), `Port ${r.port} is live again`) } : null,
          !r.hidden ? { label: r.access === 'public' ? 'Require the access token' : 'Make public', ico: r.access === 'public' ? 'lock' : 'globe', disabled: !live,
            onClick: () => change(rehost(r, { public: r.access !== 'public' }), r.access === 'public' ? 'The URL now needs its token' : 'Anyone with the URL can open it') } : null,
          { label: 'Firewall…', ico: 'shield', disabled: !live, onClick: () => firewall(r) },
          r.pathUrl && r.pathUrl !== r.url ? { label: 'Copy path URL', ico: 'copy', onClick: () => copy(r.pathUrl).then(() => toast('Copied')) } : null,
          '-',
          !r.hidden ? { label: 'Hide…', ico: 'trash', danger: true, onClick: async () => {
            if (!(await confirmDialog({ title: `Hide port ${r.port}?`, desc: 'The URL stops working. Expose the port again to bring it back with the same settings.', confirm: 'Hide', danger: true }))) return;
            change(api.del(`/sandboxes/${id}/host/${r.port}`), `Port ${r.port} hidden`);
          } } : null,
        ], { title: `Actions for port ${r.port}` }) : null },
      ],
    }));
  }
  const fwCount = (r) => (r.firewall?.allow?.length || 0) + (r.firewall?.deny?.length || 0);
  const fwText = (r) => [...(r.firewall?.allow || []).map((c) => `allow ${c}`), ...(r.firewall?.deny || []).map((c) => `deny ${c}`)].join(', ');
  function firewall(r) {
    const allow = h('textarea.textarea.mono', { rows: 3, value: (r.firewall?.allow || []).join('\n'), placeholder: '192.168.1.0/24\n100.64.0.0/10' });
    const deny = h('textarea.textarea.mono', { rows: 3, value: (r.firewall?.deny || []).join('\n'), placeholder: '192.168.1.50/32' });
    const lines = (t) => t.value.split(/[\s,]+/).map((x) => x.trim()).filter(Boolean);
    modal({ title: `Firewall for port ${r.port}`, desc: 'Which client addresses may open the URL. Deny rules always win; with any allow rule, every other address is blocked.',
      body: h('div.stack', field('Allow (CIDR, one per line)', allow), field('Deny (CIDR, one per line)', deny)),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Save', kind: 'primary', onClick: async () => {
        await rehost(r, { firewall: { allow: lines(allow), deny: lines(deny) } }); toast('Firewall saved'); load();
      } }] });
  }
  async function load() {
    try { const r = await api.get(`/sandboxes/${id}/host`, { all: 1 }); rows = r.ports || r.hostedPorts || []; }
    catch (e) { if (isMissing(e)) listSupported = false; else listEl.replaceChildren(h('div.card-body', h('span.faint', errText(e)))); }
    draw();
  }
  await load();
}
