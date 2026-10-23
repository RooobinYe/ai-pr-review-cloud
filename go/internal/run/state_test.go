package run

import "testing"

func TestCanTransition(t *testing.T) {
	tests := []struct {
		from, to State
		want     bool
	}{
		{StateQueued, StateRunning, true},
		{StateRunning, StateQueued, true}, // pod failure retried
		{StateRunning, StateReviewed, true},
		{StateReviewed, StateMerging, true},
		{StateAwaitingApproval, StateMerging, true},
		{StateQueued, StateMerging, false},
		{StateReviewed, StateSuperseded, false}, // result already posted for this SHA
		{StateDone, StateQueued, false},
		{StateFailed, StateRunning, false},
	}
	for _, tt := range tests {
		if got := CanTransition(tt.from, tt.to); got != tt.want {
			t.Errorf("CanTransition(%s, %s) = %v, want %v", tt.from, tt.to, got, tt.want)
		}
	}
}

func TestTerminalStatesHaveNoTransitions(t *testing.T) {
	for from := range transitions {
		if from.Terminal() {
			t.Errorf("terminal state %s has outgoing transitions", from)
		}
	}
	for _, s := range []State{StateDone, StateFailed, StateSuperseded} {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
}
