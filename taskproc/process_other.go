//go:build !linux

package taskproc

import "context"

func syncDirectory(string) error                      { return nil }
func (r *Registry) List(int64) ([]Process, error)     { return nil, nil }
func (r *Registry) Stop(context.Context, int64) error { return nil }
