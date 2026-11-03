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
	"delta/internal/updater"
)

// version is set by release builds (VERSION=1.8.0 scripts/build.sh, which
// passes -X main.version). A build without it is "dev": the update check never
// replaces such a self-built binary with a release.
var version = "dev"

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
	deltaManager.Updater = updater.New(version)
	deltaManager.Updater.RemoveLeftovers() // of an install that was cut short
	// Testing hook: point the update check at a local fake of the GitHub API.
	if releaseURL := os.Getenv("DELTA_RELEASE_URL"); releaseURL != "" {
		deltaManager.Updater.ReleaseURL = releaseURL
	}
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
	go deltaManager.RunUpdateChecker(stopSampler)
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

	// Run until Ctrl+C or SIGTERM (e.g. systemctl stop), or until an update
	// was installed, then shut down cleanly.
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	newBinaryPath := ""
	select {
	case <-signalCtx.Done():
		slog.Info("shutting down")
	case newBinaryPath = <-deltaManager.RestartRequests():
		slog.Info("update installed, restarting", "binary", newBinaryPath)
	}
	cancelServerCtx()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelShutdown()
	server.Shutdown(shutdownCtx)
	close(stopSampler)
	deltaManager.Shutdown()
	deltaManager.Events.Add("info", "", "", "Delta stopped")
	deltaManager.Events.CloseFile()

	// A stop request (e.g. systemctl stop, reboot) that arrived during the
	// shutdown above wins over the restart: the new binary is already in
	// place and starts the next time Delta is started.
	if newBinaryPath != "" && signalCtx.Err() == nil {
		restartInto(newBinaryPath)
	}
}

// restartInto replaces this process with the freshly installed binary, with
// the same arguments and environment. The process ID stays the same, so a
// service manager such as systemd does not notice the restart. All sockets
// are already closed by the shutdown above (Go opens them close-on-exec
// anyway). The forwarders come back from their autostart flags.
//
// The path comes from the updater: after the binary was renamed,
// os.Executable() would point to the old file ("<binary>.old").
func restartInto(binaryPath string) {
	err := syscall.Exec(binaryPath, os.Args, os.Environ())
	// Exec only returns on failure. Exit with an error, so a service manager
	// set to restart on failure starts Delta again (with the new binary).
	slog.Error("restart failed; start Delta again by hand", "binary", binaryPath, "err", err)
	os.Exit(1)
}

// displayAddress returns an address that can be clicked in the startup log.
func displayAddress(bind string, port int) string {
	if bind == "" || bind == "0.0.0.0" || bind == "::" {
		bind = "localhost"
	}
	return net.JoinHostPort(bind, strconv.Itoa(port))
}
