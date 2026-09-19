//go:build !linux

package filesystem

func RunExecutionWorker() (bool, error) { return false, nil }
