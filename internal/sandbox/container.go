package sandbox

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
)

// ContainerProxy wraps a RunnerProxy to route Exec calls through a persistent
// container via `podman exec`. File operations pass through directly
// since the workspace is bind-mounted into the container.
type ContainerProxy struct {
	inner         RunnerProxy
	containerName string
}

// NewContainerProxy creates a ContainerProxy that will route exec calls through
// a container with the given name.
func NewContainerProxy(inner RunnerProxy, containerName string) *ContainerProxy {
	return &ContainerProxy{
		inner:         inner,
		containerName: containerName,
	}
}

// Start launches a long-running container with the workspace bind-mounted.
func (c *ContainerProxy) Start(ctx context.Context, image string, workspaceDir string) error {
	resp, err := c.inner.Exec(ctx, &v1.ExecRequest{
		Command: "podman",
		Args: []string{
			"run", "-d",
			"--name", c.containerName,
			"-v", workspaceDir + ":" + workspaceDir,
			"--network", "host",
			image,
			"tail", "-f", "/dev/null",
		},
		WorkingDir: "/",
		Privileged: false,
	})
	if err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("start container failed (exit %d): %s", resp.ExitCode, resp.Stderr)
	}
	return nil
}

// Stop removes the container (best-effort).
func (c *ContainerProxy) Stop(ctx context.Context) {
	resp, err := c.inner.Exec(ctx, &v1.ExecRequest{
		Command:    "podman",
		Args:       []string{"rm", "-f", c.containerName},
		WorkingDir: "/",
		Privileged: false,
	})
	if err != nil {
		slog.Error("failed to stop container", "container", c.containerName, "error", err)
	} else if resp.ExitCode != 0 {
		slog.Error("failed to stop container", "container", c.containerName, "exit_code", resp.ExitCode, "stderr", resp.Stderr)
	}
}

// CreateSession passes through to the inner proxy, setting the container name
// so the runner creates the session shell inside the container.
func (c *ContainerProxy) CreateSession(ctx context.Context, req *v1.CreateSessionRequest) (*v1.CreateSessionResponse, error) {
	return c.inner.CreateSession(ctx, &v1.CreateSessionRequest{
		WorkingDir: req.WorkingDir,
		Container:  c.containerName,
	})
}

// SessionExec passes through to the inner proxy (session is already inside container).
func (c *ContainerProxy) SessionExec(ctx context.Context, req *v1.SessionExecRequest, onOutput OutputCallback) (*v1.SessionExecResponse, error) {
	return c.inner.SessionExec(ctx, req, onOutput)
}

// CloseSession passes through to the inner proxy.
func (c *ContainerProxy) CloseSession(ctx context.Context, req *v1.CloseSessionRequest) (*v1.CloseSessionResponse, error) {
	return c.inner.CloseSession(ctx, req)
}

// Exec routes the command through the container via `podman exec`.
func (c *ContainerProxy) Exec(ctx context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
	workDir := req.WorkingDir
	if workDir == "" {
		workDir = "/"
	}

	var args []string
	if len(req.Args) > 0 {
		// Explicit command + args: podman exec -w <dir> <container> <cmd> <args...>
		args = []string{"exec", "-w", workDir, c.containerName, req.Command}
		args = append(args, req.Args...)
	} else {
		// No args means the runner would wrap in bash -l -c; intercept and use sh -c instead
		// since arbitrary images may not have bash.
		args = []string{"exec", "-w", workDir, c.containerName, "sh", "-c", req.Command}
	}

	return c.inner.Exec(ctx, &v1.ExecRequest{
		Command:    "podman",
		Args:       args,
		WorkingDir: "/",
		Privileged: false,
	})
}

// UploadFiles passes through to the inner proxy (workspace is bind-mounted).
func (c *ContainerProxy) UploadFiles(ctx context.Context, req *v1.UploadFilesRequest) (*v1.UploadFilesResponse, error) {
	return c.inner.UploadFiles(ctx, req)
}

// ReadFile passes through to the inner proxy (workspace is bind-mounted).
func (c *ContainerProxy) ReadFile(ctx context.Context, req *v1.ReadFileRequest) (*v1.ReadFileResponse, error) {
	return c.inner.ReadFile(ctx, req)
}

// EditFile passes through to the inner proxy (workspace is bind-mounted).
func (c *ContainerProxy) EditFile(ctx context.Context, req *v1.EditFileRequest) (*v1.EditFileResponse, error) {
	return c.inner.EditFile(ctx, req)
}

// WriteFile passes through to the inner proxy (workspace is bind-mounted).
func (c *ContainerProxy) WriteFile(ctx context.Context, req *v1.WriteFileRequest) (*v1.WriteFileResponse, error) {
	return c.inner.WriteFile(ctx, req)
}

// StreamToGuest passes through to the inner proxy.
func (c *ContainerProxy) StreamToGuest(ctx context.Context, destPath string, src io.Reader, size int64) error {
	return c.inner.StreamToGuest(ctx, destPath, src, size)
}

// StreamFromGuest passes through to the inner proxy.
func (c *ContainerProxy) StreamFromGuest(ctx context.Context, srcPath string, dest io.Writer) error {
	return c.inner.StreamFromGuest(ctx, srcPath, dest)
}
