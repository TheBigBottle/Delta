package api

// The help page. Its text is the plain Markdown file web/help.md, embedded
// into the binary like the rest of the web interface. It is rendered to HTML
// once, on first use, with goldmark. To change the help, edit help.md and
// rebuild; nothing else needs to change.

import (
	"bytes"
	"io/fs"
	"net/http"
	"sync"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"

	"delta/web"
)

// markdownRenderer understands GitHub-flavored Markdown (tables, autolinks …)
// and gives every heading an id made from its text ("## Endpoint modes" ->
// id "endpoint-modes"), so the help can link to it as #/help/endpoint-modes.
// Raw HTML in the Markdown is not passed through: the help is plain text.
var markdownRenderer = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
)

// helpHTML renders help.md once; the result never changes while Delta runs.
var helpHTML = sync.OnceValues(func() (string, error) {
	return renderHelp(web.Files)
})

// renderHelp turns help.md from files into HTML.
func renderHelp(files fs.FS) (string, error) {
	source, err := fs.ReadFile(files, "help.md")
	if err != nil {
		return "", err
	}
	var rendered bytes.Buffer
	if err := markdownRenderer.Convert(source, &rendered); err != nil {
		return "", err
	}
	return rendered.String(), nil
}

// handleHelp returns the rendered help page as {"html": "…"}.
func (s *Server) handleHelp(w http.ResponseWriter, r *http.Request) {
	rendered, err := helpHTML()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"html": rendered})
}
