// Credits: balance, what your nodes earned, what your sandboxes spent.
import { h, icon, num, time, ago } from '../dom.js';
import { api, isMissing } from '../api.js';
import { pageHead, card, table, loading, errorBox, empty } from '../ui.js';
import { SIZES } from '../sbx.js';

export default async function (ctx) {
  ctx.title('Credits & usage');
  const root = h('div', loading());
  ctx.root.append(pageHead({
    title: 'Credits & usage',
    lede: 'Running sandboxes spend credits at their size’s rate; nodes earn credits for the sandbox time they host. Admins can grant credits.',
  }), root);
  try {
    const [c, l] = await Promise.allSettled([api.get('/credits', { days: 30 }), api.get('/limits')]);
    if (c.status === 'rejected') {
      if (isMissing(c.reason)) { root.replaceChildren(h('div.card', empty({ ico: 'coins', title: 'Credits are not tracked on this server', text: 'The server admin can turn on credit accounting.' }))); return; }
      throw c.reason;
    }
    const lim = l.value;
    const x = normCredits(c.value);
    const sbs = await api.get('/sandboxes', { limit: 200, include: 'cost,deletedCost' }).catch(() => null);
    if (x.burnPerHour == null) {
      if (sbs) x.burnPerHour = (sbs.sandboxes || []).filter((s) => ['ready', 'idle', 'running', 'provisioning', 'provisioned', 'cloning'].includes(s.state)).reduce((t, s) => t + (s.billingMultiplier || 0), 0);
    }
    const stat = (k, v, d, ico) => h('div.card.stat', h('span.k', icon(ico, 'sm'), k), h('span.v', v), d ? h('span.d', d) : null);
    root.replaceChildren(h('div.stack.loose',
      x.enforced === false ? h('div.notice', icon('info'), h('div', 'Credits are tracked but not enforced here: a negative balance does not block sandboxes.')) : null,
      h('div.grid.cols-4',
        stat('Balance', num(x.balance, 1), x.balance < 0 ? 'Below zero' : 'credits', 'coins'),
        stat(x.earned30d != null ? 'Earned · 30 days' : 'Earned · all time', `+${num(x.earned30d ?? x.earnedAll, 1)}`, 'hosting on your nodes', 'server'),
        stat(x.spent30d != null ? 'Spent · 30 days' : 'Spent · all time', `−${num(x.spent30d ?? x.spentAll, 1)}`, 'running your sandboxes', 'box'),
        stat('Burn rate now', `${num(x.burnPerHour, 2)}`, lim ? `credits/hour · ${lim.activeSandboxes ?? 0} of ${lim.maxActiveSandboxes ?? '∞'} active` : 'credits/hour', 'activity')),
      (x.daily || []).length ? card({ title: 'Daily credits', sub: 'Last 30 days', body: chart(x.daily) }) : null,
      sbs ? monthCard(sbs) : null,
      h('div.grid.side',
        card({ title: 'Ledger', flush: true, body: table({
          rows: x.ledger || [],
          emptyEl: h('div.card-body.faint', 'No entries yet.'),
          cols: [
            { label: 'When', render: (e) => h('span.muted', time(e.at)) },
            { label: 'What', render: (e) => h('div.stack.tight', { style: { gap: 0 } }, h('span', e.description || e.kind),
              e.sandboxId ? h('a.idlink', { href: `/sandboxes/${e.sandboxId}` }, e.sandboxId) : e.nodeName || e.nodeId ? h('span.faint.small', e.nodeName || e.nodeId) : null) },
            { label: 'Credits', cls: 'num', render: (e) => h('b', { style: { color: e.amount >= 0 ? 'var(--ok-text)' : 'var(--text)' } }, `${e.amount >= 0 ? '+' : '−'}${num(Math.abs(e.amount), 2)}`) },
          ],
        }) }),
        card({ title: 'Rates', body: h('div.stack',
          h('p.small.muted', `One credit buys one hour of a default sandbox. ${x.earnRate ? `Nodes earn ${num(x.earnRate, 2)}× what the sandboxes they host spend.` : ''}`),
          table({ rows: SIZES, cols: [
            { label: 'Size', render: (s) => h('span', { style: { textTransform: 'capitalize' } }, s.name) },
            { label: 'Credits / hour', cls: 'num', render: (s) => num(s.multiplier, 2) },
          ] }),
          x.lastGrant ? h('p.small.faint', `Last grant ${ago(x.lastGrant.at)}: +${num(x.lastGrant.amount)}`) : null) }))));
  } catch (e) { root.replaceChildren(errorBox(e)); }
}

