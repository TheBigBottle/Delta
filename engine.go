package main

// This file is the data path of Delta: it receives data on a forwarder's
// input and copies it to all of its outputs.
//
// How data flows through a running forwarder:
//
//	input endpoint ──read──► framer ──► Forwarder.dispatch ──► output queue ──► writer goroutine ──► network
//	                                         │                   (one per output, or one per
//	                                         │                    connected client for tcp-listen)
//	                                         └──► taps (inspector / capture), only while someone watches
//
// Design rules that keep it fast and robust:
//   - The input never waits for an output. Every output (and every client of a
//     tcp-listen output) has its own bounded queue. When a queue is full the
//     oldest message is dropped, so a slow or stuck receiver only loses data
//     itself and never delays anybody else.
//   - A received message is copied exactly once (out of the read buffer).
//     All outputs share that copy; nobody modifies it afterwards.
//   - Every network error (closed, reset, refused, timeout, unreachable) is
//     expected: connections are retried with growing delays, listeners keep
//     accepting, and a panic only ends the affected connection.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	tcpReadBufferSize   = 32 << 10         // bytes read from a TCP connection at once
	udpReadBufferSize   = 64 << 10         // large enough for any UDP datagram
	maxLineLength       = 8 << 10          // a "line" longer than this is forwarded anyway
	maxDatagramSize     = 65507            // largest UDP payload; bigger messages are split
	writeTimeout        = 10 * time.Second // a peer that does not accept data for this long is dropped
	dialTimeout         = 5 * time.Second
	tcpKeepAlive        = 15 * time.Second // detects dead peers (e.g. behind NAT) even without traffic
	udpSourceExpiry     = time.Minute      // a UDP sender that is silent this long is removed from the list
	defaultOutputQueue  = 10000            // messages buffered per tcp-connect / udp-send output
	defaultClientQueue  = 1024             // messages buffered per client of a tcp-listen output
	socketBufferSize    = 4 << 20          // kernel socket buffer size for UDP
	stableConnectionAge = 10 * time.Second // a connection that lasted this long resets the retry delay
)

// peerIDCounter gives every connection/peer a unique number, used by the UI to
// address a peer (e.g. to disconnect it).
var peerIDCounter atomic.Uint64

// TappedMessage is a copy of a message handed to inspectors and captures.
// Data is shared with the outputs and must never be modified.
type TappedMessage struct {
	Time   int64 // unix nanoseconds
	Source string
	Data   []byte
}

// Peer is one remote party of an endpoint: a connected TCP client, a UDP
// sender, or the remote host we connected to.
type Peer struct {
	ID          uint64
	Remote      string
	ConnectedAt time.Time
	byteCount   atomic.Uint64
	lastDataAt  atomic.Int64 // unix nanoseconds
	conn        io.Closer    // closing it disconnects the peer; nil for UDP senders
}

func (peer *Peer) countBytes(n int) {
	peer.byteCount.Add(uint64(n))
	peer.lastDataAt.Store(time.Now().UnixNano())
}

// endpointRuntime is the live state of one endpoint (the input or one output).
// The counters are atomics because they are updated on the hot path.
type endpointRuntime struct {
	config         EndpointConfig
	byteCount      atomic.Uint64
	messageCount   atomic.Uint64
	dropCount      atomic.Uint64
	errorCount     atomic.Uint64
	reconnectCount atomic.Uint64

	mu          sync.Mutex // guards the fields below
	state       string     // e.g. "listening", "connected", "connecting", "down", "partial"
	lastError   string
	lastErrorAt time.Time
	peers       map[uint64]*Peer
	connections []*remoteConnection // tcp-connect only: one per remote address, in config order
}

// remoteConnection is the state of the connection to one remote address of a
// tcp-connect endpoint. Its fields are guarded by the endpoint's mu.
type remoteConnection struct {
	address   string
	state     string // "connecting", "connected" or "down"
	lastError string
}

func newEndpointRuntime(config EndpointConfig) endpointRuntime {
	var connections []*remoteConnection
	if config.Mode == "tcp-connect" {
		for _, address := range config.connectAddresses() {
			connections = append(connections, &remoteConnection{address: address, state: "connecting"})
		}
	}
	return endpointRuntime{config: config, peers: map[uint64]*Peer{}, connections: connections}
}

// setConnectionState records the state of one remote connection and derives
// the state of the whole endpoint from all of them:
//
//	all connected → "connected", some → "partial", none → "connecting" or "down".
//
// With several remote hosts, the endpoint's error lists every host that is not
// connected, e.g. "10.0.0.5:4001: connection refused …".
func (ep *endpointRuntime) setConnectionState(connection *remoteConnection, state string, err error) {
	if err != nil {
		ep.errorCount.Add(1)
	}
	ep.mu.Lock()
	defer ep.mu.Unlock()
	connection.state = state
	if err != nil {
		connection.lastError = describeNetError(err)
		ep.lastErrorAt = time.Now()
	}
	connectedCount, anyDown := 0, false
	var problems []string
	for _, each := range ep.connections {
		switch each.state {
		case "connected":
			connectedCount++
			continue
		case "down":
			anyDown = true
		}
		if each.lastError != "" {
			problems = append(problems, each.address+": "+each.lastError)
		}
	}
	switch {
	case connectedCount == len(ep.connections):
		ep.state = "connected"
	case connectedCount > 0:
		ep.state = "partial"
	case anyDown:
		ep.state = "down"
	default:
		ep.state = "connecting"
	}
	if len(ep.connections) == 1 {
		ep.lastError = connection.lastError // one host: the error alone, as before
	} else if len(problems) > 0 {
		ep.lastError = strings.Join(problems, "; ")
	}
}

