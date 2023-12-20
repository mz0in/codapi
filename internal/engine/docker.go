// Execute commands using Docker.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nalgeon/codapi/internal/config"
	"github.com/nalgeon/codapi/internal/execy"
	"github.com/nalgeon/codapi/internal/fileio"
	"github.com/nalgeon/codapi/internal/logx"
)

var killTimeout = 5 * time.Second

const (
	actionRun  = "run"
	actionExec = "exec"
)

// A Docker engine executes a specific sandbox command
// using Docker `run` or `exec` actions.
type Docker struct {
	cfg *config.Config
	cmd *config.Command
}

// NewDocker creates a new Docker engine for a specific command.
func NewDocker(cfg *config.Config, sandbox, command string) Engine {
	cmd := cfg.Commands[sandbox][command]
	return &Docker{cfg, cmd}
}

// Exec executes the command and returns the output.
func (e *Docker) Exec(req Request) Execution {
	// all steps operate in the same temp directory
	dir, err := os.MkdirTemp("", "")
	if err != nil {
		err = NewExecutionError("create temp dir", err)
		return Fail(req.ID, err)
	}
	defer os.RemoveAll(dir)

	// if the command entry point file is not defined,
	// there is no need to store request files in the temp directory
	if e.cmd.Entry != "" {
		// write request files to the temp directory
		err = e.writeFiles(dir, req.Files)
		if err != nil {
			err = NewExecutionError("write files to temp dir", err)
			return Fail(req.ID, err)
		}
	}

	// initialization step
	if e.cmd.Before != nil {
		out := e.execStep(e.cmd.Before, req, dir, nil)
		if !out.OK {
			return out
		}
	}

	// the first step is required
	first, rest := e.cmd.Steps[0], e.cmd.Steps[1:]
	out := e.execStep(first, req, dir, req.Files)

	// the rest are optional
	if out.OK && len(rest) > 0 {
		// each step operates on the results of the previous one,
		// without using the source files - hence `nil` instead of `files`
		for _, step := range rest {
			out = e.execStep(step, req, dir, nil)
			if !out.OK {
				break
			}
		}
	}

	// cleanup step
	if e.cmd.After != nil {
		afterOut := e.execStep(e.cmd.After, req, dir, nil)
		if out.OK && !afterOut.OK {
			return afterOut
		}
	}

	return out
}

// execStep executes a step using the docker container.
func (e *Docker) execStep(step *config.Step, req Request, dir string, files Files) Execution {
	box := e.cfg.Boxes[step.Box]
	err := e.validateVersion(box, step, req)
	if err != nil {
		return Fail(req.ID, err)
	}

	err = e.copyFiles(box, dir)
	if err != nil {
		err = NewExecutionError("copy files to temp dir", err)
		return Fail(req.ID, err)
	}

	stdout, stderr, err := e.exec(box, step, req, dir, files)
	if err != nil {
		return Fail(req.ID, err)
	}

	return Execution{
		ID:     req.ID,
		OK:     true,
		Stdout: stdout,
		Stderr: stderr,
	}
}

func (e *Docker) validateVersion(box *config.Box, step *config.Step, req Request) error {
	// If the version is set in the step config, use it.
	// If the version isn't set in the request, use the latest one.
	// Otherwise, check that the version in the request is supported
	// according to the box config.
	if step.Version == "" && req.Version != "" && !slices.Contains(box.Versions, req.Version) {
		err := fmt.Errorf("box %s does not support version %s", step.Box, req.Version)
		return err
	}
	return nil
}

// copyFiles copies box files to the temporary directory.
func (e *Docker) copyFiles(box *config.Box, dir string) error {
	if box == nil || len(box.Files) == 0 {
		return nil
	}
	for _, pattern := range box.Files {
		err := fileio.CopyFiles(pattern, dir)
		if err != nil {
			return err
		}
	}
	return nil
}

// writeFiles writes request files to the temporary directory.
func (e *Docker) writeFiles(dir string, files Files) error {
	var err error
	files.Range(func(name, content string) bool {
		if name == "" {
			name = e.cmd.Entry
		}
		path := filepath.Join(dir, name)
		err = fileio.WriteFile(path, content, 0444)
		return err == nil
	})
	return err
}

