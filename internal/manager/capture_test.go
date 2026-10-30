package manager

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Reading a capture page by page returns every record exactly once, in order,
// and the filter only returns matching records.
func TestCapturePaging(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{dataDir: dir, instances: map[string]*Instance{}}
	os.MkdirAll(m.captureDir(), 0o750)
	var content strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&content, "$GPRMC,%06d,A*00\r\n", i)
		if i%100 == 0 {
			content.WriteString("$GPGGA,special\r\n")
		}
	}
	name, err := m.ImportCapture("log.txt", strings.NewReader(content.String()))
	if err != nil {
		t.Fatal(err)
	}

	var all []string
	for from := int64(0); from >= 0; {
		page, err := m.ReadCapturePage(name, from, 333, 1024, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range page.Records {
			all = append(all, string(record.Data))
		}
		from = page.Next
	}
	if len(all) != 1010 || all[0] != "$GPRMC,000000,A*00\r\n" || all[1] != "$GPGGA,special\r\n" || all[1009] != "$GPRMC,000999,A*00\r\n" {
		t.Fatalf("paging returned %d records, first %q last %q", len(all), all[0], all[len(all)-1])
	}

	for _, filter := range []string{"SPECIAL", "/^\\$GPGGA/", "/special/i"} {
		matches, err := ParseRecordFilter(filter)
		if err != nil {
			t.Fatal(err)
		}
		page, err := m.ReadCapturePage(name, 0, 1000, 1024, matches)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Records) != 10 || page.Next != -1 {
			t.Fatalf("filter %q: got %d records, next %d", filter, len(page.Records), page.Next)
		}
	}
	if _, err := ParseRecordFilter("/[unclosed/"); err == nil {
		t.Fatal("invalid regex must be rejected")
	}
	if _, err := m.ReadCapturePage("../../etc/passwd", 0, 10, 10, nil); err == nil {
		t.Fatal("path traversal must be rejected")
	}
}
