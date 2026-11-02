package api

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"delta/web"
)

// The embedded help.md renders, and every link to a help section
// (#/help/<id>) points to a heading that exists.
func TestHelpRendersAndLinksResolve(t *testing.T) {
	rendered, err := renderHelp(web.Files)
	if err != nil {
		t.Fatal(err)
	}
	headingIDs := map[string]bool{}
	for _, match := range regexp.MustCompile(`<h[23] id="([^"]+)"`).FindAllStringSubmatch(rendered, -1) {
		headingIDs[match[1]] = true
	}
	if len(headingIDs) < 10 {
		t.Fatalf("only %d headings found", len(headingIDs))
	}
	for _, match := range regexp.MustCompile(`href="#/help/([^"]+)"`).FindAllStringSubmatch(rendered, -1) {
		if !headingIDs[match[1]] {
			t.Errorf("link to #/help/%s, but there is no heading with that id", match[1])
		}
	}
	// Every image in help.md must be embedded (web/help/…), or the help shows a broken picture.
	for _, match := range regexp.MustCompile(`<img src="([^"]+)"`).FindAllStringSubmatch(rendered, -1) {
		if _, err := fs.Stat(web.Files, match[1]); err != nil {
			t.Errorf("help.md shows %s, but that file is not embedded: %v", match[1], err)
		}
	}
	if strings.Contains(rendered, "<!-- raw HTML omitted -->") {
		t.Error("help.md contains raw HTML, which is not shown; use plain Markdown")
	}
}
