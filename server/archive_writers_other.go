//go:build !linux

package server

// Non-Linux platforms use the before/after source inventory and content
// verification. Directory archives are never committed after a detected change.
func archiveListWriters(roots []string) ([]archiveWriter, error) { return nil, nil }
