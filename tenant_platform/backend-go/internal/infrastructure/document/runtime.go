package document

import "context"

// ContainerSpec contains only per-instance data. Image, process, mounts, and
// security controls are immutable runtime configuration.
type ContainerSpec struct {
	Name     string
	SlotPath string
}

type Container struct {
	ID       string
	Name     string
	SlotPath string
}

type CommandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

type Runtime interface {
	VerifyHost(context.Context) error
	CreateAndStart(context.Context, ContainerSpec) (Container, error)
	Exec(context.Context, string, []string) (CommandResult, error)
	ExecInput(context.Context, string, []string, []byte, int) (CommandResult, error)
	Destroy(context.Context, string) error
}
