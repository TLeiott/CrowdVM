// Organizations: members and invites, roles and caps, default visibility, transfer and delete.
// It manages the organization viewed in the sidebar; "Make active" sets the CLI and API keys' wallet.
import { h, btn, time, initials, num } from '../dom.js';
import { api, isMissing, errText } from '../api.js';
import { pageHead, card, table, loading, errorBox, modal, field, toast, toastErr, confirmDialog, empty, secretDialog, menuButton } from '../ui.js';

// Per-member limits on the org wallet: sandboxes at once and credits per month (over-cap sandboxes are stopped).
function caps(base, x, done) {
  const conc = h('input.input', { type: 'number', min: 0, step: 1, value: x.concurrentCap ?? '', placeholder: 'no cap' });
  const usage = h('input.input', { type: 'number', min: 0, step: 'any', value: x.usageCapCredits ?? '', placeholder: 'no cap' });
  modal({ title: `Caps for ${x.displayName || x.login}`, submit: true, desc: 'Limits on organization sandboxes this member starts. Over-cap sandboxes are stopped (and saved).',
    body: h('div.fields', field('Sandboxes at once', conc), field('Credits per month', usage)),
    actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Save', kind: 'primary', submit: true, onClick: async () => {
      const c = conc.value.trim(), u = usage.value.trim();
      await api.patch(`${base}/members/${x.id}`, { concurrentCap: c === '' ? null : parseInt(c, 10), ...(u === '' ? { clearUsageCap: true } : { usageCapCredits: Number(u) }) });
      toast('Caps saved'); done?.();
    } }] });
}