// Credits spent this calendar month per sandbox, plus sandboxes deleted since.
function monthCard(r) {
  const rows = (r.sandboxes || []).filter((s) => s.cost?.credits > 0).sort((a, b) => b.cost.credits - a.cost.credits);
  const del = r.deletedCost;
  const delCredits = del ? del.seconds / 3600 : 0;
  const total = rows.reduce((t, s) => t + s.cost.credits, 0) + delCredits;
  const since = rows[0]?.cost.since || del?.since;
  return card({ title: 'This month by sandbox', sub: since ? `Since ${new Date(since).toLocaleDateString()} · ${num(total, 2)} credits in total` : null, flush: true,
    body: table({ rows: delCredits > 0 ? [...rows, { deleted: true }] : rows, emptyEl: h('div.card-body.faint', 'No sandbox time this month.'), cols: [
      { label: 'Sandbox', render: (s) => s.deleted ? h('span.muted', 'Deleted sandboxes') : h('a', { href: `/sandboxes/${s.id}` }, s.name || s.id) },
      { label: 'Size', cls: 'hide-xs', render: (s) => s.deleted ? '' : h('span', { style: { textTransform: 'capitalize' } }, s.type || 'default') },
      { label: 'Credits', cls: 'num', render: (s) => h('b', num(s.deleted ? delCredits : s.cost.credits, 2)) },
    ] }) });
}

// Accept both the documented shape and the server's {wallet,ledger:[{delta,reason,createdAt}],earnedCredits,...}.
function normCredits(r) {
  const w = r.wallet || {};
  return {
    ...r,
    balance: r.balance ?? w.balanceCredits ?? 0,
    enforced: r.enforced ?? w.enforced,
    earnedAll: r.earnedCredits, spentAll: r.spentCredits,
    earnRate: r.earnRate ?? r.rates?.nodeEarnRate,
    ledger: (r.ledger || []).map((e) => ({ ...e, at: e.at || e.createdAt, amount: e.amount ?? e.delta ?? 0, description: e.description || e.reason || e.kind })),
  };
}

