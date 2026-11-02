'use strict';

/*
 * Delta help page (#/help, #/help/<heading id>).
 *
 * The text is web/help.md, plain Markdown, rendered by the server
 * (GET /api/help). This file only shows it: a table of contents built from
 * the "##" headings on the left, the text on the right. To change the help,
 * edit help.md; nothing here needs to change.
 *
 * Links in help.md like [Endpoint modes](#/help/endpoint-modes) change the
 * URL; the router then calls renderHelpPage again, which scrolls there.
 *
 * Loaded before app.js; uses its helpers (apiRequest, escapeHtml, $, $$)
 * only when the page is rendered.
 */

// The help never changes while Delta runs, so it is loaded once per page load.
let helpHtmlPromise = null;
const loadHelpHtml = () => (helpHtmlPromise ??= apiRequest('GET', '/api/help').then(response => response.html)
  .catch(error => { helpHtmlPromise = null; throw error; })); // try again next time

function renderHelpPage(main, headingId) {
  main.innerHTML = `<div class="page-head"><div class="grow"><h1>Help</h1><div class="sub">Loading…</div></div></div>`;
  let destroyed = false;

  loadHelpHtml().then(html => {
    if (destroyed) return;
    main.innerHTML = `
      <div class="page-head"><div class="grow"><h1>Help</h1><div class="sub">How ${APP_NAME} works</div></div></div>
      <div class="help-layout">
        <nav class="help-toc" id="help-toc"></nav>
        <article class="card help-doc" id="help-doc">${html}</article>
      </div>`;
    // Table of contents: one link per "##" heading.
    $('#help-toc', main).innerHTML = $$('#help-doc h2', main).map(heading =>
      `<a href="#/help/${escapeHtml(heading.id)}" title="Jump to “${escapeHtml(heading.textContent)}”">${escapeHtml(heading.textContent)}</a>`).join('');
    if (headingId) document.getElementById(headingId)?.scrollIntoView({ block: 'start' });
  }).catch(error => {
    if (!destroyed) main.innerHTML = `<div class="card empty"><h2>The help could not be loaded</h2><p>${escapeHtml(error.message)}</p></div>`;
  });

  return { destroy: () => { destroyed = true; } };
}
