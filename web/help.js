'use strict';

/*
 * Delta help page (#/help, #/help/<section>).
 *
 * This file is loaded before app.js. It only defines constants and
 * functions; everything it uses from app.js (icon, escapeHtml, $, $$,
 * endpointBadge, renderAsNmea, openForwarderEditor …) is looked up when the
 * help page is rendered, never while this file loads.
 *
 * The page is one long document with a table of contents. Every section is an
 * entry of HELP_SECTIONS with a function that returns its HTML. Visuals come
 * in two kinds:
 *   - diagrams: inline SVG whose colors come from the CSS tokens (.help-svg),
 *     so they follow the light/dark theme,
 *   - mock-ups: small copies of the real screens, built from the same CSS
 *     classes (and partly the same render functions) as the real UI, with
 *     numbered markers that the text below refers to.
 */

/* ================= small building blocks ================= */

// Numbered marker (1, 2, 3 …) used in mock-ups and in step lists.
const helpMarker = number => `<span class="help-num" title="Step ${number}, explained in the text">${number}</span>`;

// A figure with a caption. `isMockup` adds the "Example" label, so nobody
// mistakes the picture for the real (clickable) interface.
function helpFigure(innerHtml, caption, isMockup = false) {
  return `<figure class="help-fig">
    <div class="help-fig-body ${isMockup ? 'help-mock' : ''}">
      ${isMockup ? '<span class="help-mock-label" title="A picture of the interface, not the real thing">Example</span>' : ''}
      ${innerHtml}
    </div>
    ${caption ? `<figcaption>${caption}</figcaption>` : ''}
  </figure>`;
}

// Numbered step list: steps = [[title, bodyHtml], …]. Step numbers look different
// from the blue figure markers, so "step 5" is never mistaken for "marker 5".
function helpSteps(steps) {
  return `<ol class="help-steps">${steps.map(([title, body], index) => `
    <li><span class="help-step-num">${index + 1}</span><div><b>${title}</b>${body ? `<div class="help-step-body">${body}</div>` : ''}</div></li>`).join('')}</ol>`;
}

// Colored note box. kind: "info" (tip), "warn" (pitfall), "ok" (good to know).
const HELP_NOTE_ICONS = { info: 'info', warn: 'alert', ok: 'info' };
const helpNote = (kind, html) => `<div class="help-note ${kind}">${icon(HELP_NOTE_ICONS[kind])}<div>${html}</div></div>`;

// Link to another help section; handled by the page's click handler (no page reload).
const helpLink = (sectionId, text) => `<a href="#/help/${sectionId}" data-help-goto="${sectionId}">${text}</a>`;

// Button that does something in the real interface ("Try it").
const helpTryButton = (action, label, hint) =>
  `<button class="btn sm primary" data-help-try="${action}" title="${escapeHtml(hint)}">${icon('play')}${label}</button>`;

// Preformatted code block; the content is escaped here.
const helpCode = text => `<pre class="help-code">${escapeHtml(text)}</pre>`;

// Keyboard key.
const helpKey = key => `<kbd>${escapeHtml(key)}</kbd>`;

// Arrow markers for the SVG diagrams. Marker ids are global in the document,
// so they are defined once at the top of the help page.
const HELP_SVG_DEFINITIONS = `<svg class="help-defs" width="0" height="0" aria-hidden="true"><defs>
  ${['in', 'out', 'muted', 'err'].map(kind => `<marker id="help-arrow-${kind}" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M0 0L10 5L0 10z" class="fill-${kind}"/></marker>`).join('')}
</defs></svg>`;

// SVG text element (centered unless `anchor` says otherwise).
const svgText = (x, y, text, cssClass = '', anchor = 'middle') =>
  `<text x="${x}" y="${y}" class="${cssClass}" text-anchor="${anchor}">${escapeHtml(text)}</text>`;

// SVG box with a title line and an optional second (small, muted) line.
function svgBox(x, y, width, height, title, subtitle, cssClass = 'box') {
  const centerX = x + width / 2;
  const titleY = subtitle ? y + height / 2 - 3 : y + height / 2 + 4;
  return `<rect x="${x}" y="${y}" width="${width}" height="${height}" rx="9" class="${cssClass}"/>`
    + svgText(centerX, titleY, title, 'strong')
    + (subtitle ? svgText(centerX, y + height / 2 + 13, subtitle, 'small muted') : '');
}

/* ================= diagrams ================= */

// The big picture, with every way Delta can be connected:
//   input:  either Delta is the server (senders connect to it) or the client (it connects to the senders, tcp-connect);
//   output: clients connect to Delta, Delta connects to targets, or Delta sends UDP datagrams.
// Solid colored lines carry the data, dashed grey arrows point from whoever opens the connection.
// Every receiver has its own queue (the small slots inside the output boxes).
function helpOverviewDiagram() {
  const rowHeight = 40;
  // Dashed "who connects" arrow just above a link, and the data line just below it.
  const connectionArrow = (fromX, toX, centerY) =>
    `<path d="M${fromX} ${centerY - 8} H${toX}" class="flow-muted dashed" marker-end="url(#help-arrow-muted)"/>`;
  const dataLine = (fromX, toX, centerY, kind) =>
    `<path d="M${fromX} ${centerY + 6} H${toX}" class="flow-${kind} anim" marker-end="url(#help-arrow-${kind})"/>`;

  // Input side: two alternatives, each with its input modes and two senders.
  // [header, input modes, delta opens the connection, senders, top y]
  const inputCenterY = 220;
  const inputAlternatives = [
    ['Either: Delta is the server', ['tcp-listen', 'udp-listen'], false, [['GPS', 'connects to Delta'], ['AIS', 'connects to Delta']], 75],
    ['or: Delta is the client', ['tcp-connect'], true, [['Compass', 'Delta connects'], ['Wind sensor', 'Delta connects']], 275],
  ];
  const inputSide = inputAlternatives.map(([header, modes, deltaConnects, senders, top]) => {
    const boxCenterY = top + 45;
    return svgText(10, top - 13, header, 'small strong accent', 'start')
      + senders.map(([name, sub], index) => {
        const y = top + index * 50, centerY = y + rowHeight / 2;
        return svgBox(10, y, 110, rowHeight, name, sub)
          + (deltaConnects ? connectionArrow(173, 124, centerY) : connectionArrow(122, 171, centerY))
          + dataLine(120, 173, centerY, 'in');
      }).join('')
      // The input box names its modes, like the output boxes do.
      + `<rect x="175" y="${top}" width="90" height="90" rx="9" class="box-in"/>`
      + svgText(220, boxCenterY - 12, 'Input', 'strong')
      + modes.map((mode, index) => svgText(220, boxCenterY + 5 + index * 13, mode, 'small muted')).join('')
      // Straight down (upper box) or up (lower box) into the "copied once" circle between them.
      + (top < inputCenterY
        ? `<path d="M220 ${top + 90} V204" class="flow-in anim" marker-end="url(#help-arrow-in)"/>`
        : `<path d="M220 ${top} V236" class="flow-in anim" marker-end="url(#help-arrow-in)"/>`);
  }).join('');

  // Output side: three outputs of different modes in the same forwarder. [title, mode, header, delta connects (null = no connection), receivers, filled queue slots per receiver, top y]
  const outputs = [
    ['Output 1', 'tcp-listen', 'Clients connect to Delta', false, ['Chart plotter', 'Tablet app', 'Laptop'], [2, 0, 4], 60],
    ['Output 2', 'tcp-connect', 'Delta connects to targets', true, ['Navigation PC', 'Logger'], [1, 5], 225],
    ['Output 3', 'udp-send', 'Delta sends datagrams', null, ['Display'], [0], 340],
  ];
  const outputSide = outputs.map(([title, mode, header, deltaConnects, receivers, filledSlots, top]) => {
    const boxHeight = receivers.length * 50 - 10, boxCenterY = top + boxHeight / 2;
    let html = `<path d="M234 ${inputCenterY} C 285 ${inputCenterY}, 280 ${boxCenterY}, 328 ${boxCenterY}" class="flow-in anim" marker-end="url(#help-arrow-in)"/>`
      + `<rect x="330" y="${top}" width="175" height="${boxHeight}" rx="9" class="box"/>`
      + svgText(340, boxCenterY - 3, title, 'strong', 'start') + svgText(340, boxCenterY + 12, mode, 'small muted', 'start')
      + svgText(600, top - 8, header, 'small strong out', 'start');
    receivers.forEach((receiver, index) => {
      const y = top + index * 50, centerY = y + rowHeight / 2;
      for (let slot = 0; slot < 8; slot++) {
        html += `<rect x="${433 + slot * 9}" y="${centerY - 5}" width="7" height="10" rx="2" class="${slot >= 8 - filledSlots[index] ? 'slot-full' : 'slot'}"/>`;
      }
      if (deltaConnects === true) html += connectionArrow(509, 596, centerY);
      if (deltaConnects === false) html += connectionArrow(596, 509, centerY);
      html += dataLine(505, 597, centerY, 'out') + svgBox(600, y, 150, rowHeight, receiver, '', 'box-out');
    });
    return html;
  }).join('');

  return `<svg class="help-svg" viewBox="0 0 760 445" role="img" aria-label="One forwarder: the input either accepts connections from senders or connects to them; three outputs where clients connect to Delta, Delta connects to targets, and Delta sends UDP datagrams">
    <rect x="160" y="12" width="360" height="388" rx="16" class="frame"/>
    ${svgText(175, 32, 'One Delta forwarder', 'strong accent', 'start')}
    ${inputSide}
    ${svgText(65, inputCenterY + 4, 'or', 'strong muted')}
    <circle cx="220" cy="${inputCenterY}" r="14" class="box-in"/>${svgText(220, inputCenterY + 4, '1×', 'strong')}
    ${svgText(201, inputCenterY - 2, 'copied', 'small muted', 'end')}${svgText(201, inputCenterY + 10, 'once', 'small muted', 'end')}
    ${outputSide}
    <path d="M170 425 H200" class="flow-in"/><path d="M204 425 H234" class="flow-out"/>${svgText(242, 429, 'data', 'small muted', 'start')}
    <path d="M300 425 H336" class="flow-muted dashed" marker-end="url(#help-arrow-muted)"/>${svgText(344, 429, 'opens the connection', 'small muted', 'start')}
    <rect x="480" y="420" width="7" height="10" rx="2" class="slot-full"/><rect x="489" y="420" width="7" height="10" rx="2" class="slot"/>${svgText(503, 429, 'own queue per receiver', 'small muted', 'start')}
  </svg>`;
}

// One endpoint mode as a picture: who opens the connection, and which way the data flows.
// Data always flows left to right; `connect` is "right" (left side connects to the
// right side), "left" (the other way round), or a text shown instead of a connection.
function helpModeDiagram({ leftTitle, leftSub, rightTitle, rightSub, connect, connectLabel, dataLabel, deltaSide, dataKind }) {
  let connectionLine;
  if (connect === 'right') connectionLine = `<path d="M127 38 H251" class="flow-muted dashed" marker-end="url(#help-arrow-muted)"/>`;
  else if (connect === 'left') connectionLine = `<path d="M253 38 H129" class="flow-muted dashed" marker-end="url(#help-arrow-muted)"/>`;
  else connectionLine = '';
  return `<svg class="help-svg help-svg-small" viewBox="0 0 380 100" role="img" aria-label="${escapeHtml(`${leftTitle} to ${rightTitle}`)}">
    ${svgBox(5, 18, 120, 64, leftTitle, leftSub, deltaSide === 'left' ? 'box-in' : 'box')}
    ${svgBox(255, 18, 120, 64, rightTitle, rightSub, deltaSide === 'right' ? 'box-in' : dataKind === 'out' ? 'box-out' : 'box')}
    ${connectionLine}${svgText(190, 31, connectLabel, 'small muted')}
    <path d="M127 64 H251" class="flow-${dataKind} anim" marker-end="url(#help-arrow-${dataKind})"/>
    ${svgText(190, 80, dataLabel, 'small')}
  </svg>`;
}

// How the same bytes are forwarded with "raw" and with "lines" framing.
function helpFramingDiagram() {
  const characterWidth = 6.6;
  // Draws a row of chunks (each a rectangle with monospace text) from left to right.
  const chunkRow = (texts, y, cssClass) => {
    let x = 10, html = '';
    for (const text of texts) {
      const width = text.length * characterWidth + 14;
      html += `<rect x="${x}" y="${y}" width="${width}" height="26" rx="6" class="${cssClass}"/>`
        + `<text x="${x + 7}" y="${y + 17}" class="mono">${escapeHtml(text)}</text>`;
      x += width + 8;
    }
    return html;
  };
  const networkReads = ['$GPGGA,092750,5321.68', ',N,00630.33,W*76↵$GPRMC,0927', '50,A,5321.68,N*43↵'];
  return `<svg class="help-svg" viewBox="0 0 760 236" role="img" aria-label="Raw framing forwards chunks as received, line framing forwards whole sentences">
    ${svgText(10, 22, 'What arrives from the network (three reads, cut anywhere)', 'strong', 'start')}
    ${chunkRow(networkReads, 30, 'box')}
    ${svgText(10, 98, 'Raw byte stream → forwarded exactly like this, as fast as possible', 'strong accent', 'start')}
    ${chunkRow(networkReads, 106, 'box-in')}
    ${svgText(10, 174, 'NMEA / text lines → split at the line end, every message whole', 'strong out', 'start')}
    ${chunkRow(['$GPGGA,092750,5321.68,N,00630.33,W*76↵', '$GPRMC,092750,A,5321.68,N*43↵'], 182, 'box-out')}
    ${svgText(10, 228, '↵ = line end (\\n). A UDP output sends one datagram per chunk, so with lines every datagram is one whole sentence.', 'small muted', 'start')}
  </svg>`;
}

// Per-output queues: a slow receiver fills its own queue, the oldest message is dropped,
// and nobody else is affected.
function helpQueueDiagram() {
  const lanes = [
    { y: 27, filled: 2, receiver: 'Plotter', receiverSub: 'fast', isFull: false },
    { y: 97, filled: 10, receiver: 'TCP target', receiverSub: 'disconnected', isFull: true },
    { y: 167, filled: 1, receiver: 'UDP send', receiverSub: 'fast', isFull: false },
  ];
  const lanesHtml = lanes.map(lane => {
    const centerY = lane.y + 18;
    let slots = '';
    for (let slot = 0; slot < 10; slot++) {
      const isFilled = slot >= 10 - lane.filled;
      const isDropped = lane.isFull && slot === 9; // the oldest message sits next to the receiver
      slots += `<rect x="${210 + slot * 23}" y="${lane.y + 9}" width="18" height="18" rx="3" class="${isDropped ? 'slot-drop' : isFilled ? 'slot-full' : 'slot'}"/>`;
    }
    const dropMark = lane.isFull
      ? svgText(210 + 9 * 23 + 9, lane.y + 23, '×', 'strong white') + svgText(330, lane.y + 50, 'queue full: the oldest message is dropped (and counted)', 'small err')
      : '';
    return `<path d="M122 115 C 160 115, 160 ${centerY}, 198 ${centerY}" class="flow-in anim" marker-end="url(#help-arrow-in)"/>`
      + `<rect x="200" y="${lane.y}" width="250" height="36" rx="8" class="box"/>${slots}${dropMark}`
      + `<path d="M450 ${centerY} H518" class="${lane.isFull ? 'flow-muted dashed' : 'flow-out anim'}" marker-end="url(#help-arrow-${lane.isFull ? 'muted' : 'out'})"/>`
      + svgBox(520, lane.y, 150, 36, lane.receiver, lane.receiverSub, lane.isFull ? 'box-warn' : 'box-out');
  }).join('');
  return `<svg class="help-svg" viewBox="0 0 690 232" role="img" aria-label="Each output has its own queue; a full queue drops its oldest message">
    ${svgBox(10, 90, 112, 50, 'Input', 'never waits', 'box-in')}
    ${lanesHtml}
    ${svgText(325, 16, 'newest enters here  →  oldest leaves here', 'small muted')}
  </svg>`;
}

// Waiting time between reconnect attempts: 0.2 s doubling up to 10 s.
function helpBackoffDiagram() {
  const delays = [0.2, 0.4, 0.8, 1.6, 3.2, 6.4, 10, 10];
  const baseY = 140;
  const bars = delays.map((seconds, index) => {
    const x = 50 + index * 60;
    const height = Math.max(3, seconds * 11);
    return `<rect x="${x}" y="${baseY - height}" width="34" height="${height}" rx="4" class="bar"/>`
      + svgText(x + 17, baseY - height - 6, `${seconds} s`, 'small')
      + svgText(x + 17, baseY + 16, `#${index + 1}`, 'small muted');
  }).join('');
  return `<svg class="help-svg help-svg-medium" viewBox="0 0 540 180" role="img" aria-label="Reconnect delays grow from 0.2 seconds to 10 seconds">
    <path d="M40 ${baseY} H530" class="axis"/>
    ${bars}
    ${svgText(285, 174, 'failed attempt in a row  (plus up to 20 % random jitter)', 'small muted')}
  </svg>`;
}