// Diverging daily bars: earned above the baseline, spent below. Hover/focus a day for its values.
function chart(days) {
  const tip = h('div.tip', { hidden: true, role: 'status' });
  const wrap = h('div.chart');
  let lastW = 0;
  const draw = (W) => {
    const H = 200, top = 12, bottom = 22, left = 36;
    const max = Math.max(1, ...days.map((d) => Math.max(d.earned || 0, d.spent || 0)));
    const mid = top + (H - top - bottom) / 2, half = (H - top - bottom) / 2;
    const bw = (W - left) / days.length, barW = Math.max(2, Math.min(14, bw - 3));
    const y = (v) => (v / max) * half;
    const NS = 'http://www.w3.org/2000/svg';
    const s = (tag, attrs, ...kids) => { const el = document.createElementNS(NS, tag); for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v); el.append(...kids); return el; };
    const r = Math.min(4, barW / 2);
    // bar with rounded data-end only
    const up = (x, h0) => h0 < 0.5 ? '' : `M${x},${mid - 1} v${-Math.max(0, h0 - r)} q0,${-r} ${r},${-r} h${barW - 2 * r} q${r},0 ${r},${r} v${Math.max(0, h0 - r)} z`;
    const down = (x, h0) => h0 < 0.5 ? '' : `M${x},${mid + 1} v${Math.max(0, h0 - r)} q0,${r} ${r},${r} h${barW - 2 * r} q${r},0 ${r},${-r} v${-Math.max(0, h0 - r)} z`;
    const svg = s('svg', { viewBox: `0 0 ${W} ${H}`, width: W, height: H, role: 'img', 'aria-label': 'Daily credits earned and spent, last 30 days' },
      s('line', { class: 'grid-line', x1: left, x2: W, y1: top, y2: top }),
      s('line', { class: 'grid-line', x1: left, x2: W, y1: H - bottom, y2: H - bottom }),
      s('text', { class: 'axis-text', x: left - 6, y: top + 4, 'text-anchor': 'end' }, `+${num(max)}`),
      s('text', { class: 'axis-text', x: left - 6, y: mid + 4, 'text-anchor': 'end' }, '0'),
      s('text', { class: 'axis-text', x: left - 6, y: H - bottom + 4, 'text-anchor': 'end' }, `−${num(max)}`),
      ...days.map((d, i) => {
        const x = left + i * bw + (bw - barW) / 2;
        const g = s('g', { class: 'col', tabindex: '0', 'aria-label': `${d.day}: earned ${num(d.earned, 1)}, spent ${num(d.spent, 1)}` },
          s('rect', { class: 'hit', x: left + i * bw, y: top, width: bw, height: H - top - bottom, rx: 3 }),
          s('path', { class: 's1', d: up(x, y(d.earned || 0)) }),
          s('path', { class: 's2', d: down(x, y(d.spent || 0)) }));
        const show = () => {
          tip.hidden = false;
          tip.replaceChildren(h('b', new Date(d.day).toLocaleDateString(undefined, { month: 'short', day: 'numeric' })),
            h('div', h('span.sw', { style: { background: 'var(--series-1)' } }), `Earned ${num(d.earned, 1)}`),
            h('div', h('span.sw', { style: { background: 'var(--series-2)' } }), `Spent ${num(d.spent, 1)}`));
          const box = wrap.getBoundingClientRect(), gb = g.getBoundingClientRect();
          tip.style.left = `${Math.min(box.width - 60, Math.max(60, gb.left - box.left + gb.width / 2))}px`;
          tip.style.top = `${gb.top - box.top + 8}px`;
        };
        g.addEventListener('mouseenter', show); g.addEventListener('focus', show);
        g.addEventListener('mouseleave', () => { tip.hidden = true; }); g.addEventListener('blur', () => { tip.hidden = true; });
        return g;
      }),
      s('line', { class: 'axis-line', x1: left, x2: W, y1: mid, y2: mid }),
      ...[0, Math.floor(days.length / 2), days.length - 1].map((i) => s('text', { class: 'axis-text', x: left + i * bw + bw / 2, y: H - 4, 'text-anchor': i === 0 ? 'start' : i === days.length - 1 ? 'end' : 'middle' },
        new Date(days[i].day).toLocaleDateString(undefined, { month: 'short', day: 'numeric' }))));
    wrap.replaceChildren(svg, tip);
  };
  new ResizeObserver(([e]) => { const w = Math.round(e.contentRect.width); if (w && Math.abs(w - lastW) > 4) { lastW = w; draw(w); } }).observe(wrap);
  return h('div.stack.tight',
    h('div.legend', h('span', h('span.sw', { style: { background: 'var(--series-1)' } }), 'Earned by your nodes'), h('span', h('span.sw', { style: { background: 'var(--series-2)' } }), 'Spent by your sandboxes')),
    wrap,
    h('details', h('summary.small.muted', { style: { cursor: 'pointer' } }, 'Show as table'),
      table({ rows: [...days].reverse(), cols: [
        { label: 'Day', render: (d) => d.day },
        { label: 'Earned', cls: 'num', render: (d) => num(d.earned, 2) },
        { label: 'Spent', cls: 'num', render: (d) => num(d.spent, 2) },
      ] })));
}