// exec executes the step in the docker container
// using the files from in the temporary directory.
func (e *Docker) exec(box *config.Box, step *config.Step, req Request, dir string, files Files) (stdout string, stderr string, err error) {
	// limit the stdout/stderr size
	prog := NewProgram(step.Timeout, int64(step.NOutput))
	args := e.buildArgs(box, step, req, dir)

	if step.Stdin {
		// pass files to container from stdin
		stdin := filesReader(files)
		stdout, stderr, err = prog.RunStdin(stdin, req.ID, "docker", args...)
	} else {
		// pass files to container from temp directory
		stdout, stderr, err = prog.Run(req.ID, "docker", args...)
	}

	if err == nil {
		// success
		return
	}

	if err.Error() == "signal: killed" {
		if step.Action == actionRun {
			// we have to "docker kill" the container here, because the proccess
			// inside the container is not related to the "docker run" process,
			// and will hang forever after the "docker run" process is killed
			go func() {
				err = dockerKill(req.ID)
				if err == nil {
					logx.Debug("%s: docker kill ok", req.ID)
				} else {
					logx.Log("%s: docker kill failed: %v", req.ID, err)
				}
			}()
		}
		// context timeout
		err = ErrTimeout
		return
	}

	exitErr := new(exec.ExitError)
	if errors.As(err, &exitErr) {
		// the problem (if any) is the code, not the execution
		// so we return the error without wrapping into ExecutionError
		stderr, stdout = stdout+stderr, ""
		if stderr != "" {
			err = fmt.Errorf("%s (%s)", stderr, err)
		}
		return
	}

	// other execution error
	err = NewExecutionError("execute code", err)
	return
}

// buildArgs prepares the arguments for the `docker` command.
func (e *Docker) buildArgs(box *config.Box, step *config.Step, req Request, dir string) []string {
	var args []string
	if step.Action == actionRun {
		args = dockerRunArgs(box, step, req, dir)
	} else if step.Action == actionExec {
		args = dockerExecArgs(step)
	} else {
		// should never happen if the config is valid
		args = []string{"version"}
	}

	command := expandVars(step.Command, req.ID)
	args = append(args, command...)
	logx.Debug("%v", args)
	return args
}

// buildArgs prepares the arguments for the `docker run` command.
func dockerRunArgs(box *config.Box, step *config.Step, req Request, dir string) []string {
	args := []string{
		actionRun, "--rm",
		"--name", req.ID,
		"--runtime", box.Runtime,
		"--cpus", strconv.Itoa(box.CPU),
		"--memory", fmt.Sprintf("%dm", box.Memory),
		"--network", box.Network,
		"--pids-limit", strconv.Itoa(box.NProc),
		"--user", step.User,
	}
	if !box.Writable {
		args = append(args, "--read-only")
	}
	if step.Stdin {
		args = append(args, "--interactive")
	}
	if box.Storage != "" {
		args = append(args, "--storage-opt", fmt.Sprintf("size=%s", box.Storage))
	}
	if dir != "" {
		args = append(args, "--volume", fmt.Sprintf(box.Volume, dir))
	}
	for _, fs := range box.Tmpfs {
		args = append(args, "--tmpfs", fs)
	}
	for _, cap := range box.CapAdd {
		args = append(args, "--cap-add", cap)
	}
	for _, cap := range box.CapDrop {
		args = append(args, "--cap-drop", cap)
	}
	for _, lim := range box.Ulimit {
		args = append(args, "--ulimit", lim)
	}

	if step.Version != "" {
		// if the version is set in the step config, use it
		args = append(args, box.Image+":"+step.Version)
	} else if req.Version != "" {
		// if the version is set in the request, use it
		args = append(args, box.Image+":"+req.Version)
	} else {
		// otherwise, use the latest version
		args = append(args, box.Image)
	}
	return args
}

// dockerExecArgs prepares the arguments for the `docker exec` command.
func dockerExecArgs(step *config.Step) []string {
	return []string{
		actionExec, "--interactive",
		"--user", step.User,
		step.Box,
	}
}

// filesReader creates a reader over an in-memory collection of files.
func filesReader(files Files) io.Reader {
	var input strings.Builder
	for _, content := range files {
		input.WriteString(content)
	}
	return strings.NewReader(input.String())
}

// expandVars replaces variables in command arguments with values.
// The only supported variable is :name = container name.
func expandVars(command []string, name string) []string {
	expanded := make([]string, len(command))
	copy(expanded, command)
	for i, cmd := range expanded {
		expanded[i] = strings.Replace(cmd, ":name", name, 1)
	}
	return expanded
}

// dockerKill kills the container with the specified id/name.
func dockerKill(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), killTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "kill", id)
	return execy.Run(cmd)
}
