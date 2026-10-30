# Delta – the NMEA Proxy

Named after a river delta: the data arriving on an input splits into many channels.

Delta forwards TCP/UDP data streams, such as NMEA telegrams or raw bit streams, with very little added latency.
An input can receive from many senders at once (several clients on a listen port, several devices Delta connects to,
or UDP datagrams from any number of hosts); their messages are merged, and every output gets a copy.
You manage it through a password-protected web interface.
It is a single static binary with no dependencies, and the web UI is built in.

## Build & run

```bash
scripts/build.sh                    # format check, vet, tests, static binary ./delta
scripts/build.sh --all              # also amd64/arm64/armv7/386 binaries in dist/
VERSION=1.1.0 scripts/build.sh      # embed a version number (--skip-tests to skip the tests)
./delta                             # uses ./delta.json, creates it on first start
./delta -config /etc/delta/delta.json -port 8080
```

| Flag | Meaning |
|---|---|
| `-config <file>` | Config file (default `delta.json`). Created on first start if missing. |
| `-port <n>` | Web interface port. Overrides the config file. |
| `-bind <addr>` | Web interface bind address. Overrides the config file. |
| `-version` | Print the version. |

Open `http://<server>:8470`. On first start (no password configured) the web interface asks you to choose a password.
Until then, every other page and API call is refused.
Set the password right after starting Delta: whoever opens the page first can choose it.

**Password storage:** the config file never contains the password itself, only a salted PBKDF2-SHA256 hash (`password_hash`).
**Forgot the password?** Stop Delta, add `"password": "new-password"` (at least 8 characters) to the config file, and start Delta again.
Delta replaces it with the hash on startup. Configs from older versions that contain a plain `password` are converted the same way.

## Run as a systemd service

