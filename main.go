// Delta – the NMEA Proxy. Named after a river delta: one stream of data
// splits into many channels. Delta forwards TCP/UDP data streams (e.g. NMEA
// telegrams) between endpoints and manages the forwarders through a password
// protected web interface.
//
// Files of this program:
//
//	main.go     command line, startup and shutdown
//	config.go   config file format, loading and saving
//	engine.go   the data path: receiving, framing and sending data
//	manager.go  forwarder lifecycle, persistence, rates, alerts, event log
//	capture.go  recording traffic to files and replaying it
//	api.go      HTTP API, login and live streams for the web interface
//	web/        the web interface (embedded into the binary)
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

var version = "1.3.0"

//go:embed web
var embeddedWebFiles embed.FS

func main() {
	configPath := flag.String("config", "delta.json", "config file; created if missing (the web interface then asks for a password)")
	portFlag := flag.Int("port", 0, "web interface port (overrides the config file)")
	bindFlag := flag.String("bind", "", "web interface bind address (overrides the config file)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("delta", version)
		return
	}

	config, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "delta:", err)
		os.Exit(1)
	}

	// Command line flags win over the config file, but are not written back to it.
	webPort, webBind := config.Port, config.Bind
	if *portFlag != 0 {
		webPort = *portFlag
	}
	if *bindFlag != "" {
		webBind = *bindFlag
	}
	webAddress := net.JoinHostPort(webBind, strconv.Itoa(webPort))

	manager := newManager(*configPath, config)
	manager.webAddress = webAddress
	listener, err := net.Listen("tcp", webAddress)
	if err != nil {
		fmt.Fprintln(os.Stderr, "delta: web interface:", err)
		os.Exit(1)
	}

	// Every request context derives from serverCtx. Cancelling it ends the
	// long-lived live streams, so the HTTP server can shut down quickly.
	serverCtx, cancelServerCtx := context.WithCancel(context.Background())
	server := &http.Server{
		Handler:           newServer(manager).routes(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return serverCtx },
	}
	manager.StartAutostartForwarders()
	stopSampler := make(chan struct{})
	go manager.RunSampler(stopSampler)
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("web server failed", "err", err)
			os.Exit(1)
		}
	}()
	slog.Info("delta started", "version", version, "web", "http://"+displayAddress(webBind, webPort), "config", *configPath)
	if manager.NeedsPasswordSetup() {
		// Until a password is set, whoever opens the web interface first can set it.
		slog.Warn("no password set: open the web interface now to choose one", "web", "http://"+displayAddress(webBind, webPort))
	}

	// Run until Ctrl+C or SIGTERM (e.g. systemctl stop), then shut down cleanly.
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	<-signalCtx.Done()
	slog.Info("shutting down")
	cancelServerCtx()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelShutdown()
	server.Shutdown(shutdownCtx)
	close(stopSampler)
	manager.Shutdown()
}

// displayAddress returns an address that can be clicked in the startup log.
func displayAddress(bind string, port int) string {
	if bind == "" || bind == "0.0.0.0" || bind == "::" {
		bind = "localhost"
	}
	return net.JoinHostPort(bind, strconv.Itoa(port))
}
