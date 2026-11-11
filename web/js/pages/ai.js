// AI credentials: provider keys per credential profile, custom APIs, the proxy, usage.
import { h, icon, btn, num, ago } from '../dom.js';
import { api, isMissing } from '../api.js';
import { pageHead, card, table, loading, errorBox, modal, field, toast, toastErr, confirmDialog, stateChip, empty, menuButton } from '../ui.js';
import { accessCard } from './environment.js';

// Labels for the server's credential catalog (GET /credential-profiles → credentials).
const NAMES = {
  'anthropic-api-key': ['Anthropic', 'Claude Code, OpenCode', 'sk-ant-…'], 'openai-api-key': ['OpenAI', 'Codex, OpenCode', 'sk-…'],
  'openrouter-api-key': ['OpenRouter', 'OpenCode and others', 'sk-or-…'], 'llmgateway-api-key': ['LLM Gateway', '', ''],
  'deepseek-api-key': ['DeepSeek', '', 'sk-…'], 'moonshot-api-key': ['Moonshot (Kimi)', '', 'sk-…'], 'mistral-api-key': ['Mistral', '', ''],
  'xai-api-key': ['xAI', '', 'xai-…'], 'opencode-api-key': ['OpenCode Zen', '', ''],
  'claude-subscription': ['Claude subscription', 'Paste the token from `claude setup-token`.', 'sk-ant-oat…'],
  'chatgpt-subscription': ['ChatGPT subscription', 'Paste ~/.codex/auth.json (after `codex login`) or an access token.', '{"tokens": …}'],
  'kimi-subscription': ['Kimi Code subscription', 'Paste the Kimi Code access token.', ''],
  'mistral-vibe-subscription': ['Mistral Vibe subscription', 'Paste the Vibe API key.', ''],
  'grok-subscription': ['SuperGrok subscription', 'Paste the Grok access token.', ''],
  'claude-bedrock': ['Claude on Amazon Bedrock', 'Region, then a Bedrock API key (ABSK…) or IAM access keys.', 'ABSK… or secret access key'],
  'codex-bedrock': ['Codex on Amazon Bedrock', 'Region, then a Bedrock API key (ABSK…) or IAM access keys.', 'ABSK… or secret access key'],
};
const FALLBACK = Object.keys(NAMES).map((id) => ({ id, kind: id.endsWith('-api-key') ? 'api-key' : id.endsWith('-bedrock') ? 'bedrock' : 'sign-in' }));
const nameOf = (id) => NAMES[id]?.[0] || id;

