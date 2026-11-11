// CI runners: GitHub Actions jobs with runs-on: crowdvm, each in a fresh sandbox.
import { h, btn, icon, time, duration, num, every, patch } from '../dom.js';
import { api } from '../api.js';
import { pageHead, card, table, modal, segmented, empty, loading, errorBox, copyable, toast, toastErr, field, checkbox, menuButton, confirmDialog } from '../ui.js';

const JOB = {
  queued: ['info busy', 'queued'], running: ['accent live', 'running'], success: ['ok', 'passed'], failure: ['err', 'failed'],
  failed: ['err', 'error'], refused: ['warn', 'refused'], paused: ['warn', 'paused'], cancelled: ['', 'cancelled'], skipped: ['', 'skipped'],
};
const REPO = { ok: ['ok', 'ok'], failing: ['err', 'failing'], 'no-jobs': ['', 'no jobs yet'], 'public-blocked': ['warn', 'public: off'] };
const chip = (map, s, title) => { const [c, t] = map[s] || ['', s || 'unknown']; return h('span.chip', { class: c, title }, t); };
const isActive = (j) => j.state === 'queued' || j.state === 'running';
const sec = (s) => (s == null || s < 0 ? '—' : duration(s));
const rich = (s = '') => String(s).split('`').map((p, i) => (i % 2 ? h('code', p) : p)); // `code` in server messages
const openTab = (url) => window.open(url, '_blank', 'noopener');
const SNIPPET = 'runs-on: crowdvm';

