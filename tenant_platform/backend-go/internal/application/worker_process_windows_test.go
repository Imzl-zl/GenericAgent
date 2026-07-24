//go:build windows

package application

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

func TestConfigureWorkerProcessCreatesIndependentWindowsGroup(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "exit", "0")
	configureWorkerProcess(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("worker SysProcAttr is nil")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Fatalf("CreationFlags=%#x, missing CREATE_NEW_PROCESS_GROUP", cmd.SysProcAttr.CreationFlags)
	}
}
