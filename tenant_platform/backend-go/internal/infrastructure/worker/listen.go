package worker

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"sync"
	"time"
)

var workerListenRE = regexp.MustCompile(`WORKER_LISTEN=(\S+)`)

// WaitWorkerListen reads a worker stdout/stderr pipe until the process publishes
// its concrete listen address, or until the startup deadline expires.
//
// The blocking pipe Read runs in a dedicated goroutine so the caller's timeout
// remains real even when the child process stays alive but never writes the
// WORKER_LISTEN line. Once the address is found, the goroutine keeps draining
// the pipe to avoid blocking the worker on a full stdout buffer.
func WaitWorkerListen(r io.Reader, timeout time.Duration) (string, error) {
	type outputBuffer struct {
		mu   sync.Mutex
		data []byte
	}
	appendOutput := func(buf *outputBuffer, chunk []byte) {
		buf.mu.Lock()
		buf.data = append(buf.data, chunk...)
		buf.mu.Unlock()
	}
	snapshot := func(buf *outputBuffer) string {
		buf.mu.Lock()
		defer buf.mu.Unlock()
		return string(append([]byte(nil), buf.data...))
	}

	output := &outputBuffer{}
	addrCh := make(chan string, 1)
	errCh := make(chan error, 1)
	deliverAddr := func(value string) {
		select {
		case addrCh <- value:
		default:
		}
	}
	deliverErr := func(err error) {
		select {
		case errCh <- err:
		default:
		}
	}
	go func() {
		tmp := make([]byte, 512)
		for {
			n, err := r.Read(tmp)
			if n > 0 {
				appendOutput(output, tmp[:n])
				if m := workerListenRE.FindStringSubmatch(snapshot(output)); len(m) == 2 {
					deliverAddr(m[1])
					_, _ = io.Copy(io.Discard, r)
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					deliverErr(fmt.Errorf("worker exited before WORKER_LISTEN; output:\n%s", snapshot(output)))
					return
				}
				deliverErr(err)
				return
			}
		}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case addr := <-addrCh:
			return addr, nil
		case err := <-errCh:
			return "", err
		case <-timer.C:
			select {
			case addr := <-addrCh:
				return addr, nil
			case err := <-errCh:
				return "", err
			default:
			}
			return "", fmt.Errorf("timeout waiting for WORKER_LISTEN; output:\n%s", snapshot(output))
		}
	}
}
