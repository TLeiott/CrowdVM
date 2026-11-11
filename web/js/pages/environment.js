// One environment: variables, secret files, repositories, setup, passing rules, versions.
import { h, icon, btn, time, bytes, plural } from '../dom.js';
import { api } from '../api.js';
import { pageHead, card, table, loading, errorBox, modal, field, toast, toastErr, confirmDialog, menuButton, checkbox } from '../ui.js';
import { envVars } from './environments.js';

// Calls on an organization's environment carry ?org= (the server scopes environments by it).
export const scopedApi = (org) => {
  const q = org ? { org } : {};
  return {
    get: (p, query, o) => api.get(p, { ...query, ...q }, o),
    post: (p, b) => api.post(p, b, { query: q }), put: (p, b) => api.put(p, b, { query: q }),
    del: (p, query, o) => api.del(p, { ...query, ...q }, o),
  };
};

// "Who can use this environment / profile": an organization's access list (Boat's
// "Everyone in <org>" or "Only the people ticked below"). save(access) stores it.
export function accessCard({ what, orgName, access, members, canEdit = true, save }) {
  const acc = access || { restricted: false, members: [] };
  const name = (id) => { const m = (members || []).find((x) => x.id === id); return m ? m.displayName || m.login : id; };
  const edit = () => {
    const pick = (restricted) => h('label.check', h('input', { type: 'radio', name: 'access', value: restricted ? 'only' : 'all', checked: acc.restricted === restricted }),
      h('div', h('div.t', restricted ? 'Only the people ticked below' : `Everyone in ${orgName}`), h('div.d', restricted ? 'Owners and admins can always use it.' : 'Every member, now and later.')));
    const radios = h('div.stack.tight', { role: 'radiogroup', 'aria-label': `Who can use this ${what}` }, pick(false), pick(true));
    const list = h('div.checks', (members || []).filter((m) => !['owner', 'admin'].includes(m.role)).map((m) =>
      h('label.check', h('input', { type: 'checkbox', value: m.id, checked: acc.members.includes(m.id) }), h('div', h('div.t', m.displayName || m.login), h('div.d', `@${m.login} · ${m.role}`)))));
    const sync = () => { const only = radios.querySelector('input[value=only]').checked; list.querySelectorAll('input').forEach((i) => { i.disabled = !only; }); };
    radios.addEventListener('change', sync); sync();
    modal({ title: `Who can use this ${what}`, body: h('div.stack', radios, list.childElementCount ? list : h('p.small.faint', 'No other members yet.')),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Save', kind: 'primary', onClick: async () => {
        const restricted = radios.querySelector('input[value=only]').checked;
        await save({ restricted, members: restricted ? [...list.querySelectorAll('input:checked')].map((i) => i.value) : [] });
      } }] });
  };
  return card({ title: `Who can use this ${what}`, actions: canEdit ? btn('Edit', { size: 'sm', ico: 'edit', onClick: edit }) : null,
    body: acc.restricted ? h('div.stack.tight', h('span.small.muted', 'Only these members (and owners and admins):'),
      h('div.row.wrap', { style: { gap: '4px' } }, acc.members.length ? acc.members.map((m) => h('span.tag', name(m))) : h('span.faint', 'nobody else')))
      : h('p.small.muted', `Everyone in ${orgName}.`) });
}

