//go:build !windows && !linux

package claudecode

func isZombie(pid int) bool { return false }
