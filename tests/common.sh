#!/usr/bin/env bash
# Shared helpers for the test scripts in this folder. Not run directly:
# the other scripts load it with "source".
#
# Settings (environment variables, or the -u / -p options of the scripts):
#   DELTA_URL        address of the Delta web interface (default http://localhost:8470)
#   DELTA_PASSWORD   the Delta password; asked for if not set

DELTA_URL="${DELTA_URL:-http://localhost:8470}"
DELTA_PASSWORD="${DELTA_PASSWORD:-}"

# Prints an error message and ends the script.
die() {
  echo "error: $*" >&2
  exit 1
}

# Asks for the password if it was not given (input is not shown on screen).
require_password() {
  if [[ -z "$DELTA_PASSWORD" ]]; then
    read -r -s -p "Delta password for $DELTA_URL: " DELTA_PASSWORD
    echo >&2
  fi
}

# Calls the Delta REST API: delta_api METHOD PATH [JSON BODY].
# Logs in with HTTP basic auth (the password, empty user name) and prints the
# JSON answer. Ends the script with Delta's error message if the call fails.
delta_api() {
  local method=$1 path=$2 body=${3:-}
  local response http_status
  local curl_arguments=(--silent --show-error --max-time 15 --user ":$DELTA_PASSWORD" --request "$method")
  if [[ -n $body ]]; then
    # The body goes through stdin ("@-"), so it can be larger than a command line allows.
    curl_arguments+=(--header "Content-Type: application/json" --data-binary @-)
  fi
  response=$(curl "${curl_arguments[@]}" --write-out $'\n%{http_code}' "$DELTA_URL$path" <<<"$body") \
    || die "cannot reach Delta at $DELTA_URL"
  http_status=${response##*$'\n'}
  response=${response%$'\n'*}
  if [[ $http_status -ge 400 ]]; then
    die "$method $path failed ($http_status): $(json_value error <<<"$response")"
  fi
  printf '%s\n' "$response"
}

# Reads the first string or number value of a key from JSON on stdin,
# e.g. json_value id <<<'{"id":"f12","port":5}' prints f12. Good enough for
# Delta's flat answers; avoids a dependency on jq.
json_value() {
  grep -o "\"$1\": *\(\"[^\"]*\"\|[0-9.]*\)" | head -n 1 | sed 's/^"[^"]*": *//; s/^"//; s/"$//'
}

# Ports that must not be used: every port something on this machine listens
# on or has bound (TCP and UDP), and every port configured in a Delta
# forwarder, even a stopped one (it would clash when that one starts).
ports_in_use() {
  ss -H -l -n -t -u 2>/dev/null | awk '{ n = split($5, parts, ":"); print parts[n] }'
  local forwarders
  forwarders=$(delta_api GET /api/forwarders) || exit 1
  # "|| true": grep reports "nothing found" as an error, which is fine here.
  grep -o '"port": *[0-9]*' <<<"$forwarders" | grep -o '[0-9]*$' || true
}

# Prints COUNT free port numbers between 20000 and 29999, picked at random.
# Usage: free_ports COUNT
free_ports() {
  local wanted=$1 used candidate found=()
  used=$(ports_in_use) || exit 1
  used=" $(sort -u <<<"$used" | tr '\n' ' ') "
  for _ in $(seq 1 200); do
    candidate=$((20000 + RANDOM % 10000))
    # Skip ports that are in use or already picked.
    [[ $used == *" $candidate "* || " ${found[*]} " == *" $candidate "* ]] && continue
    found+=("$candidate")
    [[ ${#found[@]} -eq $wanted ]] && break
  done
  [[ ${#found[@]} -eq $wanted ]] || die "no free ports found between 20000 and 29999"
  echo "${found[*]}"
}

# Lookup tables for the checksum, built once when this file is loaded:
#   NMEA_CHARACTER_CODE["A"]  = 65                 (ASCII code of one character)
#   NMEA_PAIR_XOR["AB"]       = 65 XOR 66 = 3      (XOR of two characters)
# Bash can only walk through a string one step at a time, and each step costs
# a few microseconds. With the pair table the checksum needs half the steps,
# which matters when sentences are sent every millisecond. The tables cover the
# characters GPS sentences use (digits, capital letters, . , + -); any other
# character still works, just more slowly.
declare -A NMEA_CHARACTER_CODE=() NMEA_PAIR_XOR=()
nmea_characters=()
for nmea_code in {32..126}; do
  printf -v nmea_hex '%02x' "$nmea_code"
  printf -v nmea_character "\\x$nmea_hex"
  if [[ $nmea_character == [0-9A-Z.,+-] ]]; then
    NMEA_CHARACTER_CODE["$nmea_character"]=$nmea_code
    nmea_characters+=("$nmea_character")
  fi
done
for nmea_first in "${nmea_characters[@]}"; do
  for nmea_second in "${nmea_characters[@]}"; do
    NMEA_PAIR_XOR["$nmea_first$nmea_second"]=$((NMEA_CHARACTER_CODE["$nmea_first"] ^ NMEA_CHARACTER_CODE["$nmea_second"]))
  done
done
unset nmea_code nmea_hex nmea_character nmea_characters nmea_first nmea_second

# The NMEA 0183 checksum: XOR of all characters between "$" and "*", as two hex digits.
# Usage: nmea_checksum BODY [VARIABLE] (prints the checksum, or stores it in VARIABLE).
# The local variables start with "_nmea_", so they never hide the caller's
# VARIABLE (printf -v would otherwise write into the local one).
nmea_checksum() {
  local _nmea_text=$1 _nmea_sum=0 _nmea_index _nmea_value _nmea_second _nmea_length=${#1}
  # Two characters per step, using the pair table.
  for ((_nmea_index = 0; _nmea_index + 1 < _nmea_length; _nmea_index += 2)); do
    _nmea_value=${NMEA_PAIR_XOR["${_nmea_text:_nmea_index:2}"]:-}
    if [[ -z $_nmea_value ]]; then  # a character outside the table: one at a time
      _nmea_character_code _nmea_value "${_nmea_text:_nmea_index:1}"
      _nmea_character_code _nmea_second "${_nmea_text:_nmea_index+1:1}"
      _nmea_value=$((_nmea_value ^ _nmea_second))
    fi
    # Not ((…)): that "fails" when the result is 0, which would end a "set -e" script.
    _nmea_sum=$((_nmea_sum ^ _nmea_value))
  done
  if ((_nmea_index < _nmea_length)); then  # odd length: the last character alone
    _nmea_character_code _nmea_value "${_nmea_text:_nmea_index:1}"
    _nmea_sum=$((_nmea_sum ^ _nmea_value))
  fi
  if [[ -n ${2:-} ]]; then
    printf -v "$2" '%02X' "$_nmea_sum"
  else
    printf '%02X' "$_nmea_sum"
  fi
}

# The ASCII code of one character, into VARIABLE: _nmea_character_code VARIABLE CHARACTER
_nmea_character_code() {
  local _nmea_known=${NMEA_CHARACTER_CODE["$2"]:-}
  if [[ -n $_nmea_known ]]; then
    printf -v "$1" '%d' "$_nmea_known"
  else
    printf -v "$1" '%d' "'$2"
  fi
}

# Builds one complete NMEA sentence ("$" + body + "*" + checksum + CR LF)
# into VARIABLE, without starting a subshell. Usage: nmea_build VARIABLE BODY
nmea_build() {
  local _nmea_checksum
  nmea_checksum "$2" _nmea_checksum
  printf -v "$1" '$%s*%s\r\n' "$2" "$_nmea_checksum"
}

# Prints one complete NMEA sentence.
nmea_sentence() {
  local _nmea_sentence
  nmea_build _nmea_sentence "$1"
  printf '%s' "$_nmea_sentence"
}