// Timeline of an idle alert: data stops, the alert fires after idle_alert seconds,
// and resolves when data arrives again.
function helpAlertTimeline() {
  let ticks = '';
  for (let x = 30; x <= 250; x += 11) ticks += `<path d="M${x} 112 V128" class="flow-in"/>`;
  for (let x = 570; x <= 740; x += 11) ticks += `<path d="M${x} 112 V128" class="flow-in"/>`;
  return `<svg class="help-svg" viewBox="0 0 760 175" role="img" aria-label="Idle alert timeline">
    <rect x="380" y="20" width="190" height="112" class="firing-zone"/>
    <path d="M20 130 H745" class="axis" marker-end="url(#help-arrow-muted)"/>
    ${ticks}
    ${svgText(140, 104, 'data arrives', 'small accent')}
    <path d="M255 70 H378" class="flow-muted dashed"/>
    ${svgText(318, 62, 'no data …', 'small muted')}
    <path d="M380 20 V130" class="flow-err dashed"/>
    ${svgText(386, 38, 'idle_alert seconds passed', 'small err', 'start')}
    ${svgText(386, 56, '→ FIRING: event + webhook', 'strong err', 'start')}
    <path d="M570 20 V130" class="flow-ok dashed"/>
    ${svgText(576, 38, 'data again', 'small ok', 'start')}
    ${svgText(576, 56, '→ RESOLVED', 'strong ok', 'start')}
    ${svgText(380, 160, 'time →   (a “down” alert works the same way, with the global “down after” delay)', 'small muted')}
  </svg>`;
}

// Recording taps the input; a replay feeds a file into the outputs of any running forwarder.
function helpCaptureDiagram() {
  return `<svg class="help-svg" viewBox="0 0 760 190" role="img" aria-label="Recording and replaying captures">
    ${svgBox(10, 20, 120, 44, 'Source', '')}
    <path d="M130 42 H168" class="flow-in anim" marker-end="url(#help-arrow-in)"/>
    ${svgBox(170, 20, 120, 44, 'Input', 'forwarder A', 'box-in')}
    <path d="M290 42 H578" class="flow-out anim" marker-end="url(#help-arrow-out)"/>
    ${svgBox(580, 20, 170, 44, 'Outputs of A', 'unchanged, not slowed', 'box-out')}
    <circle cx="340" cy="42" r="5" class="tap"/>
    <path d="M340 47 V113" class="flow-err dashed" marker-end="url(#help-arrow-err)"/>
    ${svgText(348, 86, 'Start recording: copy + timestamps', 'small err', 'start')}
    ${svgBox(270, 115, 140, 46, 'name.dcap', 'in the data directory')}
    <path d="M410 138 H458" class="flow-muted anim" marker-end="url(#help-arrow-muted)"/>
    ${svgBox(460, 115, 120, 46, 'Replay', 'original / N× / fixed / max')}
    <path d="M580 138 H618" class="flow-out anim" marker-end="url(#help-arrow-out)"/>
    ${svgBox(620, 115, 130, 46, 'Outputs of B', 'any running forwarder', 'box-out')}
    ${svgText(20, 145, 'Upload a .dcap or a plain', 'small muted', 'start')}
    ${svgText(20, 160, 'NMEA log to replay it, too', 'small muted', 'start')}
    <path d="M190 150 H268" class="flow-muted dashed" marker-end="url(#help-arrow-muted)"/>
  </svg>`;
}

/* ================= mock-ups of the real screens ================= */

// Sample NMEA traffic for the inspector mock-up (valid checksums, one deliberately wrong).
const HELP_SAMPLE_MESSAGES = [
  ['09:27:50.012', '192.168.1.40:4001', '$GPGGA,092750.000,5321.6802,N,00630.3372,W,1,8,1.03,61.7,M,55.2,M,,*76\r\n'],
  ['09:27:50.013', '192.168.1.40:4001', '$GPRMC,092750.000,A,5321.6802,N,00630.3372,W,0.02,31.66,280511,,,A*43\r\n'],
  ['09:27:50.101', '192.168.1.40:4001', '$HEHDT,274.07,T*19\r\n'],
  ['09:27:50.250', '192.168.1.40:4001', '!AIVDM,1,1,,A,13aEOK?P00PD2wVMdLDRhgvL289?,0*26\r\n'],
  ['09:27:50.300', '192.168.1.40:4001', '$IIMWV,214.8,R,0.1,K,A*3F\r\n'],
];

