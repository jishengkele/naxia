//go:build !linux

package taskproc

// Non-Linux retains the existing platform behavior; /proc ownership/reference
// inspection and automatic service shutdown are Linux capabilities.
func References([]string) ([]Reference, error) { return nil, nil }
