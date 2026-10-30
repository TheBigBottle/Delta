package engine

import (
	"bufio"
	"net"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"delta/internal/config"
)

func freePort(t testing.TB) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func addr(port int) string { return "127.0.0.1:" + strconv.Itoa(port) }

func start(t testing.TB, cfg config.ForwarderConfig) *Forwarder {
	t.Helper()
	cfg.Name = "test"
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	fw, err := Start(cfg, func(level, msg string) { t.Logf("[%s] %s", level, msg) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fw.Stop)
	return fw
}

func dialRetry(t testing.TB, network, a string) net.Conn {
	t.Helper()
	for i := 0; i < 50; i++ {
		if c, err := net.Dial(network, a); err == nil {
			return c
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("cannot connect to %s", a)
	return nil
}

func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// NMEA sentences written in arbitrary fragments arrive as whole lines on a TCP
// client and as one sentence per datagram on a UDP target (fan-out).
func TestFanOutLines(t *testing.T) {
	in, out := freePort(t), freePort(t)
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	fw := start(t, config.ForwarderConfig{
		Framing: "lines",
		Input:   config.EndpointConfig{Mode: "tcp-listen", Host: "127.0.0.1", Port: in},
		Outputs: []config.EndpointConfig{
			{Mode: "tcp-listen", Host: "127.0.0.1", Port: out},
			{Mode: "udp-send", Host: "127.0.0.1", Port: udp.LocalAddr().(*net.UDPAddr).Port},
		},
	})
	client := dialRetry(t, "tcp", addr(out))
	defer client.Close()
	waitFor(t, "client registered", func() bool { return fw.outputs[0].peerCount() == 1 })

	src := dialRetry(t, "tcp", addr(in))
	defer src.Close()
	s1 := "$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47\r\n"
	s2 := "$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230394,003.1,W*6A\r\n"
	all := s1 + s2
	for _, part := range []string{all[:10], all[10:50], all[50:90], all[90:]} {
		src.Write([]byte(part))
		time.Sleep(5 * time.Millisecond)
	}

	r := bufio.NewReader(client)
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	for _, want := range []string{s1, s2} {
		got, err := r.ReadString('\n')
		if err != nil || got != want {
			t.Fatalf("tcp client got %q, %v; want %q", got, err, want)
		}
	}
	buf := make([]byte, 2048)
	udp.SetReadDeadline(time.Now().Add(2 * time.Second))
	for _, want := range []string{s1, s2} {
		n, _, err := udp.ReadFrom(buf)
		if err != nil || string(buf[:n]) != want {
			t.Fatalf("udp got %q, %v; want %q", buf[:n], err, want)
		}
	}
}

// A tcp-connect output buffers while its target is down and delivers the
// buffered data once the target appears; it reconnects after the target closes.
func TestOutputReconnectBuffers(t *testing.T) {
	in, target := freePort(t), freePort(t)
	fw := start(t, config.ForwarderConfig{
		Input:   config.EndpointConfig{Mode: "udp-listen", Host: "127.0.0.1", Port: in},
		Outputs: []config.EndpointConfig{{Mode: "tcp-connect", Host: "127.0.0.1", Port: target}},
	})
	src, err := net.Dial("udp", addr(in))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	src.Write([]byte("buffered-1\n"))
	src.Write([]byte("buffered-2\n"))
	waitFor(t, "input data", func() bool { return fw.input.messageCount.Load() == 2 })

	ln, err := net.Listen("tcp", addr(target))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for _, want := range []string{"buffered-1\n", "buffered-2\n"} {
		if got, err := r.ReadString('\n'); got != want {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
	}

	c.Close() // target goes away; the output must notice without data flowing
	waitFor(t, "output down", func() bool { return fw.outputs[0].snapshot(false).State != "connected" })
	c2, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	waitFor(t, "output reconnected", func() bool { return fw.outputs[0].snapshot(false).State == "connected" })
	src.Write([]byte("after-reconnect\n"))
	c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	if got, err := bufio.NewReader(c2).ReadString('\n'); got != "after-reconnect\n" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// A tcp-connect input reconnects when its source closes the connection.
func TestInputReconnect(t *testing.T) {
	srcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srcLn.Close()
	out := freePort(t)
	fw := start(t, config.ForwarderConfig{
		Input:   config.EndpointConfig{Mode: "tcp-connect", Host: "127.0.0.1", Port: srcLn.Addr().(*net.TCPAddr).Port},
		Outputs: []config.EndpointConfig{{Mode: "tcp-listen", Host: "127.0.0.1", Port: out}},
	})
	client := dialRetry(t, "tcp", addr(out))
	defer client.Close()
	r := bufio.NewReader(client)
	waitFor(t, "client registered", func() bool { return fw.outputs[0].peerCount() == 1 })

	for i, msg := range []string{"first\n", "second\n"} {
		c, err := srcLn.Accept()
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte(msg))
		client.SetReadDeadline(time.Now().Add(3 * time.Second))
		if got, err := r.ReadString('\n'); got != msg {
			t.Fatalf("round %d: got %q, %v", i, got, err)
		}
		c.Close()
	}
	if fw.input.reconnectCount.Load() == 0 {
		t.Fatal("expected a reconnect")
	}
}

// A client that stops reading must not block the forwarder or other clients.
func TestSlowClientDoesNotBlock(t *testing.T) {
	in, out := freePort(t), freePort(t)
	fw := start(t, config.ForwarderConfig{
		Input:   config.EndpointConfig{Mode: "udp-listen", Host: "127.0.0.1", Port: in},
		Outputs: []config.EndpointConfig{{Mode: "tcp-listen", Host: "127.0.0.1", Port: out, QueueSize: 64}},
	})
	stuck := dialRetry(t, "tcp", addr(out)) // never reads
	defer stuck.Close()
	good := dialRetry(t, "tcp", addr(out))
	defer good.Close()
	waitFor(t, "clients registered", func() bool { return fw.outputs[0].peerCount() == 2 })

	src, _ := net.Dial("udp", addr(in))
	defer src.Close()
	payload := make([]byte, 8000)
	payload[len(payload)-1] = '\n'
	done := make(chan int64)
	go func() {
		var total int64
		buf := make([]byte, 64<<10)
		good.SetReadDeadline(time.Now().Add(5 * time.Second))
		for total < 2000*8000 {
			n, err := good.Read(buf)
			total += int64(n)
			if err != nil {
				break
			}
		}
		done <- total
	}()
	for i := 0; i < 2000; i++ {
		src.Write(payload)
		if i%50 == 0 {
			time.Sleep(time.Millisecond) // stay below the UDP socket buffer
		}
	}
	got := <-done
	if got < 1000*8000 {
		t.Fatalf("healthy client only received %d bytes", got)
	}
	t.Logf("healthy client received %d bytes, drops %d", got, fw.outputs[0].dropCount.Load())
}

// Stopping releases the ports and leaves no goroutines behind.
func TestStopReleases(t *testing.T) {
	before := runtime.NumGoroutine()
	in, out := freePort(t), freePort(t)
	cfg := config.ForwarderConfig{Name: "x",
		Input:   config.EndpointConfig{Mode: "tcp-listen", Host: "127.0.0.1", Port: in},
		Outputs: []config.EndpointConfig{{Mode: "tcp-listen", Host: "127.0.0.1", Port: out}},
	}
	cfg.Normalize()
	for i := 0; i < 3; i++ {
		fw, err := Start(cfg, func(string, string) {})
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		a := dialRetry(t, "tcp", addr(in))
		b := dialRetry(t, "tcp", addr(out))
		waitFor(t, "peers", func() bool { return fw.input.peerCount() == 1 && fw.outputs[0].peerCount() == 1 })
		fw.Stop()
		a.Close()
		b.Close()
	}
	waitFor(t, "goroutines to exit", func() bool { return runtime.NumGoroutine() <= before+1 })
}

// TestLatency measures the time Delta adds between a TCP source and a TCP client.
func TestLatency(t *testing.T) {
	in, out := freePort(t), freePort(t)
	fw := start(t, config.ForwarderConfig{
		Framing: "lines",
		Input:   config.EndpointConfig{Mode: "tcp-listen", Host: "127.0.0.1", Port: in},
		Outputs: []config.EndpointConfig{{Mode: "tcp-listen", Host: "127.0.0.1", Port: out}},
	})
	client := dialRetry(t, "tcp", addr(out))
	defer client.Close()
	waitFor(t, "client", func() bool { return fw.outputs[0].peerCount() == 1 })
	src := dialRetry(t, "tcp", addr(in))
	defer src.Close()
	r := bufio.NewReader(client)
	msg := []byte("$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230394,003.1,W*6A\r\n")
	var lat []time.Duration
	for i := 0; i < 2000; i++ {
		t0 := time.Now()
		src.Write(msg)
		if _, err := r.ReadSlice('\n'); err != nil {
			t.Fatal(err)
		}
		lat = append(lat, time.Since(t0))
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	t.Logf("end-to-end latency over loopback (incl. 2 TCP hops): p50=%v p99=%v max=%v",
		lat[len(lat)/2], lat[len(lat)*99/100], lat[len(lat)-1])
}

// A tcp-connect input with several remote hosts merges their data, and a
// tcp-connect output with several hosts sends a copy to each. A host that is
// down makes the endpoint "partial", buffers its own data and gets it once it
// is back, without holding back the other host.
func TestMultipleConnectTargets(t *testing.T) {
	listen := func() (net.Listener, int) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		return ln, ln.Addr().(*net.TCPAddr).Port
	}
	sourceA, sourceAPort := listen()
	sourceB, sourceBPort := listen()
	targetA, targetAPort := listen()
	targetBPort := freePort(t) // nothing listens here yet: this target starts "down"

	fw := start(t, config.ForwarderConfig{
		Framing: "lines",
		Input: config.EndpointConfig{Mode: "tcp-connect", Host: "127.0.0.1", Port: sourceAPort,
			Targets: []config.RemoteTarget{{Host: "127.0.0.1", Port: sourceBPort}}},
		Outputs: []config.EndpointConfig{{Mode: "tcp-connect", Host: "127.0.0.1", Port: targetAPort,
			Targets: []config.RemoteTarget{{Host: "127.0.0.1", Port: targetBPort}}}},
	})

	accept := func(ln net.Listener) net.Conn {
		conn, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	sourceAConn, sourceBConn, targetAConn := accept(sourceA), accept(sourceB), accept(targetA)
	waitFor(t, "both input hosts connected", func() bool { return fw.input.snapshot(false).State == "connected" })
	waitFor(t, "output partially connected", func() bool { return fw.outputs[0].snapshot(false).State == "partial" })

	sourceAConn.Write([]byte("$A*00\n"))
	sourceBConn.Write([]byte("$B*00\n"))
	readTwoLines := func(conn net.Conn) []string {
		reader := bufio.NewReader(conn)
		var lines []string
		for len(lines) < 2 {
			conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("read: %v (got %q)", err, lines)
			}
			lines = append(lines, line)
		}
		sort.Strings(lines)
		return lines
	}
	if got := readTwoLines(targetAConn); strings.Join(got, "") != "$A*00\n$B*00\n" {
		t.Fatalf("target A got %q", got)
	}

	// Target B comes up: it receives what was buffered for it.
	targetB, err := net.Listen("tcp", addr(targetBPort))
	if err != nil {
		t.Fatal(err)
	}
	defer targetB.Close()
	targetBConn := accept(targetB)
	if got := readTwoLines(targetBConn); strings.Join(got, "") != "$A*00\n$B*00\n" {
		t.Fatalf("target B got %q", got)
	}
	waitFor(t, "output fully connected", func() bool { return fw.outputs[0].snapshot(false).State == "connected" })
	if status := fw.outputs[0].snapshot(false); len(status.Connections) != 2 || status.PeerCount != 2 {
		t.Fatalf("status: %+v", status)
	}
}