// Rows in the same markup the real message list uses (.ir), rendered by the real NMEA view.
function helpInspectorRows() {
  const encoder = new TextEncoder();
  return HELP_SAMPLE_MESSAGES.map(([time, source, text]) => {
    const bytes = encoder.encode(text);
    return `<div class="ir"><span class="tm">${time}</span><span class="sr">${source}</span><span class="ln">${bytes.length}</span><span class="dt">${renderAsNmea(bytes, null)}</span></div>`;
  }).join('');
}

function helpInspectorMockup() {
  return `<div class="card insp" style="height:auto">
    <div class="insp-bar">
      ${helpMarker(1)}
      <div class="seg"><button>Text</button><button class="on">NMEA</button><button>Hex</button></div>
      ${helpMarker(2)}
      <div class="search">${icon('search')}<input value="" placeholder="Filter: text or /regex/"></div>
      ${helpMarker(3)}
      <select><option>100 / s</option></select><select><option>Preview 1 KB</option></select>
      <span class="grow"></span>
      ${helpMarker(4)}
      <button class="btn sm">${icon('pause')}Pause</button><button class="btn sm">${icon('x')}Clear</button><button class="btn sm">${icon('download')}</button>
    </div>
    <div class="ir ir-head"><span>Time</span><span class="sr">Source</span><span class="ln">Bytes</span><span>Data</span></div>
    <div class="insp-log">${helpInspectorRows()}</div>
    <div class="insp-foot"><span><span class="dotc" style="background:var(--ok)"></span> Live</span><span class="grow"></span>${helpMarker(5)}<span>5 received · 0 skipped by the limits</span></div>
  </div>`;
}

// The "New forwarder" dialog, built with the editor's real field markup.
function helpEditorMockup() {
  const input = { mode: 'udp-listen', host: '', port: 10110 };
  const output = { mode: 'tcp-listen', host: '', port: 10111, max_clients: 20 };
  return `<div class="modal" style="max-width:none;box-shadow:none;animation:none">
    <div class="modal-h"><h2>New forwarder</h2></div>
    <div class="modal-b">
      <div class="sect">
        <div class="grid2">
          <label class="f"><span class="row">${helpMarker(1)}Name</span><input value="GPS bridge"></label>
          <label class="f">Tags<input value="gps, nmea"></label>
        </div>
        <div class="grid2">
          <label class="f">Idle alert (s)<input value="10"><span class="hint">Alert when no data arrives for this long</span></label>
        </div>
        <label class="f">Comment<textarea rows="2">GPS gateway on the bridge, mast cable. Call Jan before restarting.</textarea></label>
        <label class="f"><span class="row">${helpMarker(2)}Data format</span>
          <div class="seg"><button type="button" class="on">NMEA / text lines</button><button type="button">Raw byte stream</button></div>
        </label>
      </div>
      <div class="sect"><div class="sect-t">${helpMarker(3)}${icon('arrowIn')}Input</div><div class="epbox">${endpointFormFields(input, 'input', 'input')}</div></div>
      <div class="sect">
        <div class="sect-t">${helpMarker(4)}${icon('arrowOut')}Outputs <span class="grow"></span><span class="row">${helpMarker(5)}<button type="button" class="btn sm">${icon('plus')}Add output</button></span></div>
        <div class="epbox"><div class="row"><b style="font-size:13px">Output 1</b></div>${endpointFormFields(output, 'outputs.0', 'output')}</div>
      </div>
    </div>
    <div class="modal-f"><button type="button" class="btn">Cancel</button><button type="button" class="btn">Create</button>${helpMarker(6)}<button type="button" class="btn primary">${icon('play')}Create &amp; start</button></div>
  </div>`;
}

// Small static sparkline like the one in the forwarder list.
function helpSparkline(values) {
  const maxValue = Math.max(...values);
  const points = values.map((value, index) => `${(index * 80 / (values.length - 1)).toFixed(1)},${(24 - value / maxValue * 20).toFixed(1)}`).join(' ');
  return `<svg class="help-spark" viewBox="0 0 80 26" preserveAspectRatio="none"><polyline points="${points}"/></svg>`;
}

function helpListMockup() {
  const healthyRow = {
    input: { mode: 'udp-listen', host: '', port: 10110 }, inStatus: { state: 'receiving' },
    outputs: [{ mode: 'tcp-listen', host: '', port: 10111 }, { mode: 'tcp-connect', host: '192.168.1.20', port: 2000 }],
    outStatus: [{ state: 'listening' }, { state: 'connected' }],
  };
  const degradedRow = {
    input: { mode: 'tcp-connect', host: '10.0.0.5', port: 4001 }, inStatus: { state: 'connected' },
    outputs: [{ mode: 'udp-send', host: '10.0.0.9', port: 10110 }, { mode: 'tcp-connect', host: '192.168.2.108', port: 10001 }],
    outStatus: [{ state: 'sending' }, { state: 'down', error: 'connection refused' }],
  };
  const endpointCells = row => `<td>${endpointBadge(row.input, row.inStatus, true)}</td>
    <td><div class="eps">${row.outputs.map((out, i) => endpointBadge(out, row.outStatus[i], true)).join('')}</div></td>`;
  return `<div class="card">
    <div class="toolbar" style="padding:12px 12px 0">
      ${helpMarker(1)}<div class="search">${icon('search')}<input placeholder="Search…"></div>
      ${helpMarker(2)}<div class="seg"><button class="on">All</button><button>Running</button><button>Issues</button><button>Stopped</button></div>
      <select><option>All tags</option></select>
    </div>
    <div class="tbl-wrap" style="margin-top:8px"><table class="t">
      <thead><tr><th style="width:34px"><input type="checkbox"></th><th>Name</th><th>Input</th><th>Outputs</th><th class="right">Traffic in</th><th class="right">Msg/s</th><th class="right">Clients in</th><th class="right">Clients out</th><th class="right">Drops</th><th></th></tr></thead>
      <tbody>
        <tr><td>${helpMarker(3)}</td>
          <td><div class="fname"><span class="dot ok"></span><div><a>GPS bridge</a><span class="note-icon" title="GPS gateway on the bridge, mast cable">${icon('note')}</span><div class="tags"><span class="tag">gps</span><span class="tag">nmea</span></div></div></div></td>
          ${endpointCells(healthyRow)}
          <td><div class="traffic">${helpSparkline([3, 4, 3, 5, 4, 6, 5, 5, 4, 6, 7, 6])}<span class="mono">2.41 kB/s</span></div></td>
          <td class="num">28</td><td class="num">1</td><td class="num">4</td><td class="num">0</td>
          <td><div class="actions"><button class="btn icon sm stop">${icon('stop')}</button><button class="btn icon sm ghost">${icon('more')}</button></div></td></tr>
        <tr><td><input type="checkbox"></td>
          <td><div class="fname"><span class="dot degraded"></span><div><a>AIS relay</a><div class="tags"><span class="tag">ais</span><span class="ferr">Output 2: connection refused</span></div></div></div></td>
          ${endpointCells(degradedRow)}
          <td><div class="traffic">${helpSparkline([2, 2, 3, 2, 4, 3, 2, 3, 3, 2, 3, 2])}<span class="mono">812 B/s</span></div></td>
          <td class="num">9</td><td class="num">1</td><td class="num">1</td><td class="num bad">1.2k</td>
          <td><div class="actions">${helpMarker(4)}<button class="btn icon sm stop">${icon('stop')}</button><button class="btn icon sm ghost">${icon('more')}</button></div></td></tr>
      </tbody>
    </table></div>
  </div>`;
}

// Header, flow diagram and tiles of the detail page.
function helpDetailMockup() {
  const node = (cssClass, mode, title, address, statePill, meta, errorLine = '') => `<div class="node ${cssClass}">
    <div class="top"><span class="ep"><span class="m">${MODE_LABELS[mode]}</span></span><span class="ttl">${title}</span><span class="grow"></span>${statePill}</div>
    <div class="addr">${address}</div><div class="meta">${meta}</div>${errorLine}</div>`;
  return `
    <div class="page-head" style="margin-bottom:14px">
      <span class="dot ok"></span>
      <div class="grow"><h1>GPS bridge <span class="pill ok" style="vertical-align:3px;margin-left:6px">Running</span></h1>
        <div class="sub"><span class="tag">gps</span> · NMEA / line framing · running for 3h 12m</div></div>
      ${helpMarker(1)}
      <div class="row"><button class="btn stop">${icon('stop')}Stop</button><button class="btn">${icon('restart')}Restart</button><button class="btn">${icon('edit')}Edit</button></div>
    </div>
    <div class="row" style="margin-bottom:6px">${helpMarker(2)}<span class="muted" style="font-size:12.5px">live flow diagram</span></div>
    <div class="flow">
      ${node('ok', 'udp-listen', 'Input', '*:10110', '<span class="pill ok">receiving</span>', '<span><b>41.2 MB</b> received</span><span><b>1</b> sources</span>')}
      <div class="wire on"><svg viewBox="0 0 70 100" preserveAspectRatio="none"><path d="M0 50 C 35 50, 35 25, 70 25" vector-effect="non-scaling-stroke"/><path d="M0 50 C 35 50, 35 75, 70 75" vector-effect="non-scaling-stroke"/></svg></div>
      <div class="outs">
        ${node('ok', 'tcp-listen', 'Output 1', '*:10111', '<span class="pill ok">listening</span>', '<span><b>164 MB</b> sent</span><span><b>4</b> clients</span>')}
        ${node('bad', 'tcp-connect', 'plotter', '192.168.1.20:2000', '<span class="pill warn">down</span>', '<span><b>2</b> reconnects</span><span style="color:var(--warn)"><b>312</b> buffered</span>', '<div class="err">connection refused</div>')}
      </div>
    </div>
    <div class="row" style="margin-bottom:6px">${helpMarker(3)}<span class="muted" style="font-size:12.5px">live numbers</span></div>
    <div class="tiles" style="margin-bottom:12px">
      <div class="tile"><div class="k">${icon('arrowIn')}Inbound</div><div class="v">2.41 kB/s</div></div>
      <div class="tile"><div class="k">${icon('arrowOut')}Outbound</div><div class="v">9.64 kB/s</div></div>
      <div class="tile"><div class="k">${icon('activity')}Messages</div><div class="v">28<small>/s</small></div></div>
      <div class="tile"><div class="k">${icon('arrowIn')}Clients in</div><div class="v">1</div></div>
      <div class="tile"><div class="k">${icon('arrowOut')}Clients out</div><div class="v">4</div></div>
      <div class="tile"><div class="k">${icon('alert')}Dropped</div><div class="v">0</div></div>
    </div>
    <div class="row">${helpMarker(4)}<div class="tabs" style="margin:0;flex:1"><button class="on">Traffic</button><button>Inspector</button><button>Connections</button><button>Events</button><button>Captures</button><button>Configuration</button></div></div>`;
}

