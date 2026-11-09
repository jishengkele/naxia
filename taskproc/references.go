package taskproc

// Reference is evidence that a process is still using task paths, not proof of
// ownership or permission to stop it. Unknown/shared references block deletion.
type Reference struct {
	PID        int      `json:"pid"`
	StartTicks uint64   `json:"start_ticks"`
	Name       string   `json:"name"`
	Paths      []string `json:"paths"`
}

// DeleteBlockers identifies live references that cannot be stopped as part of
// this task, including a known process with references to another workspace.
type DeleteBlockers []Reference

func (DeleteBlockers) Error() string { return "任务仍有未知或共享后台进程" }
