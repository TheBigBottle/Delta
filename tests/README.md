# Test scripts

Bash scripts that test a running Delta from the outside, through its REST API and real network connections.
They need `bash`, `curl` and `ss`.

| Script | What it does |
|---|---|
| `run-test.sh` | Everything in one step: creates and starts a forwarder on free ports, sends GPS sentences into it, prints what arrives at its output, and deletes the forwarder at the end |
| `create-forwarder.sh` | Creates and starts a test forwarder (UDP or TCP listen input → TCP listen output) on two ports that nothing uses |
| `send-nmea.sh` | Sends `$GPRMC` and `$GPGGA` sentences with the current UTC time and valid checksums to any host and port, once per interval. The data changes like a GPS on a moving boat (`-f` for fixed data, `-S` for a repeatable random seed) |
| `timing-test.sh` | Runs a list of intervals through a real forwarder and shows the achieved interval, jitter, rate and lost sentences for each |
| `load-test.sh` | Starts its own temporary Delta and measures its CPU and memory with a lot of data, with many forwarders, and with both (needs `socat` as a UDP sink) |
| `common.sh` | Shared helpers (API calls, free ports, NMEA checksum); loaded by the other scripts |

Every script prints its options with `-h`.

## Examples

```bash
export DELTA_PASSWORD=yourpassword        # or use -p; without it the scripts ask
export DELTA_URL=http://localhost:8470    # the default

tests/run-test.sh                         # one round per second until Ctrl+C, then clean up
tests/run-test.sh -i 0.2 -c 50            # every 0.2 s, 50 rounds
tests/run-test.sh -m tcp -s rmc -k        # TCP input, only $GPRMC, keep the forwarder afterwards

# Step by step: create a forwarder, then send into it as long as you like
read -r id mode input_port output_port < <(tests/create-forwarder.sh)
tests/send-nmea.sh -m "$mode" -P "$input_port" -i 2

tests/send-nmea.sh -P 10110 -i 0.1 -t 60 -S 42  # 10 rounds/s for one minute, repeatable data
tests/timing-test.sh                            # how fast can this machine send?
tests/timing-test.sh -m tcp -l "0.01 0.001 0"   # only some intervals, over TCP
```

## How short can the interval be?

`send-nmea.sh` keeps to its schedule (start + n × interval) and starts no processes while it sends, so the time it needs per round sets the limit.
Measured with `timing-test.sh` on the development machine, sending through a Delta forwarder (UDP and TCP gave the same results):

| Interval | Achieved | Jitter | Lost |
|---|---|---|---|
| 1 s to 2 ms | within 0.1 % of the requested interval | about 0.1 ms | 0 |
| 1 ms | 1.003 ms (996 rounds/s) | 64 µs | 0 |
| 0.9 ms | 0.905 ms (1104 rounds/s) | 80 µs | 0 |
| below 0.83 ms, or 0 | stays at about 0.82 ms (about 1200 rounds/s, 2400 sentences/s) | 40 to 100 µs | 0 |

So the shortest usable interval is about 0.85 ms with both sentences per round, and about 0.55 ms with `-s rmc` (about 1900 rounds/s).
Below that bash cannot build the sentences any faster; the script then counts the rounds as "late" and simply sends as fast as it can.
Delta itself forwarded every sentence at all of these rates. Most of the time per round goes into the NMEA checksum, which bash computes two characters per step with a lookup table.
Run `tests/timing-test.sh` to measure your own machine.

## System load

`tests/load-test.sh -n 500` measures a temporary Delta (it never touches your running instance) in six scenarios, 10 s each, with one live stream open like a browser tab.
Results from the development machine (32 cores, Delta 1.6.0, everything on loopback):

| Scenario | Forwarders | Sentences/s | MB/s in / out | Delta CPU | Memory (RSS) |
|---|---|---|---|---|---|
| idle | 0 | 0 | 0 / 0 | 0 % | 16 MB |
| one forwarder, as fast as possible over TCP | 1 | 1.95 million | 135 / 135 | 128 % | 23 MB |
| the same with 4 outputs | 1 | 1.47 million | 101 / 405 | 343 % | 39 MB |
| many forwarders, no traffic | 500 | 0 | 0 / 0 | 0 % | 245 MB |
| many forwarders, 10 sentences/s each | 500 | 5,000 | 0.3 / 0.3 | 19 % | 279 MB |
| many forwarders, as fast as 4 bash senders can | 500 | 356,000 | 25 / 25 | 545 % | 380 MB |
| many forwarders, no traffic | 2000 | 0 | 0 / 0 | 1 % | 955 MB |
| many forwarders, 10 sentences/s each | 2000 | 20,000 | 1.4 / 1.4 | 47 % | 1194 MB |
| many forwarders, as fast as 4 bash senders can | 2000 | 347,000 | 24 / 24 | 577 % | 1405 MB |

CPU is in % of one core. No scenario lost data or slowed down the web interface: the forwarder list answered in 2 to 15 ms throughout.

- A TCP stream is cheap: one read brings many sentences, about 0.7 µs of CPU per sentence. UDP costs about 16 µs per sentence, because every datagram needs its own receive and send. In the "as fast as possible" UDP rows the four bash senders were the limit, not Delta (Delta used about 6 of the 32 cores).
- Each forwarder costs about 0.5 MB of memory, even without traffic. About half of it is the output queue, which is reserved in full when the forwarder starts (10,000 messages by default). With `-q 100` (a queue of 100 messages) 2000 idle forwarders used 442 MB instead of 955 MB.
- An open browser tab receives the complete forwarder list every second: about 1 KB per forwarder, so about 0.5 MB/s with 500 forwarders and 2 MB/s with 2000. Delta copes; the browser has to parse that much JSON every second.

Free ports are picked at random between 20000 and 29999. A port counts as used if something on this machine has it open, or if any Delta forwarder has it configured, even a stopped one.
The simulated boat starts in Hamburg harbour (53°33' N, 9°59' E) at 4 to 8 knots; with `-f` it stays there.
