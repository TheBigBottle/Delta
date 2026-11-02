#!/usr/bin/env bash
# Creates and starts a test forwarder in Delta on ports that are not in use.
#
#   input:  UDP listen (or TCP listen with -m tcp) on a free port
#   output: TCP listen on another free port (connect there to receive the data)
#
# Usage: tests/create-forwarder.sh [options]
#   -u URL        Delta web interface (default $DELTA_URL or http://localhost:8470)
#   -p PASSWORD   Delta password (default $DELTA_PASSWORD, otherwise asked for)
#   -n NAME       forwarder name (default "NMEA test <time>")
#   -m udp|tcp    input mode (default udp)
#   -h            show this help
#
# Prints one line for other scripts: <forwarder id> <input mode> <input port> <output port>
set -euo pipefail
source "$(dirname "$0")/common.sh"

name="NMEA test $(date +%H:%M:%S)"
input_protocol=udp
while getopts "u:p:n:m:h" option; do
  case $option in
    u) DELTA_URL=$OPTARG ;;
    p) DELTA_PASSWORD=$OPTARG ;;
    n) name=$OPTARG ;;
    m) input_protocol=$OPTARG ;;
    h) sed -n '2,13p' "$0"; exit 0 ;;
    *) exit 1 ;;
  esac
done
[[ $input_protocol == udp || $input_protocol == tcp ]] || die "-m must be udp or tcp"
require_password

read -r input_port output_port < <(free_ports 2)

# The forwarder as JSON, the same format as in the config file and the editor.
forwarder_json=$(cat <<EOF
{
  "name": "$name",
  "tags": ["test"],
  "comment": "Created by tests/create-forwarder.sh",
  "framing": "lines",
  "input": { "mode": "$input_protocol-listen", "host": "", "port": $input_port },
  "outputs": [{ "mode": "tcp-listen", "host": "", "port": $output_port }]
}
EOF
)
# ?start=1 creates and starts it in one step.
response=$(delta_api POST '/api/forwarders?start=1' "$forwarder_json")
forwarder_id=$(json_value id <<<"$response")
state=$(json_value state <<<"$response")
if [[ $state != running ]]; then
  die "forwarder $forwarder_id was created but did not start: $(json_value error <<<"$response")"
fi

echo "Created and started \"$name\" ($forwarder_id):" >&2
echo "  input:  $input_protocol-listen on port $input_port  (send NMEA here)" >&2
echo "  output: tcp-listen on port $output_port  (connect here to receive it)" >&2
echo "$forwarder_id $input_protocol $input_port $output_port"