export default async function (ctx) {
  ctx.title('AI credentials');
  const root = h('div', loading());
  ctx.root.append(pageHead({
    title: 'AI credentials',
    lede: 'Keys the coding agents in your sandboxes use. With the proxy on, sandboxes only get placeholders and the server adds the real key to each request.',
  }), root);
  let profiles = [], pid = null, catalog = FALLBACK, blocking = [], defaults = null;
  const members = {}; // org id → members, for the "Who can use this profile" panel

  async function load() {
    try {
      const [p, proxy, usage, envs, st, cat] = await Promise.allSettled([api.get('/credential-profiles'), api.get('/ai-proxy'), api.get('/ai-usage', { days: 30 }), api.get('/environments'),
        api.get('/account-settings'), api.get('/provider-models')]);
      defaults = st.value && cat.value ? { settings: st.value.agentDefaults || {}, providers: cat.value.providers || [] } : null;
      if (p.status === 'rejected') throw p.reason;
      profiles = p.value.profiles || [];
      if (p.value.credentials?.length) catalog = p.value.credentials;
      blocking = (envs.value?.environments || []).filter((e) => !(e.effective?.passAgentsCredentials ?? e.passAgentsCredentials));
      if (!profiles.find((x) => x.id === pid)) pid = (profiles.find((x) => x.isDefault) || profiles[0])?.id;
      const org = profiles.find((x) => x.id === pid)?.owner;
      if (org?.kind === 'org' && !members[org.id]) members[org.id] = await api.get(`/orgs/${encodeURIComponent(org.id)}/members`).then((x) => x.members || [], () => []);
      draw(proxy.value, proxy.reason, usage.value);
    } catch (e) { root.replaceChildren(errorBox(e, load)); }
  }

  function draw(proxy, proxyErr, usage) {
    const prof = profiles.find((x) => x.id === pid);
    const connected = new Set(prof?.connected || []);
    const pbase = prof ? `/credential-profiles/${encodeURIComponent(prof.id)}` : '';
    const canManage = prof?.canManage !== false;

    const profileBar = h('div.row.wrap', { style: { marginBottom: '16px' } },
      profiles.length > 1 ? h('select.select', { 'aria-label': 'Credential profile', style: { width: 'auto' }, onchange: (e) => { pid = e.target.value; load(); } },
        profiles.map((p) => h('option', { value: p.id, selected: p.id === pid }, `${p.name}${p.isDefault ? ' (default)' : ''}${p.owner?.kind === 'org' ? ` · ${p.owner.name}` : ''}`))) : prof ? h('span.muted', 'Profile: ', h('b', prof.name)) : null,
      btn('New profile', { size: 'sm', ico: 'plus', onClick: newProfile }),
      prof && !prof.isDefault && canManage ? btn('Make default', { size: 'sm', onClick: () => api.put(pbase, { isDefault: true }).then(load, toastErr) }) : null,
      prof && !prof.isDefault && canManage ? btn('Delete profile', { size: 'sm', kind: 'danger', onClick: async () => {
        if (await confirmDialog({ title: `Delete profile ${prof.name}?`, confirm: 'Delete', danger: true })) api.del(pbase).then(load, toastErr);
      } }) : null);

    const credTable = (rows) => table({
      rows,
      cols: [
        { label: 'Provider', render: (c) => h('div.stack.tight', { style: { gap: 0 } }, h('b', nameOf(c.id)), NAMES[c.id]?.[1] && c.kind === 'api-key' ? h('span.faint.small', NAMES[c.id][1]) : null,
          c.getUrl && !connected.has(c.id) ? h('a.small', { href: c.getUrl, target: '_blank', rel: 'noopener' }, c.kind === 'api-key' ? 'Get a key ↗' : 'Get access ↗') : null) },
        { label: 'Status', render: (c) => connected.has(c.id) ? stateChip('active', 'connected') : h('span.chip.plain', 'not set') },
        { label: 'Account', cls: 'hide-xs', render: (c) => accountCell(prof, pbase, c.id, canManage) },
        { label: h('span.sr-only', 'Actions'), cls: 'act', render: (c) => canManage ? h('div.row.end',
          btn(connected.has(c.id) ? 'Replace' : c.kind === 'api-key' ? 'Set key' : 'Connect', { size: 'sm', onClick: () => setKey(pbase, c) }),
          connected.has(c.id) ? btn('', { size: 'sm', kind: 'ghost', ico: 'trash', title: `Remove ${nameOf(c.id)}`, onClick: async () => {
            if (await confirmDialog({ title: `Remove ${nameOf(c.id)}?`, desc: 'Removes every saved account of it. Agents in sandboxes using this profile lose access at their next request.', confirm: 'Remove', danger: true }))
              api.del(`${pbase}/keys/${c.id}`).then(() => { toast('Removed'); load(); }, toastErr);
          } }) : null) : null },
      ],
    });
    const noProfile = empty({ ico: 'sparkles', title: 'No credential profile', text: 'Create one to store provider keys.' });
    const keys = prof ? credTable(catalog.filter((c) => c.kind === 'api-key')) : noProfile;
    const signIns = prof ? credTable(catalog.filter((c) => c.kind !== 'api-key')) : null;
    const custom = prof?.customApis || [];

    const proxyCard = card({
      title: 'Credentials proxy',
      body: proxyErr && isMissing(proxyErr) ? h('p.faint', 'Not available on this server.') : proxy ? h('div.stack',
        h('label.check', h('input', { type: 'checkbox', checked: !!proxy.enabled, onchange: (e) => api.put('/ai-proxy', { enabled: e.target.checked }).then(() => { toast(`Proxy ${e.target.checked ? 'on' : 'off'}`); load(); }, toastErr) }),
          h('div', h('div.t', 'Keep real keys out of sandboxes'), h('div.d', 'Sandboxes get per-sandbox placeholder tokens; the server swaps in the key on the way out. Revoke a sandbox without rotating keys.'))),
        proxy.proxyUrl ? h('div.field', h('span.label', 'Proxy URL'), h('code.small', proxy.proxyUrl)) : null,
        proxy.enabledAt ? h('p.small.faint', `On since ${ago(proxy.enabledAt)}`) : null) : h('p.faint', 'Unavailable.'),
    });

    const t = usage?.totals;
    const usageCard = card({
      title: 'Usage', sub: 'Last 30 days, through the proxy',
      body: t ? h('div.stack',
        h('div.grid.cols-2', { style: { gap: '8px' } },
          h('div.stat', { style: { padding: 0 } }, h('span.k', 'Requests'), h('span.v', num(t.requests)), t.errors ? h('span.d', `${num(t.errors)} errors`) : null),
          h('div.stat', { style: { padding: 0 } }, h('span.k', 'Tokens'), h('span.v', compact((t.inputTokens || 0) + (t.outputTokens || 0))), h('span.d', `${compact(t.inputTokens)} in · ${compact(t.outputTokens)} out`))),
        (usage.sandboxes || []).length ? h('div.stack.tight', h('span.small.muted', 'Top sandboxes'),
          [...usage.sandboxes].sort((a, b) => (b.totals?.requests || 0) - (a.totals?.requests || 0)).slice(0, 5).map((s) =>
            h('div.row.between.small', h('a.truncate', { href: `/sandboxes/${s.sandboxId}` }, s.name || s.sandboxId), h('span.faint', `${num(s.totals?.requests)} req`)))) : null)
        : h('p.faint', 'No proxied requests yet.'),
    });

    const warn = blocking.length ? h('div.notice.warn', { style: { marginBottom: '16px' } }, icon('alert'), h('div',
      blocking.length === 1 ? 'The environment ' : 'The environments ', blocking.map((e, i) => [i ? ', ' : '', h('a', { href: `/environments/${encodeURIComponent(e.id)}` }, e.name)]),
      ` ${blocking.length === 1 ? 'does' : 'do'} not pass AI credentials, so agents in ${blocking.length === 1 ? 'its' : 'their'} sandboxes get none of these keys.`)) : null;
    root.replaceChildren(...[profileBar, warn].filter(Boolean), h('div.grid.side',
      h('div.stack.loose',
        card({ title: 'Provider keys', sub: prof ? `Profile ${prof.name}${prof.owner?.kind === 'org' ? ` (${prof.owner.name})` : ''}` : null, flush: true, body: keys }),
        signIns ? card({ title: 'Subscriptions and sign-ins', sub: 'Use a Claude, ChatGPT, Kimi, Vibe or Grok plan instead of pay-as-you-go keys.', flush: true, body: signIns }) : null,
        prof ? card({ title: 'Custom APIs', sub: 'Any other API key, exported as NAME and NAME_BASE_URL in sandboxes.', flush: true,
          actions: canManage ? btn('Add', { size: 'sm', ico: 'plus', onClick: () => customApi(pbase) }) : null,
          body: table({ rows: custom, emptyEl: h('div.card-body.faint', 'None.'), cols: [
            { label: 'Variable', render: (n) => h('code', n) },
            { label: h('span.sr-only', 'Actions'), cls: 'act', render: (n) => canManage ? btn('', { size: 'sm', kind: 'ghost', ico: 'trash', title: `Remove ${n}`, onClick: () => api.del(`${pbase}/custom-apis/${encodeURIComponent(n)}`).then(load, toastErr) }) : null },
          ] }) }) : null),
      h('div.stack.loose',
        prof?.owner?.kind === 'org' ? accessCard({ what: 'profile', orgName: prof.owner.name || 'the organization', access: prof.access, members: members[prof.owner.id], canEdit: canManage,
          save: async (access) => { await api.put(pbase, { access }); toast('Access saved'); load(); } }) : null,
        defaults ? defaultsCard(defaults) : null, proxyCard, usageCard)));
  }

  // Default harness, model and fast mode for prompts that name none (PATCH /account-settings).
  function defaultsCard({ settings, providers }) {
    const save = (agentDefaults, msg) => api.patch('/account-settings', { agentDefaults }).then(() => { toast(msg); load(); }, toastErr);
    const prov = providers.find((x) => x.provider === settings.provider);
    const shownProv = prov || providers.find((x) => x.provider === 'claude') || providers[0];
    const model = shownProv?.models?.find((m) => m.id === (settings.model || shownProv.defaultModel));
    const provSel = h('select.select', { onchange: (e) => save({ provider: e.target.value || null }, 'Default harness saved') },
      h('option', { value: '' }, `Automatic${shownProv && !prov ? ` (${shownProv.name || shownProv.provider})` : ''}`),
      providers.map((x) => h('option', { value: x.provider, selected: x.provider === settings.provider }, `${x.name || x.provider}${x.configured ? '' : ' · no key'}`)));
    const modelSel = h('select.select', { onchange: (e) => save({ model: e.target.value || null }, 'Default model saved') },
      h('option', { value: '' }, `Harness default${shownProv?.defaultModel ? ` (${shownProv.defaultModel})` : ''}`),
      (shownProv?.models || []).map((m) => h('option', { value: m.id, selected: m.id === settings.model }, m.name || m.id)));
    const fast = h('label.check', h('input', { type: 'checkbox', checked: !!settings.fast, disabled: model && !model.fastMode, onchange: (e) => save({ fast: e.target.checked }, `Fast mode ${e.target.checked ? 'on' : 'off'}`) }),
      h('div', h('div.t', 'Fast mode'), h('div.d', model && !model.fastMode ? `${model.name || model.id} has no fast mode.` : 'Faster output where the model supports it.')));
    return card({ title: 'Agent defaults', sub: 'Used when a prompt names no harness or model.', body: h('div.stack', field('Harness', provSel), field('Model', modelSel), fast) });
  }

  function setKey(pbase, c) {
    const [name, hint, ph] = NAMES[c.id] || [c.id, '', ''];
    const isKey = c.kind === 'api-key', isJSON = c.id === 'chatgpt-subscription';
    const v = isJSON ? h('textarea.textarea.mono', { rows: 5, placeholder: ph, spellcheck: 'false' }) : h('input.input.mono', { type: 'password', placeholder: ph, autocomplete: 'off', spellcheck: 'false' });
    const region = c.kind === 'bedrock' ? h('input.input.mono', { placeholder: 'us-east-1' }) : null;
    const akid = c.kind === 'bedrock' ? h('input.input.mono', { placeholder: 'AKIA…', autocomplete: 'off', spellcheck: 'false' }) : null;
    const stok = c.kind === 'bedrock' ? h('input.input.mono', { type: 'password', autocomplete: 'off', spellcheck: 'false' }) : null;
    const label = h('input.input', { placeholder: 'e.g. work account', maxlength: 64 });
    const keep = h('input', { type: 'checkbox' });
    modal({ title: name, submit: true, desc: 'Stored encrypted on the server. It is never shown again.',
      body: h('div.stack', !isKey && hint ? h('p.small.muted', hint) : null, field(isKey ? 'Key' : 'Token', v), region ? field('Region', region) : null,
        akid ? field('Access key id', akid, 'IAM access keys only. Leave empty for a Bedrock API key; the token above is then the secret access key.') : null,
        stok ? field('Session token', stok, 'Optional, for temporary IAM credentials.') : null,
        field('Label', label, 'Optional. Tells saved accounts apart.'),
        connected(c.id) ? h('label.check', keep, h('div', h('div.t', 'Keep the current one as a backup account'), h('div.d', 'Switch back to it later without pasting it again.'))) : null,
        c.getUrl ? h('p.small', h('a', { href: c.getUrl, target: '_blank', rel: 'noopener' }, isKey ? `Get a ${name} key ↗` : `About ${name} ↗`)) : null),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Save', kind: 'primary', submit: true, onClick: async () => {
        if (!v.value.trim()) throw new Error(isKey ? 'Paste the key.' : 'Paste the token.');
        if (region && !region.value.trim()) throw new Error('Enter the Bedrock region.');
        await api.put(`${pbase}/${isKey ? 'keys' : 'sign-ins'}/${c.id}`, { value: v.value.trim(), keepCurrent: keep.checked || undefined, label: label.value.trim() || undefined, region: region?.value.trim() || undefined,
          accessKeyId: akid?.value.trim() || undefined, sessionToken: stok?.value.trim() || undefined });
        toast(`${name} saved`); load();
      } }] });
  }
  const connected = (id) => (profiles.find((x) => x.id === pid)?.connected || []).includes(id);
  // Several saved accounts for one credential: pick the active one or drop one.
  function accountCell(prof, pbase, id, canManage) {
    const list = prof.accounts?.[id] || [];
    const a = list.find((x) => x.active);
    if (list.length < 2 || !canManage) return a ? h('code.small', a.label) : h('span.faint', '—');
    return h('div.row', { style: { gap: '4px' } }, h('code.small.truncate', a?.label || '—'), menuButton(() => [
      { header: 'Switch account' },
      ...list.map((x) => ({ label: `${x.label || x.id}${x.active ? '  ✓' : ''}`, onClick: () => !x.active && api.put(`${pbase}/accounts/${id}/${encodeURIComponent(x.id)}/active`, {}).then(() => { toast(`Using ${x.label || x.id}`); load(); }, toastErr) })),
      '-',
      ...list.filter((x) => !x.active).map((x) => ({ label: `Forget ${x.label || x.id}`, ico: 'trash', danger: true, onClick: () => api.del(`${pbase}/accounts/${id}/${encodeURIComponent(x.id)}`).then(load, toastErr) })),
    ], { title: `Accounts for ${nameOf(id)}` }));
  }
  function customApi(pbase) {
    const n = h('input.input.mono', { placeholder: 'ACME_API_KEY' });
    const url = h('input.input.mono', { placeholder: 'https://api.acme.dev' });
    const v = h('input.input.mono', { type: 'password', autocomplete: 'off' });
    modal({ title: 'Add custom API', submit: true, body: h('div.stack', field('Variable name', n), field('Base URL', url), field('Secret', v)),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Save', kind: 'primary', submit: true, onClick: async () => {
        if (!/^[A-Z_][A-Z0-9_]*$/.test(n.value)) throw new Error('Use an UPPER_SNAKE_CASE variable name.');
        await api.put(`${pbase}/custom-apis/${n.value}`, { value: v.value, baseUrl: url.value.trim() }); load();
      } }] });
  }
  function newProfile() {
    const n = h('input.input', { placeholder: 'team-claude' });
    const copyFrom = h('select.select', h('option', { value: '' }, 'Start empty'), profiles.map((p) => h('option', { value: p.id }, `Copy ${p.name}`)));
    const org = ctx.session.orgs.filter((o) => o.type === 'org');
    const owner = org.length ? h('select.select', h('option', { value: '' }, 'Personal'), org.map((o) => h('option', { value: o.id, selected: o.id === ctx.session.view?.id }, o.name))) : null;
    modal({ title: 'New credential profile', submit: true, desc: 'Profiles let environments use different keys, e.g. a team key for org sandboxes.',
      body: h('div.stack', field('Name', n), field('Start from', copyFrom), owner ? field('Owner', owner) : null),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Create', kind: 'primary', submit: true, onClick: async () => {
        const r = await api.post('/credential-profiles', { name: n.value.trim(), copyFrom: copyFrom.value || undefined, org: owner?.value || undefined });
        pid = r.profile?.id || pid; load();
      } }] });
  }

  await load();
}

function compact(n) { return n == null ? '—' : Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 }).format(n); }
