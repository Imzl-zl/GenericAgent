//go:build windows

package application

import "errors"

func mkfifoForTest(path string) error {
	return errors.New("mkfifo unsupported on windows")
}
