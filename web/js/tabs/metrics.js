// Metrics tab: CPU, memory, disk and I/O pressure of one sandbox over time.
// Data: GET /sandboxes/{id}/metrics?from=<RFC 3339> → {stepSeconds, points:[{at, cpuUtilization (0..1),
// memoryUsedBytes, memoryTotalBytes, diskUsedBytes, diskTotalBytes, ioPressure (0..1)}]}
// (web/README.md). The guest samples every 30 s; the server keeps 7 days and averages
// the samples into at most 360 points; steps without samples (stopped) are left out.
import { h, bytes, num, every, dateTime } from '../dom.js';
import { api, isMissing, errText } from '../api.js';
import { card, empty, loading, segmented, table } from '../ui.js';

const RANGES = [['1h', '1 h', 3600], ['6h', '6 h', 6 * 3600], ['24h', '24 h', 86400], ['7d', '7 days', 7 * 86400]];
const pct = (v) => (v == null ? '—' : `${num(v, v < 10 ? 1 : 0)}%`);

let range = '1h';

export default async function (ctx, root) {
  const grid = h('div.grid.cols-2');
  const foot = h('div');
  const status = h('span.small.faint', { 'aria-live': 'polite' });
  const seg = segmented(RANGES.map(([v, l]) => [v, l]), range, (v) => { range = v; load(); }, 'Time range');
  root.append(h('div.stack',
    h('div.row.wrap', seg, h('span.grow'), status),
    h('div', grid), foot));
  grid.replaceChildren(loading());

  let shown = null;
  async function load() {
    const secs = RANGES.find(([v]) => v === range)[2];
    const until = Date.now(), since = until - secs * 1000;
    let r;
    try {
      r = await api.get(`/sandboxes/${ctx.id}/metrics`, { from: new Date(since).toISOString() }, { signal: ctx.signal });
    } catch (e) {
      if (e.name === 'AbortError') return;
      if (isMissing(e)) return show('missing', h('div.card', empty({ ico: 'activity', title: 'This server does not record metrics yet', text: 'Update crowdvm-server to see CPU, memory and disk graphs here.' })));
      if (e.code === 'monitoring_off') return show('off', h('div.card', empty({ ico: 'activity', title: 'Monitoring is off for this sandbox', text: 'No samples are kept while monitoring is off (Account → Data retention).' })));
      status.textContent = `Could not refresh: ${errText(e)}`;
      return;
    }
    const s = norm(r);
    const step = (r.stepSeconds || r.intervalSeconds || 30) * 1000;
    status.textContent = s.length ? `${s.length} points, ${step >= 60000 ? `${Math.round(step / 60000)} min` : `${step / 1000} s`} each · updated ${new Date().toLocaleTimeString(undefined, { timeStyle: 'short' })}` : '';
    if (!s.length) {
      return show('empty', h('div.card', empty({ ico: 'activity', title: 'No samples in this range',
        text: 'The sandbox reports a sample every 30 seconds while it runs. Pick a longer range, or resume the sandbox.' })));
    }
    const last = s[s.length - 1];
    const memTotal = last.memTotal || Math.max(...s.map((x) => x.memTotal || 0)) || null;
    const diskTotal = last.diskTotal || Math.max(...s.map((x) => x.diskTotal || 0)) || null;
    const x = [since, until], gap = Math.max(90000, 3 * step);
    const charts = [
      ['CPU', 'All vCPUs', s.map((p) => [p.t, p.cpu]), { x, gap, max: 100, fmt: pct }],
      ['Memory', memTotal ? `of ${bytes(memTotal)}` : null, s.map((p) => [p.t, p.mem]), { x, gap, max: memTotal, fmt: (v) => bytes(v) }],
      ['Disk', diskTotal ? `of ${bytes(diskTotal)} (root filesystem)` : 'Root filesystem', s.map((p) => [p.t, p.disk]), { x, gap, max: diskTotal, fmt: (v) => bytes(v) }],
      ['I/O pressure', 'Share of time tasks waited on I/O (60 s average)', s.map((p) => [p.t, p.io]), { x, gap, max: 100, fmt: pct }],
    ];
    show(`data:${range}`, charts.map(([title, sub, pts, o]) => {
      const vals = pts.filter(([, v]) => v != null);
      const now = vals.length ? vals[vals.length - 1][1] : null;
      return card({ title, sub, actions: h('span.metric-now', o.fmt(now)), body: lineChart(title, pts, o) });
    }), h('details', h('summary.small.muted', { style: { cursor: 'pointer' } }, 'Show as table'),
      table({ rows: [...s].reverse().slice(0, 500), cols: [
        { label: 'Time', render: (p) => dateTime(p.t) },
        { label: 'CPU', cls: 'num', render: (p) => pct(p.cpu) },
        { label: 'Memory', cls: 'num', render: (p) => bytes(p.mem) },
        { label: 'Disk', cls: 'num', render: (p) => bytes(p.disk) },
        { label: 'I/O pressure', cls: 'num', render: (p) => pct(p.io) },
      ] })), s);
  }

  // Redraw only when the data changed, so hover and scroll survive polling.
  function show(key, gridKids, footKid = null, data = null) {
    const k = key + (data ? `:${data.length}:${data[data.length - 1].t}` : '');
    if (shown === k) return;
    shown = k;
    grid.replaceChildren(...[gridKids].flat());
    grid.className = key.startsWith('data') ? 'grid cols-2' : '';
    foot.replaceChildren(...(footKid ? [footKid] : []));
  }

  await load();
  every(30000, load, ctx.signal);
}

