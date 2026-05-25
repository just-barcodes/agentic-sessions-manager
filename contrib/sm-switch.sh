#!/usr/bin/env sh
# sm-switch — pick a waiting agent session in walker and jump to its window.
#
# Lists every session in the `waiting` state, lets you fuzzy-pick one in walker,
# and raises the terminal window (and tmux pane) hosting that session's agent.
#
# Bind it in hyprland.conf, e.g.:
#   bind = $mod, S, exec, ~/.local/bin/sm-switch.sh
#
# Requires: sm, jq, walker.
set -eu

sel=$(sm status --json \
	| jq -r '.[] | select(.Status == "waiting")
	         | "\(.ID)\t\(.Agent)  \(.CWD)"' \
	| walker -d -p "waiting session…")

[ -n "$sel" ] || exit 0
exec sm focus "${sel%%	*}" # first tab-delimited field is the full session id
