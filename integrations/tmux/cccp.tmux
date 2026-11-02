# cccp — tmux integration.
#
# Source this from ~/.tmux.conf (`cccp install` adds the line):
#   source-file /path/to/cccp/integrations/tmux/cccp.tmux
#
# Everything here is event driven: tmux runs `cccp notify` only when focus
# or the pane layout changes, and the status line reads options the daemon
# sets, so tmux never runs a command on its periodic redraw.

# Terminal focus lets cccp tell "the pane is on screen and you are looking at
# that terminal" apart from "the pane is on screen in a terminal you left".
# (Claude Code's own tmux guidance recommends this setting too.)
set -g focus-events on

# --- hooks: tell the daemon what is on screen ------------------------------
# Index [97] keeps cccp's entries separate from any hooks you set yourself
# (those normally use index 0) and makes re-sourcing idempotent.
set-hook -g client-session-changed[97] 'run-shell -b "$HOME/.local/bin/cccp notify tmux client-session-changed"'
set-hook -g session-window-changed[97] 'run-shell -b "$HOME/.local/bin/cccp notify tmux session-window-changed"'
set-hook -g window-pane-changed[97]    'run-shell -b "$HOME/.local/bin/cccp notify tmux window-pane-changed"'
set-hook -g client-focus-in[97]        'run-shell -b "$HOME/.local/bin/cccp notify tmux client-focus-in"'
set-hook -g client-focus-out[97]       'run-shell -b "$HOME/.local/bin/cccp notify tmux client-focus-out"'
set-hook -g client-attached[97]        'run-shell -b "$HOME/.local/bin/cccp notify tmux client-attached"'
set-hook -g client-detached[97]        'run-shell -b "$HOME/.local/bin/cccp notify tmux client-detached"'
set-hook -g after-new-window[97]       'run-shell -b "$HOME/.local/bin/cccp notify tmux after-new-window"'
set-hook -g after-split-window[97]     'run-shell -b "$HOME/.local/bin/cccp notify tmux after-split-window"'
set-hook -g pane-exited[97]            'run-shell -b "$HOME/.local/bin/cccp notify tmux pane-exited"'
set-hook -g session-created[97]        'run-shell -b "$HOME/.local/bin/cccp notify tmux session-created"'
set-hook -g session-closed[97]         'run-shell -b "$HOME/.local/bin/cccp notify tmux session-closed"'

# --- keys -------------------------------------------------------------------
# prefix a: jump to the most urgent session that needs you; press again to
#           cycle through the others.
# prefix A: the cockpit.
bind-key a run-shell -b "$HOME/.local/bin/cccp jump --next --client '#{client_name}'"
bind-key A display-popup -E -w 92% -h 88% -T " cccp " "$HOME/.local/bin/cccp ui --client '#{client_name}'"

# --- status line --------------------------------------------------------------
# @cccp_status is set by the daemon: counts of sessions that need you (⚠),
# finished turns you have not seen (⏎), working (●), idle (○), cold cache (❄).
# This prepends it to tmux's default status-right; if you customise
# status-right, put #{@cccp_status} where you want it instead.
set -g status-right-length 90
set -g status-right '#{?@cccp_status,#{@cccp_status} │ ,}#{?window_bigger,[#{window_offset_x}#,#{window_offset_y}] ,}"#{=21:pane_title}" %H:%M %d-%b-%y'

# @cccp_glyph is set per window: the state of the most urgent session in it.
set -g window-status-format         '#{@cccp_glyph}#I:#W#{?window_flags,#{window_flags}, }'
set -g window-status-current-format '#{@cccp_glyph}#I:#W#{?window_flags,#{window_flags}, }'
