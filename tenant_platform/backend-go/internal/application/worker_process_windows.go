//go:build windows

package application

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func configureWorkerProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}