// Tolerant reader: {points|samples:[…]} with at|time|sampledAt; utilisations 0..1 → %.
function norm(r) {
  const arr = r.points || r.samples || (Array.isArray(r) ? r : []);
  const s = arr.map((p) => ({
    t: new Date(p.at || p.time || p.sampledAt || p.t).getTime(),
    cpu: p.cpuUtilization ?? p.cpu, mem: p.memoryUsedBytes, memTotal: p.memoryTotalBytes,
    disk: p.diskUsedBytes, diskTotal: p.diskTotalBytes, io: p.ioPressure,
  })).filter((p) => !isNaN(p.t)).sort((a, b) => a.t - b.t);
  for (const k of ['cpu', 'io']) {
    const max = Math.max(0, ...s.map((p) => p[k] ?? 0));
    if (max <= 1.000001) for (const p of s) if (p[k] != null) p[k] *= 100;
  }
  return s;
}

// One-series line chart with an area wash, time on x. Gaps longer than `gap` ms
// (the sandbox was stopped) break the line. Crosshair + tooltip on hover, and
// arrow keys move it when the chart has focus.
function lineChart(label, pts, { x: [x0, x1], gap, max, fmt }) {
  const data = pts.filter(([, v]) => v != null);
  const wrap = h('div.chart.line', { style: { minHeight: '170px' } });
  const tip = h('div.tip', { hidden: true });
  const yMax = niceMax(Math.max(max || 0, ...data.map(([, v]) => v)) || 1, max);
  let lastW = 0;

  const draw = (W) => {
    const H = 170, top = 8, bottom = 22, left = 56, right = 8;
    const X = (t) => left + ((t - x0) / (x1 - x0)) * (W - left - right);
    const Y = (v) => top + (1 - v / yMax) * (H - top - bottom);
    const NS = 'http://www.w3.org/2000/svg';
    const s = (tag, attrs, ...kids) => { const el = document.createElementNS(NS, tag); for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v); el.append(...kids); return el; };
    const runs = [];
    for (const p of data) {
      if (!runs.length || p[0] - runs[runs.length - 1].at(-1)[0] > gap) runs.push([]);
      runs[runs.length - 1].push(p);
    }
    const line = (run) => run.map(([t, v], i) => `${i ? 'L' : 'M'}${X(t).toFixed(1)},${Y(v).toFixed(1)}`).join('');
    const area = (run) => `${line(run)}L${X(run.at(-1)[0]).toFixed(1)},${Y(0)}L${X(run[0][0]).toFixed(1)},${Y(0)}Z`;
    const ticks = [0, yMax / 2, yMax];
    const xt = [x0, (x0 + x1) / 2, x1];
    const span = x1 - x0;
    const tfmt = (t) => new Date(t).toLocaleString(undefined, span > 2 * 86400000 ? { month: 'short', day: 'numeric' }
      : span >= 86400000 ? { weekday: 'short', hour: '2-digit', minute: '2-digit' } : { hour: '2-digit', minute: '2-digit' });
    const cross = s('line', { class: 'crosshair', x1: 0, x2: 0, y1: top, y2: H - bottom, visibility: 'hidden' });
    const dot = s('circle', { class: 'dot', r: 4, visibility: 'hidden' });
    const svg = s('svg', { viewBox: `0 0 ${W} ${H}`, width: W, height: H, role: 'img', tabindex: '0',
      'aria-label': `${label} over time. Latest ${data.length ? fmt(data.at(-1)[1]) : 'none'}. Use the arrow keys to read samples.` },
      ...ticks.map((v) => s('line', { class: v ? 'grid-line' : 'axis-line', x1: left, x2: W - right, y1: Y(v), y2: Y(v) })),
      ...ticks.map((v) => s('text', { class: 'axis-text', x: left - 6, y: Y(v) + 4, 'text-anchor': 'end' }, fmt(v))),
      ...xt.map((t, i) => s('text', { class: 'axis-text', x: X(t), y: H - 5, 'text-anchor': ['start', 'middle', 'end'][i] }, tfmt(t))),
      ...runs.map((r) => s('path', { class: 'area', d: area(r) })),
      ...runs.map((r) => r.length > 1 ? s('path', { class: 'ln', d: line(r) }) : s('circle', { class: 'pt', cx: X(r[0][0]), cy: Y(r[0][1]), r: 2.5 })),
      cross, dot);
    let idx = -1;
    const at = (i) => {
      if (!data.length) return;
      idx = Math.max(0, Math.min(data.length - 1, i));
      const [t, v] = data[idx], cx = X(t), cy = Y(v);
      for (const [el, a] of [[cross, { x1: cx, x2: cx }], [dot, { cx, cy }]]) { for (const [k, val] of Object.entries(a)) el.setAttribute(k, val); el.setAttribute('visibility', 'visible'); }
      tip.hidden = false;
      tip.replaceChildren(h('b', fmt(v)), h('div.faint', new Date(t).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })));
      const sx = wrap.clientWidth / W;
      tip.style.left = `${Math.min(wrap.clientWidth - 70, Math.max(70, cx * sx))}px`;
      tip.style.top = `${Math.max(36, cy * sx - 6)}px`;
    };
    const hide = () => { tip.hidden = true; cross.setAttribute('visibility', 'hidden'); dot.setAttribute('visibility', 'hidden'); };
    svg.addEventListener('pointermove', (e) => {
      const b = svg.getBoundingClientRect(), t = x0 + ((e.clientX - b.left) * (W / b.width) - left) / (W - left - right) * (x1 - x0);
      let best = 0;
      for (let i = 1; i < data.length; i++) if (Math.abs(data[i][0] - t) < Math.abs(data[best][0] - t)) best = i;
      if (data.length && Math.abs(data[best][0] - t) < gap) at(best); else hide();
    });
    svg.addEventListener('pointerleave', hide);
    svg.addEventListener('blur', hide);
    svg.addEventListener('focus', () => at(idx < 0 ? data.length - 1 : idx));
    svg.addEventListener('keydown', (e) => {
      const d = { ArrowLeft: -1, ArrowRight: 1, Home: -1e9, End: 1e9 }[e.key];
      if (d) { e.preventDefault(); at(idx + d); }
    });
    wrap.replaceChildren(svg, tip);
  };
  new ResizeObserver(([e]) => { const w = Math.round(e.contentRect.width); if (w && Math.abs(w - lastW) > 4) { lastW = w; draw(w); } }).observe(wrap);
  return wrap;
}

// Round the y-axis top up to a readable value; a known total (100%, RAM) stays as is.
function niceMax(v, fixed) {
  if (fixed) return fixed;
  const p = 10 ** Math.floor(Math.log10(v));
  return [1, 2, 2.5, 5, 10].map((m) => m * p).find((m) => m >= v) || v;
}