function helpRecordingMockup() {
  return `<div class="card"><div class="card-b">
    <div class="row wrap" style="align-items:flex-end;gap:12px">
      ${helpMarker(1)}
      <label class="f">Stop after (seconds)<input placeholder="no limit" value="600" style="width:150px"></label>
      <label class="f">Max size (MB)<input placeholder="no limit" style="width:150px"></label>
      ${helpMarker(2)}<button class="btn primary">${icon('record')}Start recording</button>
    </div></div></div>`;
}

function helpReplayMockup() {
  return `<div class="modal small" style="box-shadow:none;animation:none;margin:0 auto">
    <div class="modal-h"><h2>Replay capture</h2></div>
    <div class="modal-b">
      <div class="muted mono" style="font-size:12.5px">GPS-bridge-20261030-092750.dcap</div>
      <label class="f"><span class="row">${helpMarker(1)}Into forwarder</span><select><option>Test bench</option></select></label>
      <label class="f"><span class="row">${helpMarker(2)}Timing</span>
        <div class="seg"><button type="button" class="on">Original</button><button type="button">Fixed interval</button><button type="button">As fast as possible</button></div></label>
      <div class="grid2">
        <label class="f">Speed<input value="10"><span class="hint">1 = real time, 10 = ten times faster</span></label>
        <label class="chk" style="align-self:end;margin-bottom:8px">${helpMarker(3)}<input type="checkbox" checked> Loop forever</label>
      </div>
    </div>
    <div class="modal-f"><button type="button" class="btn">Cancel</button><button type="button" class="btn primary">${icon('play')}Start replay</button></div>
  </div>`;
}

function helpAlertSettingsMockup() {
  return `<div class="card" style="max-width:560px">
    <div class="card-h">${icon('bell')}<h2>Alerts</h2></div>
    <div class="card-b stack">
      <label class="f"><span class="row">${helpMarker(1)}Webhook URL</span><input value="https://ntfy.sh/my-boat-alerts"></label>
      <label class="f"><span class="row">${helpMarker(2)}Alert when a forwarder is down for (s)</span><input value="30" style="max-width:160px"></label>
      <div class="row"><button class="btn primary">Save</button>${helpMarker(3)}<button type="button" class="btn">Send test</button></div>
    </div></div>`;
}

// A recipe's setup as a compact flow: input node, the delta logo as "fan out", output nodes.
function helpMiniFlow(input, outputs) {
  const node = (endpoint, label) => `<div class="node off"><div class="top"><span class="ep"><span class="m">${MODE_LABELS[endpoint.mode]}</span></span><span class="ttl">${escapeHtml(label)}</span></div><div class="addr">${escapeHtml(allEndpointAddresses(endpoint).join('  ·  '))}</div></div>`;
  return `<div class="help-miniflow">
    ${node(input, input.label)}
    <div class="help-miniflow-fan" title="Delta copies every message to each output">${LOGO_SVG}</div>
    <div class="outs">${outputs.map(output => node(output, output.label)).join('')}</div>
  </div>`;
}

// Forwarder JSON as it appears in the config file and the REST API.
function helpForwarderJson(name, input, outputs, extra = {}) {
  const strip = ({ label, ...endpoint }) => endpoint; // `label` is only for the picture
  return JSON.stringify({ name, framing: 'lines', ...extra, input: strip(input), outputs: outputs.map(strip) }, null, 2);
}

/* ================= the sections ================= */

