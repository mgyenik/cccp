// Package cccp embeds the integration bundles so `cccp install` does not
// depend on where the repository is checked out.
package cccp

import "embed"

// Assets holds the Claude Code marketplace manifest, the plugin bundles, the
// tmux snippet and the systemd unit.
//
//go:embed all:.claude-plugin all:integrations
var Assets embed.FS