func (ep *endpointRuntime) setState(state string) {
	ep.mu.Lock()
	ep.state = state
	ep.mu.Unlock()
}

// setStateWithError sets the state and remembers err as the last error.
func (ep *endpointRuntime) setStateWithError(state string, err error) {
	ep.errorCount.Add(1)
	ep.mu.Lock()
	ep.state = state
	if err != nil {
		ep.lastError, ep.lastErrorAt = describeNetError(err), time.Now()
	}
	ep.mu.Unlock()
}

func (ep *endpointRuntime) addPeer(remote string, conn io.Closer) *Peer {
	peer := &Peer{ID: peerIDCounter.Add(1), Remote: remote, ConnectedAt: time.Now(), conn: conn}
	peer.lastDataAt.Store(peer.ConnectedAt.UnixNano())
	ep.mu.Lock()
	ep.peers[peer.ID] = peer
	ep.mu.Unlock()
	return peer
}

func (ep *endpointRuntime) removePeer(peer *Peer) {
	ep.mu.Lock()
	delete(ep.peers, peer.ID)
	ep.mu.Unlock()
}

func (ep *endpointRuntime) peerCount() int {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	return len(ep.peers)
}

// disconnectPeer closes the connection of one peer. It returns false if the
// peer does not exist or cannot be disconnected (UDP senders).
func (ep *endpointRuntime) disconnectPeer(peerID uint64) bool {
	ep.mu.Lock()
	peer := ep.peers[peerID]
	ep.mu.Unlock()
	if peer == nil || peer.conn == nil {
		return false
	}
	peer.conn.Close()
	return true
}

// PeerStatus and EndpointStatus are the JSON views of the runtime state.
type PeerStatus struct {
	ID          uint64    `json:"id"`
	Remote      string    `json:"remote"`
	ConnectedAt time.Time `json:"since"`
	LastDataAt  time.Time `json:"last"`
	Bytes       uint64    `json:"bytes"`
}

type EndpointStatus struct {
	Name        string       `json:"name,omitempty"`
	Mode        string       `json:"mode"`
	Address     string       `json:"addr"`
	State       string       `json:"state"`
	LastError   string       `json:"error,omitempty"`
	LastErrorAt *time.Time   `json:"error_at,omitempty"`
	Bytes       uint64       `json:"bytes"`
	Messages    uint64       `json:"chunks"`
	Drops       uint64       `json:"drops"`
	Errors      uint64       `json:"errors"`
	Reconnects  uint64       `json:"reconnects"`
	Queued      int          `json:"queued,omitempty"` // messages waiting (e.g. buffered while the target is down)
	PeerCount   int          `json:"peer_count"`
	Peers       []PeerStatus `json:"peers,omitempty"`
	// tcp-connect: the state of the connection to every remote address.
	Connections []ConnectionStatus `json:"connections,omitempty"`
}

