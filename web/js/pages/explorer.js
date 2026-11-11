// API explorer: the server's own OpenAPI document (/api/v1/openapi.json, generated
// from its route table) with a "Try it" form that sends real requests as the
// signed-in user (session cookie + CSRF header). No external viewer, no CDN.
import { h, icon, btn, copy } from '../dom.js';
import { request, errText } from '../api.js';
import { pageHead, card, loading, errorBox, empty, confirmDialog, toast } from '../ui.js';

const METHODS = ['get', 'post', 'put', 'patch', 'delete'];
let specCache = null;

export default async function (ctx) {
  ctx.title('API explorer');
  ctx.wide();
  const root = h('div', loading('Loading the API description…'));
  ctx.root.append(pageHead({
    title: 'API explorer',
    lede: [`Every /api/v1 route this server serves. “Send” runs the request as you, against ${location.host}. Agents and scripts use an API key instead: `,
      h('a', { href: '/keys' }, 'API keys'), '.'],
    actions: [
      h('a.btn', { href: '/openapi/crowdvm-v1.yaml', download: 'crowdvm-v1.yaml', 'data-native': '' }, icon('download'), 'OpenAPI YAML'),
      h('a.btn', { href: '/api/v1/openapi.json', target: '_blank', rel: 'noopener' }, 'JSON'),
    ],
  }), root);

  let spec;
  try {
    spec = specCache ||= (await request('GET', '/api/v1/openapi.json', undefined, { signal: ctx.signal }));
  } catch (e) {
    if (e.name === 'AbortError') return;
    root.replaceChildren(errorBox(e));
    return;
  }
  const ops = [];
  for (const [path, item] of Object.entries(spec.paths || {})) {
    for (const m of METHODS) {
      const op = item[m];
      if (!op) continue;
      const params = [...(item.parameters || []), ...(op.parameters || [])];
      ops.push({ key: `${m.toUpperCase()} ${path}`, method: m.toUpperCase(), path, op, params,
        group: path.split('/')[1] || '/', ext: !!op['x-crowdvm-extension'] || (op.tags || []).includes('CrowdVM') });
    }
  }
  ops.sort((a, b) => a.group.localeCompare(b.group) || a.path.localeCompare(b.path) || METHODS.indexOf(a.method.toLowerCase()) - METHODS.indexOf(b.method.toLowerCase()));
  const base = new URL(spec.servers?.[0]?.url || '/api/v1', location.origin).pathname.replace(/\/$/, '');

  const search = h('input.input', { type: 'search', placeholder: 'Filter: path, summary or scope', 'aria-label': 'Filter operations', 'data-search': '' });
  const list = h('nav.ops', { 'aria-label': 'Operations' });
  const detail = h('div.op-detail');
  root.replaceChildren(h('div.card.explorer', h('div.ops-pane', h('div.ops-search', search), list), detail));

  let selected = ops.find((o) => o.key === ctx.query.get('op')) || ops.find((o) => o.key === 'GET /sandboxes') || ops[0];

  function drawList() {
    const q = search.value.trim().toLowerCase();
    const shown = ops.filter((o) => !q || `${o.key} ${o.op.summary || ''} ${o.op['x-crowdvm-action'] || ''}`.toLowerCase().includes(q));
    const groups = Map.groupBy ? Map.groupBy(shown, (o) => o.group) : shown.reduce((m, o) => m.set(o.group, [...(m.get(o.group) || []), o]), new Map());
    list.replaceChildren(...(shown.length ? [...groups].map(([g, items]) => h('div.ops-group',
      h('div.nav-label', g),
      items.map((o) => h('a.op', { href: `?op=${encodeURIComponent(o.key)}`, 'aria-current': o === selected ? 'true' : null, title: o.op.summary || o.key,
        onclick: (e) => { e.preventDefault(); select(o); } },
      methodChip(o.method), h('span.truncate', o.path.replace(/^\/[^/]+/, '') || `/${o.group}`))))) : [h('p.small.faint', { style: { padding: '12px' } }, 'No operation matches.')]));
  }
  search.addEventListener('input', drawList);

  function select(o) {
    selected = o;
    history.replaceState(history.state, '', `${location.pathname}?op=${encodeURIComponent(o.key)}`);
    drawList();
    drawDetail();
    if (matchMedia('(max-width: 860px)').matches) detail.scrollIntoView({ block: 'start' });
  }

  function drawDetail() {
    const o = selected;
    if (!o) { detail.replaceChildren(empty({ ico: 'braces', title: 'No operations', text: 'The server published an empty API description.' })); return; }
    const pathP = o.params.filter((p) => p.in === 'path');
    const otherP = o.params.filter((p) => p.in === 'query' || p.in === 'header');
    const inputs = new Map();
    const input = (p) => {
      const el = h('input.input.mono.sm', { placeholder: p.schema?.type || 'string', 'aria-label': `${p.in} parameter ${p.name}` });
      if (p.in === 'path' && /sandbox/i.test(p.name) && ctx.query.get('sandbox')) el.value = ctx.query.get('sandbox');
      inputs.set(p, el);
      return el;
    };
    const hasBody = !!o.op.requestBody;
    const body = hasBody ? h('textarea.textarea.mono', { rows: 6, spellcheck: false, 'aria-label': 'Request body (JSON)' }, '{\n  \n}') : null;
    const out = h('div.op-out');
    const sendBtn = btn('Send', { kind: 'primary', ico: 'play' });
    let ctl = null;

    const build = () => {
      let p = o.path;
      for (const pp of pathP) {
        const v = inputs.get(pp).value.trim();
        if (!v) throw new Error(`Fill in ${pp.name}.`);
        p = p.replace(`{${pp.name}}`, encodeURIComponent(v));
      }
      const url = new URL(base + p, location.origin);
      const headers = {};
      for (const qp of otherP) {
        const v = inputs.get(qp).value;
        if (v === '') continue;
        if (qp.in === 'query') url.searchParams.set(qp.name, v); else headers[qp.name] = v;
      }
      let b;
      if (hasBody) {
        try { b = body.value.trim() ? JSON.parse(body.value) : {}; } catch (e) { throw new Error(`The body is not valid JSON: ${e.message}`); }
      }
      return { url, headers, body: b };
    };

    sendBtn.addEventListener('click', async () => {
      if (ctl) { ctl.abort(); return; }
      let req;
      try { req = build(); } catch (e) { out.replaceChildren(h('div.notice.err', icon('alert'), h('div', e.message))); return; }
      if (o.method === 'DELETE' && !(await confirmDialog({ title: 'Send this DELETE?', desc: `${o.method} ${req.url.pathname}${req.url.search} runs for real and may delete data.`, confirm: 'Send', danger: true }))) return;
      ctl = new AbortController();
      sendBtn.replaceChildren(icon('stop'), 'Cancel');
      out.replaceChildren(h('div.loading', h('span.spinner'), 'Waiting for the server…'));
      const t0 = performance.now();
      try {
        const res = await fetch(req.url, {
          method: o.method, credentials: 'same-origin', signal: ctl.signal,
          headers: { Accept: 'application/json, application/x-ndjson;q=0.9, */*;q=0.5', ...(o.method !== 'GET' ? { 'X-CrowdVM-CSRF': '1' } : {}),
            ...(req.body !== undefined ? { 'Content-Type': 'application/json' } : {}), ...req.headers },
          body: req.body !== undefined ? JSON.stringify(req.body) : undefined,
        });
        const pre = h('pre.code.op-body');
        const head = h('div.row.wrap.small',
          h('span.chip', { class: res.ok ? 'ok' : res.status >= 500 ? 'err' : 'warn' }, `${res.status} ${res.statusText}`),
          h('span.faint', `${Math.round(performance.now() - t0)} ms`),
          h('span.faint', res.headers.get('content-type') || ''),
          res.headers.get('x-request-id') ? h('code.small.faint', res.headers.get('x-request-id')) : null,
          h('span.grow'), btn('', { size: 'sm', kind: 'ghost', ico: 'copy', title: 'Copy response', onClick: () => copy(pre.textContent).then(() => toast('Copied')) }));
        out.replaceChildren(head, pre);
        const ct = res.headers.get('content-type') || '';
        if (/json/.test(ct) && !/ndjson/.test(ct)) {
          const txt = await res.text();
          try { pre.textContent = JSON.stringify(JSON.parse(txt), null, 2); } catch { pre.textContent = txt; }
        } else if (/^(image|audio|video|application\/(octet-stream|zip|gzip|x-tar))/.test(ct)) {
          const blob = await res.blob();
          pre.textContent = `${blob.size.toLocaleString()} bytes of ${ct}.`;
        } else {
          // Text and NDJSON streams: show frames as they arrive (Cancel stops the stream).
          const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
          for (;;) {
            const { value, done } = await reader.read();
            if (done) break;
            pre.textContent += value;
            if (pre.textContent.length > 512 * 1024) { pre.textContent += '\n… (truncated at 512 KB)'; ctl.abort(); break; }
          }
        }
      } catch (e) {
        if (e.name !== 'AbortError') out.replaceChildren(h('div.notice.err', icon('alert'), h('div', errText(e))));
      } finally {
        ctl = null;
        sendBtn.replaceChildren(icon('play'), 'Send');
      }
    });

    const curl = () => {
      try {
        const r = build();
        const parts = [`curl -X ${o.method} '${r.url.href}'`, `-H "Authorization: Bearer $CROWDVM_API_KEY"`,
          ...Object.entries(r.headers).map(([k, v]) => `-H '${k}: ${v}'`)];
        if (r.body !== undefined) parts.push(`-H 'Content-Type: application/json'`, `-d '${JSON.stringify(r.body).replace(/'/g, "'\\''")}'`);
        copy(parts.join(' \\\n  ')).then(() => toast('curl command copied'));
      } catch (e) { toast(e.message, 'err'); }
    };

    const paramRows = (ps) => ps.map((p) => h('div.param',
      h('span.param-name', h('code', p.name), p.required ? h('span.req', { title: 'required' }, '*') : null, h('span.faint.small', p.in)),
      input(p)));
    const action = o.op['x-crowdvm-action'];
    detail.replaceChildren(...[
      h('div.op-head', methodChip(o.method), h('code.op-path', base + o.path)),
      h('h2.op-title', o.op.summary || o.op.operationId || o.key),
      h('div.row.wrap.small',
        action ? h('span.chip.plain', { title: 'API key scope this call needs' }, icon('key', 'sm'), action === 'session' ? 'dashboard session only' : action) : null,
        o.ext ? h('span.chip.accent', { title: 'Not part of Boat’s API' }, 'CrowdVM extension') : h('span.chip.plain', 'Boat-compatible'),
        o.op.deprecated ? h('span.chip.warn', 'deprecated') : null),
      o.op.description ? h('p.muted', o.op.description) : null,
      card({ title: 'Try it', sub: 'Runs as you, with your dashboard session.', actions: btn('Copy as curl', { size: 'sm', ico: 'copy', onClick: curl }),
        body: h('div.stack',
          pathP.length ? h('div.stack.tight', h('div.label.small.muted', 'Path'), paramRows(pathP)) : null,
          otherP.length ? h('details', { open: otherP.some((p) => p.required) }, h('summary.small.muted', `Query and header parameters (${otherP.length})`),
            h('div.stack.tight', { style: { marginTop: '8px' } }, paramRows(otherP))) : null,
          body ? h('div.stack.tight', h('div.label.small.muted', 'JSON body'), body) : null,
          h('div.row', sendBtn, h('span.small.faint', o.method === 'GET' ? 'Read-only.' : 'Changes data on this server.')),
          out) }),
      h('details.small', h('summary.muted', 'Raw OpenAPI operation'), h('pre.code', { style: { marginTop: '8px' } }, JSON.stringify(o.op, null, 2))),
    ].filter(Boolean));
  }

  drawList();
  drawDetail();
  const cur = list.querySelector('[aria-current]'); // scroll the list only, never the page
  if (cur) list.scrollTop = cur.offsetTop - list.clientHeight / 2;
}

function methodChip(m) {
  return h('span.method', { class: m.toLowerCase() }, m === 'DELETE' ? 'DEL' : m);
}
