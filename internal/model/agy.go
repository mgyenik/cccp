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

// AgyExecution is how an agy conversation's latest execution (the agent's
// run on one prompt) ended, from agy's own database (agydb.Executions).
type AgyExecution struct {
	Known    bool
	Reason   int // ExecutorTerminationReason
	LastStep int // the execution's last step index (transcript step_index)
}

// agy's ExecutorTerminationReason values cccp acts on.
const (
	AgyUserCanceled = 2
	AgyNoToolCall   = 4
)
