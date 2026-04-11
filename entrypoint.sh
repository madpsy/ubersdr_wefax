#!/bin/sh
# entrypoint.sh — translate environment variables into ubersdr_wefax flags
#
# Environment variables:
#   UBERSDR_URL       UberSDR WebSocket URL (default: ws://ubersdr:8080/ws)
#   UBERSDR_CHANNELS  Comma-separated freq:mode pairs, e.g. 7880000:usb,13882500:usb
#   UBERSDR_PASS      UberSDR bypass password (optional)
#   OUTPUT_DIR        Output directory for images and metadata (default: /data)
#   WEB_PORT          Port for the web gallery server (default: 6094)
#   LPM               Lines per minute: 120 (default) or 60
#   IMAGE_WIDTH       Image width in pixels: 1809 (IOC-576, default) or 904 (IOC-288)
#   NO_PHASING        Set to 1 to disable horizontal phasing sync
#   NO_AUTOSTOP       Set to 1 to disable auto-stop on STOP tone
#   NO_AUTOSTART      Set to 1 to disable auto-start on START tone

set -e

args=""

[ -n "$UBERSDR_URL"  ] && args="$args -url $UBERSDR_URL"
[ -n "$UBERSDR_PASS" ] && args="$args -password $UBERSDR_PASS"
[ -n "$OUTPUT_DIR"   ] && args="$args -output $OUTPUT_DIR"
[ -n "$LPM"          ] && args="$args -lpm $LPM"
[ -n "$IMAGE_WIDTH"  ] && args="$args -width $IMAGE_WIDTH"
[ "$NO_PHASING"   = "1" ] && args="$args -no-phasing"
[ "$NO_AUTOSTOP"  = "1" ] && args="$args -no-autostop"
[ "$NO_AUTOSTART" = "1" ] && args="$args -no-autostart"

# WEB_PORT → -listen :<port>
if [ -n "$WEB_PORT" ]; then
    args="$args -listen :$WEB_PORT"
else
    args="$args -listen :6094"
fi

# UBERSDR_CHANNELS is a comma-separated list; expand each entry as a -channel flag
if [ -n "$UBERSDR_CHANNELS" ]; then
    old_ifs="$IFS"
    IFS=","
    for ch in $UBERSDR_CHANNELS; do
        ch="$(echo "$ch" | tr -d ' ')"
        [ -n "$ch" ] && args="$args -channel $ch"
    done
    IFS="$old_ifs"
fi

# Append any CLI args passed directly to the container
# shellcheck disable=SC2086
exec /usr/local/bin/ubersdr_wefax $args "$@"
