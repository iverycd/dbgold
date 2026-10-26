package damengnative

import "fmt"

// EventStateRecovery is the observed result of undoing one attempted final
// activation. Only the policy state is reset; its CREATE, data and compilation
// steps remain complete. SQL identifies the original frozen activation unit.
type EventStateRecovery struct {
	Schema string
	Name   string
	SQL    string
	Status string // pending: paused/missing; unknown: unproved; succeeded: authenticated desired DISABLED
	Error  string
}

// EventFinalizationFailure carries compensation results across the writer,
// migrator and recovery coordinator without passing executable cleanup SQL to
// the generic repair editor.
type EventFinalizationFailure struct {
	Cause  error
	States []EventStateRecovery
}

func (e *EventFinalizationFailure) Error() string {
	return fmt.Sprintf("事件触发器最终状态未全部恢复，已尝试暂停本轮事件: %v", e.Cause)
}

func (e *EventFinalizationFailure) Unwrap() error { return e.Cause }
