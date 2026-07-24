//go:build !windows

package application

import "os/exec"

func configureWorkerProcess(_ *exec.Cmd) {}