export default async function (ctx) {
  ctx.title('CI runners');
  const f = { state: 'all', repo: '' };
  let data = null;
  const repos = new Set();
  const top = h('div.stack.loose'), seg = h('div'), jobsEl = h('div'), side = h('div.stack.loose'), accts = h('div.stack.loose');
  const repoSel = h('select.select.sm', { 'aria-label': 'Filter by repository', style: { width: 'auto', maxWidth: '100%', marginLeft: 'auto' } });
  repoSel.addEventListener('change', () => { f.repo = repoSel.value; load(); });
  const grid = h('div.grid.side',
    card({ title: 'Jobs', flush: true, body: h('div', h('div.card-body.tight.row.wrap', seg, repoSel), jobsEl) }), side);
  const layout = h('div.stack.loose', top, grid, accts);
  const body = h('div', loading());
  ctx.root.append(pageHead({
    title: 'CI runners',
    lede: h('span', 'GitHub Actions jobs with ', h('code', SNIPPET), ' run in a fresh sandbox each, deleted when the job ends. The server polls GitHub with your token, so nothing has to reach this network from outside.'),
    actions: btn('Connect GitHub', { kind: 'primary', ico: 'plug', onClick: connect, attrs: { 'data-create': '' } }),
  }), body);

  async function load(quiet) {
    try {
      const r = await api.get('/ci', { state: f.state, repo: f.repo, limit: 100 }, { signal: ctx.signal });
      data = r;
      render(r);
      if (!layout.isConnected) body.replaceChildren(layout);
    } catch (e) {
      if (e.name === 'AbortError' || quiet) return;
      if (data) toastErr(e); else body.replaceChildren(errorBox(e, load));
    }
  }

  function render(r) {
    const linked = r.installations.length > 0;
    grid.hidden = accts.hidden = !linked;
    patch(top, { s: r.setup, st: r.stats, linked }, () => [
      r.setup.complete ? null : setupCard(r.setup),
      linked ? stats(r.stats) : null,
    ]);
    patch(seg, r.counts, (c) => segmented([['all', `All · ${num(c.all)}`], ['active', `Active · ${num(c.active)}`], ['failed', `Failed · ${num(c.failed)}`]],
      f.state, (v) => { f.state = v; load(); }, 'Job state'));
    for (const i of r.installations) for (const x of i.repositories) repos.add(x.fullName);
    patch(repoSel, [...repos].sort(), (list) => [h('option', { value: '' }, 'All repositories'), ...list.map((n) => h('option', { value: n, selected: n === f.repo }, n))]);
    patch(jobsEl, { j: r.jobs, f }, () => jobsTable(r.jobs));
    patch(side, { d: r.stats.perDay, p: r.setup.problems }, () => [
      card({ title: 'Last 7 days', body: dayChart(r.stats.perDay) }),
      r.setup.problems.length ? card({ title: 'Problems', sub: 'Jobs that could not run, last 7 days', body: h('div.stack.tight', r.setup.problems.map(problem)) }) : null,
      card({ title: 'Use it in a workflow', body: workflowHelp() }),
    ]);
    patch(accts, r.installations.filter((i) => !f.repo || i.repositories.length), (list) => list.map(accountCard)); // repo filter hides the other accounts
  }

  // ---------------- sections ----------------
  function setupCard(s) {
    const n = s.steps.filter((x) => x.done).length;
    return card({ title: 'Set up CI runners', sub: `${n} of ${s.steps.length} steps done`, body: h('ol.stack', { style: { listStyle: 'none', margin: 0, padding: 0 } },
      s.steps.map((st, i) => h('li.row', { style: { alignItems: 'flex-start' }, 'aria-current': st.id === s.next ? 'step' : null },
        h('span', { 'aria-hidden': 'true', style: { flex: 'none', width: '24px', height: '24px', borderRadius: '50%', display: 'grid', placeItems: 'center', fontSize: '12px', fontWeight: 600,
          background: st.done ? 'var(--ok-soft)' : st.id === s.next ? 'var(--accent)' : 'var(--surface-3)', color: st.done ? 'var(--ok-text)' : st.id === s.next ? 'var(--accent-fg)' : 'var(--text-2)' } },
        st.done ? icon('check', 'sm') : String(i + 1)),
        h('div.stack.tight.grow', { style: { gap: '6px', minWidth: 0 } },
          h('b', st.title, h('span.sr-only', st.done ? ' (done)' : ' (to do)')),
          stepDetail(st))))) });
  }
  function stepDetail(st) {
    if (st.id === 'install' && !st.done) return h('div.row.wrap', h('span.small.muted', 'Paste a GitHub token. CrowdVM has no GitHub App.'), btn('Connect GitHub', { size: 'sm', ico: 'plug', onClick: connect }));
    if (st.id === 'workflow' && !st.done) return h('div.stack.tight', { style: { maxWidth: '420px' } }, copyable(SNIPPET, { label: 'Copy runs-on line' }), h('span.small.muted', 'Or crowdvm-small, crowdvm-large, crowdvm-xlarge.'));
    return h('span.small.muted', rich(st.detail));
  }

  function stats(st) {
    const tile = (k, v, d, ico) => h('div.card.stat', h('span.k', icon(ico, 'sm'), k), h('span.v', v), d ? h('span.d', d) : null);
    return h('div.grid.cols-4',
      tile('Jobs · 7 days', num(st.jobs), `${num(st.running)} running now`, 'git'),
      tile('Success rate', st.successRate == null ? '—' : `${Math.round(st.successRate * 100)}%`, 'of finished jobs', 'check'),
      tile('Median wait', sec(st.medianWaitSeconds), st.medianRunSeconds == null ? 'until the runner starts' : `then ${sec(st.medianRunSeconds)} to run`, 'clock'),
      tile('Cost · 7 days', num(st.costDollars, 2), 'credits', 'coins'));
  }

  function jobsTable(jobs) {
    const what = { all: 'No jobs yet', active: 'No active jobs', failed: 'No failed jobs' }[f.state];
    return table({
      rows: jobs, onRow: (j) => openJob(j), rowLabel: (j) => `${j.jobName || j.id} in ${j.repo}: ${(JOB[j.state] || [, j.state])[1]}`,
      emptyEl: empty({ ico: 'git', title: what, text: f.state === 'all' ? `Jobs appear here once a workflow uses ${SNIPPET}.` : f.repo ? `Nothing in ${f.repo} in this view.` : null }),
      cols: [
        { label: 'Job', render: (j) => h('div.stack.tight', { style: { gap: 0, minWidth: 0 } },
          h('b.truncate', { style: { display: 'block', maxWidth: 'min(230px, 45vw)' } }, j.jobName || `Job ${j.id}`),
          h('span.faint.small.truncate', { style: { display: 'block', maxWidth: 'min(230px, 45vw)' } }, j.repo, j.workflowName ? ` · ${j.workflowName}` : '')) },
        { label: 'State', render: (j) => chip(JOB, j.state, j.error || null) },
        { label: 'Branch', cls: 'hide-md', render: (j) => (j.headBranch ? h('span.tag', { title: j.headBranch, style: { maxWidth: '120px', overflow: 'hidden', textOverflow: 'ellipsis' } }, j.headBranch) : '—') },
        { label: 'Queued', cls: 'hide-xs', render: (j) => h('span.muted', time(j.queuedAt)) },
        { label: 'Took', cls: 'num hide-xs', render: (j) => took(j) },
        { label: h('span.sr-only', 'Actions'), cls: 'act', render: (j) => menuButton([
          { label: 'View log…', ico: 'file', onClick: () => openJob(j) },
          j.sandboxId && j.sandboxState !== 'deleted' ? { label: 'Open sandbox', ico: 'box', href: `/sandboxes/${j.sandboxId}` } : null,
          j.htmlUrl ? { label: 'Open on GitHub', ico: 'globe', onClick: () => openTab(j.htmlUrl) } : null,
        ], { title: `Actions for ${j.jobName || j.id}` }) },
      ],
    });
  }
  function took(j) {
    if (j.runSeconds != null) return duration(j.runSeconds);
    if (j.state === 'running' && j.runStartedAt) return h('span.muted', duration((Date.now() - Date.parse(j.runStartedAt)) / 1000));
    return h('span.faint', '—');
  }

  // Stacked pass/fail/other bars per UTC day.
  function dayChart(days) {
    const parts = [['success', 'passed', 'var(--ok)'], ['failure', 'failed', 'var(--err)'], ['other', 'other', 'var(--text-3)']];
    const max = Math.max(1, ...days.map((d) => d.success + d.failure + d.other));
    const fmt = (d, o) => new Date(`${d}T00:00:00Z`).toLocaleDateString(undefined, { timeZone: 'UTC', ...o });
    return h('div.stack',
      h('ul', { 'aria-label': 'Jobs per day', style: { display: 'grid', gridTemplateColumns: 'repeat(7, minmax(0, 1fr))', gap: '6px', height: '110px', margin: 0, padding: 0, listStyle: 'none' } },
        days.map((d) => {
          const total = d.success + d.failure + d.other;
          const label = `${fmt(d.day, { weekday: 'long', month: 'short', day: 'numeric' })}: ${total ? parts.map(([k, l]) => `${d[k]} ${l}`).join(', ') : 'no jobs'}`;
          return h('li', { title: label, style: { display: 'flex', flexDirection: 'column', alignItems: 'center', gap: '4px', minWidth: 0 } },
            h('span.sr-only', label),
            h('div', { 'aria-hidden': 'true', style: { flex: 1, width: '100%', maxWidth: '26px', display: 'flex', flexDirection: 'column-reverse', borderRadius: '4px', overflow: 'hidden', background: 'var(--surface-2)' } },
              parts.map(([k, , c]) => (d[k] ? h('div', { style: { height: `${(d[k] / max) * 100}%`, background: c } }) : null))),
            h('span.faint', { 'aria-hidden': 'true', style: { fontSize: '11px' } }, fmt(d.day, { weekday: 'narrow' })));
        })),
      h('div.legend', { 'aria-hidden': 'true' }, parts.map(([, l, c]) => h('span', h('span.sw', { style: { background: c } }), l))));
  }

  function problem(p) {
    return h('div.notice.warn', icon('alert'),
      h('div.grow', { style: { minWidth: 0, overflowWrap: 'anywhere' } }, h('div', rich(p.message)),
        h('div.small', { style: { opacity: 0.8 } }, p.repo, ' · ', time(p.at), p.count > 1 ? ` · ${p.count} jobs` : '')),
      btn('', { kind: 'ghost', size: 'sm', ico: 'eye', title: `Open job ${p.jobId}`, onClick: () => openJob(p.jobId) }));
  }

  function workflowHelp() {
    const c = (t) => h('code', t);
    return h('div.stack.tight.small.muted',
      copyable(SNIPPET, { label: 'Copy runs-on line' }),
      h('p', 'Sizes: ', c('crowdvm-small'), ', ', c('crowdvm'), ', ', c('crowdvm-large'), ', ', c('crowdvm-xlarge'), '.'),
      h('p', 'Add ', c('/debug'), ' to keep a failed job’s sandbox for 30 minutes, or ', c('/from=<snapshot>'), ' to boot from a saved snapshot, e.g. ', c('crowdvm-large/debug'), '.'));
  }

  function accountCard(a) {
    const org = a.accountType === 'Organization';
    const pub = !a.allowPublicRepos ? 'public repos off' : a.publicRepoUsers.length ? `public repos: ${a.publicRepoUsers.length} allowed ${a.publicRepoUsers.length === 1 ? 'user' : 'users'}` : 'public repos: anyone';
    return card({
      title: h('span.row', { style: { gap: '8px' } }, icon(org ? 'users' : 'user'), a.accountLogin),
      sub: `${org ? 'Organization' : 'Personal account'} · ${pub}`,
      actions: [btn('Public repos…', { size: 'sm', ico: 'globe', onClick: () => publicDialog(a) }),
        menuButton([{ label: 'Open on GitHub', ico: 'globe', onClick: () => openTab(a.manageUrl) }, '-', { label: 'Unlink…', ico: 'trash', danger: true, onClick: () => unlink(a) }], { title: `Actions for ${a.accountLogin}` })],
      flush: true,
      body: table({
        rows: a.repositories,
        emptyEl: h('div.card-body.small.muted', 'No repositories visible to this token yet. The server checks GitHub every 15 seconds.'),
        cols: [
          { label: 'Repository', render: (x) => h('span.row', { style: { gap: '6px' } }, icon(x.private ? 'lock' : 'globe', 'sm'), h('span.sr-only', x.private ? 'Private:' : 'Public:'),
            h('a', { href: x.htmlUrl, target: '_blank', rel: 'noopener' }, x.fullName)) },
          { label: 'Status', render: (x) => chip(REPO, x.status, x.lastError || null) },
          { label: 'Jobs · 7d', cls: 'num hide-xs', render: (x) => num(x.jobs7d) },
          { label: 'Last job', cls: 'hide-md', render: (x) => (x.lastAt ? h('span.row', { style: { gap: '6px' } }, chip(JOB, x.lastState), h('span.muted', time(x.lastAt))) : h('span.faint', '—')) },
        ],
      }),
    });
  }

  // ---------------- dialogs ----------------
  function connect() {
    const token = h('input.input.mono', { type: 'password', autocomplete: 'off', spellcheck: 'false', placeholder: 'github_pat_… or ghp_…' });
    const account = h('input.input', { autocomplete: 'off', spellcheck: 'false', placeholder: 'The token’s own user' });
    const apiUrl = h('input.input.mono', { type: 'url', placeholder: 'https://api.github.com' });
    const wallet = ctx.session.view; // the connect bills the viewed organization (X-CrowdVM-Org)
    modal({
      title: 'Connect GitHub', submit: true,
      desc: 'The server polls GitHub with this token for queued jobs. It is stored encrypted and never shown again.',
      body: h('div.stack',
        field('GitHub token', token, h('span', 'Fine-grained: Actions read, Administration read/write and Metadata read on the repositories. Classic: ', h('code', 'repo'), ', plus ', h('code', 'admin:org'), ' for organizations.')),
        field('Account or organization', account, 'Whose repositories run on CrowdVM. A personal account needs its own token.'),
        h('details', h('summary.small.muted', { style: { cursor: 'pointer' } }, 'GitHub Enterprise Server'),
          h('div', { style: { marginTop: '10px' } }, field('API URL', apiUrl, 'For example https://github.example.com/api/v3'))),
        wallet ? h('p.small.muted', 'Jobs bill to ', h('b', wallet.name), '.') : null),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Connect', kind: 'primary', submit: true, onClick: async () => {
        let r = await api.post('/ci/connect', { token: token.value.trim(), account: account.value.trim() || undefined, apiUrl: apiUrl.value.trim() || undefined, confirm: true });
        if (r.link.confirmRequired) r = await api.post('/ci/connect', { linkId: r.link.id, confirm: true });
        toast(`Linked ${r.link.accountLogin}. Its repositories show up within 15 seconds.`);
        load();
      } }],
    });
  }

  function confirmLink(id) {
    history.replaceState(null, '', '/ci');
    modal({
      title: 'Link GitHub account?',
      desc: 'A GitHub account was connected with a token from the command line. Confirm to run its CI jobs in sandboxes billed to you.',
      body: h('p.small.muted', 'Pending link ', h('code', id)),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Link account', kind: 'primary', submit: true, onClick: async () => {
        const r = await api.post('/ci/connect', { linkId: id, confirm: true });
        toast(`Linked ${r.link.accountLogin}`);
        load();
      } }],
    });
  }

  function publicDialog(a) {
    const allow = checkbox('allow', 'Run jobs from public repositories', 'Anyone who can open a pull request could otherwise run code on your nodes. Limit it to the users below.', a.allowPublicRepos);
    const users = h('textarea.textarea.mono', { rows: 3, spellcheck: 'false', placeholder: 'octocat, hubot', value: a.publicRepoUsers.join(', ') });
    modal({
      title: `Public repositories of ${a.accountLogin}`, submit: true,
      body: h('div.stack', allow, field('Allowed GitHub users', users, 'Both the author of a run and whoever started it must be listed. Leave empty to allow anyone.')),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Save', kind: 'primary', submit: true, onClick: async () => {
        await api.post('/ci/public', { account: a.id, allow: allow.querySelector('input').checked, users: users.value });
        toast('Public repository settings saved');
        load();
      } }],
    });
  }

  async function unlink(a) {
    if (!(await confirmDialog({ title: `Unlink ${a.accountLogin}?`, desc: 'CrowdVM stops picking up jobs from its repositories and deletes the stored token.', confirm: 'Unlink', danger: true }))) return;
    try { await api.del(`/ci/links/${a.id}`); toast(`${a.accountLogin} unlinked`); load(); } catch (e) { toastErr(e); }
  }

  // Job details + log. Accepts a job or a job id; refreshes itself while the job is active.
  function openJob(jobOrId) {
    let j = typeof jobOrId === 'object' ? jobOrId : null;
    const id = j ? j.id : jobOrId;
    const info = h('div', loading()), logBox = h('div');
    const stop = new AbortController();
    ctx.signal.addEventListener('abort', () => stop.abort());
    const m = modal({
      title: j?.jobName || `Job ${id}`, desc: j ? j.repo : null, wide: true,
      body: h('div.stack', info, logBox),
      actions: [{ label: 'Refresh', ico: 'refresh', onClick: async () => { await refresh(); return false; } }, { label: 'Close', kind: 'primary', onClick: () => true }],
      onClose: () => stop.abort(),
    });
    const pre = h('pre.console', { tabindex: '0', 'aria-label': 'Job log', style: { maxHeight: '50vh', margin: 0 } });
    async function refresh() {
      try {
        j = (await api.get(`/ci/jobs/${id}`, undefined, { signal: stop.signal })).job;
        m.dlg.querySelector('#dlg-t').textContent = j.jobName || `Job ${id}`;
        info.replaceChildren(details(j, m.close));
        const { log } = await api.get(`/ci/jobs/${id}/logs`, undefined, { signal: stop.signal });
        const atEnd = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 24;
        pre.textContent = log || (j.state === 'queued' ? 'The job has not started yet.' : 'No log yet.');
        if (!pre.isConnected) logBox.replaceChildren(pre);
        if (atEnd) pre.scrollTop = pre.scrollHeight;
      } catch (e) { if (e.name !== 'AbortError') (j ? logBox : info).replaceChildren(errorBox(e, refresh)); }
    }
    refresh();
    every(5000, () => j && isActive(j) && refresh(), stop.signal);
  }

  function details(j, close) {
    const row = (k, v) => [h('dt', k), h('dd', v)];
    return h('div.stack',
      j.error ? h('div.notice.err', icon('alert'), h('div', { style: { overflowWrap: 'anywhere' } }, rich(j.error))) : null,
      h('dl.kv',
        row('State', chip(JOB, j.state)),
        row('Repository', h('span', j.repo, j.workflowName ? h('span.muted', ` · ${j.workflowName}`) : null)),
        j.headBranch ? row('Commit', [h('span.tag', j.headBranch), j.headSha ? h('code.small', j.headSha.slice(0, 7)) : null]) : null,
        row('Runner', [h('code.small', j.label), h('span.muted', `${j.machineClass} size`)]),
        row('Sandbox', j.sandboxId ? (j.sandboxState === 'deleted' ? h('span.faint', `${j.sandboxId} (deleted)`)
          : [h('a.idlink', { href: `/sandboxes/${j.sandboxId}`, onclick: () => close() }, j.sandboxId), j.sandboxState === 'kept' ? h('span.chip.warn', 'kept for debugging') : null]) : h('span.faint', 'none')),
        row('Timing', h('span', 'queued ', time(j.queuedAt), ` · waited ${sec(j.waitSeconds)} · ran ${j.runSeconds != null ? sec(j.runSeconds) : j.state === 'running' ? 'still running' : '—'}`)),
        j.costDollars != null ? row('Cost', `${num(j.costDollars, 3)} credits`) : null),
      j.htmlUrl ? h('a.small', { href: j.htmlUrl, target: '_blank', rel: 'noopener' }, 'Open on GitHub ↗') : null);
  }

  await load();
  const pending = ctx.query.get('confirm');
  if (pending) confirmLink(pending);
  every(5000, () => data && (data.counts.active > 0 || (!f.repo && data.installations.some((i) => !i.repositories.length))) && load(true), ctx.signal);
}
