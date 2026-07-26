//go:build !windows

package worker

import "os/exec"

func configureWorkerProcess(_ *exec.Cmd) {}