// [id, title, icon, group, render function]. Groups become headings in the table of contents.
const HELP_SECTIONS = [
  ['overview', 'What Delta does', 'info', 'Start here', () => `
    <p>Delta receives data on an <b>input</b> and sends a copy of every message to <b>one or more outputs</b>,
      like a river that splits into the channels of a delta. An input can receive from <b>many senders at once</b>:
      several devices connecting to a TCP port, several devices Delta connects to, or UDP datagrams from any number of hosts.
      Their messages are merged into one stream. It was built for NMEA 0183 (GPS, AIS, compass, wind …),
      but works with any TCP or UDP data, including binary streams.</p>
    ${helpFigure(helpOverviewDiagram(), 'One <b>forwarder</b> = an input + its outputs. The input uses <b>one</b> mode: either senders connect to Delta (Delta is the server), or Delta connects to one or more senders (Delta is the client); either way it can have many senders. Each output can use a different mode: clients connect to Delta, Delta connects to targets, or Delta sends UDP datagrams. Every message is copied once and shared by all outputs; every receiver has its own queue, so a slow one never holds up the others.')}
    <h3>The words used in Delta</h3>
    <table class="t compact help-table">
      <thead><tr><th>Term</th><th>Meaning</th></tr></thead>
      <tbody>
        <tr><td><b>Forwarder</b></td><td>An input and its outputs. The input can have many senders. You can run as many forwarders as you like, independently of each other.</td></tr>
        <tr><td><b>Input</b></td><td>Where the data comes from: a port Delta listens on, or one or more devices Delta connects to. ${helpLink('modes', 'Endpoint modes')}</td></tr>
        <tr><td><b>Output</b></td><td>Where a copy of the data goes: clients that connect to Delta, one or more devices Delta connects to, or UDP datagrams.</td></tr>
        <tr><td><b>Message</b></td><td>One unit of data. With the <i>NMEA / text lines</i> format a message is one sentence; with <i>raw</i> it is whatever arrived in one network read. ${helpLink('framing', 'Data format')}</td></tr>
        <tr><td><b>Client / source</b></td><td>A program or device that exchanges data with Delta: either it connected to one of Delta's ports, or Delta connected to it. <i>Clients in</i> and <i>Clients out</i> count them per side.</td></tr>
        <tr><td><b>Drop</b></td><td>A message thrown away because a receiver was too slow and its queue was full. ${helpLink('robustness', 'Queues and drops')}</td></tr>
        <tr><td><b>Capture</b></td><td>A recording of the input with timestamps (a <code>.dcap</code> file) that you can inspect, download and replay. ${helpLink('record-replay', 'Record and replay')}</td></tr>
      </tbody>
    </table>
    <h3>Good to know</h3>
    <ul>
      <li>Everything is stored in <b>one config file</b> (<code>delta.json</code>). Forwarders that were running come back by themselves after a restart or reboot.</li>
      <li>Forwarding is fast: about 31 µs from input to output on a local machine. The web interface never slows it down.</li>
      <li>Connections that break are retried automatically. You do not need to restart anything after a device reboots.</li>
      <li>The event log is written to a file (<code>delta-events.log</code> next to the config file, set in <a href="#/settings">Settings</a>), so it survives a restart. The traffic charts are kept in memory only and start empty after Delta restarts.</li>
    </ul>`],

  ['first-forwarder', 'Tutorial: your first forwarder', 'play', 'Start here', () => `
    <p>Goal: a GPS gateway sends NMEA sentences as UDP to port <code>10110</code>. Several navigation apps should be able to
      connect to Delta over TCP on port <code>10111</code> and all get the same data.</p>
    ${helpFigure(helpEditorMockup(), 'The “New forwarder” dialog. The numbers match the steps below.', true)}
    ${helpSteps([
      ['Open the dialog', `On the <a href="#/">Forwarders</a> page click <b>New forwarder</b> (or press ${helpKey('N')}). Give it a <b>name</b> ${helpMarker(1)} you will recognize later. Tags are optional and help when you have many forwarders. The <b>comment</b> holds free-text notes, such as where the device is or who to ask; it is shown on the detail page and as a ${icon('note')} icon in the list.`],
      ['Choose the data format', `${helpMarker(2)} Keep <b>NMEA / text lines</b> for NMEA and any other line-based text. Choose <b>Raw byte stream</b> only for binary data. ${helpLink('framing', 'Why this matters')}`],
      ['Set up the input', `${helpMarker(3)} Mode <b>UDP listen</b>, port <code>10110</code>. Keep <i>Bind address</i> at <b>All interfaces</b> to receive on every network; the dropdown also lists each IP address of the Delta machine.`],
      ['Set up the output', `${helpMarker(4)} Mode <b>TCP server</b> (tcp-listen), port <code>10111</code>. Optionally limit the number of clients. Use ${helpMarker(5)} <b>Add output</b> to send the same data to more places, for example a plotter.`],
      ['Create and start', `${helpMarker(6)} <b>Create &amp; start</b> saves the forwarder and starts it at once. Delta opens its detail page. If it cannot start, for example because the port is already in use, it is still saved: a message shows the reason, the detail page shows a red <b>Start failed</b> banner, and Delta tries again every 15 s. Change the port with <b>Edit</b>, or stop the program that uses it. (<b>Create</b> without “start” only saves it.)`],
      ['Check that data flows', 'On the detail page the line between input and outputs is animated while data arrives (on wide screens), and the <b>Inbound</b> tile shows the data rate. Open the <b>Inspector</b> tab to see the sentences themselves.'],
      ['Connect your app', 'In your navigation app add a TCP connection to <code>&lt;Delta machine IP&gt;:10111</code>. It appears on the <b>Connections</b> tab.'],
    ])}
    <div class="row" style="margin:6px 0 4px">${helpTryButton('new-forwarder', 'Try it: open the New forwarder dialog', 'Opens the real dialog. Nothing is saved until you click Create')}</div>
    ${helpNote('warn', `<b>Works on this machine but not from others?</b> Usually a firewall blocks the port. See ${helpLink('troubleshooting', 'Troubleshooting')}.`)}`],

  ['ui-tour', 'A tour of the interface', 'layers', 'Start here', () => `
    <h3>The Forwarders page</h3>
    <p>The start page lists all forwarders with their live state. The tiles at the top add up all forwarders; the red number next to
      <b>Forwarders</b> in the sidebar counts the ones with a problem.</p>
    ${helpFigure(helpListMockup(), 'The forwarder list. Green dot: everything works. Orange: running, but an endpoint is not (fully) connected. Red: could not start. Grey: stopped.', true)}
    ${helpSteps([
      ['Search', `Finds names, tags, comments, modes and addresses. Press ${helpKey('/')} to jump into the search field.`],
      ['Filter', '<b>Issues</b> shows only forwarders that failed, have a disconnected endpoint or a firing alert. Click a tag chip to filter by that tag.'],
      ['Select', 'Tick the boxes to get a bar with bulk actions: start, stop, restart, tag, export, delete.'],
      ['Act', 'The square button stops a running forwarder (or one that failed to start and keeps retrying), the triangle starts a stopped one. <b>⋯</b> opens edit, duplicate, export and delete. Click the name for the detail page.'],
    ])}
    <p>Click a column header to sort by it (Name, Traffic in, Msg/s, Clients in/out, Drops); click again to reverse. Numbers sort largest first.
      The small line in <i>Traffic in</i> shows the last 60 seconds, collected while the page is open.
      <b>Clients in</b> counts the sources connected to the input (TCP clients, UDP senders heard from in the last minute, or the devices Delta connected to);
      <b>Clients out</b> counts the receivers connected to the outputs. A ${icon('note')} icon next to the name means the forwarder has a comment: hover it to read it.</p>
    <h3>The detail page of a forwarder</h3>
    ${helpFigure(helpDetailMockup(), 'Top of the detail page: actions, the live flow diagram, live numbers and the tabs.', true)}
    ${helpSteps([
      ['Actions', '<b>Stop</b>/<b>Start</b>, <b>Restart</b> (re-opens all connections) and <b>Edit</b>. Saving changes to a running forwarder restarts it.'],
      ['Flow diagram', 'The comment of the forwarder (if any) is shown above it. One box per endpoint with its live state, bytes, number of clients, reconnects and buffered messages. An orange box shows the last error. Warnings above the diagram explain what is wrong.'],
      ['Tiles', 'Data rate in and out, messages per second, clients on the input and on the outputs, drops and when the last data arrived. Outbound is higher than inbound when several outputs or clients get a copy.'],
      ['Tabs', `<b>Traffic</b>: chart of the last 10 minutes (bytes or messages; hover for values). <b>Inspector</b>: the live data (${helpLink('inspect', 'tutorial')}). <b>Connections</b>: every connected client, source and remote host; TCP connections have a <i>Disconnect</i> button (UDP senders have no connection to close). <b>Events</b>: this forwarder's log. <b>Captures</b>: recording (${helpLink('record-replay', 'tutorial')}). <b>Configuration</b>: summary and JSON to copy.`],
    ])}
    <h3>Other pages</h3>
    <ul>
      <li><a href="#/events">Events</a>: one log of everything: starts, stops, connects, disconnects, errors and alerts. Filter by level (<span class="pill info">info</span> normal, <span class="pill warn">warn</span> Delta handles it itself, <span class="pill error">error</span> needs attention) or search. The newest 5000 events are shown. Every event is also appended to the event log file (one syslog-style text line per event, rotated at a set size; see <a href="#/settings">Settings</a>), loaded again after a restart, and written to Delta's own output (e.g. the systemd journal).</li>
      <li><a href="#/captures">Captures</a>: all recordings, uploads and running replays.</li>
      <li><a href="#/settings">Settings</a>: alerts, event log file, password, backup, and where the config file is.</li>
    </ul>
    <h3>Hints and shortcuts</h3>
    <p>Hover over any button, column header, tile or badge to get a short explanation.</p>
    <table class="t compact help-table">
      <thead><tr><th>Key</th><th>Where</th><th>What it does</th></tr></thead>
      <tbody>
        <tr><td>${helpKey('N')}</td><td>Forwarders</td><td>New forwarder</td></tr>
        <tr><td>${helpKey('/')}</td><td>Forwarders</td><td>Focus the search</td></tr>
        <tr><td>${helpKey('?')}</td><td>Everywhere</td><td>Open this help</td></tr>
        <tr><td>${helpKey('Esc')}</td><td>Dialogs, menus</td><td>Close</td></tr>
      </tbody>
    </table>
    <p>The sun/moon button at the bottom of the sidebar switches between light and dark theme.</p>`],

  ['recipes', 'Common setups', 'copy', 'Tutorials', () => {
    const recipes = [
      ['Share one UDP source with many TCP apps',
        'A WiFi or serial gateway broadcasts NMEA as UDP. Apps such as OpenCPN or a tablet app prefer to connect over TCP.',
        { mode: 'udp-listen', host: '', port: 10110, label: 'Gateway' },
        [{ mode: 'tcp-listen', host: '', port: 10111, label: 'Apps connect here', max_clients: 20 }]],
      ['Fetch from an NMEA server and push to a plotter',
        'The multiplexer is a TCP server; the plotter is a TCP server too. Delta connects to both and reconnects whenever one of them reboots. While the plotter is off, Delta buffers the newest messages.',
        { mode: 'tcp-connect', host: '192.168.1.40', port: 4001, label: 'Multiplexer' },
        [{ mode: 'tcp-connect', host: '192.168.1.20', port: 2000, label: 'Plotter' }, { mode: 'tcp-listen', host: '', port: 10111, label: 'Others' }]],
      ['Collect from devices and send into another network',
        'Devices connect to Delta and send their data; Delta sends every sentence as a UDP datagram to a host in another network (for example over a VPN).',
        { mode: 'tcp-listen', host: '', port: 5000, label: 'Devices connect', max_clients: 5 },
        [{ mode: 'udp-send', host: '10.8.0.1', port: 10110, label: 'Remote' }]],
      ['Subscribe to a UDP service',
        'Some servers only send UDP to clients that registered with a “hello” datagram. Delta sends it at start and repeats it, so the server keeps sending.',
        { mode: 'udp-connect', host: 'ais.example.org', port: 5631, hello: 'HELLO\\r\\n', hello_interval: 30, label: 'AIS service' },
        [{ mode: 'tcp-listen', host: '', port: 10112, label: 'Apps connect here' }]],
    ];
    return `<p>Five typical ways to use Delta. Each one shows the setup and the JSON you could put in the config file or send to the
      ${helpLink('api', 'REST API')}. In the editor, pick the same modes and ports.</p>
      ${recipes.map(([title, text, input, outputs], index) => `
        <h3>${index + 1}. ${title}</h3>
        <p>${text}</p>
        ${helpFigure(helpMiniFlow(input, outputs), '')}
        <details class="help-details"><summary title="Show the configuration of this setup as JSON">Show as JSON</summary>${helpCode(helpForwarderJson(title, input, outputs))}</details>`).join('')}
      <h3>5. Several TCP sources or targets in one forwarder</h3>
      <p>In <b>TCP client</b> mode the editor has an <b>Add host</b> button below the host field. For an input, Delta connects to every
        listed host and merges what they send; for an output, every host gets its own copy. The flow diagram lists each host with its own state.
        If only some hosts are connected, the endpoint shows <span class="pill warn">partial</span> and the forwarder counts as degraded.</p>
      ${helpFigure(helpMiniFlow({ mode: 'tcp-connect', host: '192.168.1.40', port: 4001, label: 'GPS + AIS', targets: [{ host: '192.168.1.41', port: 4001 }] },
        [{ mode: 'tcp-connect', host: '192.168.1.20', port: 2000, label: 'Plotters', targets: [{ host: '192.168.1.21', port: 2000 }] }]), '')}
      <details class="help-details"><summary title="Show the configuration of this setup as JSON">Show as JSON</summary>${helpCode(helpForwarderJson('Merge two sources, feed two plotters',
        { mode: 'tcp-connect', host: '192.168.1.40', port: 4001, targets: [{ host: '192.168.1.41', port: 4001 }] },
        [{ mode: 'tcp-connect', host: '192.168.1.20', port: 2000, targets: [{ host: '192.168.1.21', port: 2000 }] }]))}</details>
      ${helpNote('info', 'Tip: once one forwarder works, use <b>⋯ → Duplicate</b> to create similar ones; only change the ports.')}`;
  }],

  ['inspect', 'Tutorial: watch live data', 'eye', 'Tutorials', () => `
    <p>The <b>Inspector</b> tab of a forwarder shows the messages its input receives, live. Use it to check that a device sends what you expect,
      to find wrong checksums, or to watch a single sentence type.</p>
    ${helpFigure(helpInspectorMockup(), 'The Inspector in NMEA view. Each sentence is split into talker (blue), type and fields; the badge at the end checks the checksum. The last line has a wrong checksum.', true)}
    ${helpSteps([
      ['Pick a view', `<b>Text</b> shows readable text with invisible bytes as dim symbols (␍ ␊ ␉ or \\xHH). <b>NMEA</b> colors the parts of every sentence and checks the checksum: <span class="nm" style="display:inline-flex"><span class="ck ok">✓</span></span> correct, <span class="nm" style="display:inline-flex"><span class="ck bad">✗</span></span> wrong (hover it to see the expected value). <b>Hex</b> shows every byte, for binary data.`],
      ['Filter', `Type text to show only messages that contain it (not case-sensitive). Write <code>/…/</code> for a regular expression, for example <code>/^\\$GP(RMC|GGA)/</code> for position sentences only, or <code>/^!AIVD/</code> for AIS; add <code>i</code> after the last slash to ignore case. Matches are highlighted. The filter works in your browser on the messages that passed the limits (step 3), including the last 2000 already received. On a busy stream a rare sentence can be skipped before the filter sees it: raise the limit, or record a capture and search it.`],
      ['Limit what the browser gets', 'The rate box (e.g. <i>100 / s</i>) sets how many messages per second are sent to the browser (10–2000); on top of that the server sends at most 256 KB of data per second. Messages above these limits are not shown, only counted as “skipped”. <b>Preview</b> sets how much of each message is transferred (256 B–16 KB); a longer message is shortened and marked “(first …)”.'],
      ['Pause, clear, save', 'Pause freezes the view so you can read it; new messages are still collected and appear when you resume. Clear empties the list. The download button saves the kept messages (the last 2000, only those matching the filter) as a text file with time, source and content.'],
      ['Read the counters', 'The footer shows how many messages arrived and how many were skipped by the limits.'],
    ])}
    ${helpNote('ok', '<b>The Inspector never slows down forwarding.</b> Only copies are shown, and when the browser cannot keep up, the server leaves out inspector data, never forwarded data. Replayed messages appear here too, with the source <code>replay</code>.')}
    <p>Long messages (more than 400 characters, or 64 bytes in Hex view) are collapsed; click <b>… more bytes</b> to expand one.</p>`],

  ['record-replay', 'Tutorial: record and replay', 'disc', 'Tutorials', () => `
    <p>A <b>capture</b> records everything an input receives, with the exact time of every message. Later you can look through it,
      download it, or <b>replay</b> it into any running forwarder, as if the data were arriving live. Typical uses: reproduce a problem,
      test a plotter at the dock with data from the last trip, or feed a test system.</p>
    ${helpFigure(helpCaptureDiagram(), 'Recording copies the input into a file without affecting the outputs. A replay sends a file into the outputs of a running forwarder.')}
    <h3>Record</h3>
    ${helpFigure(helpRecordingMockup(), 'The Captures tab of a forwarder.', true)}
    ${helpSteps([
      ['Set limits (optional)', 'Open the forwarder, tab <b>Captures</b>. Enter a duration and/or a maximum size; the recording stops by itself when one is reached. Leave both empty to record until you stop it.'],
      ['Start recording', 'The forwarder must be running, and it can record one capture at a time. A red <span class="pill error">REC</span> badge appears in the list, and a banner on the detail page shows the number of messages and bytes. Click <b>Stop</b> in the banner (or <b>Stop recording</b> on the tab) when you are done. Stopping or restarting the forwarder, also by saving a changed configuration, ends the recording.'],
    ])}
    <p>The file is called <code>&lt;forwarder name&gt;-&lt;date&gt;-&lt;time&gt;.dcap</code> and is stored in the <code>captures</code> folder of the data directory (see Settings → About).
      A recording contains everything that passes through the forwarder's input, including messages of a replay into it.</p>
    <h3>Look at a capture</h3>
    <p>On the <a href="#/captures">Captures</a> page click <b>Inspect</b> (or the file name). The capture viewer has the same Text / NMEA / Hex views as the Inspector.
      More records load while you scroll down. The <b>search</b> runs on the server through the <i>whole</i> file, so it also finds
      messages that are not loaded yet; <code>/gga/i</code> searches with a regular expression, ignoring case. The time column shows the time since the start of the recording, e.g. <code>+02:15.250</code> (minutes:seconds.milliseconds).</p>
    <h3>Replay</h3>
    ${helpFigure(helpReplayMockup(), 'The replay dialog.', true)}
    ${helpSteps([
      ['Choose a forwarder', 'Click <b>Replay</b> next to a capture. Only running forwarders are offered, and each runs one replay at a time. The messages are handled as if the input had received them: they go to all <i>outputs</i>, show up in the Inspector (source <code>replay</code>), count as inbound traffic and keep an idle alert quiet. The forwarder\'s own input keeps working at the same time.'],
      ['Choose the timing', '<b>Original</b> keeps the recorded gaps between messages; <i>Speed</i> 10 plays ten times faster, 0.5 half as fast. <b>Fixed interval</b> sends one message every N milliseconds. <b>As fast as possible</b> sends without pauses (useful for load tests).'],
      ['Loop', 'Starts over at the end until you stop it. Running replays are listed at the top of the Captures page and in a banner on the forwarder, each with a Stop button. Stopping or restarting the forwarder ends the replay too.'],
    ])}
    <h3>Download and upload</h3>
    <ul>
      <li><b>.dcap</b> keeps the timestamps (to replay later or on another Delta). <b>Raw</b> contains only the data itself, for example a plain NMEA log for other tools.</li>
      <li><b>Upload file</b> on the Captures page accepts <code>.dcap</code> files and plain text logs with one NMEA sentence per line. A text log becomes one record per line; any other file is split into records of 1 KB. Uploads have no timing (with <i>Original</i> everything would be sent at once), so replay them with a <i>fixed interval</i>. At most 256 MB per file.</li>
    </ul>
    <div class="row">${helpTryButton('open-captures', 'Go to Captures', 'Opens the Captures page')}</div>`],

  ['alerts', 'Tutorial: get alerted', 'bell', 'Tutorials', () => `
    <p>Delta can tell you when something stops working, by writing an event and calling a <b>webhook</b> (a URL that receives a
      short JSON message). Slack, Mattermost, Microsoft Teams, Discord, ntfy and most monitoring tools accept webhooks.</p>
    <h3>Two kinds of alerts</h3>
    <table class="t compact help-table">
      <thead><tr><th>Alert</th><th>Fires when …</th><th>Set where</th></tr></thead>
      <tbody>
        <tr><td><span class="pill error">down</span></td><td>for longer than <i>down after</i> seconds, the forwarder could not start, or an input/output was not (fully) connected; with several hosts one missing host is enough</td><td>Settings (for all forwarders)</td></tr>
        <tr><td><span class="pill error">idle</span></td><td>the forwarder runs, but its input received nothing for <i>idle alert</i> seconds, e.g. the GPS lost power</td><td>Per forwarder, in the editor (empty = off)</td></tr>
      </tbody>
    </table>
    ${helpFigure(helpAlertTimeline(), 'An alert fires once when its condition has lasted long enough, and resolves as soon as the condition is gone. Both moments are sent to the webhook.')}
    <h3>Set it up</h3>
    ${helpFigure(helpAlertSettingsMockup(), 'Settings → Alerts.', true)}
    ${helpSteps([
      ['Enter the webhook URL', 'For example a Slack “incoming webhook” URL, a Discord channel webhook, or simply <code>https://ntfy.sh/&lt;a-secret-topic&gt;</code> to get push notifications on your phone with the ntfy app.'],
      ['Choose the down delay', 'How long a forwarder may fail to start, or an endpoint may be disconnected, before the alert fires. Short drops (a device reboot) then do not wake you up. Default: 30 s.'],
      ['Send a test', '<b>Send test</b> posts a test message (status <code>TEST</code>) to the URL in the field right away, even before you save. If the call fails, the reason is shown; failed webhook calls also appear on the Events page. Click <b>Save</b> to keep the settings.'],
      ['Add idle alerts', `Edit each forwarder whose silence matters and set <b>Idle alert (s)</b>, for example 10 for a GPS that sends every second.`],
    ])}
    <h3>What the webhook receives</h3>
    <p>A JSON <code>POST</code>. <code>text</code> is understood by Slack, Teams and Mattermost, <code>content</code> by Discord; the other fields are for your own scripts.
      <code>status</code> is <code>FIRING</code> or <code>RESOLVED</code>; a resolved message has an empty <code>detail</code>.</p>
    ${helpCode(JSON.stringify({ text: '[Delta] FIRING: GPS bridge is idle — no input data for 12s', content: '[Delta] FIRING: GPS bridge is idle — no input data for 12s', forwarder: 'GPS bridge', id: 'f4918dc90495', alert: 'idle', status: 'FIRING', detail: 'no input data for 12s', time: '2026-10-30T09:27:50Z' }, null, 2))}
    ${helpNote('info', 'Firing alerts also appear as a red badge in the forwarder list, as a banner on the detail page and in the Events log, even without a webhook.')}`],

  ['organize', 'Tutorial: organize many forwarders', 'tag', 'Tutorials', () => `
    <p>With dozens of forwarders, tags and bulk actions keep the list manageable.</p>
    ${helpSteps([
      ['Tags', 'Keywords such as <code>gps</code>, <code>ais</code>, <code>test</code>. A forwarder can have several. Click a tag chip in the list (or use the tag dropdown) to show only forwarders with that tag.'],
      ['Bulk actions', 'Tick several forwarders (or the box in the header for all shown ones). The dark bar that appears starts, stops, restarts, tags, exports or deletes them all at once. To remove a tag from all selected, enter it with a leading minus, e.g. <code>-test</code>.'],
      ['Duplicate', '<b>⋯ → Duplicate</b> opens the editor with a copy named “… copy”; change the name and the listen ports (two forwarders cannot listen on the same port) and save.'],
      ['Export and import', '<b>Export</b> downloads forwarders as JSON (all, a selection, or one). <b>Import</b> adds the forwarders from such a file as new, stopped forwarders, so nothing starts by surprise and nothing is overwritten.'],
    ])}
    ${helpNote('info', 'Moving to a new machine: export all forwarders, install Delta there, import the file, then start the forwarders you need. Or simply copy the config file while Delta is stopped.')}`],

  ['modes', 'Endpoint modes', 'arrowIn', 'Reference', () => {
    const inputModes = [
      ['tcp-listen', 'TCP server', 'Devices or programs connect to Delta and send their data. Several sources may be connected at once; their data is merged. <i>Max clients</i> limits how many.', { leftTitle: 'Source', leftSub: 'device', rightTitle: 'Delta', rightSub: 'listens :port', connect: 'right', connectLabel: 'source connects', dataLabel: 'data', deltaSide: 'right', dataKind: 'in' }],
      ['tcp-connect', 'TCP client', 'Delta connects to a device that offers its data as a TCP server (most NMEA multiplexers and WiFi gateways). Reconnects automatically. With <b>Add host</b> Delta connects to several sources at once and merges their data, e.g. a GPS and an AIS receiver.', { leftTitle: 'Source', leftSub: 'TCP server', rightTitle: 'Delta', rightSub: 'connects', connect: 'left', connectLabel: 'Delta connects', dataLabel: 'data', deltaSide: 'right', dataKind: 'in' }],
      ['udp-listen', 'UDP listen', 'Delta receives datagrams on a port, from any sender. There is no connection; this is the usual way for gateways that broadcast NMEA.', { leftTitle: 'Source', leftSub: 'sends datagrams', rightTitle: 'Delta', rightSub: 'listens :port', connect: 'none', connectLabel: 'no connection', dataLabel: 'datagrams', deltaSide: 'right', dataKind: 'in' }],
      ['udp-connect', 'UDP client', 'For UDP services that only send to registered clients: Delta sends a <i>hello</i> datagram (escapes <code>\\r \\n \\xHH</code> allowed) and repeats it every <i>hello interval</i> seconds (default 30).', { leftTitle: 'Server', leftSub: 'UDP service', rightTitle: 'Delta', rightSub: 'sends hello', connect: 'left', connectLabel: '“hello” every N s', dataLabel: 'datagrams', deltaSide: 'right', dataKind: 'in' }],
    ];
    const outputModes = [
      ['tcp-listen', 'TCP server', 'Programs connect to Delta and each gets its own copy of the data. <i>Max clients</i> limits how many may connect. Every client has its own queue (default 1024 messages); a stuck client is disconnected after 10 s without progress.', { leftTitle: 'Delta', leftSub: 'listens :port', rightTitle: 'Client', rightSub: 'app, plotter', connect: 'left', connectLabel: 'client connects', dataLabel: 'data', deltaSide: 'left', dataKind: 'out' }],
      ['tcp-connect', 'TCP client', 'Delta connects to a device that accepts data as a TCP server. While it is unreachable, up to <i>queue</i> messages (default 10000) are kept and delivered once it is back. With <b>Add host</b> Delta sends to several targets at once; each has its own connection and buffer, so one target that is down never delays the others.', { leftTitle: 'Delta', leftSub: 'connects', rightTitle: 'Target', rightSub: 'TCP server', connect: 'right', connectLabel: 'Delta connects', dataLabel: 'data', deltaSide: 'left', dataKind: 'out' }],
      ['udp-send', 'UDP send', 'Every message is sent as one datagram to a host and port (a message larger than 64 KB, only possible with raw, is split). Use a broadcast address (e.g. 192.168.1.255) to reach the whole network.', { leftTitle: 'Delta', leftSub: 'sends', rightTitle: 'Target', rightSub: 'host:port', connect: 'none', connectLabel: 'no connection', dataLabel: 'datagrams', deltaSide: 'left', dataKind: 'out' }],
    ];
    const modeCards = modes => `<div class="help-cards">${modes.map(([mode, label, text, diagram]) => `
      <div class="help-card"><div class="row"><span class="ep"><span class="m" title="${escapeHtml(MODE_HINTS[mode])}">${MODE_LABELS[mode]}</span></span><b>${label}</b><code class="faint">${mode}</code></div>
        ${helpModeDiagram(diagram)}<p>${text}</p></div>`).join('')}</div>`;
    return `<p>Every input and output has a <b>mode</b>. The question to ask: <i>who opens the connection?</i> If the other side
        connects to Delta, use a <b>listen</b> mode; if Delta has to reach out, use a <b>connect</b> mode. Data always flows from the
        input to the outputs, whatever the direction of the connection. The dashed arrow shows who connects; the colored arrow the data.</p>
      <h3>Input modes</h3>${modeCards(inputModes)}
      <h3>Output modes</h3>${modeCards(outputModes)}
      <h3>Address fields</h3>
      <ul>
        <li><b>Bind address</b> (listen modes): a dropdown with <b>All interfaces</b> and every IP address of the Delta machine, with its interface name. Pick one address to accept connections only from that network, or <code>127.0.0.1</code> to accept only programs on the same machine.</li>
        <li><b>Host</b> (connect and send modes): name or IP of the remote device. Names are looked up again for every new connection, so DNS changes are picked up. A TCP client can have several hosts (<b>Add host</b>).</li>
        <li><b>Port 10110</b> is the usual port for NMEA over IP.</li>
      </ul>`;
  }],

  ['framing', 'Data format: lines or raw', 'activity', 'Reference', () => `
    <p>TCP delivers a stream of bytes, not messages. The network may cut a sentence anywhere, so one read can hold half a sentence
      or two and a half. The <b>data format</b> decides what Delta does with that.</p>
    ${helpFigure(helpFramingDiagram(), 'The same three network reads, forwarded with the two data formats.')}
    <table class="t compact help-table">
      <thead><tr><th>Format</th><th>Use it for</th><th>Behaviour</th></tr></thead>
      <tbody>
        <tr><td><b>NMEA / text lines</b> <code class="faint">lines</code></td><td>NMEA 0183, AIS (!AIVDM), any text protocol with one message per line</td><td>Splits at <code>\\n</code>. Receivers always get whole sentences, UDP datagrams contain whole sentences, the Inspector shows one sentence per row, and <i>Msg/s</i> counts sentences. A line longer than 8 KB is forwarded in parts; an unfinished last line is sent when its connection closes.</td></tr>
        <tr><td><b>Raw byte stream</b> <code class="faint">raw</code></td><td>Binary protocols, NMEA 2000 gateways with binary output, anything that is not line based</td><td>Bytes are passed on exactly as they arrive, without waiting for a line end. The lowest possible delay.</td></tr>
      </tbody>
    </table>
    ${helpNote('warn', 'With <b>raw</b> and a UDP output, a sentence can be split across two datagrams, which many NMEA programs do not accept. Use <b>lines</b> for NMEA.')}`],

  ['states', 'Health and states', 'alert', 'Reference', () => `
    <h3>Forwarder health</h3>
    <p>The colored dot in front of each forwarder (and the badge on its detail page):</p>
    <table class="t compact help-table">
      <thead><tr><th>Badge</th><th>Meaning</th><th>What to do</th></tr></thead>
      <tbody>
        <tr><td><span class="dot ok"></span> <span class="pill ok">Running</span></td><td>All endpoints are connected or listening.</td><td>Nothing.</td></tr>
        <tr><td><span class="dot degraded"></span> <span class="pill degraded">Degraded</span></td><td>Running, but at least one endpoint is not connected, or a TCP client with several hosts reaches only some of them (<i>partial</i>). Everything else keeps working.</td><td>Read the orange banner on the detail page; Delta keeps retrying.</td></tr>
        <tr><td><span class="dot error"></span> <span class="pill error">Error</span></td><td>Could not start: usually the port is already used by another program or forwarder, or the bind address does not exist on this machine (any more).</td><td>Change the port or bind address, or stop the other program. Delta retries every 15 s.</td></tr>
        <tr><td><span class="dot"></span> <span class="pill">Stopped</span></td><td>Stopped by you. It stays stopped after a restart of Delta.</td><td>Start it when needed.</td></tr>
      </tbody>
    </table>
    <h3>Endpoint states</h3>
    <p>Shown in the flow diagram and as a small dot in the list (hover the dot for details):</p>
    <table class="t compact help-table">
      <thead><tr><th>State</th><th>Meaning</th></tr></thead>
      <tbody>
        ${Object.entries(STATE_HINTS).map(([stateName, hint]) => `<tr><td><span class="pill ${HEALTHY_STATES.has(stateName) ? 'ok' : 'warn'}">${stateName}</span></td><td>${escapeHtml(hint)}</td></tr>`).join('')}
      </tbody>
    </table>
    <h3>Start with Delta</h3>
    <p>Delta remembers whether you left a forwarder running. After a restart of Delta or the machine, exactly those forwarders start again.
      Stopping Delta itself does not change this.</p>`],

  ['robustness', 'Queues, drops and reconnects', 'restart', 'Reference', () => `
    <p>Delta is built so that one problem never spreads: a slow receiver, a broken connection or a device that reboots only affects itself.</p>
    <h3>Every output has its own queue</h3>
    ${helpFigure(helpQueueDiagram(), 'The input hands every message to all queues and moves on immediately. Only the queue of the slow or disconnected receiver fills up.')}
    <ul>
      <li>When a queue is full, the <b>oldest</b> message is thrown away, so the receiver gets the newest data when it catches up. Thrown-away messages are counted in <b>Drops</b>.</li>
      <li>Queue size: default 10000 messages for a TCP client output (per host, if it has several) and for a UDP send output, 1024 per connected client of a TCP server output. You can change it in the editor (<i>Queue</i>).</li>
      <li>A few drops after a target was offline are normal. Drops that keep rising while everything is connected mean a receiver (or the network to it) is too slow.</li>
    </ul>
    <h3>Reconnects</h3>
    <p>When a connection fails or breaks, Delta tries again, waiting twice as long each time, from 0.2 s up to 10 s.
      Up to 20 % random jitter is added, so many forwarders do not retry in the same moment. After a connection that lasted at least
      10 s, the next attempt starts at 0.2 s again.</p>
    ${helpFigure(helpBackoffDiagram(), '')}
    <ul>
      <li>Dead connections are noticed through TCP keepalive (15 s) and write timeouts (10 s), even when no data flows.</li>
      <li>A repeated error is logged only once, so the Events page does not fill up while a device is off.</li>
      <li>Messages for a TCP client output are kept in its queue while it is down and delivered once it is back.</li>
    </ul>`],

  ['settings', 'Settings, event log, password and backup', 'sliders', 'Reference', () => `
    <h3>Password</h3>
    <ul>
      <li>On first start Delta asks you to choose a password (at least 8 characters). Until then nothing else works, and whoever opens the page first can choose it, so do it right away.</li>
      <li>Change it under <a href="#/settings">Settings → Password</a> (you need the current one). The config file stores only a salted hash, never the password.</li>
      <li><b>Forgot it?</b> Stop Delta, add <code>"password": "new-password"</code> to the config file, start Delta. It replaces the line with the hash.</li>
      <li>A login lasts 24 hours (setting <code>session_hours</code> in the config file). <b>Sign out</b> is at the bottom of the sidebar.</li>
    </ul>
    <h3>The config file</h3>
    <p>All settings and forwarders are in one JSON file (its path is shown under Settings → About). Delta saves it safely after every change.
      You may edit it by hand while Delta is stopped. Unknown keys are not accepted: Delta then refuses to start and names the problem, so typos are noticed.
      A relative <code>data_dir</code> is resolved next to the config file; captures go into its <code>captures</code> folder.</p>
    ${helpCode(`{
  "port": 8470,
  "bind": "",
  "password_hash": "pbkdf2-sha256$600000$…",
  "data_dir": "delta-data",
  "session_hours": 24,
  "event_log": "delta-events.log",
  "event_log_max_mb": 10,
  "alerts": { "webhook_url": "", "down_after": 30 },
  "forwarders": [ … ]
}`)}
    <h3>Event log file</h3>
    <p>Every event is appended to <code>event_log</code> (default <code>delta-events.log</code>, relative to the config file), one line per event in a syslog-style format
      (<code>2026-10-30T20:37:41.112+01:00 myhost delta[1234]: ERROR [f3a9c2e17b04] GPS bridge: connection refused</code>), so you can read it with <code>less</code>, <code>grep</code> or <code>tail -f</code>.
      When it reaches <code>event_log_max_mb</code> it is renamed to <code>&lt;file&gt;.1</code> and a new file is started, so the log never uses more than twice that size.
      At startup the newest 5000 events are loaded from both files. Set <code>event_log</code> to <code>off</code> to keep events in memory only.
      Both can be changed under <a href="#/settings">Settings → Event log</a> while Delta runs.</p>
    <h3>Starting Delta</h3>
    <table class="t compact help-table">
      <thead><tr><th>Option</th><th>Meaning</th></tr></thead>
      <tbody>
        <tr><td><code>-config &lt;file&gt;</code></td><td>Config file (default <code>delta.json</code>); created on first start.</td></tr>
        <tr><td><code>-port &lt;n&gt;</code></td><td>Port of the web interface (default 8470). Overrides <code>port</code> in the config file.</td></tr>
        <tr><td><code>-bind &lt;address&gt;</code></td><td>Address of the web interface, e.g. <code>127.0.0.1</code> for local access only. Overrides <code>bind</code> in the config file.</td></tr>
        <tr><td><code>-version</code></td><td>Print the version.</td></tr>
      </tbody>
    </table>
    ${helpNote('warn', 'The web interface uses plain HTTP. Outside a trusted network, put it behind a reverse proxy with HTTPS.')}`],

  ['api', 'REST API', 'download', 'Reference', () => `
    <p>Everything the web interface does is available over HTTP, for scripts and other tools. Scripts log in with HTTP basic auth: the Delta password, with any user name (usually empty, as in <code>-u :PASSWORD</code>).
      Request and response bodies are JSON; errors come back as <code>{"error": "…"}</code>.</p>
    ${helpCode(`# list all forwarders with their live state
curl -u :PASSWORD http://${location.host}/api/forwarders

# stop / start / restart one
curl -u :PASSWORD -X POST http://${location.host}/api/forwarders/<id>/stop

# create a forwarder and start it at once
curl -u :PASSWORD -X POST 'http://${location.host}/api/forwarders?start=1' \\
     -d '{"name":"ais","framing":"lines","input":{"mode":"tcp-connect","host":"10.0.0.5","port":4001},"outputs":[{"mode":"udp-send","host":"10.0.0.9","port":10110}]}'`)}
    <table class="t compact help-table">
      <thead><tr><th>Route</th><th>Purpose</th></tr></thead>
      <tbody>
        <tr><td><code>GET|POST /api/forwarders</code></td><td>List, or create (<code>?start=1</code> starts it)</td></tr>
        <tr><td><code>GET|PUT|DELETE /api/forwarders/{id}</code></td><td>Read, change, delete one forwarder</td></tr>
        <tr><td><code>POST /api/forwarders/{id}/{action}</code></td><td><code>start</code>, <code>stop</code>, <code>restart</code>, <code>capture-stop</code>, <code>replay-stop</code>;
          <code>kick</code> with <code>{"endpoint": "in" | "out:0", "peer": &lt;id&gt;}</code> (ids from <code>GET /api/forwarders/{id}</code>);
          <code>capture-start</code> with <code>{"max_seconds": 0, "max_mb": 0}</code> (0 = no limit)</td></tr>
        <tr><td><code>GET /api/forwarders/{id}/history</code></td><td>Throughput of the last 10 minutes</td></tr>
        <tr><td><code>GET /api/forwarders/{id}/inspect</code></td><td>Live messages (server-sent events)</td></tr>
        <tr><td><code>POST /api/bulk</code></td><td>One action for several forwarders: <code>{"ids": […], "action": "start|stop|restart|delete|tag|untag", "value": "…"}</code></td></tr>
        <tr><td><code>GET /api/stream</code></td><td>Live state of all forwarders and new events (server-sent events)</td></tr>
        <tr><td><code>GET /api/events</code></td><td>Event log, newest first (<code>?forwarder=&lt;id&gt;</code>, <code>?limit=</code>)</td></tr>
        <tr><td><code>GET /api/interfaces</code></td><td>IP addresses of the Delta machine (bind address choices)</td></tr>
        <tr><td><code>GET /api/export</code>, <code>POST /api/import</code></td><td>Backup and import of forwarders as <code>{"forwarders": […]}</code>; <code>?id=</code> (repeatable) exports only some</td></tr>
        <tr><td><code>GET|PUT /api/settings</code></td><td>Alert settings, event log file and password (<code>{"webhook_url", "down_after"}</code>, <code>{"event_log", "event_log_max_mb"}</code>, or <code>{"current_password", "password"}</code>)</td></tr>
        <tr><td><code>POST /api/settings/test-webhook</code></td><td>Send a test message to <code>{"url": "…"}</code></td></tr>
        <tr><td><code>GET|POST /api/captures</code></td><td>List captures, upload a file</td></tr>
        <tr><td><code>GET|DELETE /api/captures/{name}</code></td><td>Download or delete a capture</td></tr>
        <tr><td><code>GET /api/captures/{name}/records</code></td><td>Read records page by page (<code>from</code>, <code>limit</code> up to 1000, <code>bytes</code>, <code>filter</code>); <code>next</code> in the answer is the <code>from</code> of the next page, <code>-1</code> at the end of the file</td></tr>
        <tr><td><code>POST /api/captures/{name}/replay</code></td><td>Start a replay: <code>{"forwarder": "&lt;id&gt;", "speed": 1, "interval_ms": 0, "loop": false}</code> (speed and interval 0 = as fast as possible)</td></tr>
        <tr><td><code>POST /api/login</code>, <code>POST /api/logout</code>, <code>GET /api/session</code></td><td>Browser login with <code>{"password": "…"}</code> (sets a cookie); not needed with basic auth</td></tr>
      </tbody>
    </table>
    <p>The JSON of a forwarder is the same as in the config file and in exports; the <b>Configuration</b> tab of every forwarder shows it with a copy button.</p>`],

  ['troubleshooting', 'Troubleshooting', 'search', 'Reference', () => `
    <h3>A listen port works on the Delta machine, but other machines cannot connect</h3>
    <p>Almost always a firewall on the Delta machine. Check that Delta listens on all interfaces (the address shows <code>*:port</code>), then open the port:</p>
    ${helpCode(`# firewalld (Fedora, RHEL, openSUSE, many Arch setups); use /udp for UDP inputs
sudo firewall-cmd --permanent --add-port=10110/tcp
sudo firewall-cmd --reload

# ufw (Ubuntu, Debian)
sudo ufw allow 10110/tcp`)}
    <h3>An output says “host unreachable” although the device answers ping</h3>
    <p>A firewall on the <b>target</b> device rejects that port. Open it there, the same way.</p>
    <h3>“connection refused”</h3>
    <p>The target is reachable, but nothing listens on that port: wrong port, the service is off, or the device expects Delta to be the server
      (then use a <i>listen</i> mode instead). See ${helpLink('modes', 'Endpoint modes')}.</p>
    <h3>“address already in use” when starting</h3>
    <p>Another forwarder or program uses the port. On Linux, <code>ss -ltnup 'sport = :10110'</code> shows which one.</p>
    <h3>Data arrives, but the receiver shows nothing</h3>
    <ul>
      <li>Open the Inspector and check the sentences: correct checksums (✓)? Expected sentence types?</li>
      <li>For UDP outputs use the <i>NMEA / text lines</i> format, so every datagram holds whole sentences. ${helpLink('framing', 'Data format')}</li>
      <li>Check the <b>Connections</b> tab: is the receiver connected? Does its <i>Bytes</i> number grow?</li>
    </ul>
    <h3>The Drops number keeps growing</h3>
    <p>A receiver reads slower than data arrives, or was offline for a while. ${helpLink('robustness', 'Queues and drops')} explains when this is normal.</p>
    <h3>Where do I see what happened?</h3>
    <p>The <a href="#/events">Events</a> page lists every connect, disconnect, error and alert with time and forwarder.</p>`],
];

/* ================= the page ================= */

function renderHelpPage(main, initialSectionId) {
  // Table of contents, grouped by the section's group name.
  const groupNames = [...new Set(HELP_SECTIONS.map(section => section[3]))];
  const tableOfContents = groupNames.map(groupName => `
    <div class="help-toc-group">${escapeHtml(groupName)}</div>
    ${HELP_SECTIONS.filter(section => section[3] === groupName).map(([id, title, iconName]) =>
      `<a href="#/help/${id}" data-help-goto="${id}" title="Jump to “${escapeHtml(title)}”">${icon(iconName)}<span>${escapeHtml(title)}</span></a>`).join('')}`).join('');

  main.innerHTML = `${HELP_SVG_DEFINITIONS}
    <div class="page-head">
      <div class="grow"><h1>Help</h1><div class="sub">How ${APP_NAME} works, with step-by-step tutorials</div></div>
      <div class="search">${icon('search')}<input id="help-search" placeholder="Search help…" title="Show only the help sections that contain this text"></div>
    </div>
    <div class="help-layout">
      <nav class="help-toc" id="help-toc">${tableOfContents}</nav>
      <div class="help-content" id="help-content">
        ${HELP_SECTIONS.map(([id, title, iconName, , renderSection]) => `
          <section class="card help-sec" id="help-${id}" data-help-section="${id}">
            <h2><span class="help-sec-icon">${icon(iconName)}</span>${escapeHtml(title)}</h2>
            ${renderSection()}
          </section>`).join('')}
        <div class="card empty hidden" id="help-no-results"><h2>Nothing found</h2><p>Try another word, or clear the search.</p></div>
      </div>
    </div>`;

  // Mock-ups are pictures: their fields must not take keyboard focus.
  $$('.help-mock input, .help-mock select, .help-mock button', main).forEach(element => { element.tabIndex = -1; });

  const sectionElement = id => $(`#help-${CSS.escape(id)}`, main);
  function markActiveInToc(id) {
    $$('#help-toc a', main).forEach(link => link.classList.toggle('on', link.dataset.helpGoto === id));
  }
  // Scrolls to a section and puts it into the URL without re-rendering the page
  // (replaceState does not fire "hashchange").
  function goToSection(id, smooth = true) {
    const target = sectionElement(id);
    if (!target) return;
    target.scrollIntoView({ behavior: smooth ? 'smooth' : 'auto', block: 'start' });
    history.replaceState(null, '', '#/help/' + id);
    markActiveInToc(id);
  }

  // Highlights the section currently at the top of the window in the table of contents.
  const visibleSections = new Set();
  const sectionObserver = new IntersectionObserver(entries => {
    for (const entry of entries) {
      if (entry.isIntersecting) visibleSections.add(entry.target);
      else visibleSections.delete(entry.target);
    }
    const topmost = [...visibleSections].sort((a, b) => a.getBoundingClientRect().top - b.getBoundingClientRect().top)[0];
    if (topmost) markActiveInToc(topmost.dataset.helpSection);
  }, { rootMargin: '0px 0px -60% 0px' });
  $$('[data-help-section]', main).forEach(section => sectionObserver.observe(section));

  main.onclick = event => {
    const sectionLink = event.target.closest('[data-help-goto]');
    if (sectionLink) {
      event.preventDefault();
      return goToSection(sectionLink.dataset.helpGoto);
    }
    const tryAction = event.target.closest('[data-help-try]')?.dataset.helpTry;
    if (tryAction === 'new-forwarder') openForwarderEditor();
    if (tryAction === 'open-captures') location.hash = '#/captures';
  };

  // Search: hides every section that does not contain all typed words.
  main.oninput = event => {
    if (event.target.id !== 'help-search') return;
    const words = event.target.value.toLowerCase().split(/\s+/).filter(Boolean);
    let shownCount = 0;
    for (const section of $$('[data-help-section]', main)) {
      const text = section.textContent.toLowerCase();
      const matches = words.every(word => text.includes(word));
      section.classList.toggle('hidden', !matches);
      const tocLink = $(`#help-toc [data-help-goto="${section.dataset.helpSection}"]`, main);
      tocLink.classList.toggle('help-toc-dim', !matches);
      if (matches) shownCount++;
    }
    $('#help-no-results', main).classList.toggle('hidden', shownCount > 0);
  };

  if (initialSectionId) requestAnimationFrame(() => goToSection(initialSectionId, false));
  else markActiveInToc(HELP_SECTIONS[0][0]);

  return { destroy: () => sectionObserver.disconnect() };
}
