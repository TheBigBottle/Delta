package engine

import (
	"context"
	"sync"
	"testing"

	"delta/internal/config"
)

// newBenchForwarder builds a forwarder without sockets: one tcp-listen output
// with clientCount clients whose queues are drained by goroutines, plus an
// optional tap watcher. It measures the hot path (Dispatch) alone.
func newBenchForwarder(b *testing.B, clientCount int, withTap bool) *Forwarder {
	b.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	inputRuntime := newEndpointRuntime(config.EndpointConfig{Mode: "tcp-listen"})
	fw := &Forwarder{ctx: ctx, cancel: cancel, input: &inputRuntime, logEvent: func(string, string) {}}
	out := &outputRuntime{endpointRuntime: newEndpointRuntime(config.EndpointConfig{Mode: "tcp-listen"}), isListen: true}
	for range clientCount {
		client := &outputClient{queue: make(chan []byte, defaultClientQueue)}
		out.addClient(client)
		go drainUntilCancelled(ctx, client.queue)
	}
	fw.outputs = []*outputRuntime{out}
	if withTap {
		tapped := fw.Subscribe(65536)
		go drainUntilCancelled(ctx, tapped)
	}
	b.Cleanup(cancel)
	return fw
}

func drainUntilCancelled[T any](ctx context.Context, queue chan T) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-queue:
		}
	}
}

// BenchmarkDispatchParallel simulates many input clients (one goroutine each)
// sending NMEA lines at the same time.
func BenchmarkDispatchParallel(b *testing.B) {
	message := []byte("$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47\r\n")
	for _, benchCase := range []struct {
		name        string
		clientCount int
		withTap     bool
	}{
		{"clients=4", 4, false},
		{"clients=4+tap", 4, true},
	} {
		b.Run(benchCase.name, func(b *testing.B) {
			fw := newBenchForwarder(b, benchCase.clientCount, benchCase.withTap)
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					fw.Dispatch(message, "bench")
				}
			})
		})
	}
}

// TestClientListChangesDuringDispatch adds and removes tcp-listen clients and
// tap watchers while other goroutines dispatch, so the race detector checks
// the copy-on-write lists. Afterwards both lists must be empty.
func TestClientListChangesDuringDispatch(t *testing.T) {
	inputRuntime := newEndpointRuntime(config.EndpointConfig{Mode: "tcp-listen"})
	fw := &Forwarder{input: &inputRuntime}
	out := &outputRuntime{endpointRuntime: newEndpointRuntime(config.EndpointConfig{Mode: "tcp-listen"}), isListen: true}
	fw.outputs = []*outputRuntime{out}

	stopDispatching := make(chan struct{})
	var dispatchers sync.WaitGroup
	for range 4 {
		dispatchers.Go(func() {
			for {
				select {
				case <-stopDispatching:
					return
				default:
					fw.Dispatch([]byte("$GPTXT,test\r\n"), "test")
				}
			}
		})
	}
	var changers sync.WaitGroup
	for range 4 {
		changers.Go(func() {
			for range 200 {
				client := &outputClient{queue: make(chan []byte, 4)}
				out.addClient(client)
				tapped := fw.Subscribe(4)
				out.removeClient(client)
				fw.Unsubscribe(tapped)
			}
		})
	}
	changers.Wait()
	close(stopDispatching)
	dispatchers.Wait()

	if clients := out.clients.Load(); clients != nil && len(*clients) != 0 {
		t.Fatalf("%d clients left after all were removed", len(*clients))
	}
	if fw.taps.hasWatchers() {
		t.Fatal("tap watchers left after all unsubscribed")
	}
}
