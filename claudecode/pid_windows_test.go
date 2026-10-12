//go:build windows

package claudecode

// 윈도우에서는 프로세스 종료를 pid 로 확인하지 않는다(테스트는 유닉스 계열 전제).
func pidAlive(pid int) bool { return false }
