//go:build !windows

package claudecode

import (
	"os/exec"
	"syscall"
)

// claude 가 자식 프로세스를 띄워도 한 번에 정리되도록 새 프로세스 그룹으로 시작한다.
func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminate 는 프로세스 그룹에 SIGTERM, killNow 는 SIGKILL 을 보낸다.
// 문서: SIGTERM 은 진행 중인 턴을 미완으로 두고 종료 코드 143 으로 끝난다.
func terminate(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}

func killNow(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
	}
}
