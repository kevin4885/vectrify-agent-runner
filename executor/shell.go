package executor

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ShellChunk is a piece of output from a running shell command.
type ShellChunk struct {
	Stream string // "stdout" or "stderr"
	Data   string
}

// ShellResult is the final result of a shell command.
type ShellResult struct {
	ExitCode int
	OK       bool
}

// Shell runs shell commands on the local machine.
type Shell struct {
	workspaceRoot string
}

// NewShell creates a Shell scoped to workspaceRoot as the default working dir.
func NewShell(workspaceRoot string) *Shell {
	return &Shell{workspaceRoot: workspaceRoot}
}

// Run executes cmd in workingDir (defaults to workspaceRoot), streaming output
// chunks to the chunks channel.  Sends ShellResult to result channel when done.
//
// The chunks and result channels are closed before Run returns.
func (s *Shell) Run(
	cmd         string,
	workingDir  string,
	timeoutSecs int,
	chunks      chan<- ShellChunk,
	result      chan<- ShellResult,
) {
	defer close(chunks)
	defer close(result)

	if workingDir == "" {
		workingDir = s.workspaceRoot
	}

	timeout := time.Duration(timeoutSecs) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", cmd)
	} else {
		c = exec.CommandContext(ctx, "bash", "-c", cmd)
	}
	c.Dir = workingDir

	stdoutPipe, err := c.StdoutPipe()
	if err != nil {
		chunks <- ShellChunk{Stream: "stderr", Data: fmt.Sprintf("Error creating stdout pipe: %v\n", err)}
		result <- ShellResult{ExitCode: 1, OK: false}
		return
	}
	stderrPipe, err := c.StderrPipe()
	if err != nil {
		chunks <- ShellChunk{Stream: "stderr", Data: fmt.Sprintf("Error creating stderr pipe: %v\n", err)}
		result <- ShellResult{ExitCode: 1, OK: false}
		return
	}

	if err := c.Start(); err != nil {
		chunks <- ShellChunk{Stream: "stderr", Data: fmt.Sprintf("Error starting command: %v\n", err)}
		result <- ShellResult{ExitCode: 1, OK: false}
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go streamPipe(stdoutPipe, "stdout", chunks, &wg)
	go streamPipe(stderrPipe, "stderr", chunks, &wg)

	wg.Wait()
	err = c.Wait()

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else if ctx.Err() == context.DeadlineExceeded {
			chunks <- ShellChunk{Stream: "stderr", Data: fmt.Sprintf("\n[runner: command timed out after %ds]\n", timeoutSecs)}
			exitCode = -1
		} else {
			exitCode = 1
		}
	}

	result <- ShellResult{ExitCode: exitCode, OK: exitCode == 0}
}

func streamPipe(r io.Reader, stream string, chunks chan<- ShellChunk, wg *sync.WaitGroup) {
	defer wg.Done()
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunks <- ShellChunk{Stream: stream, Data: string(buf[:n])}
		}
		if err != nil {
			break
		}
	}
}

// RunGit executes a structured git operation and returns combined output.
func (s *Shell) RunGit(op, workingDir string, params map[string]interface{}) (string, error) {
	if workingDir == "" {
		workingDir = s.workspaceRoot
	}

	args, err := buildGitArgs(op, params)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = workingDir

	out, err := c.CombinedOutput()
	output := strings.TrimSpace(string(out))

	if err != nil {
		if output != "" {
			return "", fmt.Errorf("%s\n%s", err.Error(), output)
		}
		return "", err
	}
	return output, nil
}

func buildGitArgs(op string, p map[string]interface{}) ([]string, error) {
	str := func(key string) string {
		v, _ := p[key].(string)
		return v
	}
	boolVal := func(key string) bool {
		v, _ := p[key].(bool)
		return v
	}
	intVal := func(key string, def int) int {
		if v, ok := p[key].(float64); ok {
			return int(v)
		}
		return def
	}

	switch op {
	case "status":
		return []string{"status", "--short"}, nil

	case "diff":
		if boolVal("staged") {
			return []string{"diff", "--staged"}, nil
		}
		return []string{"diff"}, nil

	case "log":
		n := intVal("count", 10)
		return []string{"log", fmt.Sprintf("--oneline"), fmt.Sprintf("-n%d", n)}, nil

	case "branch":
		branch := str("branch")
		if boolVal("create") {
			if branch == "" {
				return nil, fmt.Errorf("branch name is required when create=true")
			}
			return []string{"checkout", "-b", branch}, nil
		}
		return []string{"branch", "--list"}, nil

	case "checkout":
		branch := str("branch")
		if branch == "" {
			return nil, fmt.Errorf("branch name is required for checkout")
		}
		return []string{"checkout", branch}, nil

	case "add":
		paths := str("paths")
		if paths != "" {
			return append([]string{"add"}, strings.Fields(paths)...), nil
		}
		return []string{"add", "-A"}, nil

	case "commit":
		msg := str("message")
		if msg == "" {
			return nil, fmt.Errorf("commit message is required")
		}
		return []string{"commit", "-m", msg}, nil

	case "push":
		remote := str("remote")
		if remote == "" {
			remote = "origin"
		}
		branch := str("branch")
		args := []string{"push"}
		if boolVal("set_upstream") {
			args = append(args, "--set-upstream")
		}
		args = append(args, remote)
		if branch != "" {
			args = append(args, branch)
		}
		return args, nil

	case "pull":
		remote := str("remote")
		if remote == "" {
			remote = "origin"
		}
		branch := str("branch")
		args := []string{"pull", remote}
		if branch != "" {
			args = append(args, branch)
		}
		return args, nil

	case "clone":
		repoURL := str("repo_url")
		if repoURL == "" {
			return nil, fmt.Errorf("repo_url is required for clone")
		}
		return []string{"clone", repoURL, "."}, nil

	default:
		return nil, fmt.Errorf("unknown git operation: %q", op)
	}
}
