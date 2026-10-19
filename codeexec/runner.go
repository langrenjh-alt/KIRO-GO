package codeexec

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Runner is a trusted command boundary. Arguments are Docker CLI arguments,
// never a shell command; code is delivered only through stdin.
type Runner interface {
	Run(context.Context, []string, io.Reader, io.Writer, io.Writer) error
}

// ExecRunner uses a fresh empty Docker configuration and a minimal environment,
// excluding host tokens, proxy settings, Docker contexts and DOCKER_HOST.
type ExecRunner struct {
	binary    string
	env       []string
	directory string
	closeOnce sync.Once
}

func NewExecRunner(binary string) (*ExecRunner, error) {
	if binary == "" {
		binary = "docker"
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp("", "kiro-executor-cli-")
	if err != nil {
		return nil, err
	}
	env := []string{"DOCKER_CONFIG=" + directory, "HOME=" + directory}
	for _, key := range []string{"PATH", "SystemRoot", "WINDIR", "TEMP", "TMP"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return &ExecRunner{binary: resolved, env: env, directory: directory}, nil
}

func (r *ExecRunner) Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	command := exec.CommandContext(ctx, r.binary, args...)
	command.Env = append([]string(nil), r.env...)
	command.Stdin, command.Stdout, command.Stderr = stdin, stdout, stderr
	// A CLI descendant must not hold output pipes open indefinitely after its
	// parent exits. Container cleanup has a separate, detached deadline.
	command.WaitDelay = time.Second
	return command.Run()
}

func (r *ExecRunner) Close() error {
	var err error
	r.closeOnce.Do(func() {
		// Remove only the exact private directory created by this runner.
		absolute, resolveErr := filepath.Abs(r.directory)
		if resolveErr != nil || !strings.HasPrefix(filepath.Base(absolute), "kiro-executor-cli-") {
			return
		}
		err = os.RemoveAll(absolute)
	})
	return err
}
