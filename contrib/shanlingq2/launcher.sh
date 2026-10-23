#!/bin/sh
# Installed as /mnt/mmc/.deezer/run. Q2 Pod starts it from Streaming's Deezer row (a card app,
# q2-pod's patch/addon.c). It runs from /tmp, never the card: the app and this launcher are
# copied there first and the card's copy of this script is left at once (exec), so nothing keeps
# the card busy and USB storage can always take it. The loop restarts the app after a failure,
# waiting 5 s and doubling to a minute; an exit of 0 (stopped from the Q2, or idle) ends
# it. Q2 Pod stops it with a TERM to the loop's pid in /tmp/q2-deezer, the queue saved.
pid=/tmp/q2-deezer
if [ "$1" != --loop ]; then
    [ -f $pid ] && kill -0 "$(cat $pid)" 2>/dev/null && exit 0
    dir=$(cd "$(dirname "$0")" && pwd)
    cp -f "$dir/q2-deezer" /tmp/q2-deezer.bin && chmod 755 /tmp/q2-deezer.bin || exit 1
    cp -f "$0" /tmp/q2-deezer.run || exit 1
    cd /tmp
    exec /bin/sh /tmp/q2-deezer.run --loop "$dir"
fi
dir=$2
(
    child=
    trap '[ -n "$child" ] && kill -TERM $child 2>/dev/null && wait $child; rm -f $pid /tmp/q2-deezer.bin /tmp/q2-deezer.run; exit 0' TERM INT
    delay=5
    while :; do
        nice -n 10 /tmp/q2-deezer.bin -dir "$dir" & # downloads are background work: playback first
        child=$!
        wait $child
        code=$?
        [ $code = 0 ] && break
        sleep $delay
        [ $delay -lt 60 ] && delay=$((delay * 2))
    done
    rm -f $pid /tmp/q2-deezer.bin /tmp/q2-deezer.run
) </dev/null >/dev/null 2>&1 &
echo $! > $pid
