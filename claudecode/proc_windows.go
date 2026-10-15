//go:build windows

package claudecode

import (
	"os/exec"
	"strconv"
)

func setProcAttrs(cmd *exec.Cmd) {}

// Windows 에는 프로세스 그룹 신호가 없다. claude 가 띄운 자식까지 정리하려면 taskkill /T(트리) 를 쓴다.
// 실패하면 최소한 claude 본체는 죽인다.
func terminate(cmd *exec.Cmd) { killTree(cmd) }

func killNow(cmd *exec.Cmd) { killTree(cmd) }

func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run(); err != nil {
		_ = cmd.Process.Kill()
	}
}
