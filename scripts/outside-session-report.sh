#!/bin/sh
# Optional Claude hook. Stdin is display data; it never reaches an action API.
# RELAY_OUTSIDE_SPOOL, when set, is the spool of the relay instance this
# session reports to; it must be the same absolute path that instance has.
umask 077
dir="${RELAY_OUTSIDE_SPOOL:-$HOME/.claude/relay-outside-reports}"
case "$dir" in /*) ;; *) exit 1 ;; esac
# Relay drains only a clean path (no trailing slash, //, . or .. element), so
# a report written under any other spelling would never be read.
case "$dir" in
  /) ;;
  */ | *//* | */./* | */. | */../* | */..)
    echo "outside-session-report: RELAY_OUTSIDE_SPOOL is not a clean absolute path: $dir" >&2
    exit 1 ;;
esac
parent=$(dirname "$dir")
[ ! -L "$parent" ] && [ ! -L "$dir" ] || exit 1
mkdir -p "$dir" || exit 1
chmod 700 "$dir" || exit 1
file=$(mktemp "$dir/.report.XXXXXXXX") || exit 1
trap 'rm -f "$file"' EXIT HUP INT TERM
# A bad or oversize event is discarded. Relay validates the native hook shape.
head -c 65537 > "$file"
[ "$(wc -c < "$file")" -le 65536 ] || exit 0
# Bound the spool: keep the 255 newest reports so 256 remain after this one.
ls -t "$dir"/report.*.json 2>/dev/null | tail -n +256 | while IFS= read -r old; do
  [ -f "$old" ] && [ ! -L "$old" ] && rm -f "$old"
done
mv "$file" "$dir/report.${file##*.}.json"
