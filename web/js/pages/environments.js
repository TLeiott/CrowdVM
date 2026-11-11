// Environments list.
import { h, icon, btn, time, plural } from '../dom.js';
import { api, isMissing } from '../api.js';
import { pageHead, table, empty, loading, errorBox, modal, field, toast, toastErr, card, confirmDialog } from '../ui.js';
import { scopedApi } from './environment.js';

export const envVars = (txt = '') => txt.split('\n').map((l) => l.trim()).filter((l) => l && !l.startsWith('#') && l.includes('='));

export default async function (ctx) {
  ctx.title('Environments');
  const body = h('div', loading());
  // The viewed organization's environments (listed with ?org=), or your personal ones.
  const view = ctx.session.view;
  const org = view?.type === 'org' ? view.id : '';
  const eapi = scopedApi(org);
  const qs = () => (org ? `?org=${encodeURIComponent(org)}` : '');
  ctx.root.append(pageHead({
    title: 'Environments',
    lede: `${org ? `${view.name}’s environments. ` : ''}What a sandbox gets when it starts: variables, secret files, repositories and a setup script. Editing one makes a new version; running sandboxes keep theirs until you upgrade them.`,
    actions: btn('New environment', { kind: 'primary', ico: 'plus', onClick: create, attrs: { 'data-create': '' } }),
  }), body);

  function create() {
    const name = h('input.input.mono', { placeholder: 'customer-demos', maxlength: 64, autocomplete: 'off' });
    modal({ title: 'New environment', submit: true, body: field('Name', name, 'Letters, digits, dot, dash or underscore.'),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Create', kind: 'primary', submit: true, onClick: async () => {
        if (!/^[a-zA-Z0-9._-]{1,64}$/.test(name.value)) throw new Error('Use letters, digits, dot, dash or underscore (max 64).');
        const r = await eapi.post('/environments', { name: name.value });
        toast(`Created ${name.value}`);
        const e = r.environment || (r.environments || []).find((x) => x.name === name.value);
        if (e?.id) ctx.nav(`/environments/${e.id}${qs()}`); else load();
      } }] });
  }

  const hosts = h('div');
  async function loadHosts() {
    try {
      const r = await api.get('/repos/sources', undefined, { signal: ctx.signal });
      hosts.replaceChildren(card({ title: 'Git hosts', sub: 'Access tokens for private repositories on GitHub, GitLab, Gitea/Forgejo or any git server. Repositories on these hosts are cloned with the token.', flush: true,
        actions: btn('Add', { size: 'sm', ico: 'plus', onClick: addHost }),
        body: table({ rows: r.sources || [], emptyEl: h('div.card-body.faint', 'None. Public repositories work without one.'), cols: [
          { label: 'Host', render: (g) => h('div.stack.tight', { style: { gap: 0 } }, h('span.row', icon('git', 'sm'), h('b', g.name)), h('code.faint.small.truncate', g.baseUrl)) },
          { label: 'Kind', cls: 'hide-xs', render: (g) => h('span.tag', g.kind) },
          { label: 'Token', cls: 'hide-xs', render: (g) => h('code.small.faint', g.token || '••••') },
          { label: 'Added', cls: 'hide-md', render: (g) => h('span.muted', time(g.createdAt)) },
          { label: h('span.sr-only', 'Actions'), cls: 'act', render: (g) => btn('', { size: 'sm', kind: 'ghost', ico: 'trash', title: `Remove ${g.name}`, onClick: async () => {
            if (await confirmDialog({ title: `Remove ${g.name}?`, desc: 'New clones of its private repositories fail until you add a token again.', confirm: 'Remove', danger: true }))
              api.del(`/repos/sources/${encodeURIComponent(g.id)}`).then(() => { toast('Git host removed'); loadHosts(); }, toastErr);
          } }) },
        ] }) }));
    } catch (e) { if (e.name !== 'AbortError') hosts.replaceChildren(isMissing(e) ? '' : errorBox(e, loadHosts)); }
  }
  function addHost() {
    const kind = h('select.select', [['github', 'GitHub'], ['gitlab', 'GitLab'], ['gitea', 'Gitea / Forgejo'], ['git', 'Other (git over HTTPS)']].map(([v, l]) => h('option', { value: v }, l)));
    const baseUrl = h('input.input.mono', { type: 'url', placeholder: 'https://github.com' });
    const name = h('input.input', { placeholder: 'Optional' });
    const token = h('input.input.mono', { type: 'password', autocomplete: 'off', spellcheck: 'false' });
    const ph = { github: 'https://github.com', gitlab: 'https://gitlab.com', gitea: 'https://git.lan', git: 'https://git.lan' };
    kind.addEventListener('change', () => { baseUrl.placeholder = ph[kind.value]; });
    modal({ title: 'Add git host', submit: true, desc: 'The token is stored encrypted and only used to clone and list repositories.',
      body: h('div.fields', field('Kind', kind), field('Name', name), field('URL', baseUrl, 'Leave empty for github.com or gitlab.com.', { full: true }), field('Access token', token, 'A personal access token with read access to the repositories.', { full: true })),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Add', kind: 'primary', submit: true, onClick: async () => {
        if (!token.value.trim()) throw new Error('Paste an access token.');
        if (!baseUrl.value.trim() && (kind.value === 'gitea' || kind.value === 'git')) throw new Error('Enter the URL of the git host.');
        await api.post('/repos/sources', { kind: kind.value, name: name.value.trim() || undefined, baseUrl: baseUrl.value.trim() || undefined, token: token.value.trim() });
        toast('Git host added'); loadHosts();
      } }] });
  }

  async function load() {
    try {
      const r = await eapi.get('/environments', undefined, { signal: ctx.signal });
      const rows = r.environments || [];
      body.replaceChildren(h('div.card', table({
        rows,
        onRow: (e) => ctx.nav(`/environments/${e.id}${qs()}`),
        emptyEl: empty({ ico: 'layers', title: 'No environments', text: 'Create one to pass variables, secrets and repos to new sandboxes.' }),
        cols: [
          { label: 'Name', render: (e) => h('div.row', h('a.primary-cell', { href: `/environments/${e.id}${qs()}` }, e.name), e.isDefault ? h('span.chip.accent.plain', 'default') : null, e.safeForThirdParties ? h('span.chip.warn.plain', 'passes nothing') : null) },
          { label: 'Version', cls: 'hide-xs', render: (e) => `v${e.latestVersionNumber ?? e.versions?.length ?? 1}` },
          { label: 'Contents', render: (e) => h('span.muted', [plural(envVars(e.envContents).length, 'var'), plural((e.secretFiles || []).length, 'file'), plural((e.selectedRepositories || []).length, 'repo')].join(' · ')) },
          { label: 'Sandboxes', cls: 'num hide-xs', render: (e) => String((e.versions || []).reduce((s, v) => s + (v.sandboxCount || 0), 0)) },
          { label: 'Updated', cls: 'hide-md', render: (e) => h('span.muted', time(e.versions?.at(-1)?.createdAt || e.updatedAt)) },
        ],
      })), h('div', { style: { marginTop: '24px' } }, hosts));
    } catch (e) { if (e.name !== 'AbortError') body.replaceChildren(errorBox(e, load)); }
  }
  await Promise.all([load(), loadHosts()]);
}
