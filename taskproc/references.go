package taskproc

// Reference is evidence that a process is still using task paths, not proof of
// ownership or permission to stop it. Unknown/shared references block deletion.
type Reference struct {
	PID        int      `json:"pid"`
	StartTicks uint64   `json:"start_ticks"`
	Name       string   `json:"name"`
	Paths      []string `json:"paths"`
}
