// Desktop tab: the sandbox's Linux desktop over noVNC.
import { h, icon } from '../dom.js';
import { empty, isLive } from '../ui.js';
import { desktopViewer } from '../vnc.js';

export default async function (ctx, root) {
  if (!isLive(ctx.sb.state)) {
    root.append(h('div.card', empty({ ico: 'monitor', title: 'The desktop runs only while the sandbox runs', text: 'Resume the sandbox, then open its desktop here.' })));
    return;
  }
  const v = desktopViewer({ id: ctx.id, signal: ctx.signal, title: ctx.sb.name });
  root.append(h('div.stack', v.el,
    h('p.small.faint', icon('info', 'sm'), ' Click the screen to type into it. The agent’s computer-use tools drive this same screen. From a terminal: ', h('code', `crowdvm desktop ${ctx.id}`))));
  v.connect();
}
