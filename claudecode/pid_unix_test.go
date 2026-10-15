//go:build !windows

package claudecode

import "syscall"

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// 좀비(회수 전 종료)는 Signal(0) 이 성공하므로 /proc 상태도 본다.
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	return !isZombie(pid)
}
