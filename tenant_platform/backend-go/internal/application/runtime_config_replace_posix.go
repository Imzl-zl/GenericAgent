//go:build !windows

package application

import "os"

func replaceFileAtomic(source, destination string) error {
	return os.Rename(source, destination)
}
