// Package run defines the review run lifecycle (docs/architecture.md §3).
package run

// State is the lifecycle state of one review run, keyed by (repo, PR, head SHA).
type State string

const (
	StateQueued           State = "queued"
	StateRunning          State = "running"
	StatePreviewing       State = "previewing"
	StateReviewed         State = "reviewed"
	StateAwaitingApproval State = "awaiting_approval"
	StateMerging          State = "merging"
	StateDone             State = "done"
	StateFailed           State = "failed"
	StateSuperseded       State = "superseded"
)

// States lists every state, in lifecycle order.
var States = []State{
	StateQueued, StateRunning, StatePreviewing, StateReviewed, StateAwaitingApproval,
	StateMerging, StateDone, StateFailed, StateSuperseded,
}

// transitions lists every allowed edge of the state machine. The store mirrors
// the state values in a CHECK constraint; keep both in sync.
var transitions = map[State][]State{
	StateQueued:           {StateRunning, StateSuperseded},
	StateRunning:          {StatePreviewing, StateReviewed, StateQueued, StateFailed, StateSuperseded},
	StatePreviewing:       {StateRunning, StateFailed, StateSuperseded},
	StateReviewed:         {StateAwaitingApproval, StateMerging, StateDone},
	StateAwaitingApproval: {StateMerging, StateSuperseded},
	StateMerging:          {StateDone, StateFailed},
}

// CanTransition reports whether a run may move from one state to another.
func CanTransition(from, to State) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Terminal reports whether no further transitions are possible from s.
func (s State) Terminal() bool {
	return s == StateDone || s == StateFailed || s == StateSuperseded
}
