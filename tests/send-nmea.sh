#!/usr/bin/env bash
# Sends NMEA GPS sentences ($GPRMC and $GPGGA with the current UTC time and
# valid checksums) to a host and port, once per interval, until Ctrl+C or
# until the given number of rounds was sent.
#
# The data changes like a real GPS on a moving boat: the position follows
# course and speed over the real elapsed time, course and speed drift at
# random, and satellites, HDOP and altitude vary. -f sends fixed data instead.
#
# Usage: tests/send-nmea.sh -P PORT [options]
#   -P PORT          target port (required)
#   -H HOST          target host (default 127.0.0.1)
#   -m udp|tcp       protocol (default udp: one datagram per sentence; tcp keeps one connection open)
#   -i SECONDS       interval between rounds, decimals allowed, 0 = as fast as possible (default 1)
#   -c COUNT         stop after COUNT rounds (default 0 = no limit)
#   -t SECONDS       stop after SECONDS (default 0 = no limit)
#   -s rmc|gga|both  which sentences to send each round (default both)
#   -f               fixed data: same position, speed 0, no random changes
#   -S SEED          seed for the random changes, for repeatable runs
#   -q               quiet: do not print the sent sentences
#   -h               show this help
#
# At the end it prints what was achieved, e.g.
#   sent 1000 rounds (2000 sentences) in 0.999 s: interval 1000 µs, 1000 rounds/s, jitter 21 µs, max gap 310 µs, 0 late
# "interval" is the average time between two rounds, "jitter" the standard
# deviation of these gaps, "max gap" the longest one. A round is "late" when
# it started more than one interval after it was due.
set -euo pipefail
source "$(dirname "$0")/common.sh"

host=127.0.0.1
port=""
protocol=udp
interval=1
count=0
duration=0
sentences=both
fixed_data=false
quiet=false
while getopts "P:H:m:i:c:t:s:fS:qh" option; do
  case $option in
    P) port=$OPTARG ;;
    H) host=$OPTARG ;;
    m) protocol=$OPTARG ;;
    i) interval=$OPTARG ;;
    c) count=$OPTARG ;;
    s) sentences=$OPTARG ;;
    f) fixed_data=true ;;
    S) RANDOM=$OPTARG ;;
    q) quiet=true ;;
    t) duration=$OPTARG ;;
    h) sed -n '2,29p' "$0"; exit 0 ;;
    *) exit 1 ;;
  esac
done
interval=${interval/,/.}  # accept "0,5" as well as "0.5"
duration=${duration/,/.}
[[ $duration =~ ^[0-9]*\.?[0-9]+$ ]] || die "-t must be a number of seconds"
[[ $port =~ ^[0-9]+$ ]] || die "-P PORT is required (see -h)"
[[ $protocol == udp || $protocol == tcp ]] || die "-m must be udp or tcp"
[[ $interval =~ ^[0-9]*\.?[0-9]+$ ]] || die "-i must be a number of seconds, e.g. 1 or 0.005"
[[ $count =~ ^[0-9]+$ ]] || die "-c must be a whole number"
[[ $sentences =~ ^(rmc|gga|both)$ ]] || die "-s must be rmc, gga or both"

# ---- timing without starting processes ----
# Everything in the send loop uses bash builtins: starting a process (date,
# sleep, a subshell) costs about a millisecond, far more than a round itself.

export TZ=UTC0  # printf '%(…)T' then prints UTC, as NMEA requires

# The current time in microseconds since 1970, from bash's $EPOCHREALTIME,
# stored in the variable named by $1. (The local variable has an unusual name,
# so it never hides the caller's variable of the same name.)
now_us() {
  local epoch_realtime_text=$EPOCHREALTIME
  printf -v "$1" '%s' "${epoch_realtime_text//[.,]/}"  # "." or "," depending on the locale
}

