#!/usr/bin/env bash
# Timing test: how short can the interval between sends be?
#
# Creates a forwarder on free ports, connects to its output like a navigation
# app, and runs send-nmea.sh once per interval of a list, each for a few
# seconds. For every interval it shows what the sender achieved, how many
# sentences Delta received, and how many arrived at the output. The forwarder
# is deleted at the end.
#
# Usage: tests/timing-test.sh [options]
#   -u URL         Delta web interface (default $DELTA_URL or http://localhost:8470)
#   -p PASSWORD    Delta password (default $DELTA_PASSWORD, otherwise asked for)
#   -m udp|tcp     input mode and sender protocol (default udp)
#   -d SECONDS     how long each interval is tested (default 3)
#   -l "LIST"      intervals in seconds (default "1 0.1 0.01 0.005 0.002 0.001 0.0005 0.0002 0.0001 0")
#   -s rmc|gga|both  sentences per round (default both)
#   -h             show this help
#
# Columns: requested and achieved interval, jitter (standard deviation of the
# gaps), the longest gap, rounds per second, late rounds, sentences sent, how
# many Delta's input counted, how many arrived at the output, and how many
# were lost on the way.
set -euo pipefail
source "$(dirname "$0")/common.sh"
script_dir=$(dirname "$0")

protocol=udp
step_seconds=3
intervals="1 0.1 0.01 0.005 0.002 0.001 0.0005 0.0002 0.0001 0"
sentences=both
while getopts "u:p:m:d:l:s:h" option; do
  case $option in
    u) DELTA_URL=$OPTARG ;;
    p) DELTA_PASSWORD=$OPTARG ;;
    m) protocol=$OPTARG ;;
    d) step_seconds=$OPTARG ;;
    l) intervals=$OPTARG ;;
    s) sentences=$OPTARG ;;
    h) sed -n '2,23p' "$0"; exit 0 ;;
    *) exit 1 ;;
  esac
done
require_password
export DELTA_URL DELTA_PASSWORD

# The number of messages Delta's input has counted so far (the top-level "chunks").
input_message_count() {
  delta_api GET "/api/forwarders/$forwarder_id" | json_value chunks
}

created=$("$script_dir/create-forwarder.sh" -m "$protocol" -n "Timing test $(date +%H:%M:%S)") || exit 1
read -r forwarder_id input_protocol input_port output_port <<<"$created"

# Receive the output like an app would; cat writes everything into a file and
# the line count is read after each step.
received_file=$(mktemp)
exec 4<"/dev/tcp/127.0.0.1/$output_port" || die "cannot connect to the output port $output_port"
cat <&4 >"$received_file" &
receiver_pid=$!
cleanup() {
  kill "$receiver_pid" 2>/dev/null || true
  exec 4<&-
  rm -f "$received_file"
  delta_api DELETE "/api/forwarders/$forwarder_id" >/dev/null && echo "deleted forwarder $forwarder_id" >&2
}
trap cleanup EXIT
trap 'exit 130' INT TERM
sleep 0.3

echo "Each interval runs ${step_seconds}s over $input_protocol into Delta, $sentences sentences per round." >&2
printf '%10s %12s %10s %10s %10s %6s %9s %9s %9s %6s\n' \
  requested achieved jitter "max gap" rounds/s late sent "at Delta" "at output" lost
for interval in $intervals; do
  delta_before=$(input_message_count)
  output_before=$(wc -l <"$received_file")
  summary=$("$script_dir/send-nmea.sh" -q -m "$input_protocol" -P "$input_port" -i "$interval" -t "$step_seconds" -s "$sentences" -S 1 2>&1 >/dev/null | tail -n 1)
  sleep 0.5  # let the last sentences arrive
  delta_received=$(($(input_message_count) - delta_before))
  output_received=$(($(wc -l <"$received_file") - output_before))
  # e.g. "sent 500 rounds (1000 sentences) in 0.999 s: interval 2002 µs, 499 rounds/s, jitter 114 µs, max gap 3243 µs, 1 late"
  if [[ $summary =~ \(([0-9]+)\ sentences\).*interval\ ([0-9]+).*\ ([0-9]+)\ rounds/s.*jitter\ ([0-9]+).*max\ gap\ ([0-9]+).*\ ([0-9]+)\ late ]]; then
    sent=${BASH_REMATCH[1]}
    printf '%10s %9s µs %7s µs %7s µs %10s %6s %9s %9s %9s %6s\n' \
      "${interval}s" "${BASH_REMATCH[2]}" "${BASH_REMATCH[4]}" "${BASH_REMATCH[5]}" "${BASH_REMATCH[3]}" "${BASH_REMATCH[6]}" \
      "$sent" "$delta_received" "$output_received" $((sent - output_received))
  else
    echo "${interval}s: unexpected sender output: $summary" >&2
  fi
done