// ConnectionStatus is the JSON view of one remote connection of a tcp-connect endpoint.
type ConnectionStatus struct {
	Address string `json:"addr"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
	Queued  int    `json:"queued,omitempty"` // outputs: messages waiting for this host
}

// snapshot returns the current status. The peer list is only included when
// requested, because it can be long and the live stream does not need it.
func (ep *endpointRuntime) snapshot(includePeers bool) EndpointStatus {
	status := EndpointStatus{
		Name: ep.config.Name, Mode: ep.config.Mode, Address: ep.config.address(),
		Bytes: ep.byteCount.Load(), Messages: ep.messageCount.Load(), Drops: ep.dropCount.Load(),
		Errors: ep.errorCount.Load(), Reconnects: ep.reconnectCount.Load(),
	}
	ep.mu.Lock()
	status.State, status.LastError, status.PeerCount = ep.state, ep.lastError, len(ep.peers)
	for _, connection := range ep.connections {
		connectionStatus := ConnectionStatus{Address: connection.address, State: connection.state}
		if connection.state != "connected" {
			connectionStatus.Error = connection.lastError
		}
		status.Connections = append(status.Connections, connectionStatus)
	}
	if !ep.lastErrorAt.IsZero() {
		errorAt := ep.lastErrorAt
		status.LastErrorAt = &errorAt
	}
	if includePeers {
		for _, peer := range ep.peers {
			status.Peers = append(status.Peers, PeerStatus{
				ID: peer.ID, Remote: peer.Remote, ConnectedAt: peer.ConnectedAt,
				LastDataAt: time.Unix(0, peer.lastDataAt.Load()), Bytes: peer.byteCount.Load(),
			})
		}
	}
	ep.mu.Unlock()
	return status
}

// isHealthyState reports whether an endpoint state means "working as intended".
func isHealthyState(state string) bool {
	switch state {
	case "listening", "connected", "open", "receiving", "sending": // "partial" is not: some hosts are missing
		return true
	}
	return false
}

// outputRuntime is an output endpoint. A tcp-listen output has one queue per
// connected client (clients), a tcp-connect output one queue per remote host
// (connectQueues, same order as connections), and udp-send a single queue.
type outputRuntime struct {
	endpointRuntime
	queue         chan []byte   // udp-send
	connectQueues []chan []byte // tcp-connect
	clientsMu     sync.RWMutex
	clients       map[*outputClient]struct{} // tcp-listen
}

type outputClient struct {
	queue chan []byte
	peer  *Peer
}

// enqueue hands a message to the output without ever blocking the caller.
func (out *outputRuntime) enqueue(message []byte) {
	if out.clients != nil {
		out.clientsMu.RLock()
		for client := range out.clients {
			if !enqueueDropOldest(client.queue, message) {
				out.dropCount.Add(1)
			}
		}
		out.clientsMu.RUnlock()
		return
	}
	if out.connectQueues != nil {
		for _, queue := range out.connectQueues {
			if !enqueueDropOldest(queue, message) {
				out.dropCount.Add(1)
			}
		}
		return
	}
	if !enqueueDropOldest(out.queue, message) {
		out.dropCount.Add(1)
	}
}

// enqueueDropOldest puts message into queue. If the queue is full, the oldest
// queued message is thrown away to make room, so the freshest data wins.
// It returns false if a message had to be dropped.
func enqueueDropOldest(queue chan []byte, message []byte) bool {
	select {
	case queue <- message:
		return true
	default:
	}
	select {
	case <-queue: // drop the oldest message
	default:
	}
	select {
	case queue <- message:
	default: // another goroutine filled the slot first; drop this one
	}
	return false
}

// tapHub distributes copies of the passing data to watchers (the live
// inspector and captures). It costs nothing while nobody watches: dispatch
// only checks one atomic counter.
type tapHub struct {
	watcherCount atomic.Int32
	mu           sync.Mutex
	watchers     map[chan TappedMessage]struct{}
}

func (hub *tapHub) hasWatchers() bool { return hub.watcherCount.Load() > 0 }

func (hub *tapHub) subscribe(bufferSize int) chan TappedMessage {
	ch := make(chan TappedMessage, bufferSize)
	hub.mu.Lock()
	if hub.watchers == nil {
		hub.watchers = map[chan TappedMessage]struct{}{}
	}
	hub.watchers[ch] = struct{}{}
	hub.watcherCount.Store(int32(len(hub.watchers)))
	hub.mu.Unlock()
	return ch
}

func (hub *tapHub) unsubscribe(ch chan TappedMessage) {
	hub.mu.Lock()
	delete(hub.watchers, ch)
	hub.watcherCount.Store(int32(len(hub.watchers)))
	hub.mu.Unlock()
}

func (hub *tapHub) publish(message TappedMessage) {
	hub.mu.Lock()
	for ch := range hub.watchers {
		select {
		case ch <- message:
		default: // a slow watcher misses messages instead of slowing down forwarding
		}
	}
	hub.mu.Unlock()
}

// Forwarder is one running forwarder instance. It is created by
// startForwarder and runs until Stop is called.
type Forwarder struct {
	config     ForwarderConfig
	ctx        context.Context // cancelled by Stop; every goroutine watches it
	cancel     context.CancelFunc
	workers    sync.WaitGroup
	input      *endpointRuntime
	outputs    []*outputRuntime
	taps       tapHub
	lastDataAt atomic.Int64 // unix nanoseconds of the last received message
	startedAt  time.Time
	logEvent   func(level, message string)
}

// startForwarder opens all listening sockets right away, so problems such as
// "address already in use" are reported to the caller immediately, and then
// starts one worker goroutine per endpoint.
func startForwarder(config ForwarderConfig, logEvent func(level, message string)) (*Forwarder, error) {
	ctx, cancel := context.WithCancel(context.Background())
	inputRuntime := newEndpointRuntime(config.Input)
	fw := &Forwarder{config: config, ctx: ctx, cancel: cancel, startedAt: time.Now(), logEvent: logEvent, input: &inputRuntime}

	var openedSockets []io.Closer
	abort := func(err error) (*Forwarder, error) {
		cancel()
		for _, socket := range openedSockets {
			socket.Close()
		}
		return nil, err
	}
	listenConfig := net.ListenConfig{KeepAlive: tcpKeepAlive}
	var workerFuncs []func()

	switch input := config.Input; input.Mode {
	case "tcp-listen":
		listener, err := listenConfig.Listen(ctx, "tcp", input.address())
		if err != nil {
			return abort(fmt.Errorf("input: %w", err))
		}
		openedSockets = append(openedSockets, listener)
		workerFuncs = append(workerFuncs, func() { fw.runTCPListenInput(listener) })
	case "udp-listen":
		packetConn, err := listenConfig.ListenPacket(ctx, "udp", input.address())
		if err != nil {
			return abort(fmt.Errorf("input: %w", err))
		}
		openedSockets = append(openedSockets, packetConn)
		workerFuncs = append(workerFuncs, func() { fw.runUDPListenInput(packetConn.(*net.UDPConn)) })
	case "tcp-connect":
		for _, connection := range fw.input.connections { // one connection loop per remote host
			workerFuncs = append(workerFuncs, func() { fw.runTCPConnectInput(connection) })
		}
	case "udp-connect":
		workerFuncs = append(workerFuncs, fw.runUDPConnectInput)
	}

	for i, outputConfig := range config.Outputs {
		out := &outputRuntime{endpointRuntime: newEndpointRuntime(outputConfig)}
		switch outputConfig.Mode {
		case "tcp-listen":
			listener, err := listenConfig.Listen(ctx, "tcp", outputConfig.address())
			if err != nil {
				return abort(fmt.Errorf("output %d: %w", i+1, err))
			}
			openedSockets = append(openedSockets, listener)
			out.clients = map[*outputClient]struct{}{}
			workerFuncs = append(workerFuncs, func() { fw.runTCPListenOutput(out, listener) })
		case "tcp-connect":
			// Every remote host gets its own queue, so one host that is down
			// or slow never holds back the others.
			for _, connection := range out.connections {
				queue := make(chan []byte, outputConfig.queueSizeOr(defaultOutputQueue))
				out.connectQueues = append(out.connectQueues, queue)
				workerFuncs = append(workerFuncs, func() { fw.runTCPConnectOutput(out, connection, queue) })
			}
		case "udp-send":
			out.queue = make(chan []byte, outputConfig.queueSizeOr(defaultOutputQueue))
			workerFuncs = append(workerFuncs, func() { fw.runUDPSendOutput(out) })
		}
		fw.outputs = append(fw.outputs, out)
	}

	for _, worker := range workerFuncs {
		fw.runWorker(worker)
	}
	return fw, nil
}

// Stop cancels all goroutines and waits (at most 5 seconds) until every
// socket is closed, so the ports can be reused immediately.
func (fw *Forwarder) Stop() {
	fw.cancel()
	allDone := make(chan struct{})
	go func() { fw.workers.Wait(); close(allDone) }()
	select {
	case <-allDone:
	case <-time.After(5 * time.Second):
		fw.logEvent("warn", "stop timed out, some connections are still closing")
	}
}

// runWorker runs a long-lived endpoint worker. If the worker returns or
// panics before the forwarder is stopped, it is started again after a second.
func (fw *Forwarder) runWorker(worker func()) {
	fw.workers.Add(1)
	go func() {
		defer fw.workers.Done()
		for {
			fw.runRecovering(worker)
			if !sleepOrCancel(fw.ctx, time.Second) {
				return
			}
		}
	}()
}

// runConnection runs the handler of a single connection. A panic in it only
// ends that one connection.
func (fw *Forwarder) runConnection(handler func()) {
	fw.workers.Add(1)
	go func() {
		defer fw.workers.Done()
		fw.runRecovering(handler)
	}()
}

func (fw *Forwarder) runRecovering(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			fw.logEvent("error", fmt.Sprintf("internal error: %v", r))
		}
	}()
	fn()
}

// dispatch is the hot path: every message received by the input passes
// through here and is handed to all outputs (and to the taps, if watched).
func (fw *Forwarder) dispatch(message []byte, source string) {
	now := time.Now().UnixNano()
	fw.lastDataAt.Store(now)
	fw.input.byteCount.Add(uint64(len(message)))
	fw.input.messageCount.Add(1)
	for _, out := range fw.outputs {
		out.enqueue(message)
	}
	if fw.taps.hasWatchers() {
		fw.taps.publish(TappedMessage{Time: now, Source: source, Data: message})
	}
}

// logEventUnlessStopping avoids "client disconnected" noise while the
// forwarder itself is being stopped.
func (fw *Forwarder) logEventUnlessStopping(level, message string) {
	if fw.ctx.Err() == nil {
		fw.logEvent(level, message)
	}
}

// ---- framing ----

// framer turns received bytes into messages. In raw mode every read becomes
// one message. In line mode every message is exactly one line (for example
// one NMEA sentence), so outputs never receive half a telegram.
type framer struct {
	splitLines  bool
	partialLine []byte // start of a line whose end has not arrived yet
}

// feedStream processes bytes read from a TCP stream, where a line may be
// split across several reads.
func (fr *framer) feedStream(received []byte, source string, deliver func([]byte, string)) {
	data := bytes.Clone(received) // the only copy; the read buffer is reused
	if !fr.splitLines {
		deliver(data, source)
		return
	}
	for len(data) > 0 {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			// No line end yet: keep the bytes until the rest arrives.
			fr.partialLine = append(fr.partialLine, data...)
			if len(fr.partialLine) >= maxLineLength {
				deliver(bytes.Clone(fr.partialLine), source)
				fr.partialLine = fr.partialLine[:0]
			}
			return
		}
		if len(fr.partialLine) > 0 {
			line := make([]byte, 0, len(fr.partialLine)+newline+1)
			line = append(append(line, fr.partialLine...), data[:newline+1]...)
			fr.partialLine = fr.partialLine[:0]
			deliver(line, source)
		} else {
			// The full slice expression limits capacity so nobody can append into the next line.
			deliver(data[:newline+1:newline+1], source)
		}
		data = data[newline+1:]
	}
}

// flush delivers an unfinished line, e.g. when the connection closes.
func (fr *framer) flush(source string, deliver func([]byte, string)) {
	if len(fr.partialLine) > 0 {
		deliver(bytes.Clone(fr.partialLine), source)
		fr.partialLine = fr.partialLine[:0]
	}
}

// feedDatagram processes one UDP datagram. A datagram is always complete, so
// in line mode a trailing piece without line end is delivered as well.
func (fr *framer) feedDatagram(received []byte, source string, deliver func([]byte, string)) {
	data := bytes.Clone(received)
	if !fr.splitLines {
		deliver(data, source)
		return
	}
	for len(data) > 0 {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			deliver(data, source)
			return
		}
		deliver(data[:newline+1:newline+1], source)
		data = data[newline+1:]
	}
}

// ---- inputs ----

// runTCPListenInput accepts TCP clients; everything they send is forwarded.
func (fw *Forwarder) runTCPListenInput(listener net.Listener) {
	input := fw.input
	defer context.AfterFunc(fw.ctx, func() { listener.Close() })() // Stop unblocks Accept
	input.setState("listening")
	for {
		conn, err := listener.Accept()
		if err != nil {
			if fw.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			// e.g. "too many open files": wait briefly instead of spinning
			input.setStateWithError("listening", err)
			sleepOrCancel(fw.ctx, 100*time.Millisecond)
			continue
		}
		if limit := input.config.MaxClients; limit > 0 && input.peerCount() >= limit {
			input.errorCount.Add(1)
			fw.logEvent("warn", fmt.Sprintf("input: rejected %s, limit of %d clients reached", conn.RemoteAddr(), limit))
			conn.Close()
			continue
		}
		fw.runConnection(func() {
			peer := input.addPeer(conn.RemoteAddr().String(), conn)
			fw.logEvent("info", "input: client "+peer.Remote+" connected")
			err := fw.readFromConnection(conn, peer)
			input.removePeer(peer)
			fw.logEventUnlessStopping("info", "input: client "+peer.Remote+" disconnected ("+describeNetError(err)+")")
		})
	}
}

// readFromConnection reads a TCP connection until it closes and forwards
// everything it receives. It returns the reason the connection ended.
func (fw *Forwarder) readFromConnection(conn net.Conn, peer *Peer) error {
	defer conn.Close()
	defer context.AfterFunc(fw.ctx, func() { conn.Close() })() // Stop unblocks Read
	disableNagle(conn)
	lineFramer := framer{splitLines: fw.config.Framing == "lines"}
	defer lineFramer.flush(peer.Remote, fw.dispatch)
	readBuffer := make([]byte, tcpReadBufferSize)
	for {
		n, err := conn.Read(readBuffer)
		if n > 0 {
			peer.countBytes(n)
			lineFramer.feedStream(readBuffer[:n], peer.Remote, fw.dispatch)
		}
		if err != nil {
			return err
		}
	}
}

// runTCPConnectInput connects to one remote TCP source and reads from it,
// reconnecting with growing delays whenever the connection fails or ends.
// An input with several remote hosts runs this once per host.
func (fw *Forwarder) runTCPConnectInput(connection *remoteConnection) {
	input := fw.input
	address := connection.address
	dialer := net.Dialer{Timeout: dialTimeout, KeepAlive: tcpKeepAlive}
	var retry retryDelay
	failureLogged := false // log only the first failure of a series
	for fw.ctx.Err() == nil {
		input.setConnectionState(connection, "connecting", nil)
		conn, err := dialer.DialContext(fw.ctx, "tcp", address)
		if err != nil {
			if fw.ctx.Err() != nil {
				return
			}
			input.setConnectionState(connection, "down", err)
			if !failureLogged {
				fw.logEvent("warn", "input: cannot connect to "+address+" ("+describeNetError(err)+"), retrying")
				failureLogged = true
			}
			sleepOrCancel(fw.ctx, retry.next())
			continue
		}
		failureLogged = false
		input.setConnectionState(connection, "connected", nil)
		fw.logEvent("info", "input: connected to "+address)
		peer := input.addPeer(conn.RemoteAddr().String(), conn)
		err = fw.readFromConnection(conn, peer)
		input.removePeer(peer)
		if fw.ctx.Err() != nil {
			return
		}
		input.reconnectCount.Add(1)
		input.setConnectionState(connection, "down", err)
		fw.logEvent("warn", "input: connection to "+address+" lost ("+describeNetError(err)+")")
		if time.Since(peer.ConnectedAt) > stableConnectionAge {
			retry.reset() // a long-lived connection: reconnect quickly
		}
		sleepOrCancel(fw.ctx, retry.next())
	}
}

// runUDPListenInput receives datagrams on a local port from any sender.
func (fw *Forwarder) runUDPListenInput(udpConn *net.UDPConn) {
	input := fw.input
	defer context.AfterFunc(fw.ctx, func() { udpConn.Close() })()
	udpConn.SetReadBuffer(socketBufferSize)
	input.setState("listening")
	lineFramer := framer{splitLines: fw.config.Framing == "lines"}

	// UDP has no connections, so senders are tracked by address and expire when silent.
	senders := map[netip.AddrPort]*Peer{}
	defer func() {
		for _, peer := range senders {
			input.removePeer(peer)
		}
	}()
	lastExpiryCheck := time.Now()

	readBuffer := make([]byte, udpReadBufferSize)
	for {
		n, senderAddr, err := udpConn.ReadFromUDPAddrPort(readBuffer)
		if err != nil {
			if fw.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			input.setStateWithError("listening", err)
			continue
		}
		// Show IPv4 senders as 1.2.3.4 instead of [::ffff:1.2.3.4] on dual-stack sockets.
		senderAddr = netip.AddrPortFrom(senderAddr.Addr().Unmap(), senderAddr.Port())
		peer := senders[senderAddr]
		if peer == nil {
			peer = input.addPeer(senderAddr.String(), nil)
			senders[senderAddr] = peer
		}
		peer.countBytes(n)
		lineFramer.feedDatagram(readBuffer[:n], peer.Remote, fw.dispatch)

		if now := time.Now(); now.Sub(lastExpiryCheck) > 10*time.Second {
			lastExpiryCheck = now
			for addr, peer := range senders {
				if now.UnixNano()-peer.lastDataAt.Load() > int64(udpSourceExpiry) {
					input.removePeer(peer)
					delete(senders, addr)
				}
			}
		}
	}
}

// runUDPConnectInput receives datagrams from one remote host. UDP has no
// handshake, so a configurable "hello" datagram tells the remote where to send.
func (fw *Forwarder) runUDPConnectInput() {
	input := fw.input
	dialer := net.Dialer{Timeout: dialTimeout}
	var retry retryDelay
	for fw.ctx.Err() == nil {
		input.setState("connecting")
		conn, err := dialer.DialContext(fw.ctx, "udp", input.config.address())
		if err != nil {
			if fw.ctx.Err() != nil {
				return
			}
			input.setStateWithError("down", err) // e.g. the host name cannot be resolved
			sleepOrCancel(fw.ctx, retry.next())
			continue
		}
		retry.reset()
		err = fw.receiveUDPSession(conn.(*net.UDPConn))
		conn.Close()
		if fw.ctx.Err() != nil {
			return
		}
		input.reconnectCount.Add(1)
		input.setStateWithError("down", err)
		sleepOrCancel(fw.ctx, retry.next())
	}
}

// receiveUDPSession sends the hello datagrams and reads from a connected UDP
// socket until a fatal error occurs.
func (fw *Forwarder) receiveUDPSession(udpConn *net.UDPConn) error {
	input := fw.input
	sessionCtx, endSession := context.WithCancel(fw.ctx)
	defer endSession()
	defer context.AfterFunc(sessionCtx, func() { udpConn.Close() })()
	udpConn.SetReadBuffer(socketBufferSize)
	peer := input.addPeer(udpConn.RemoteAddr().String(), udpConn)
	defer input.removePeer(peer)

	if hello := unescapeHello(input.config.Hello); len(hello) > 0 {
		interval := time.Duration(input.config.HelloIntervalSeconds) * time.Second
		if interval <= 0 {
			interval = 30 * time.Second
		}
		fw.runConnection(func() {
			for {
				udpConn.Write(hello) // send errors show up on the read side
				if !sleepOrCancel(sessionCtx, interval) {
					return
				}
			}
		})
	}

	state := "open"
	input.setState(state)
	lineFramer := framer{splitLines: fw.config.Framing == "lines"}
	readBuffer := make([]byte, udpReadBufferSize)
	for {
		n, err := udpConn.Read(readBuffer)
		if err != nil {
			// "Port unreachable" (ICMP) does not break the socket: keep waiting,
			// the remote service may come back.
			if errors.Is(err, syscall.ECONNREFUSED) {
				if state != "unreachable" {
					fw.logEvent("warn", "input: "+input.config.address()+" port unreachable")
					state = "unreachable"
				}
				input.setStateWithError(state, err)
				if !sleepOrCancel(sessionCtx, time.Second) {
					return nil
				}
				continue
			}
			return err
		}
		if state != "receiving" {
			state = "receiving"
			input.setState(state)
		}
		peer.countBytes(n)
		lineFramer.feedDatagram(readBuffer[:n], peer.Remote, fw.dispatch)
	}
}

// ---- outputs ----

// writeQueueToConn sends queued messages to a TCP connection until the
// connection fails or the forwarder stops. Messages that are already waiting
// are combined into one system call (writev) to save time under load.
// A background reader notices when the remote closes the connection, even
// while no data is being sent.
func (fw *Forwarder) writeQueueToConn(conn net.Conn, queue chan []byte, ep *endpointRuntime, peer *Peer) error {
	remoteClosed := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, conn) // we never expect data back; just watch for EOF
		if err == nil {
			err = io.EOF
		}
		remoteClosed <- err
	}()

	var batchStorage [64][]byte
	for {
		var message []byte
		select {
		case <-fw.ctx.Done():
			return nil
		case err := <-remoteClosed:
			return err
		case message = <-queue:
		}

		batch := append(batchStorage[:0], message)
		batchBytes := len(message)
	collectWaiting:
		for len(batch) < len(batchStorage) {
			select {
			case message = <-queue:
				batch = append(batch, message)
				batchBytes += len(message)
			default:
				break collectWaiting
			}
		}

		conn.SetWriteDeadline(time.Now().Add(writeTimeout)) // a stuck peer gets dropped
		buffers := net.Buffers(batch)
		if _, err := buffers.WriteTo(conn); err != nil {
			ep.dropCount.Add(uint64(len(batch)))
			return err
		}
		ep.byteCount.Add(uint64(batchBytes))
		ep.messageCount.Add(uint64(len(batch)))
		peer.countBytes(batchBytes)
	}
}

// runTCPListenOutput accepts TCP clients and sends every message to all of them.
func (fw *Forwarder) runTCPListenOutput(out *outputRuntime, listener net.Listener) {
	defer context.AfterFunc(fw.ctx, func() { listener.Close() })()
	out.setState("listening")
	label := outputLabel(out)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if fw.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			out.setStateWithError("listening", err)
			sleepOrCancel(fw.ctx, 100*time.Millisecond)
			continue
		}
		if limit := out.config.MaxClients; limit > 0 && out.peerCount() >= limit {
			out.errorCount.Add(1)
			fw.logEvent("warn", fmt.Sprintf("%s: rejected %s, limit of %d clients reached", label, conn.RemoteAddr(), limit))
			conn.Close()
			continue
		}

		// A new client only receives data from now on, through its own queue.
		client := &outputClient{
			queue: make(chan []byte, out.config.queueSizeOr(defaultClientQueue)),
			peer:  out.addPeer(conn.RemoteAddr().String(), conn),
		}
		out.clientsMu.Lock()
		out.clients[client] = struct{}{}
		out.clientsMu.Unlock()

		fw.runConnection(func() {
			disableNagle(conn)
			fw.logEvent("info", label+": client "+client.peer.Remote+" connected")
			err := fw.writeQueueToConn(conn, client.queue, &out.endpointRuntime, client.peer)
			out.clientsMu.Lock()
			delete(out.clients, client)
			out.clientsMu.Unlock()
			conn.Close()
			out.removePeer(client.peer)
			fw.logEventUnlessStopping("info", label+": client "+client.peer.Remote+" disconnected ("+describeNetError(err)+")")
		})
	}
}

// runTCPConnectOutput connects to one remote TCP target and sends the data
// there. While the target is unreachable, messages stay in its queue (up to
// its size) and are delivered after reconnecting. An output with several
// remote hosts runs this once per host, each with its own queue.
func (fw *Forwarder) runTCPConnectOutput(out *outputRuntime, connection *remoteConnection, queue chan []byte) {
	address := connection.address
	label := outputLabel(out)
	dialer := net.Dialer{Timeout: dialTimeout, KeepAlive: tcpKeepAlive}
	var retry retryDelay
	failureLogged := false
	for fw.ctx.Err() == nil {
		out.setConnectionState(connection, "connecting", nil)
		conn, err := dialer.DialContext(fw.ctx, "tcp", address)
		if err != nil {
			if fw.ctx.Err() != nil {
				return
			}
			out.setConnectionState(connection, "down", err)
			if !failureLogged {
				fw.logEvent("warn", label+": cannot connect to "+address+" ("+describeNetError(err)+"), buffering and retrying")
				failureLogged = true
			}
			sleepOrCancel(fw.ctx, retry.next())
			continue
		}
		failureLogged = false
		disableNagle(conn)
		out.setConnectionState(connection, "connected", nil)
		fw.logEvent("info", label+": connected to "+address)
		peer := out.addPeer(conn.RemoteAddr().String(), conn)
		err = fw.writeQueueToConn(conn, queue, &out.endpointRuntime, peer)
		conn.Close()
		out.removePeer(peer)
		if fw.ctx.Err() != nil {
			return
		}
		out.reconnectCount.Add(1)
		out.setConnectionState(connection, "down", err)
		fw.logEvent("warn", label+": connection to "+address+" lost ("+describeNetError(err)+")")
		if time.Since(peer.ConnectedAt) > stableConnectionAge {
			retry.reset()
		}
		sleepOrCancel(fw.ctx, retry.next())
	}
}

// runUDPSendOutput sends every message as a datagram to a remote host.
func (fw *Forwarder) runUDPSendOutput(out *outputRuntime) {
	dialer := net.Dialer{Timeout: dialTimeout}
	var retry retryDelay
	for fw.ctx.Err() == nil {
		out.setState("connecting")
		conn, err := dialer.DialContext(fw.ctx, "udp", out.config.address())
		if err != nil {
			if fw.ctx.Err() != nil {
				return
			}
			out.setStateWithError("down", err) // e.g. the host name cannot be resolved
			sleepOrCancel(fw.ctx, retry.next())
			continue
		}
		retry.reset()
		err = fw.sendQueueOverUDP(conn.(*net.UDPConn), out)
		conn.Close()
		if fw.ctx.Err() != nil {
			return
		}
		out.reconnectCount.Add(1)
		out.setStateWithError("down", err)
		sleepOrCancel(fw.ctx, retry.next())
	}
}

func (fw *Forwarder) sendQueueOverUDP(udpConn *net.UDPConn, out *outputRuntime) error {
	udpConn.SetWriteBuffer(socketBufferSize)
	peer := out.addPeer(udpConn.RemoteAddr().String(), udpConn)
	defer out.removePeer(peer)
	out.setState("sending")
	reachable := true
	for {
		var message []byte
		select {
		case <-fw.ctx.Done():
			return nil
		case message = <-out.queue:
		}
		// Messages larger than one datagram (only possible in raw mode) are split.
		for len(message) > 0 {
			size := min(len(message), maxDatagramSize)
			_, err := udpConn.Write(message[:size])
			switch {
			case err == nil:
				if !reachable {
					out.setState("sending")
					reachable = true
				}
				out.byteCount.Add(uint64(size))
				peer.countBytes(size)
			case errors.Is(err, syscall.ECONNREFUSED):
				// The remote reported "port unreachable". Keep sending: it may come back.
				if reachable {
					fw.logEvent("warn", outputLabel(out)+": "+out.config.address()+" port unreachable")
				}
				out.setStateWithError("unreachable", err)
				out.dropCount.Add(1)
				reachable = false
			default:
				return err
			}
			message = message[size:]
		}
		out.messageCount.Add(1)
	}
}

// ---- status & control ----

func (fw *Forwarder) snapshot(includePeers bool) (EndpointStatus, []EndpointStatus) {
	outputs := make([]EndpointStatus, len(fw.outputs))
	for i, out := range fw.outputs {
		outputs[i] = out.snapshot(includePeers)
		outputs[i].Queued = len(out.queue) // 0 for tcp-listen (no shared queue)
		for j, queue := range out.connectQueues {
			outputs[i].Queued += len(queue)
			outputs[i].Connections[j].Queued = len(queue)
		}
	}
	return fw.input.snapshot(includePeers), outputs
}

// trafficCounters are the totals since the forwarder was started.
type trafficCounters struct {
	BytesIn, BytesOut, Messages, Drops uint64
}

func (fw *Forwarder) counters() trafficCounters {
	totals := trafficCounters{
		BytesIn:  fw.input.byteCount.Load(),
		Messages: fw.input.messageCount.Load(),
		Drops:    fw.input.dropCount.Load(),
	}
	for _, out := range fw.outputs {
		totals.BytesOut += out.byteCount.Load()
		totals.Drops += out.dropCount.Load()
	}
	return totals
}

// disconnectPeer closes one connection. endpoint is "in" for the input or
// "out:<index>" for an output.
func (fw *Forwarder) disconnectPeer(endpoint string, peerID uint64) bool {
	if endpoint == "in" {
		return fw.input.disconnectPeer(peerID)
	}
	index, err := strconv.Atoi(strings.TrimPrefix(endpoint, "out:"))
	if err != nil || index < 0 || index >= len(fw.outputs) {
		return false
	}
	return fw.outputs[index].disconnectPeer(peerID)
}

// ---- helpers ----

// outputLabel names an output in log messages, e.g. "output plotter".
func outputLabel(out *outputRuntime) string {
	if out.config.Name != "" {
		return "output " + out.config.Name
	}
	return "output " + out.config.Mode + " " + out.config.address() // the first host, also with several
}

// disableNagle makes TCP send small messages immediately instead of waiting
// to combine them; this keeps the latency low.
func disableNagle(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetNoDelay(true)
	}
}

// describeNetError turns network errors into short, human readable text,
// including a hint what to check for the common connection problems.
func describeNetError(err error) string {
	var opErr *net.OpError
	isDialError := errors.As(err, &opErr) && opErr.Op == "dial"
	switch {
	case err == nil:
		return "closed"
	case errors.Is(err, io.EOF):
		return "closed by remote"
	case errors.Is(err, net.ErrClosed):
		return "closed"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset by remote"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused: nothing is listening on this port"
	case errors.Is(err, syscall.EPIPE):
		return "broken pipe: remote closed the connection"
	case errors.Is(err, syscall.EHOSTUNREACH):
		// Linux reports this both when the host is offline and when a firewall
		// on the target rejects the port (e.g. firewalld's icmp-host-prohibited).
		return "host unreachable: host offline, or a firewall on it blocks this port"
	case errors.Is(err, syscall.ENETUNREACH):
		return "network unreachable: check the IP address and routing"
	case isDialError && opErr.Timeout():
		return "connection timed out: host does not answer (offline or firewall drops packets)"
	case errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout: peer stopped reading"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "cannot resolve " + dnsErr.Name
	}
	return err.Error()
}

// retryDelay produces growing waiting times between reconnect attempts:
// 200 ms, 400 ms, 800 ms … up to 10 s, plus up to 20% random jitter so many
// forwarders do not all retry at the same moment.
type retryDelay struct{ current time.Duration }

func (r *retryDelay) next() time.Duration {
	if r.current == 0 {
		r.current = 200 * time.Millisecond
	} else if r.current *= 2; r.current > 10*time.Second {
		r.current = 10 * time.Second
	}
	return r.current + rand.N(r.current/5+1)
}

func (r *retryDelay) reset() { r.current = 0 }

// sleepOrCancel waits for d. It returns false if ctx was cancelled first.
func sleepOrCancel(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// unescapeHello converts the escapes \r \n \t \\ and \xHH in a hello message,
// so binary hello datagrams can be entered as text in the web interface.
func unescapeHello(text string) []byte {
	var out []byte
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c != '\\' || i+1 >= len(text) {
			out = append(out, c)
			continue
		}
		i++
		switch text[i] {
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case '\\':
			out = append(out, '\\')
		case 'x':
			if i+2 < len(text) {
				if value, err := strconv.ParseUint(text[i+1:i+3], 16, 8); err == nil {
					out = append(out, byte(value))
					i += 2
					continue
				}
			}
			out = append(out, '\\', 'x')
		default:
			out = append(out, '\\', text[i])
		}
	}
	return out
}
