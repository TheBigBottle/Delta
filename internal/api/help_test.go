package api

import (
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
	if strings.Contains(rendered, "<!-- raw HTML omitted -->") {
		t.Error("help.md contains raw HTML, which is not shown; use plain Markdown")
	}
}