export default async function (ctx) {
  ctx.title('Organization');
  const root = h('div', loading());
  ctx.root.append(pageHead({
    title: 'Organization',
    lede: 'Share a credit wallet and sandboxes with a team. Snapshots and personal environments stay yours.',
    actions: [btn('Join with a code', { ico: 'link', onClick: joinCode }), btn('New organization', { ico: 'plus', onClick: create })],
  }), root);
  let sel = ctx.session.view?.type === 'org' ? ctx.session.view.id : null;

  async function load() {
    try {
      const orgsR = await api.get('/orgs');
      const orgs = orgsR.orgs || [];
      const teams = orgs.filter((o) => o.type === 'org');
      if (!teams.find((o) => o.id === sel)) sel = null;
      const parts = [];
      parts.push(card({ title: 'Your workspaces', sub: 'The CLI and API keys bill the active one. The dashboard bills the one you view in the sidebar.', flush: true, body: table({
        rows: orgs,
        cols: [
          { label: 'Name', render: (o) => h('div.row', h('span.avatar', { style: o.type === 'personal' ? { background: 'var(--surface-3)', color: 'var(--text-2)' } : null }, initials(o.name)), h('b', o.name), o.type === 'personal' ? h('span.faint', 'personal') : null) },
          { label: 'Role', cls: 'hide-xs', render: (o) => h('span.muted', o.role) },
          { label: 'Members', cls: 'num hide-xs', render: (o) => String(o.memberCount ?? 1) },
          { label: h('span.sr-only', 'Actions'), cls: 'act', render: (o) => h('div.row.end',
            o.active ? h('span.chip.accent', { title: 'The CLI and API keys bill it' }, 'active') : btn('Make active', { size: 'sm', title: 'Bill the CLI and API keys here', onClick: () => switchTo(o) }),
            o.type === 'org' && o.id !== sel ? btn('Manage', { size: 'sm', kind: 'ghost', title: 'View this organization in the dashboard', onClick: () => ctx.setView(o.id) }) : null) },
        ],
      }) }));
      const org = teams.find((o) => o.id === sel);
      if (org) parts.push(await manage(org));
      else if (teams.length) parts.push(h('div.card', empty({ ico: 'users', title: 'Viewing your personal account', text: 'Pick an organization in the sidebar, or select Manage above, to see its members and settings.' })));
      else parts.push(h('div.card', empty({ ico: 'users', title: 'No organization yet', text: 'Create one to share sandboxes, environments and a credit wallet with your team.', action: btn('New organization', { kind: 'primary', ico: 'plus', onClick: create }) })));
      root.replaceChildren(h('div.stack.loose', parts));
    } catch (e) { root.replaceChildren(errorBox(e, load)); }
  }

  async function manage(org) {
    const owner = org.role === 'owner' || org.role === 'admin', isOwner = org.role === 'owner';
    const base = `/orgs/${encodeURIComponent(org.id)}`;
    let m = null, mErr = null;
    try { m = await api.get(`${base}/members`); } catch (e) { mErr = e; }
    if (m && owner) { try { m.invites = (await api.get(`${base}/invites`)).invites; } catch { /* older servers list invites with members */ } }
    const invites = (m?.invites || []).filter((i) => !i.status || i.status === 'pending');
    const members = card({
      title: `Members of ${org.name}`, flush: true,
      actions: owner && !isMissing(mErr) ? btn('Invite', { size: 'sm', kind: 'primary', ico: 'plus', onClick: () => invite(base) }) : null,
      body: mErr ? h('div.card-body.faint', isMissing(mErr) ? 'Member management is not available on this server.' : errText(mErr)) : h('div',
        table({ rows: m.members || [], cols: [
          { label: 'Member', render: (x) => h('div.row', h('span.avatar', initials(x.displayName || x.login)), h('div.stack.tight', { style: { gap: 0 } }, h('b', x.displayName || x.login), h('span.faint.small', x.login))) },
          // Ownership moves only through Transfer ownership, so the owner's row is not a select.
          { label: 'Role', render: (x) => owner && x.id !== ctx.session.me?.id && x.role !== 'owner' ? h('select.select.sm', { style: { width: 'auto' }, 'aria-label': `Role of ${x.login}`, onchange: (e) => api.patch(`${base}/members/${x.id}`, { role: e.target.value }).then(() => toast('Role updated'), (err) => { toastErr(err); load(); }) },
            ['member', 'admin'].map((r) => h('option', { value: r, selected: r === x.role }, r))) : h('span.muted', x.role) },
          { label: 'Caps', cls: 'hide-xs', render: (x) => x.concurrentCap == null && x.usageCapCredits == null ? h('span.faint', 'none')
            : h('span.small', [x.concurrentCap != null ? `${x.concurrentCap} at once` : null, x.usageCapCredits != null ? `${num(x.usageCapCredits, 1)} credits/month` : null].filter(Boolean).join(' · ')) },
          { label: 'Joined', cls: 'hide-md', render: (x) => h('span.muted', time(x.joinedAt)) },
          { label: h('span.sr-only', 'Actions'), cls: 'act', render: (x) => owner ? menuButton(() => [
            { label: 'Caps…', ico: 'shield', onClick: () => caps(base, x, load) },
            x.id !== ctx.session.me?.id ? '-' : null,
            x.id !== ctx.session.me?.id ? { label: 'Remove…', ico: 'trash', danger: true, onClick: async () => {
              if (await confirmDialog({ title: `Remove ${x.login}?`, desc: 'Sandboxes they created go back to their personal account.', confirm: 'Remove', danger: true })) api.del(`${base}/members/${x.id}`).then(load, toastErr);
            } } : null,
          ], { title: `Actions for ${x.login}` }) : null },
        ] }),
        invites.length ? h('div', h('div.card-head', { style: { borderTop: '1px solid var(--border)' } }, h('h3', 'Pending invites')), table({ rows: invites, cols: [
          { label: 'Invited', render: (i) => h('div.stack.tight', { style: { gap: 0 } }, h('b', i.login || i.email || 'Anyone with the link'), i.role ? h('span.faint.small', i.role) : null) },
          { label: 'Sent', render: (i) => h('span.muted', time(i.createdAt)) },
          { label: h('span.sr-only', 'Actions'), cls: 'act', render: (i) => owner ? btn('Revoke', { size: 'sm', onClick: () => api.del(`${base}/invites/${i.id}`).then(load, toastErr) }) : null },
        ] })) : null),
    });

    const visibility = h('div.stack.tight', { role: 'radiogroup', 'aria-label': 'Default visibility of new sandboxes' },
      [['shared', 'Shared with members', 'Every member can open, run commands in and prompt them.'], ['private', 'Private to their creator', 'Members can see and stop them, nothing else.']].map(([v, t, d]) =>
        h('label.check', h('input', { type: 'radio', name: 'vis', value: v, checked: (org.newSandboxesPrivate ? 'private' : 'shared') === v,
          onchange: () => api.patch(base, { newSandboxesPrivate: v === 'private' }).then(() => toast('Default visibility saved'), toastErr) }), h('div', h('div.t', t), h('div.d', d)))));
    const settings = card({ title: 'Settings', body: h('div.stack.loose',
      h('div.field', h('span.label', 'New sandboxes are'), owner ? visibility : h('span.muted', org.newSandboxesPrivate ? 'private' : 'shared'),
        h('div.hint', 'A sandbox that holds its creator’s personal logins is always private.')),
      isOwner ? h('div.row.wrap', btn('Rename…', { size: 'sm', ico: 'edit', onClick: () => rename(base, org) }),
        btn('Transfer ownership…', { size: 'sm', ico: 'users', onClick: () => transfer(base, org, m?.members || []) })) : null,
      h('hr'),
      isOwner ? h('div.row.between.wrap', h('div', h('b', 'Delete organization'), h('p.small.faint', 'Stop its sandboxes first. They go back to their creators’ personal billing, and its keys are revoked.')),
        btn('Delete…', { size: 'sm', kind: 'danger', onClick: () => remove(base, org) }))
        : h('div.row.between.wrap', h('div', h('b', 'Leave organization'), h('p.small.faint', 'Your sandboxes in it go back to personal billing.')),
          btn('Leave', { size: 'sm', kind: 'danger', onClick: async () => {
            if (await confirmDialog({ title: `Leave ${org.name}?`, confirm: 'Leave', danger: true })) api.del(`${base}/members/${encodeURIComponent(ctx.session.me?.id || 'me')}`).then(() => { ctx.refreshShell(); load(); }, toastErr);
          } }))) });
    return h('div.grid.side', members, settings);
  }

  async function switchTo(o) {
    try { await api.patch('/orgs/active', { org: o.type === 'personal' ? null : o.id }); toast(`The CLI and API keys now bill ${o.name}`); await ctx.refreshShell(); load(); } catch (e) { toastErr(e); }
  }
  // Owner only: make another member the owner; you become a member.
  function transfer(base, org, members) {
    const others = members.filter((x) => x.id !== ctx.session.me?.id);
    if (!others.length) return toast('Invite someone first. The new owner must be a member.');
    const to = h('select.select', others.map((x) => h('option', { value: x.login }, `${x.displayName || x.login} (@${x.login}, ${x.role})`)));
    modal({ title: `Transfer ownership of ${org.name}`, submit: true, desc: 'The new owner gets billing, members, rename and delete. You become a member.',
      body: field('New owner', to),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Transfer…', kind: 'primary', submit: true, onClick: async () => {
        const x = others.find((y) => y.login === to.value);
        if (!(await confirmDialog({ title: `Make ${x.displayName || x.login} the owner?`, desc: `You become a member of ${org.name} and cannot undo this yourself.`, confirm: 'Transfer ownership', danger: true }))) return false;
        await api.post(`${base}/transfer`, { user: x.login });
        toast(`${x.displayName || x.login} now owns ${org.name}`); await ctx.refreshShell(); load();
      } }] });
  }
  async function remove(base, org) {
    if (!(await confirmDialog({ title: `Delete ${org.name}?`, danger: true, confirm: 'Delete organization', typeToConfirm: org.name,
      desc: 'Its sandboxes go back to their creators’ personal billing, its API keys are revoked and every member loses access. This cannot be undone.' }))) return;
    try { await api.del(base); toast(`Deleted ${org.name}`); sel = null; await ctx.refreshShell(); ctx.nav('/org'); } catch (e) { toastErr(e); }
  }
  function create() {
    const name = h('input.input', { placeholder: 'acme' });
    modal({ title: 'New organization', submit: true, desc: 'You become its owner. Then invite people with a link.', body: field('Name', name),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Create', kind: 'primary', submit: true, onClick: async () => {
        if (!name.value.trim()) throw new Error('Enter a name.');
        const r = await api.post('/orgs', { name: name.value.trim() });
        await ctx.refreshShell(); if (r.org?.id) ctx.setView(r.org.id); else load();
      } }] });
  }
  function invite(base) {
    const email = h('input.input', { type: 'email', placeholder: 'dana@example.com (optional)', autocomplete: 'off' });
    const role = h('select.select', ['member', 'admin'].map((r) => h('option', { value: r }, r)));
    modal({ title: 'Invite a member', submit: true, desc: 'You get a link to send them. With an email, the server also mails it when SMTP is set up.',
      body: h('div.fields', field('Email', email), field('Role', role)),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Create invite', kind: 'primary', submit: true, onClick: async () => {
        const r = await api.post(`${base}/invites`, { email: email.value.trim() || undefined, role: role.value });
        const link = r.url || (r.code ? `${location.origin}/invite?code=${r.code}` : null);
        if (link) secretDialog({ title: 'Invite created', desc: 'Anyone who opens this link while signed in joins the organization. It works once, for 14 days.', secret: link,
          extra: r.code ? h('p.small.muted', 'Or from a terminal: ', h('code', `crowdvm org join ${r.code}`)) : null });
        else toast('Invite sent');
        load();
      } }] });
  }
  function joinCode() {
    const code = h('input.input.mono', { placeholder: 'cvi_… or the invite link', autocomplete: 'off', spellcheck: 'false' });
    modal({ title: 'Join an organization', submit: true, body: field('Invite code', code),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Join', kind: 'primary', submit: true, onClick: async () => {
        const v = code.value.trim(); const c = v.includes('code=') ? new URL(v, location.href).searchParams.get('code') : v;
        if (!c) throw new Error('Paste the invite code or link.');
        const r = await api.post('/org-invites/accept', { code: c });
        toast(`Joined ${r.org?.name || 'the organization'}`); await ctx.refreshShell(); if (r.org?.id) ctx.setView(r.org.id); else load();
      } }] });
  }
  function rename(base, org) {
    const name = h('input.input', { value: org.name });
    modal({ title: 'Rename organization', submit: true, body: field('Name', name, 'Scripts that pass --org by name must use the new name. The id stays.'),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Rename', kind: 'primary', submit: true, onClick: async () => { await api.patch(base, { name: name.value.trim() }); await ctx.refreshShell(); load(); } }] });
  }

  await load();
}
