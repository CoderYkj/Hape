#!/bin/sh
# ds-match-drill.sh - one real match, with all three parties separate:
#
#   client   this repo's minimal client (Nakama or dev identity)
#   provider tools/fakeds -mode worker: pops the queue, allocates, launches the server
#   server   the reference dedicated server, with the credential injected
#
# What it proves that the other drills do not: a client that only ever saw a match id
# finds the server, connects over UDP, gets bytes back, and reads its own payout after
# the server's authoritative report lands.
#
#   IDENTITY=nakama QUEUE=ue5-drill sh scripts/ds-match-drill.sh
#   IDENTITY=dev    QUEUE=ue5-drill sh scripts/ds-match-drill.sh
#
# DS_LAUNCH=docker (default, what ships) or exec (same binary, no Docker needed).
set -eu

queue="${QUEUE:-ue5-drill}"
image="${DS_IMAGE:-hape-ds:ci}"
seconds="${MATCH_SECONDS:-15}"
identity="${IDENTITY:-nakama}"
launch="${DS_LAUNCH:-docker}"
log="${CLIENT_LOG:-/tmp/client-full.log}"

CLIENT_FULL=1
export CLIENT_FULL QUEUE="$queue"
echo "drill: identity=$identity queue=$queue duration=${seconds}s launch=$launch"

# The client starts first: it is the one that enqueues, and the provider's pop loop
# waits for a full match on this queue.
IDENTITY="$identity" go run ./clients/minimal > "$log" 2>&1 &
client=$!

provider_status=0
go run ./tools/fakeds -mode worker -queue "$queue" -launch-ds "$launch" \
  -ds-image "$image" -match-seconds "$seconds" || provider_status=$?

client_status=0
wait "$client" || client_status=$?

echo "=== client ==="
cat "$log"

if [ "$provider_status" -ne 0 ] || [ "$client_status" -ne 0 ]; then
  echo "drill failed: provider=$provider_status client=$client_status" >&2
  exit 1
fi

# The assertions are the client's own observations, not the server's log: a drill that
# only reads the server's side cannot tell a broken client from a chatty one.
grep -qx 'ECHOES=5' "$log"
grep -qx 'SESSION_STATE=finished' "$log"
grep -qx 'CLIENT_OK=1' "$log"
echo "drill: the client played a real match and was paid"