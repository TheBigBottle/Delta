// Delta – the NMEA Proxy. Named after a river delta: the data arriving on an
// input (from one sender or many) splits into many channels. Delta forwards
// TCP/UDP data streams (e.g. NMEA telegrams) between endpoints and manages the
// forwarders through a password protected web interface.
//
// This command only reads the flags, wires the packages together and handles
// startup and shutdown. The code is organized as:
//
//	cmd/delta          this file: command line, startup and shutdown
//	internal/config    config file format, loading, saving, password hashing
//	internal/engine    the data path: receiving, framing and sending data
//	internal/manager   forwarder lifecycle, rates, alerts, event log, captures
//	internal/api       HTTP API, login and live streams for the web interface
//	web                the web interface (embedded into the binary)
package main

import (
	"context"
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

	"delta/internal/api"
	"delta/internal/config"
	"delta/internal/manager"
)

var version = "1.3.0"

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

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "delta:", err)
		os.Exit(1)
	}

	// Command line flags win over the config file, but are not written back to it.
	webPort, webBind := cfg.Port, cfg.Bind
	if *portFlag != 0 {
		webPort = *portFlag
	}
	if *bindFlag != "" {
		webBind = *bindFlag
	}
	webAddress := net.JoinHostPort(webBind, strconv.Itoa(webPort))

	deltaManager := manager.New(*configPath, cfg)
	deltaManager.WebAddress = webAddress
	deltaManager.Version = version
	listener, err := net.Listen("tcp", webAddress)
	if err != nil {
		fmt.Fprintln(os.Stderr, "delta: web interface:", err)
		os.Exit(1)
	}

	// Every request context derives from serverCtx. Cancelling it ends the
	// long-lived live streams, so the HTTP server can shut down quickly.
	serverCtx, cancelServerCtx := context.WithCancel(context.Background())
	server := &http.Server{
		Handler:           api.NewServer(deltaManager).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return serverCtx },
	}
	deltaManager.StartAutostartForwarders()
	stopSampler := make(chan struct{})
	go deltaManager.RunSampler(stopSampler)
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("web server failed", "err", err)
			os.Exit(1)
		}
	}()
	slog.Info("delta started", "version", version, "web", "http://"+displayAddress(webBind, webPort), "config", *configPath)
	// Recorded in the event log, so restarts are visible in its history.
	deltaManager.Events.Add("info", "", "", "Delta "+version+" started")
	if deltaManager.NeedsPasswordSetup() {
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
	deltaManager.Shutdown()
	deltaManager.Events.Add("info", "", "", "Delta stopped")
	deltaManager.Events.CloseFile()
}

// displayAddress returns an address that can be clicked in the startup log.
func displayAddress(bind string, port int) string {
	if bind == "" || bind == "0.0.0.0" || bind == "::" {
		bind = "localhost"
	}
	return net.JoinHostPort(bind, strconv.Itoa(port))
}
