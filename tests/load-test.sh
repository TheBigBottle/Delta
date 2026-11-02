#!/usr/bin/env bash
# Load test: how much CPU and memory does Delta use when it forwards a lot of
# data, when it has many forwarders, and when both happen at once?
#
# Starts its own temporary Delta (your running instance is not touched), runs
# the scenarios below one after the other, measures the Delta process from
# /proc while each one runs, prints a table and removes everything again.
#
#   idle        Delta without forwarders
#   1 fwd max   one forwarder, NMEA sent as fast as possible over TCP, 1 output
#   1 fwd x4    the same with 4 outputs (every message copied 4 times)
#   N idle      N running forwarders without traffic
#   N x R/s     N forwarders, each receiving R sentences per second over UDP
#   N max       N forwarders, as many sentences as 4 bash senders can produce
#
# During all scenarios one live stream (/api/stream) is open, like a browser tab.
#
# Usage: tests/load-test.sh [options]
#   -n COUNT     number of forwarders for the "N" scenarios (default 500)
#   -r RATE      sentences per second and forwarder for "N x R/s" (default 10)
#   -d SECONDS   measuring time per scenario (default 10)
#   -q QUEUE     queue size of the outputs of the N forwarders (default: Delta's default, 10000)
#   -b BINARY    the Delta binary (default: ./delta in the project folder)
#   -h           show this help
#
# Needs bash, curl, ss and socat (as a UDP sink for the outputs).
#
# Columns:
#   msgs/s in    sentences per second Delta received (all forwarders together)
#   MB/s in/out  data received and sent (out is higher when there are several outputs)
#   Delta CPU    CPU time of the Delta process, in % of one core (200 % = two cores busy)
#   system CPU   CPU use of the whole machine (all cores = 100 %), load generators included
#   RSS          memory of the Delta process (resident), in MB
#   threads/fds  operating system threads and open file descriptors of Delta
#   list ms      time for GET /api/forwarders
#   stream KB/s  what an open browser tab receives from /api/stream
set -euo pipefail
script_dir=$(cd "$(dirname "$0")" && pwd)
source "$script_dir/common.sh"

forwarder_count=500
rate_per_forwarder=10
measure_seconds=10
delta_binary="$script_dir/../delta"
output_queue=""
while getopts "n:r:d:b:q:h" option; do
  case $option in
    n) forwarder_count=$OPTARG ;;
    r) rate_per_forwarder=$OPTARG ;;
    d) measure_seconds=$OPTARG ;;
    b) delta_binary=$OPTARG ;;
    q) output_queue=$OPTARG ;;
    h) sed -n '2,38p' "$0"; exit 0 ;;
    *) exit 1 ;;
  esac
done
[[ -x $delta_binary ]] || die "Delta binary not found: $delta_binary (build it with scripts/build.sh, or use -b)"
command -v socat >/dev/null || die "socat is needed as a UDP sink (e.g. pacman -S socat / apt install socat)"
enable -f sleep sleep 2>/dev/null || enable -f /usr/lib/bash/sleep sleep 2>/dev/null || true  # no process per pause

# ---- free ports ----

# Ports in use on this machine, as " 22 80 8470 … " for quick lookups.
used_ports=" $(ss -H -l -n -t -u 2>/dev/null | awk '{ n = split($5, parts, ":"); print parts[n] }' | sort -u | tr '\n' ' ') "

# Finds COUNT consecutive free ports between 30000 and 60000 and stores the
# first one in $block_start. Ports handed out before are skipped too.
# (It sets a variable instead of printing, because $(…) would run it in a
# subshell, and the subshell's next_free_port would be forgotten.)
next_free_port=30000
free_port_block() {
  local wanted=$1 start=$next_free_port port
  for ((port = next_free_port; port < 60000; port++)); do
    if [[ $used_ports == *" $port "* ]]; then
      start=$((port + 1))
    elif ((port - start + 1 == wanted)); then
      next_free_port=$((port + 1))
      block_start=$start
      return
    fi
  done
  die "no block of $wanted free ports found"
}