export default async function (ctx) {
  const id = ctx.params.id, org = ctx.query.get('org') || '';
  const eapi = scopedApi(org);
  const root = h('div', loading());
  ctx.root.append(root);
  let env, profiles = null, members = null;

  async function load() {
    try {
      const r = await eapi.get(`/environments/${encodeURIComponent(id)}`).catch(async (e) => {
        if (e.status !== 404 && e.status !== 405) throw e;
        const l = await eapi.get('/environments'); // servers without GET /environments/{id}
        const x = (l.environments || []).find((v) => v.id === id || v.name === id);
        if (!x) throw e;
        return { environment: x };
      });
      env = r.environment || r;
      ctx.title(env.name);
      const org = env.owner?.kind === 'org' ? env.owner.id : null;
      [profiles, members] = await Promise.all([
        profiles ?? api.get('/credential-profiles').then((x) => x.profiles || [], () => []),
        members ?? (org ? api.get(`/orgs/${encodeURIComponent(org)}/members`).then((x) => x.members || [], () => []) : []),
      ]);
      draw();
    } catch (e) { root.replaceChildren(errorBox(e, load)); }
  }
  const base = () => `/environments/${encodeURIComponent(env.id)}`;
  const act = (p) => p.then(() => load(), (e) => { toastErr(e); load(); });

  function draw() {
    const vars = envVars(env.envContents);
    const latest = env.latestVersionNumber ?? env.versions?.length ?? 1;
    const behind = (env.versions || []).filter((v) => v.versionNumber < latest).reduce((s, v) => s + (v.sandboxCount || 0), 0);

    const varsTable = table({
      rows: vars.map((l) => { const i = l.indexOf('='); return { k: l.slice(0, i).trim(), v: l.slice(i + 1) }; }),
      emptyEl: h('div.card-body.faint', 'No variables.'),
      cols: [
        { label: 'Key', render: (r) => h('code', r.k) },
        { label: 'Value', render: (r) => { const s = h('code.faint', '••••••••'); const b = btn('', { size: 'sm', kind: 'ghost', ico: 'eye', title: `Reveal ${r.k}`, onClick: () => { s.textContent = s.textContent.startsWith('•') ? r.v : '••••••••'; } }); return h('div.row', s, b); } },
        { label: h('span.sr-only', 'Actions'), cls: 'act', render: (r) => h('div.row.end', btn('', { size: 'sm', kind: 'ghost', ico: 'edit', title: `Edit ${r.k}`, onClick: () => editVar(r.k, r.v) }),
          btn('', { size: 'sm', kind: 'ghost', ico: 'trash', title: `Remove ${r.k}`, onClick: async () => { if (await confirmDialog({ title: `Remove ${r.k}?`, confirm: 'Remove', danger: true })) act(eapi.del(`${base()}/vars/${encodeURIComponent(r.k)}`)); } })) },
      ],
    });

    const filesTable = table({
      rows: env.secretFiles || [],
      emptyEl: h('div.card-body.faint', 'No secret files.'),
      cols: [
        { label: 'Path', render: (f) => h('code', f.path) },
        { label: 'Size', cls: 'num', render: (f) => bytes(new Blob([f.contents || '']).size) },
        { label: h('span.sr-only', 'Actions'), cls: 'act', render: (f) => h('div.row.end', btn('', { size: 'sm', kind: 'ghost', ico: 'edit', title: `Edit ${f.path}`, onClick: () => editFile(f) }),
          btn('', { size: 'sm', kind: 'ghost', ico: 'trash', title: `Remove ${f.path}`, onClick: async () => { if (await confirmDialog({ title: `Remove ${f.path}?`, confirm: 'Remove', danger: true })) act(eapi.del(`${base()}/secret-files`, { path: f.path })); } })) },
      ],
    });

    const repos = env.selectedRepositories || [];
    const reposTable = table({
      rows: repos,
      emptyEl: h('div.card-body.faint', 'No repositories. They are cloned into the home folder on first boot.'),
      cols: [
        { label: 'Repository', render: (r) => h('span.row', icon('git', 'sm'), h('code.truncate', r.fullName || r.url), r.private ? icon('lock', 'sm') : null) },
        { label: 'Branch', cls: 'hide-xs', render: (r) => h('span.tag', r.baseBranch || 'main') },
        { label: 'Setup', cls: 'hide-xs', render: (r) => (r.setupScript ? h('span.row', { style: { gap: '6px' } }, h('code.small.truncate', { style: { display: 'block', maxWidth: '200px' } }, r.setupScript.split('\n')[0]), r.setupBlocking ? h('span.chip.warn.plain', { title: 'Agent prompts wait for this setup' }, 'blocking') : null) : h('span.faint', '—')) },
        { label: h('span.sr-only', 'Actions'), cls: 'act', render: (r) => h('div.row.end',
          btn('', { size: 'sm', kind: 'ghost', ico: 'edit', title: `Edit ${r.fullName || r.url}`, onClick: () => editRepo(r) }),
          btn('', { size: 'sm', kind: 'ghost', ico: 'trash', title: `Remove ${r.fullName || r.url}`, onClick: async () => {
            if (await confirmDialog({ title: 'Remove repository?', desc: r.fullName || r.url, confirm: 'Remove', danger: true })) act(eapi.del(`${base()}/repos/${encodeURIComponent(r.databaseId)}`));
          } })) },
      ],
    });

    const eff = env.effective || {};
    const flags = [
      ['safeForThirdParties', 'Safe for third parties', 'Pass nothing: no variables, files, repos or credentials. Overrides the rest. Use it when other people drive the sandboxes.'],
      ['passSecrets', 'Variables and secret files', ''],
      ['passGithub', 'Repositories and git credentials', 'Clones the repositories and runs their setup.'],
      ['passSandboxCredentials', 'CrowdVM credentials', 'A per-sandbox API key, so the crowdvm CLI works inside the sandbox.'],
      ['passAgentsCredentials', 'AI credentials', 'Agents get the keys of the credential profile below (or proxy placeholders).'],
      ['proxyAiCredentials', 'AI credentials through the proxy', 'Placeholders only; the server adds the real key. Also works when the environment is safe for third parties.'],
    ].map(([k, t, d]) => {
      const off = k !== 'safeForThirdParties' && env[k] && eff[k] === false;
      const c = checkbox(k, t, off ? h('span', d, d ? ' ' : '', h('span.chip.warn.plain', 'withheld')) : d, !!env[k]);
      c.querySelector('input').addEventListener('change', (e) => act(eapi.put(base(), { [k]: e.target.checked })));
      return c;
    });
    const profSel = h('select.select', { onchange: (e) => act(eapi.put(base(), { credentialProfileId: e.target.value || null }).then(() => toast('Credential profile saved'))) },
      h('option', { value: '' }, 'Default profile'),
      (profiles || []).map((p) => h('option', { value: p.id, selected: p.id === env.credentialProfileId }, `${p.name}${p.owner?.kind === 'org' ? ` · ${p.owner.name}` : ''}`)));

    const envOrg = env.owner?.kind === 'org' ? ctx.session.orgs.find((o) => o.id === env.owner.id) || { name: 'the organization', role: '' } : null;
    root.replaceChildren(
      pageHead({
        crumbs: [{ label: 'Environments', href: `/environments${org ? `?org=${encodeURIComponent(org)}` : ''}` }, { label: env.name }],
        title: h('span.row', env.name, env.isDefault ? h('span.chip.accent.plain', 'default') : null, h('span.chip.plain', `v${latest}`)),
        actions: [
          behind ? btn(`Upgrade ${plural(behind, 'sandbox', 'sandboxes')}`, { kind: 'primary', ico: 'refresh', onClick: upgrade }) : null,
          menuButton([
            !env.isDefault ? { label: 'Make default', ico: 'check', onClick: () => act(eapi.put(base(), { isDefault: true })) } : null,
            { label: 'Rename…', ico: 'edit', onClick: rename },
            { label: 'Edit as .env…', ico: 'file', onClick: bulk },
            '-',
            { label: 'Delete…', ico: 'trash', danger: true, disabled: env.isDefault, onClick: remove },
          ], { kind: '', size: '' }),
        ],
      }),
      behind ? h('div.notice', { style: { marginBottom: '16px' } }, icon('info'), h('div', `${plural(behind, 'sandbox is', 'sandboxes are')} on an older version. Upgrade pushes the latest config to running sandboxes now and to stopped ones on resume.`)) : null,
      h('div.grid.side',
        h('div.stack.loose',
          card({ title: 'Variables', sub: 'Exported in commands, SSH sessions and agents.', flush: true, actions: btn('Add', { size: 'sm', ico: 'plus', onClick: () => editVar() }), body: varsTable }),
          card({ title: 'Secret files', sub: 'Written on every boot, mode 0600. Paths are relative to /home/user.', flush: true, actions: btn('Add', { size: 'sm', ico: 'plus', onClick: () => editFile() }), body: filesTable }),
          card({ title: 'Repositories', sub: 'Cloned on first boot. Each one can have a setup script that runs in the repo.', flush: true, actions: btn('Add', { size: 'sm', ico: 'plus', onClick: () => editRepo() }), body: reposTable }),
          card({ title: 'Registry logins', sub: 'docker login on every boot (Docker Hub, ghcr.io, a LAN registry…).', flush: true, actions: btn('Add', { size: 'sm', ico: 'plus', onClick: () => editRegistry() }),
            body: table({ rows: env.registryLogins || [], emptyEl: h('div.card-body.faint', 'None.'), cols: [
              { label: 'Registry', render: (r) => h('code', r.registry) },
              { label: 'User', cls: 'hide-xs', render: (r) => h('span.muted', r.username) },
              { label: h('span.sr-only', 'Actions'), cls: 'act', render: (r) => btn('', { size: 'sm', kind: 'ghost', ico: 'trash', title: `Remove ${r.registry}`,
                onClick: () => act(eapi.put(base(), { registryLogins: env.registryLogins.filter((x) => x !== r) })) }) },
            ] }) })),
        h('div.stack.loose',
          card({ title: 'What sandboxes get', body: h('div.stack',
            env.warning ? h('div.notice.warn', icon('alert'), h('div', env.warning)) : null,
            flags,
            field('Credential profile', profSel, h('span', 'Which ', h('a', { href: '/ai' }, 'AI credentials'), ' agents get.'))) }),
          envOrg ? accessCard({ what: 'environment', orgName: envOrg.name, access: env.access, members, canEdit: ['owner', 'admin'].includes(envOrg.role),
            save: async (access) => { await eapi.put(base(), { access }); toast('Access saved'); load(); } }) : null,
          card({ title: 'Versions', flush: true, body: (env.versions || []).length ? table({
            rows: [...env.versions].reverse(),
            cols: [
              { label: 'Version', render: (v) => h('b', `v${v.versionNumber}`) },
              { label: 'Sandboxes', cls: 'num', render: (v) => String(v.sandboxCount || 0) },
              { label: 'Created', render: (v) => h('span.muted', time(v.createdAt)) },
            ],
          }) : h('div.card-body.faint', 'No versions yet.') }))));
  }

  function editVar(k = '', v = '') {
    const key = h('input.input.mono', { value: k, placeholder: 'DATABASE_URL', disabled: !!k });
    const val = h('textarea.textarea.mono', { rows: 2, value: v });
    modal({ title: k ? `Edit ${k}` : 'Add variable', submit: true, body: h('div.stack', field('Key', key), field('Value', val)),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Save', kind: 'primary', submit: true, onClick: async () => {
        if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key.value)) throw new Error('Keys use letters, digits and _, and do not start with a digit.');
        await eapi.put(`${base()}/vars/${encodeURIComponent(key.value)}`, { value: val.value }); load();
      } }] });
  }
  function editFile(f) {
    const path = h('input.input.mono', { value: f?.path || '', placeholder: '.config/service-account.json', disabled: !!f });
    const contents = h('textarea.textarea.mono', { rows: 8, value: f?.contents || '' });
    modal({ title: f ? `Edit ${f.path}` : 'Add secret file', wide: true, body: h('div.stack', field('Path', path), field('Contents', contents)),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Save', kind: 'primary', onClick: async () => {
        if (!path.value.trim()) throw new Error('Enter a path.');
        await eapi.put(`${base()}/secret-files`, { path: path.value.trim(), contents: contents.value }); load();
      } }] });
  }
  // Repositories are saved as the full list (PUT repositories[]), which is the only way to set per-repo setup.
  const repoReq = (r) => ({ repositoryId: r.databaseId, baseBranch: r.baseBranch, setupScript: r.setupScript || '', setupBlocking: !!r.setupBlocking });
  function editRepo(r) {
    const url = h('input.input.mono', { placeholder: 'https://github.com/acme/web.git or acme/web', disabled: !!r, value: r ? r.fullName || r.url : '' });
    const branch = h('input.input.mono', { placeholder: 'main', value: r?.baseBranch || '' });
    const setup = h('textarea.textarea.mono', { rows: 6, value: r?.setupScript || '', placeholder: 'npm ci\nnpm run build' });
    const blocking = checkbox('blocking', 'Blocking', 'Agent prompts wait until this setup has finished.', !!r?.setupBlocking);
    modal({ title: r ? `Edit ${r.fullName || r.url}` : 'Add repository', wide: true, submit: true,
      body: h('div.fields',
        field('Repository', url, r ? null : 'A clone URL, or owner/name of a repo your git host token can see. Private repos need a git host on the Environments page.', { full: true }),
        field('Branch', branch), h('div'),
        field('Setup script', setup, 'Bash, in the repo folder, after it is cloned and on every boot.', { full: true }),
        h('div.field.full', blocking)),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: r ? 'Save' : 'Add repository', kind: 'primary', submit: true, onClick: async () => {
        const id = r ? r.databaseId : url.value.trim();
        if (!id) throw new Error('Enter a repository.');
        const next = { repositoryId: id, baseBranch: branch.value.trim() || undefined, setupScript: setup.value, setupBlocking: blocking.querySelector('input').checked };
        const list = (env.selectedRepositories || []).map(repoReq);
        const i = r ? list.findIndex((x) => x.repositoryId === r.databaseId) : -1;
        if (i >= 0) list[i] = next; else list.push(next);
        await eapi.put(base(), { repositories: list });
        toast(r ? 'Repository saved' : 'Repository added'); load();
      } }] });
  }
  function editRegistry() {
    const reg = h('input.input.mono', { placeholder: 'ghcr.io' }), user = h('input.input.mono', { autocomplete: 'off' }), pw = h('input.input.mono', { type: 'password', autocomplete: 'off' });
    modal({ title: 'Add registry login', submit: true, body: h('div.stack', field('Registry', reg, 'Host name; docker.io for Docker Hub.'), field('Username', user), field('Password or token', pw)),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Add', kind: 'primary', submit: true, onClick: async () => {
        if (!reg.value.trim() || !user.value.trim() || !pw.value) throw new Error('Fill in all three fields.');
        await eapi.put(base(), { registryLogins: [...(env.registryLogins || []), { registry: reg.value.trim(), username: user.value.trim(), password: pw.value }] }); load();
      } }] });
  }
  function bulk() {
    const ta = h('textarea.textarea.mono', { rows: 14, value: env.envContents || '' });
    modal({ title: 'Edit as .env', wide: true, desc: 'Replaces every variable. One KEY=value per line; # starts a comment.', body: ta,
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Save', kind: 'primary', onClick: async () => { await eapi.put(base(), { envContents: ta.value }); load(); } }] });
  }
  function rename() {
    const name = h('input.input.mono', { value: env.name });
    modal({ title: 'Rename environment', submit: true, body: field('Name', name, 'Sandboxes that name it in --env must use the new name.'),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Rename', kind: 'primary', submit: true, onClick: async () => { await eapi.put(base(), { name: name.value.trim() }); load(); } }] });
  }
  async function upgrade() {
    try { const r = await eapi.post(`${base()}/upgrade`, {}); toast(`Upgraded ${r.upgraded ?? 0} sandboxes${r.failed ? `, ${r.failed} failed` : ''}`); load(); } catch (e) { toastErr(e); }
  }
  async function remove() {
    if (!(await confirmDialog({ title: `Delete ${env.name}?`, desc: 'Running sandboxes keep their copy. New sandboxes cannot use it.', confirm: 'Delete', danger: true, typeToConfirm: env.name }))) return;
    try { await eapi.del(base()); toast(`Deleted ${env.name}`); ctx.nav(`/environments${org ? `?org=${encodeURIComponent(org)}` : ''}`); } catch (e) { toastErr(e); }
  }

  await load();
}