Download the binary for your platform from the [releases page](https://github.com/TheBigBottle/Delta/releases)
(`delta-linux-amd64`, `-arm64`, `-armv7` or `-386`; check it against `SHA256SUMS`) or build it with `scripts/build.sh`.

**1. Install the binary and create a service user**

```bash
sudo install -m 0755 delta-linux-amd64 /usr/local/bin/delta
sudo useradd --system --no-create-home --shell /usr/sbin/nologin delta
```

**2. Create the unit file** `/etc/systemd/system/delta.service`:

```ini
[Unit]
Description=Delta – the NMEA Proxy
Wants=network-online.target
After=network-online.target

[Service]
User=delta
Group=delta
# Creates /var/lib/delta (owned by the delta user) for the config file and captures.
StateDirectory=delta
StateDirectoryMode=0750
ExecStart=/usr/local/bin/delta -config /var/lib/delta/delta.json
Restart=on-failure
RestartSec=2
# Delta shuts down cleanly on SIGTERM; give running forwarders time to stop.
TimeoutStopSec=15

# Hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
# Uncomment if a forwarder or the web UI must listen on a port below 1024:
#AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
```

The config directory must be writable by the service, because Delta saves the config atomically (temporary file + rename) in the same folder.
`data_dir` (captures) and `event_log` are resolved relative to the config file, so they end up in `/var/lib/delta/delta-data`
and `/var/lib/delta/delta-events.log`. To write the event log to `/var/log/delta/` instead, add `LogsDirectory=delta`
to the `[Service]` section and set `"event_log": "/var/log/delta/events.log"`.

**3. Start it and set the password**

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now delta
systemctl status delta
journalctl -u delta -f            # live log
```

Open `http://<server>:8470` right away and choose the password (see above). Open the web port and the forwarder ports in the firewall if needed (see *Troubleshooting*).

Forwarders that were running are started again after a reboot (`autostart`). To reset a forgotten password, stop the service,
add `"password"` to `/var/lib/delta/delta.json` and start it again.
To update, replace `/usr/local/bin/delta` and run `sudo systemctl restart delta`.

## Config file

One JSON file holds the settings and all forwarders. Delta rewrites it atomically whenever you change something in the UI.
You can also edit it by hand while Delta is stopped. Unknown keys are rejected, so typos get noticed.
Forwarders without an `id` get one automatically.

```json
{
  "port": 8470,
  "bind": "",
  "password_hash": "pbkdf2-sha256$600000$…",
  "data_dir": "delta-data",
  "session_hours": 24,
  "event_log": "delta-events.log",
  "event_log_max_mb": 10,
  "alerts": { "webhook_url": "", "down_after": 30 },
  "forwarders": [
    {
      "name": "GPS bridge",
      "tags": ["nmea"],
      "comment": "Gateway on the bridge, mast cable",
      "framing": "lines",
      "autostart": true,
      "idle_alert": 10,
      "input":   { "mode": "udp-listen", "port": 10110 },
      "outputs": [
        { "mode": "tcp-listen", "port": 10111, "max_clients": 50 },
        { "name": "plotter", "mode": "tcp-connect", "host": "192.168.1.20", "port": 2000 }
      ]
    }
  ]
}
```

- `comment` holds free-text notes (up to 4000 characters), shown on the detail page and as a note icon in the list.
- `autostart` records whether the forwarder was running. Forwarders that were running come back after a restart or reboot.
- `data_dir` is where captures are stored. A relative path is resolved against the config file's directory.
- `event_log` is the file every event (starts, stops, connects, errors, alerts) is appended to, one line per event in a syslog-style format:
  ```
  2026-10-30T20:37:39.059+01:00 myhost delta[1234]: INFO Delta 1.3.0 started
  2026-10-30T20:37:41.112+01:00 myhost delta[1234]: ERROR [f3a9c2e17b04] GPS bridge: connect 10.0.0.5:4001: connection refused
  ```
  Severity is `INFO`, `WARNING` or `ERROR`; events of a forwarder carry its ID in brackets and its name before the colon.
  A relative path is resolved against the config file's directory; `off` keeps events in memory only.
  At startup the newest 5000 events are loaded again, so the event list survives a restart.
- `event_log_max_mb` (default 10): when the event log reaches this size it is renamed to `<file>.1` (replacing the older one)
  and a new file is started, so at most about twice this size is used. Both settings can also be changed under Settings → Event log.
- `framing`:
  - `lines` splits the stream at `\n`, so every NMEA sentence is forwarded whole, including to UDP targets.
  - `raw` forwards the bytes exactly as they arrive.

### Endpoint modes

| Mode | As input | As output |
|---|---|---|
| `tcp-listen` | Any number of sources connect to Delta (up to `max_clients`); their data is merged | Clients connect and receive the data (`max_clients`, `queue` per client) |
| `tcp-connect` | Delta connects to a source and reconnects automatically. More sources in `targets` are merged | Delta connects to a target, reconnects, and buffers up to `queue` messages while it is down. More targets in `targets` each get a copy and their own buffer |
| `udp-listen` | Receive datagrams on a port, from any sender | — |
| `udp-connect` | Delta registers at a remote host, optionally sending a `hello` datagram every `hello_interval` s | — |
| `udp-send` | — | Send datagrams to a host |

`targets` (tcp-connect only) lists more remote hosts besides `host`/`port`, e.g. `"targets": [{"host": "10.0.0.6", "port": 4001}]`. Delta keeps one connection to each; while only some are connected the endpoint state is `partial`.

`host` is the bind address in listen modes (empty means all interfaces; the editor offers the machine's IP addresses in a dropdown) and the remote host otherwise.

## Robustness

- Every output and every connected client has its own bounded queue. A slow or stuck receiver never blocks the input or the other outputs. When a queue is full, the oldest data is dropped and counted.
- Write timeouts (10 s) drop peers that stop reading. TCP keepalive (15 s) detects dead peers. Remote closes are noticed even while no data is flowing.
- Reconnects use exponential backoff with jitter, from 200 ms up to 10 s. DNS names are resolved again on every attempt.
- Port conflicts are reported immediately. A forwarder that fails to start, for example at boot, is retried every 15 s.
- A panic in one worker only affects that connection or worker, never the process.

Measured on loopback (`go test -run Latency -v ./internal/engine`), TCP source → Delta → TCP client, NMEA sentences: **p50 ≈ 31 µs, p99 ≈ 55 µs**. Both TCP hops are included.

## Web interface

- **Forwarders:** sortable and filterable list with tags, comments, live throughput sparklines, clients in/out, and bulk start/stop/restart/tag/delete/export.
- **Detail page:**
  - A live diagram of the input and outputs, with their states.
  - A throughput chart covering the last 10 minutes.
  - Inspector with Text, Hex and NMEA views. The NMEA view checks checksums. The inspector is rate limited and filterable, and never slows down forwarding.
  - A list of connected clients, each with a disconnect button.
  - Events, captures, and the configuration.
- **Inspector limits:** the browser receives at most the chosen messages per second (10–2000), at most 256 KB/s of data, and only the first "Preview" bytes of each message (256 B–16 KB). Everything else is counted as skipped, and forwarding is never slowed down. Long messages are collapsed, and a click expands them. Filter matches are highlighted.
- **Capture viewer:** the "Inspect" button on a capture shows its messages with the same Text/NMEA/Hex views. Pages load while you scroll. The search runs on the server over the whole file (text, or `/regex/` with an optional `i` for ignore case).
- **Captures:** record traffic with timestamps (`.dcap`) and download it as `.dcap` or as raw data. Replay it into any running forwarder at original speed, at N× speed, at a fixed interval, or as fast as possible, optionally in a loop. You can also upload `.dcap` files or plain NMEA logs.
- **Alerts:**
  - *Down:* the forwarder failed to start, or an endpoint has been disconnected for longer than `down_after`.
  - *Idle:* no input data for longer than `idle_alert`.
  - Alerts are sent to a webhook (works with Slack, Mattermost, Teams, Discord, ntfy, …) and written to the event log.
- **Events:** one log of everything, filterable by level and searchable. It is also written to the event log file and survives restarts.
- **Settings:** webhook, event log file, password change, JSON export and import.
- **Help:** built-in documentation with tutorials, diagrams and annotated examples of every screen (`#/help`, or press `?`).

## REST API

Every UI action is available over HTTP. Scripts can use basic auth with the password:

```bash
curl -u :PASSWORD http://localhost:8470/api/forwarders
curl -u :PASSWORD -X POST http://localhost:8470/api/forwarders/<id>/stop
curl -u :PASSWORD -X POST 'http://localhost:8470/api/forwarders?start=1' \
     -d '{"name":"ais","framing":"lines","input":{"mode":"tcp-connect","host":"10.0.0.5","port":4001},"outputs":[{"mode":"udp-send","host":"10.0.0.9","port":10110}]}'
```

Main routes:
- `GET|POST /api/forwarders`
- `GET|PUT|DELETE /api/forwarders/{id}`
- `POST /api/forwarders/{id}/{start|stop|restart|kick|capture-start|capture-stop|replay-stop}`
- `GET /api/forwarders/{id}/history`
- `GET /api/forwarders/{id}/inspect` (SSE)
- `POST /api/bulk`
- `GET /api/stream` (SSE)
- `GET /api/events`
- `GET /api/interfaces` (IP addresses of the machine, offered as bind addresses)
- `GET /api/export`, `POST /api/import`
- `GET|PUT /api/settings`
- `GET|POST /api/captures`
- `GET|DELETE /api/captures/{name}`
- `GET /api/captures/{name}/records?from=&limit=&bytes=&filter=` (paged; `next` in the response continues)
- `POST /api/captures/{name}/replay`

## Troubleshooting

**A listen port works from localhost but not from other machines.**
Delta listens on all interfaces unless a bind address is set; check with `ss -ltnp 'sport = :10110'` (it should show `*:10110`).
If it does, a host firewall is blocking the port. On systems with firewalld (Fedora, RHEL, openSUSE, Arch setups …) only a few services are open by default:

```bash
sudo firewall-cmd --permanent --add-port=10110/tcp   # use /udp for UDP inputs
sudo firewall-cmd --reload
```

For ufw: `sudo ufw allow 10110/tcp`.

**An output shows "host unreachable" although the target answers ping.** A firewall on the *target* rejects that port; open it there (same commands).

## Development

```bash
go test -race ./...        # all tests; the engine tests run over real sockets (fan-out, framing, reconnect, slow clients, leaks, latency)
go run ./cmd/delta -config /tmp/delta-test/delta.json -port 18470   # run from source
```

Project layout (standard Go layout, standard library only):

```
cmd/delta/          main package: command line, startup, shutdown
internal/config/    config file format, validation, atomic saving, password hashing
internal/engine/    the data path: endpoints, framing, per-output queues, reconnects
internal/manager/   forwarder lifecycle, rates, alerts, event log, captures and replay
internal/api/       HTTP API, login, live streams (server-sent events)
web/                the web interface (HTML, CSS, JavaScript), embedded into the binary
scripts/build.sh    format check, vet, tests, static (cross-)builds
```

**Releases** are made locally with the [GitHub CLI](https://cli.github.com/) (`gh auth login` once):

```bash
VERSION=1.3.0 scripts/build.sh --all                 # tests + all binaries in dist/
(cd dist && sha256sum delta-* > SHA256SUMS)
git tag -a v1.3.0 -m "Delta 1.3.0" && git push origin v1.3.0
gh release create v1.3.0 dist/delta-* dist/SHA256SUMS --title "Delta v1.3.0" --generate-notes
```

The web interface uses plain HTTP. If you expose it beyond a trusted network, put it behind a TLS reverse proxy.

## AI disclaimer

Delta was developed with substantial help from an AI coding assistant (Anthropic's Claude), under human direction and review.
The code is covered by automated tests and was tested by hand, but it may still contain mistakes.
Review it and test it in your own environment before relying on it, especially for safety-relevant navigation data.
Delta is provided as is, without warranty of any kind.