# ---- the temporary Delta ----

work_dir=$(mktemp -d)
background_pids=()
delta_pid=""
cleanup() {
  for pid in "${background_pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  [[ -n $delta_pid ]] && kill "$delta_pid" 2>/dev/null && wait "$delta_pid" 2>/dev/null
  rm -rf "$work_dir"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

free_port_block 1; web_port=$block_start
"$delta_binary" -config "$work_dir/delta.json" -port "$web_port" -bind 127.0.0.1 >"$work_dir/delta.log" 2>&1 &
delta_pid=$!
DELTA_URL="http://127.0.0.1:$web_port"
DELTA_PASSWORD="load-test-$RANDOM$RANDOM"
for _ in {1..50}; do curl -s -o /dev/null "$DELTA_URL/api/setup" && break; sleep 0.1; done
curl -s -f -o /dev/null "$DELTA_URL/api/setup" -d "{\"password\":\"$DELTA_PASSWORD\"}" || die "could not set up the temporary Delta"
echo "temporary Delta $("$delta_binary" -version) (pid $delta_pid) on $DELTA_URL, $(nproc) CPU cores" >&2

# One open live stream for the whole test, like a browser tab.
curl -s -N -u ":$DELTA_PASSWORD" "$DELTA_URL/api/stream" >/dev/null &
background_pids+=($!)

# The outputs of the many forwarders send their datagrams here.
free_port_block 1; sink_port=$block_start
socat -u "UDP-RECV:$sink_port,bind=127.0.0.1,rcvbuf=8388608" /dev/null 2>/dev/null &
background_pids+=($!)

# ---- measuring ----

# CPU ticks (1/100 s) the Delta process has used so far: user + system time.
delta_cpu_ticks() {
  local fields
  read -r -a fields <"/proc/$delta_pid/stat"
  echo $((fields[13] + fields[14]))
}

# Busy and total ticks of the whole machine (all cores), from /proc/stat.
system_ticks() {
  local label user nice system idle iowait irq softirq steal _
  read -r label user nice system idle iowait irq softirq steal _ </proc/stat
  echo "$((user + nice + system + irq + softirq + steal)) $((user + nice + system + idle + iowait + irq + softirq + steal))"
}

# Total bytes in, bytes out and messages of all forwarders: "BYTES_IN BYTES_OUT MESSAGES".
# The list has "bytes_in":…,"bytes_out":…,"chunks":… once per forwarder at the top level.
traffic_totals() {
  delta_api GET /api/forwarders | awk 'BEGIN { RS = "\"bytes_in\":" }
    NR > 1 && match($0, /^[0-9]+,"bytes_out":[0-9]+,"chunks":[0-9]+/) {
      text = substr($0, RSTART, RLENGTH); gsub(/[^0-9,]/, "", text); split(text, value, ",")
      bytes_in += value[1]; bytes_out += value[2]; messages += value[3]
    }
    END { printf "%d %d %d\n", bytes_in, bytes_out, messages }'
}

status_value() { awk -v key="$1:" '$1 == key { print $2 }' "/proc/$delta_pid/status"; }

printf '\n%-14s %10s %10s %10s %10s %10s %8s %8s %7s %8s %11s\n' \
  scenario forwarders "msgs/s in" "MB/s in" "MB/s out" "Delta CPU" "system" "RSS MB" threads "list ms" "stream KB/s"

# Measures for measure_seconds and prints one row of the table.
measure() {
  local name=$1 forwarders=$2
  local cpu_before system_before traffic_before start_us end_us cpu_after system_after traffic_after
  local seconds_us busy_before total_before busy_after total_after in_before out_before messages_before
  local in_after out_after messages_after list_seconds stream_bytes
  sleep 2  # let the load settle
  cpu_before=$(delta_cpu_ticks); system_before=$(system_ticks); traffic_before=$(traffic_totals)
  start_us=${EPOCHREALTIME//[.,]/}
  sleep "$measure_seconds"
  end_us=${EPOCHREALTIME//[.,]/}
  cpu_after=$(delta_cpu_ticks); system_after=$(system_ticks); traffic_after=$(traffic_totals)
  seconds_us=$((end_us - start_us))
  read -r busy_before total_before <<<"$system_before"
  read -r busy_after total_after <<<"$system_after"
  read -r in_before out_before messages_before <<<"$traffic_before"
  read -r in_after out_after messages_after <<<"$traffic_after"
  list_seconds=$(curl -s -o /dev/null -w '%{time_total}' -u ":$DELTA_PASSWORD" "$DELTA_URL/api/forwarders")
  # 3 seconds of the live stream: the first full list plus about three updates.
  stream_bytes=$(curl -s -N --max-time 3 -u ":$DELTA_PASSWORD" "$DELTA_URL/api/stream" | wc -c || true)
  printf '%-14s %10d %10d %10s %10s %9d%% %7d%% %8d %7d %8s %11s\n' "$name" "$forwarders" \
    $(((messages_after - messages_before) * 1000000 / seconds_us)) \
    "$(awk -v b=$((in_after - in_before)) -v us="$seconds_us" 'BEGIN { printf "%.1f", b / us }')" \
    "$(awk -v b=$((out_after - out_before)) -v us="$seconds_us" 'BEGIN { printf "%.1f", b / us }')" \
    $(((cpu_after - cpu_before) * 10000 * 100 / seconds_us)) \
    $(((busy_after - busy_before) * 100 / (total_after - total_before))) \
    $(($(status_value VmRSS) / 1024)) "$(status_value Threads)" \
    "$(awk -v s="${list_seconds/,/.}" 'BEGIN { printf "%.1f", s * 1000 }')" \
    "$(awk -v b="$stream_bytes" 'BEGIN { printf "%.1f", b / 3 / 1024 }')"
}

# Stops the load generators started for a scenario (not the stream and the sink).
stop_generators() {
  local pid
  for pid in "${generator_pids[@]}"; do
    pkill -P "$pid" 2>/dev/null || true  # e.g. the cat inside a sender loop
    kill "$pid" 2>/dev/null || true
  done
  for pid in "${generator_pids[@]}"; do wait "$pid" 2>/dev/null || true; done
  generator_pids=()
}
generator_pids=()

# ---- scenario: idle ----

measure "idle" 0

# ---- scenarios: one forwarder with as much data as possible ----

# 2000 different sentences (about 140 KB) that the sender repeats endlessly.
sentence_file="$work_dir/sentences.nmea"
for ((index = 0; index < 2000; index++)); do
  printf -v time_field '%06d' "$index"
  printf -v minutes_field '%04d' "$index"
  nmea_build sentence "GPRMC,$time_field.00,A,5333.$minutes_field,N,00959.0000,E,5.2,123.4,021126,,,A"
  printf '%s' "$sentence"
done >"$sentence_file"

# Creates a forwarder with a tcp-listen input and OUTPUTS tcp-listen outputs,
# blasts the file into it over TCP and reads every output with cat.
run_blast_scenario() {
  local name=$1 output_count=$2 first_port outputs="" index forwarder_id
  free_port_block $((output_count + 1)); first_port=$block_start
  for ((index = 1; index <= output_count; index++)); do
    outputs+="${outputs:+,}{\"mode\":\"tcp-listen\",\"host\":\"127.0.0.1\",\"port\":$((first_port + index)),\"queue\":100000}"
  done
  forwarder_id=$(delta_api POST '/api/forwarders?start=1' \
    "{\"name\":\"$name\",\"framing\":\"lines\",\"input\":{\"mode\":\"tcp-listen\",\"host\":\"127.0.0.1\",\"port\":$first_port},\"outputs\":[$outputs]}" | json_value id)
  for ((index = 1; index <= output_count; index++)); do
    cat </dev/tcp/127.0.0.1/$((first_port + index)) >/dev/null &
    generator_pids+=($!)
  done
  sleep 0.3
  (while cat "$sentence_file"; do :; done >"/dev/tcp/127.0.0.1/$first_port") 2>/dev/null &
  generator_pids+=($!)
  measure "$name" 1
  stop_generators
  delta_api DELETE "/api/forwarders/$forwarder_id" >/dev/null
}
run_blast_scenario "1 fwd max" 1
run_blast_scenario "1 fwd x4" 4

# ---- scenarios: many forwarders ----

# N forwarders: udp-listen on consecutive ports, output udp-send to the sink.
# Created with one import (one config save) and started with one bulk call.
free_port_block "$forwarder_count"; first_input_port=$block_start
import_json='{"forwarders":['
for ((index = 0; index < forwarder_count; index++)); do
  ((index > 0)) && import_json+=,
  import_json+="{\"name\":\"load $index\",\"framing\":\"lines\",\"input\":{\"mode\":\"udp-listen\",\"host\":\"127.0.0.1\",\"port\":$((first_input_port + index))},\"outputs\":[{\"mode\":\"udp-send\",\"host\":\"127.0.0.1\",\"port\":$sink_port${output_queue:+,\"queue\":$output_queue}}]}"
done
import_json+=']}'
delta_api POST /api/import "$import_json" >/dev/null
forwarder_ids=$(delta_api GET /api/forwarders | grep -o '"id":"[^"]*"' | cut -d'"' -f4 | paste -sd, | sed 's/[^,]*/"&"/g')
delta_api POST /api/bulk "{\"ids\":[$forwarder_ids],\"action\":\"start\"}" >/dev/null
echo "created and started $forwarder_count forwarders (UDP ports $first_input_port to $((first_input_port + forwarder_count - 1)))" >&2

measure "N idle" "$forwarder_count"

# Sends one sentence to each port of a range per sweep, SWEEPS_PER_SECOND times
# per second (0 = as fast as possible). One UDP socket per port stays open.
udp_fan_out() {
  local first=$1 count=$2 sweeps_per_second=$3 fds=() fd index next_us now_us interval_us=0
  local sentence pause
  nmea_build sentence "GPRMC,120000.00,A,5333.0000,N,00959.0000,E,5.2,123.4,021126,,,A"
  for ((index = 0; index < count; index++)); do
    exec {fd}>"/dev/udp/127.0.0.1/$((first + index))"
    fds+=("$fd")
  done
  ((sweeps_per_second > 0)) && interval_us=$((1000000 / sweeps_per_second))
  next_us=${EPOCHREALTIME//[.,]/}
  while :; do
    for fd in "${fds[@]}"; do printf '%s' "$sentence" >&"$fd" 2>/dev/null || true; done
    if ((interval_us > 0)); then
      next_us=$((next_us + interval_us))
      now_us=${EPOCHREALTIME//[.,]/}
      if ((next_us > now_us)); then
        printf -v pause '%d.%06d' $(((next_us - now_us) / 1000000)) $(((next_us - now_us) % 1000000))
        sleep "$pause"
      fi
    fi
  done
}

udp_fan_out "$first_input_port" "$forwarder_count" "$rate_per_forwarder" &
generator_pids+=($!)
measure "N x ${rate_per_forwarder}/s" "$forwarder_count"
stop_generators

# As fast as possible: 4 senders, each covering a quarter of the forwarders.
quarter=$(((forwarder_count + 3) / 4))
for ((index = 0; index < 4; index++)); do
  start=$((index * quarter))
  ((start >= forwarder_count)) && break
  count=$((forwarder_count - start < quarter ? forwarder_count - start : quarter))
  udp_fan_out $((first_input_port + start)) "$count" 0 &
  generator_pids+=($!)
done
measure "N max" "$forwarder_count"
stop_generators

echo >&2
echo "peak memory of Delta during the test: $(($(status_value VmHWM) / 1024)) MB, open files now: $(ls "/proc/$delta_pid/fd" | wc -l)" >&2
