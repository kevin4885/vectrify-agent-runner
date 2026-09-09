package client

// commandClass partitions command types into two reservation classes so that
// a burst of expensive commands can never starve cheap ones of every
// dispatch slot. See classifyCommand for the mapping.
type commandClass int

const (
	// classLight commands may use the full global concurrency limit.
	classLight commandClass = iota
	// classHeavy commands are additionally capped by the heavy sub-limit
	// (config.Config.MaxHeavyConcurrency), which is always <= the global
	// limit, guaranteeing at least (global - heavy) slots stay reachable by
	// light commands even when every heavy slot is occupied.
	classHeavy
)

func (c commandClass) String() string {
	if c == classHeavy {
		return "heavy"
	}
	return "light"
}

// classifyCommand maps a command's "type" string to its reservation class.
//
// heavy: commands that can legitimately run for a long time or block on
// external I/O — "shell" (arbitrary user command, up to maxShellTimeout) and
// "file_transfer" (S3 upload/download of up to 100 MiB). These are exactly
// the command types that produced the original incident: enough of them
// wedged simultaneously starved the fast, interactive commands of every
// slot.
//
// light: everything else — "file_op", "git", "update_key", and any unknown
// or future command type. Unknown types default to light deliberately, NOT
// heavy: an unrecognized type is almost certainly a fast/no-op path that
// runner.Dispatch's `default` case rejects immediately with an "unknown
// command type" error (see runner/runner.go) — it never does real work and
// never holds a slot for long. Defaulting it to heavy instead would let a
// typo'd or newly-added-but-not-yet-classified command type quietly eat into
// the capacity this whole mechanism exists to reserve for light commands.
// When a new heavy command type is introduced, it MUST be added to the
// switch below explicitly — that is the one and only place this decision is
// made.
func classifyCommand(cmdType string) commandClass {
	switch cmdType {
	case "shell", "file_transfer":
		return classHeavy
	default:
		return classLight
	}
}
