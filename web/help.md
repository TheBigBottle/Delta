## What Delta does

Delta receives data on an **input** and sends a copy of every message to **one or more outputs**. It was built for NMEA 0183 (GPS, AIS, compass, wind …), but works with any TCP or UDP data, including binary streams.

An input and its outputs together are a **forwarder**. You can run as many forwarders as you like, independently of each other.

- An input can receive from many senders at once; their messages are merged into one stream.
- Every message is copied once and shared by all outputs.
- Every receiver has its own queue, so a slow receiver never holds up the others.
- Forwarding takes about 31 µs from input to output on a local machine. The web interface never slows it down.
- Everything is stored in one config file (`delta.json`). Forwarders that were running start again after a restart or reboot.
- Broken connections are retried automatically. Nothing needs a restart after a device reboots.

### Words used in Delta

| Term | Meaning |
|---|---|
| Forwarder | An input and its outputs. |
| Input | Where the data comes from: a port Delta listens on, or devices Delta connects to. |
| Output | Where a copy of the data goes: clients that connect to Delta, devices Delta connects to, or UDP datagrams. |
| Message | With the *lines* format one NMEA sentence; with *raw* whatever arrived in one network read. |
| Client / source | A device or program connected to Delta, or one Delta connected to. |
| Drop | A message thrown away because a receiver was too slow and its queue was full. |
| Capture | A recording of an input with timestamps (a `.dcap` file). |

## Your first forwarder

Example: a GPS gateway sends NMEA as UDP to port `10110`. Several navigation apps should connect to Delta over TCP on port `10111` and all get the same data.

