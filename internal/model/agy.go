package model

import "time"

// AgyConversation is one row of agy's conversation_summaries table.
type AgyConversation struct {
	ID           string
	Title        string
	Preview      string
	Workspaces   []string // absolute paths
	LastModified time.Time
}
