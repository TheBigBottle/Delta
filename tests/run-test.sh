#!/usr/bin/env bash
# Complete test in one step: creates and starts a forwarder on free ports,
# sends NMEA GPS sentences into its input at the chosen interval, and shows
# what arrives at its output. At the end (after -c rounds, or Ctrl+C) it
# deletes the forwarder again, unless -k is given.
#
# Usage: tests/run-test.sh [options]
#   -u URL        Delta web interface (default $DELTA_URL or http://localhost:8470)
#   -p PASSWORD   Delta password (default $DELTA_PASSWORD, otherwise asked for)
#   -m udp|tcp    input mode of the forwarder and protocol of the sender (default udp)
#   -i SECONDS    interval between rounds, decimals allowed (default 1)
#   -c COUNT      number of rounds (default 0 = until Ctrl+C)
#   -s rmc|gga|both  which sentences to send (default both)
#   -k            keep the forwarder afterwards (it stays running)
#   -h            show this help
#
# Example: tests/run-test.sh -p mypassword -i 0.5 -c 10
set -euo pipefail
source "$(dirname "$0")/common.sh"
script_dir=$(dirname "$0")

protocol=udp
interval=1
count=0
sentences=both
keep=false
while getopts "u:p:m:i:c:s:kh" option; do
  case $option in
    u) DELTA_URL=$OPTARG ;;
    p) DELTA_PASSWORD=$OPTARG ;;
    m) protocol=$OPTARG ;;
    i) interval=$OPTARG ;;
    c) count=$OPTARG ;;
    s) sentences=$OPTARG ;;
    k) keep=true ;;
    h) sed -n '2,17p' "$0"; exit 0 ;;
    *) exit 1 ;;
  esac
done
require_password
export DELTA_URL DELTA_PASSWORD

# 1. Create and start the forwarder.
created=$("$script_dir/create-forwarder.sh" -m "$protocol" -n "NMEA test $(date +%H:%M:%S)") || exit 1
read -r forwarder_id input_protocol input_port output_port <<<"$created"

# 2. Connect to the output, like a navigation app would, and print what arrives.
exec 4<"/dev/tcp/127.0.0.1/$output_port" || die "cannot connect to the output port $output_port"
receiver_log=$(mktemp)
(while IFS= read -r line <&4; do printf 'received: %s\n' "${line%$'\r'}"; echo >>"$receiver_log"; done) &
receiver_pid=$!

# At the end: stop the receiver, report, and delete the forwarder unless -k.
cleanup() {
  kill "$receiver_pid" 2>/dev/null || true
  exec 4<&-
  received_lines=$(wc -l <"$receiver_log")
  rm -f "$receiver_log"
  echo "received $received_lines sentences at the output (port $output_port)" >&2
  if $keep; then
    echo "kept forwarder $forwarder_id" >&2
  else
    delta_api DELETE "/api/forwarders/$forwarder_id" >/dev/null && echo "deleted forwarder $forwarder_id" >&2
  fi
}
trap cleanup EXIT
trap 'exit 0' INT TERM

# 3. Send the test sentences into the input (prints each sent sentence).
sleep 0.3  # let the output register the receiver before the first sentence
"$script_dir/send-nmea.sh" -m "$input_protocol" -P "$input_port" -i "$interval" -c "$count" -s "$sentences" -q || true
sleep 0.5  # give the last sentences time to arrive