1. On the [Forwarders](#/) page click **New forwarder** (shortcut `N`).
2. Enter a **name**. Tags and a comment are optional.
3. Keep the data format **NMEA / text lines**. Use **Raw byte stream** only for binary data (see [Data format](#/help/data-format)).
4. Input: mode **UDP listen**, port `10110`. Keep the bind address at **All interfaces**.
5. Output: mode **TCP listen**, port `10111`. Use **Add output** to send the same data to more places.
6. Click **Create & start**. Delta opens the detail page of the new forwarder.
7. Check that data arrives: the **Inbound** tile shows the data rate, the **Inspector** tab shows the sentences.
8. In your navigation app, connect over TCP to `<Delta machine IP>:10111`. The app appears on the **Connections** tab.

If the forwarder cannot start, for example because the port is in use, it is still saved. The detail page shows the reason and Delta tries again every 15 s. Change the port with **Edit**, or stop the other program.

> Works on this machine but not from others? A firewall probably blocks the port. See [Troubleshooting](#/help/troubleshooting).

## The interface

### Forwarders page

The start page lists all forwarders with their live state.

- **Search** finds names, tags, comments, modes and addresses (shortcut `/`).
- **Issues** shows only forwarders that failed, have a disconnected endpoint or a firing alert. Click a tag to filter by it.
- **Tick boxes** to start, stop, restart, tag, export or delete several forwarders at once.
- **⋯** next to a forwarder opens edit, duplicate, export and delete.
- Click a **column header** to sort; click again to reverse.

The colored dot shows the health: green = running, orange = running but an endpoint is not connected, red = could not start, grey = stopped.

### Detail page

Click a forwarder's name to open it. At the top: Stop/Start, Restart and Edit, a live diagram of the input and outputs, and tiles with the current numbers. Saving changes to a running forwarder restarts it.

| Tab | Shows |
|---|---|
| Traffic | Data rate of the last 10 minutes. Hover the chart for the values. |
| Inspector | The live data (see [Watch live data](#/help/watch-live-data)). |
| Connections | Every connected client, source and remote host. TCP connections can be disconnected. |
| Events | This forwarder's log. |
| Captures | Record the input (see [Record and replay](#/help/record-and-replay)). |
| Configuration | Summary and the JSON of the forwarder. |

### Other pages

- [Events](#/events): one log of everything (starts, stops, connects, errors, alerts), newest 5000.
- [Captures](#/captures): all recordings, uploads and running replays.
- [Settings](#/settings): alerts, event log file, password, backup, config file location.

### Shortcuts

| Key | What it does |
|---|---|
| `N` | New forwarder (Forwarders page) |
| `/` | Focus the search (Forwarders page) |
| `?` | Open this help |
| `Esc` | Close a dialog or menu |

Hover any button, column header or badge for a short explanation. The sun/moon button at the bottom of the sidebar switches the theme.

## Common setups

These examples show the forwarder JSON as it is used in the config file, in exports and in the [REST API](#/help/rest-api). In the editor, choose the same modes and ports.

### Share one UDP source with many TCP apps

A WiFi gateway broadcasts NMEA as UDP; apps such as OpenCPN prefer TCP.

```json
{
  "name": "GPS to apps",
  "framing": "lines",
  "input": { "mode": "udp-listen", "host": "", "port": 10110 },
  "outputs": [{ "mode": "tcp-listen", "host": "", "port": 10111, "max_clients": 20 }]
}
```

### Fetch from an NMEA server and push to a plotter

Both the multiplexer and the plotter are TCP servers, so Delta connects to both. While the plotter is off, Delta keeps the newest messages for it.

```json
{
  "name": "Multiplexer to plotter",
  "framing": "lines",
  "input": { "mode": "tcp-connect", "host": "192.168.1.40", "port": 4001 },
  "outputs": [
    { "mode": "tcp-connect", "host": "192.168.1.20", "port": 2000 },
    { "mode": "tcp-listen", "host": "", "port": 10111 }
  ]
}
```

### Send into another network over UDP

Devices connect to Delta; Delta sends every sentence as a UDP datagram to a remote host (for example over a VPN).

```json
{
  "name": "Devices to remote",
  "framing": "lines",
  "input": { "mode": "tcp-listen", "host": "", "port": 5000, "max_clients": 5 },
  "outputs": [{ "mode": "udp-send", "host": "10.8.0.1", "port": 10110 }]
}
```

### Several hosts in one endpoint

In **TCP connect** mode, **Add host** adds more hosts. On an input, Delta merges what all hosts send; on an output, every host gets its own copy. If only some hosts are connected, the endpoint shows *partial* and the forwarder counts as degraded.

```json
"input": {
  "mode": "tcp-connect", "host": "192.168.1.40", "port": 4001,
  "targets": [{ "host": "192.168.1.41", "port": 4001 }]
}
```

Tip: once one forwarder works, use **⋯ → Duplicate** for similar ones and only change the ports.

## Watch live data

The **Inspector** tab shows the messages the input receives, live.

- **Text / NMEA / Hex** switch the view. NMEA colors the parts of each sentence and checks the checksum (✓ correct, ✗ wrong).
- **Filter**: type text to show only matching messages. Use `/…/` for a regular expression, e.g. `/^\$GP(RMC|GGA)/`; add `i` after the last slash to ignore case.
- **Rate limit**: how many messages per second the browser gets (10–2000). The server also sends at most 256 KB per second. Messages above the limits are counted as *skipped*.
- **Preview**: how much of each message is transferred (256 B–16 KB).
- **Pause** freezes the view, **Clear** empties it, the download button saves the last 2000 messages as text.

The Inspector never slows down forwarding: when the browser cannot keep up, only inspector data is left out.

## Record and replay

A **capture** records everything an input receives, with the time of every message.

### Record

1. Open the forwarder (it must be running), tab **Captures**.
2. Optionally enter a duration and/or a maximum size.
3. Click **Start recording**. Click **Stop** when done.

The file `<name>-<date>-<time>.dcap` is stored in the `captures` folder of the data directory. Stopping or restarting the forwarder also ends the recording.

### Look at a capture

On the [Captures](#/captures) page click **Inspect**. The viewer has the same views as the Inspector. Its search runs through the whole file on the server.

### Replay

1. Click **Replay** next to a capture and choose a running forwarder.
2. Choose the timing: **Original** (with a speed factor), **Fixed interval**, or **As fast as possible**.
3. Optionally tick **Loop**.

The replayed messages go to all outputs of that forwarder, as if its input had received them.

### Download and upload

- **.dcap** keeps the timestamps; **Raw** contains only the data (e.g. a plain NMEA log).
- **Upload file** accepts `.dcap` files and text logs with one sentence per line (max. 256 MB). Uploads have no timing, so replay them with a fixed interval.

## Alerts

Delta writes an event and can call a **webhook** (Slack, Teams, Mattermost, Discord, ntfy, …) when something stops working.

| Alert | Fires when | Set in |
|---|---|---|
| down | a forwarder could not start, or an endpoint was not connected, for longer than *down after* seconds | Settings (all forwarders) |
| idle | a running forwarder received no input for *idle alert* seconds | the editor of each forwarder |

Set up:

1. [Settings](#/settings) → Alerts: enter the webhook URL, e.g. `https://ntfy.sh/<secret-topic>`.
2. Choose the *down after* delay (default 30 s).
3. Click **Send test**, then **Save**.
4. For forwarders whose silence matters, set **Idle alert (s)** in the editor.

The webhook receives a JSON `POST`. `status` is `FIRING` or `RESOLVED`:

```json
{
  "text": "[Delta] FIRING: GPS bridge is idle — no input data for 12s",
  "content": "[Delta] FIRING: GPS bridge is idle — no input data for 12s",
  "forwarder": "GPS bridge",
  "id": "f4918dc90495",
  "alert": "idle",
  "status": "FIRING",
  "detail": "no input data for 12s",
  "time": "2026-10-30T09:27:50Z"
}
```

## Endpoint modes

Ask: *who opens the connection?* If the other side connects to Delta, use a **listen** mode. If Delta has to reach out, use a **connect** or **send** mode. Data always flows from the input to the outputs.

| Mode | As input | As output |
|---|---|---|
| `tcp-listen` | Devices connect to Delta and send data. | Programs connect to Delta and each gets a copy. Own queue per client (default 1024); a stuck client is disconnected after 10 s. |
| `tcp-connect` | Delta connects to a device that serves data (most multiplexers and WiFi gateways). | Delta connects to a device that accepts data. While it is down, up to *queue* messages (default 10000) are kept. |
| `udp-listen` | Delta receives datagrams from any sender. | – |
| `udp-send` | – | Every message is sent as one datagram. A broadcast address (e.g. 192.168.1.255) reaches the whole network. |

- **Bind address** (listen modes): *All interfaces*, or one IP address of the Delta machine. `127.0.0.1` accepts only programs on the same machine.
- **Host** (connect and send modes): name or IP of the remote device. Names are looked up again for every new connection.
- Port **10110** is the usual port for NMEA over IP.

## Data format

TCP delivers a stream of bytes, not messages: one read can hold half a sentence or several. The data format decides what Delta does with that.

| Format | Use for | Behaviour |
|---|---|---|
| NMEA / text lines (`lines`) | NMEA 0183, AIS, any line-based text | Splits at `\n`, so receivers and UDP datagrams always get whole sentences. Lines over 8 KB are sent in parts. |
| Raw byte stream (`raw`) | Binary protocols | Passes bytes on exactly as they arrive, with the lowest delay. |

With **raw** and a UDP output, a sentence can be split across two datagrams, which most NMEA programs reject. Use **lines** for NMEA.

## Health and states

| Forwarder | Meaning |
|---|---|
| Running (green) | All endpoints are connected or listening. |
| Degraded (orange) | Running, but an endpoint is not (fully) connected. Delta keeps retrying. |
| Error (red) | Could not start, usually because the port is in use or the bind address does not exist. Retried every 15 s. |
| Stopped (grey) | Stopped by you. It stays stopped after a restart of Delta. |

| Endpoint state | Meaning |
|---|---|
| listening | Port is open and waiting. |
| connected | Connection to the remote host is established. |
| connecting | Trying to connect. |
| down | Not connected; retried automatically. |
| open | UDP socket open, no datagram yet. |
| receiving / sending | Datagrams are flowing. |
| unreachable | The remote host reported "port unreachable". |
| partial | Connected to some of several hosts. |

## Queues, drops and reconnects

- Every output, and every client of a TCP listen output, has its own **queue**. The input never waits for a slow receiver.
- When a queue is full, the **oldest** message is dropped and counted in **Drops**. A few drops after a target was offline are normal; drops that keep rising mean a receiver is too slow.
- Queue sizes can be changed in the editor (*Queue*).
- Failed connections are retried after 0.2 s, doubling up to 10 s, plus up to 20 % random jitter. After a connection that lasted 10 s, the delay starts at 0.2 s again.
- Dead connections are noticed by TCP keepalive (15 s) and write timeouts (10 s).
- A repeated error is logged only once.

## Settings and files

### Password

- On first start Delta asks for a password (at least 8 characters). Set it right away: until then, whoever opens the page first can choose it.
- Change it under [Settings](#/settings). The config file stores only a salted hash.
- **Forgot it?** Stop Delta, add `"password": "new-password"` to the config file and start Delta. It replaces the line with the hash.
- A login lasts 24 hours (`session_hours`).

### Config file

All settings and forwarders are in one JSON file. Delta saves it after every change. Edit it by hand only while Delta is stopped. Unknown keys are refused, so typos are noticed.

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
  "forwarders": []
}
```

Relative paths (`data_dir`, `event_log`) are resolved next to the config file.

### Event log file

Every event is appended to `event_log` as one syslog-style line, readable with `less`, `grep` or `tail -f`:

```
2026-10-30T20:37:41.112+01:00 myhost delta[1234]: ERROR [f3a9c2e17b04] GPS bridge: connection refused
```

At `event_log_max_mb` the file is renamed with a timestamp (the previous one is deleted) and a new file is started. At startup the newest 5000 events are loaded again. `"event_log": "off"` keeps events in memory only.

### Command line

| Option | Meaning |
|---|---|
| `-config <file>` | Config file (default `delta.json`), created on first start. |
| `-port <n>` | Port of the web interface (default 8470). |
| `-bind <address>` | Address of the web interface, e.g. `127.0.0.1` for local access only. |
| `-version` | Print the version. |

The web interface uses plain HTTP. Outside a trusted network, put it behind a reverse proxy with HTTPS.

## REST API

Everything the web interface does is available over HTTP. Scripts use HTTP basic auth with the Delta password and any user name. Bodies are JSON; errors come back as `{"error": "…"}`.

```bash
# list all forwarders
curl -u :PASSWORD http://DELTA-HOST:8470/api/forwarders

# stop one
curl -u :PASSWORD -X POST http://DELTA-HOST:8470/api/forwarders/<id>/stop

# create a forwarder and start it
curl -u :PASSWORD -X POST 'http://DELTA-HOST:8470/api/forwarders?start=1' \
     -d '{"name":"ais","framing":"lines","input":{"mode":"tcp-connect","host":"10.0.0.5","port":4001},"outputs":[{"mode":"udp-send","host":"10.0.0.9","port":10110}]}'
```

| Route | Purpose |
|---|---|
| `GET`, `POST /api/forwarders` | List, or create (`?start=1` starts it) |
| `GET`, `PUT`, `DELETE /api/forwarders/{id}` | Read, change, delete one forwarder |
| `POST /api/forwarders/{id}/{action}` | `start`, `stop`, `restart`, `kick`, `capture-start`, `capture-stop`, `replay-stop` |
| `GET /api/forwarders/{id}/history` | Throughput of the last 10 minutes |
| `GET /api/forwarders/{id}/inspect` | Live messages (server-sent events) |
| `POST /api/bulk` | One action for several forwarders: `{"ids": […], "action": "start", "value": ""}` |
| `GET /api/stream` | Live state of all forwarders and new events (server-sent events) |
| `GET /api/events` | Event log, newest first (`?forwarder=<id>`, `?limit=`) |
| `GET /api/interfaces` | IP addresses of the Delta machine |
| `GET /api/export`, `POST /api/import` | Backup and import as `{"forwarders": […]}` |
| `GET`, `PUT /api/settings` | Alerts, event log file, password |
| `POST /api/settings/test-webhook` | Send a test message to `{"url": "…"}` |
| `GET`, `POST /api/captures` | List captures, upload a file |
| `GET`, `DELETE /api/captures/{name}` | Download or delete a capture |
| `GET /api/captures/{name}/records` | Read a capture page by page |
| `POST /api/captures/{name}/replay` | Start a replay: `{"forwarder": "<id>", "speed": 1, "interval_ms": 0, "loop": false}` |
| `GET /api/help` | This help, as `{"html": "…"}` |
| `POST /api/login`, `POST /api/logout` | Browser login (not needed with basic auth) |

## Troubleshooting

### Other machines cannot connect to a listen port

Almost always a firewall on the Delta machine. Open the port:

```bash
# firewalld (Fedora, RHEL, openSUSE, many Arch setups); use /udp for UDP inputs
sudo firewall-cmd --permanent --add-port=10110/tcp
sudo firewall-cmd --reload

# ufw (Ubuntu, Debian)
sudo ufw allow 10110/tcp
```

### "host unreachable" although the device answers ping

A firewall on the target device rejects that port. Open it there.

### "connection refused"

Nothing listens on that port: wrong port, the service is off, or the device expects Delta to be the server (use a listen mode, see [Endpoint modes](#/help/endpoint-modes)).

### "address already in use"

Another forwarder or program uses the port. `ss -ltnup 'sport = :10110'` shows which one.

### Data arrives, but the receiver shows nothing

- Check the sentences in the Inspector: correct checksums? Expected sentence types?
- For UDP outputs use the *lines* format (see [Data format](#/help/data-format)).
- Check the **Connections** tab: is the receiver connected, and does its byte count grow?

### Where do I see what happened?

The [Events](#/events) page lists every connect, disconnect, error and alert.