# Seconds with decimals ("0.005") -> whole microseconds (5000).
seconds_to_us() {
  local whole=${1%%.*} fraction=000000
  [[ $1 == *.* ]] && fraction=${1#*.}000000
  [[ -z $whole ]] && whole=0
  echo $((10#$whole * 1000000 + 10#${fraction:0:6}))
}
interval_us=$(seconds_to_us "$interval")
duration_us=$(seconds_to_us "$duration")

# Pauses for the given microseconds. Uses bash's loadable "sleep" builtin if
# the system has it (package bash, /usr/lib/bash/sleep), otherwise read with a
# timeout on a pipe that never delivers data. Both avoid starting a process.
if enable -f sleep sleep 2>/dev/null || enable -f /usr/lib/bash/sleep sleep 2>/dev/null; then
  pause_us() { local seconds; printf -v seconds '%d.%06d' $(($1 / 1000000)) $(($1 % 1000000)); sleep "$seconds"; }
else
  exec {never_ready_fd}<> <(:)
  pause_us() { local seconds; printf -v seconds '%d.%06d' $(($1 / 1000000)) $(($1 % 1000000)); read -r -t "$seconds" -u "$never_ready_fd" _ || true; }
fi

# ---- the simulated boat ----
# Integer arithmetic only (bash has no floating point):
#   position in 1e-10 degrees, course in micro-degrees, speed in micro-knots.

latitude_e10=535500000000    # 53.55° N, Hamburg harbour
longitude_e10=99833333333    #  9.9833° E
course_udeg=$((RANDOM % 360 * 1000000))
speed_ukn=$((4000000 + RANDOM % 4000000))  # 4 to 8 knots
satellites=$((7 + RANDOM % 5))
hdop_tenths=$((8 + RANDOM % 5))
altitude_dm=$((95 + RANDOM % 10))
if $fixed_data; then
  course_udeg=0 speed_ukn=0 satellites=8 hdop_tenths=9 altitude_dm=100
fi

# sin and cos of every whole degree, times 1,000,000 (computed once, so the
# send loop only looks values up).
mapfile -t SINE < <(awk 'BEGIN { for (d = 0; d < 360; d++) printf "%d\n", sin(d * 3.14159265358979 / 180) * 1000000 }')
mapfile -t COSINE < <(awk 'BEGIN { for (d = 0; d < 360; d++) printf "%d\n", cos(d * 3.14159265358979 / 180) * 1000000 }')
cos_latitude=${COSINE[$((latitude_e10 / 10000000000))]}

# Random whole number between -limit and +limit.
random_between() { printf -v "$1" '%d' $((RANDOM % ($2 * 2 + 1) - $2)); }

# Moves the boat by the elapsed microseconds and lets course, speed and the
# GPS quality values drift. Changes are scaled by the elapsed time, so the
# track looks the same whether rounds are sent every second or every millisecond.
move_boat() {
  local elapsed_us=$1 change degree distance_e10
  ((elapsed_us > 10000000)) && elapsed_us=10000000  # after a long pause, move at most 10 s worth
  # Course changes by up to ±3 °/s, speed by up to ±0.2 kn/s, pulled back towards 6 kn.
  random_between change 3000
  course_udeg=$(((course_udeg + change * elapsed_us / 1000 + 360000000) % 360000000))
  random_between change 200
  speed_ukn=$((speed_ukn + change * elapsed_us / 1000 + (6000000 - speed_ukn) * elapsed_us / 30000000))
  ((speed_ukn < 0)) && speed_ukn=0
  ((speed_ukn > 20000000)) && speed_ukn=20000000
  # Distance in 1e-10 degrees of latitude: knots * 1852 m/h / 111,320 m per degree.
  distance_e10=$((speed_ukn / 100000 * elapsed_us * 46213 / 10000000))
  degree=$((course_udeg / 1000000))
  latitude_e10=$((latitude_e10 + distance_e10 * COSINE[degree] / 1000000))
  longitude_e10=$((longitude_e10 + distance_e10 * SINE[degree] / cos_latitude))
  # Now and then the receiver sees one satellite more or less, and HDOP and altitude wobble.
  if ((RANDOM % 50 == 0)); then
    random_between change 1
    satellites=$((satellites + change))
    ((satellites < 5)) && satellites=5
    ((satellites > 12)) && satellites=12
    hdop_tenths=$((7 + (12 - satellites) + RANDOM % 3))
  fi
  random_between change 2
  altitude_dm=$((altitude_dm + change))
  ((altitude_dm < 80)) && altitude_dm=80
  ((altitude_dm > 120)) && altitude_dm=120
  return 0
}

# Degrees in 1e-10 -> NMEA "DDMM.MMMM,N" (or DDDMM.MMMM for longitude).
format_coordinate() {
  local variable=$1 value=$2 degree_digits=$3 positive=$4 negative=$5 hemisphere minutes_e4
  hemisphere=$positive
  ((value < 0)) && { value=$((-value)); hemisphere=$negative; }
  # Round to the nearest 1/10000 minute (as an integer of 1/10000 minutes in total).
  minutes_e4=$((((value % 10000000000) * 6 + 50000) / 100000 + value / 10000000000 * 600000))
  printf -v "$variable" "%0${degree_digits}d%02d.%04d,%s" $((minutes_e4 / 600000)) $((minutes_e4 % 600000 / 10000)) $((minutes_e4 % 10000)) "$hemisphere"
}

# Builds the sentences of one round into round_sentences (an array).
build_round() {
  local time_us=$1 seconds=$(($1 / 1000000)) time date latitude longitude sentence satellite_count
  printf -v time '%(%H%M%S)T.%02d' "$seconds" $((time_us % 1000000 / 10000))
  printf -v date '%(%d%m%y)T' "$seconds"
  format_coordinate latitude "$latitude_e10" 2 N S
  format_coordinate longitude "$longitude_e10" 3 E W
  round_sentences=()
  if [[ $sentences != gga ]]; then
    # RMC: time, status A (valid), position, speed (kn), course (°), date, mode A (autonomous)
    nmea_build sentence "GPRMC,$time,A,$latitude,$longitude,$((speed_ukn / 1000000)).$((speed_ukn / 100000 % 10)),$((course_udeg / 1000000)).$((course_udeg / 100000 % 10)),$date,,,A"
    round_sentences+=("$sentence")
  fi
  if [[ $sentences != rmc ]]; then
    # GGA: time, position, fix 1 (GPS), satellites, HDOP, altitude (m), geoid separation 46.0 m
    printf -v satellite_count '%02d' "$satellites"
    nmea_build sentence "GPGGA,$time,$latitude,$longitude,1,$satellite_count,$((hdop_tenths / 10)).$((hdop_tenths % 10)),$((altitude_dm / 10)).$((altitude_dm % 10)),M,46.0,M,,"
    round_sentences+=("$sentence")
  fi
}

# ---- sending ----

# Bash opens network connections through /dev/udp/HOST/PORT and /dev/tcp/HOST/PORT.
# The connection stays open for the whole run.
if [[ $protocol == tcp ]]; then
  exec 3<>"/dev/tcp/$host/$port" || die "cannot connect to $host:$port"
else
  exec 3>"/dev/udp/$host/$port" || die "cannot open UDP socket to $host:$port"
fi

round=0
sentence_count=0
late_rounds=0
send_errors=0
now_us start_us
next_due_us=$start_us
last_move_us=$start_us
first_round_us=$start_us
last_round_us=$start_us
gap_sum_squares=0  # for the jitter (standard deviation of the gaps between rounds)
max_gap_us=0

# Prints what was achieved (also after Ctrl+C). The interval is measured
# from the first to the last round: N rounds have N - 1 gaps between them.
print_summary() {
  local span_us=$((last_round_us - first_round_us)) mean_interval_us=0 rate=0 variance=0 jitter_us=0
  if ((round > 1 && span_us > 0)); then
    mean_interval_us=$((span_us / (round - 1)))
    rate=$(((round - 1) * 1000000 / span_us))
    # Standard deviation = square root of (mean of the squares - square of the mean),
    # with an integer square root (Newton's method), since bash has no floating point.
    variance=$((gap_sum_squares / (round - 1) - mean_interval_us * mean_interval_us))
    if ((variance > 0)); then
      jitter_us=$variance
      local next=$(((jitter_us + variance / jitter_us) / 2))
      while ((next < jitter_us)); do jitter_us=$next; next=$(((jitter_us + variance / jitter_us) / 2)); done
    fi
  fi
  printf 'sent %d rounds (%d sentences) in %d.%03d s: interval %d µs, %d rounds/s, jitter %d µs, max gap %d µs, %d late%s\n' \
    "$round" "$sentence_count" $((span_us / 1000000)) $((span_us / 1000 % 1000)) \
    "$mean_interval_us" "$rate" "$jitter_us" "$max_gap_us" "$late_rounds" \
    "$( ((send_errors)) && echo ", $send_errors send errors (nobody listening?)")" >&2
}
trap 'print_summary; exit 0' INT TERM

echo "sending to $protocol://$host:$port every ${interval}s$( ((count)) && echo ", $count rounds")$($fixed_data && echo ", fixed data") (Ctrl+C stops)" >&2
while ((count == 0 || round < count)); do
  now_us now
  if ((round == 0)); then
    first_round_us=$now
  else
    gap=$((now - last_round_us))
    gap_sum_squares=$((gap_sum_squares + gap * gap))
    ((gap > max_gap_us)) && max_gap_us=$gap
  fi
  last_round_us=$now
  $fixed_data || move_boat $((now - last_move_us))
  last_move_us=$now
  build_round "$now"
  if [[ $protocol == tcp ]]; then
    # One write per round; Delta's "lines" framing splits it into sentences.
    printf '%s' "${round_sentences[@]}" >&3 || die "connection to $host:$port lost"
  else
    for sentence in "${round_sentences[@]}"; do
      # A connected UDP socket reports "connection refused" when nobody listens; keep going.
      printf '%s' "$sentence" >&3 2>/dev/null || send_errors=$((send_errors + 1))
    done
  fi
  round=$((round + 1))
  sentence_count=$((sentence_count + ${#round_sentences[@]}))
  if ! $quiet; then
    for sentence in "${round_sentences[@]}"; do printf '%s\n' "${sentence%$'\r\n'}" >&2; done
  fi
  ((count > 0 && round >= count)) && break
  ((duration_us > 0 && now - start_us >= duration_us)) && break

  # Wait until the next round is due. The schedule is start + n * interval, so
  # the time spent building and sending does not add up. A round that is more
  # than one interval behind counts as late, and the schedule restarts from now
  # instead of sending a burst to catch up.
  if ((interval_us > 0)); then
    next_due_us=$((next_due_us + interval_us))
    now_us now
    if ((next_due_us > now)); then
      pause_us $((next_due_us - now))
    elif ((now - next_due_us > interval_us)); then
      late_rounds=$((late_rounds + 1))
      next_due_us=$now
    fi
  fi
done
print_summary
