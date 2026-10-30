package manager

import (
	"encoding/json"
	"net"
	"strings"
	"testing"

	"delta/internal/config"
)

// freeTestPort returns a TCP port that is free on the loopback interface.
func freeTestPort(t testing.TB) int {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// Regression test: a forwarder created without tags must still have a "tags"
// list in its JSON, otherwise the web interface cannot render the list.
func TestCreatedForwarderHasTagList(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(dir + "/delta.json")
	if err != nil {
		t.Fatal(err)
	}
	m := New(dir+"/delta.json", cfg)
	status, err := m.Create(config.ForwarderConfig{
		Name:    "no tags",
		Input:   config.EndpointConfig{Mode: "udp-listen", Port: freeTestPort(t)},
		Outputs: []config.EndpointConfig{{Mode: "udp-send", Host: "127.0.0.1", Port: 9}},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{status, m.List()[0]} {
		data, _ := json.Marshal(v)
		if !strings.Contains(string(data), `"tags":[]`) {
			t.Fatalf("tags missing in %s", data)
		}
	}
}
