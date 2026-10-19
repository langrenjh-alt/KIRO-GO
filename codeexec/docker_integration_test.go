package codeexec

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// These tests deliberately require opt-in and an already-present trusted image.
// They exercise temporary containers, not the installed production service.
func realDockerExecutor(t *testing.T, timeout time.Duration) (*Executor, *fakeRunner) {
	t.Helper()
	if os.Getenv("KIRO_CODE_EXECUTOR_INTEGRATION") != "1" {
		t.Skip("set KIRO_CODE_EXECUTOR_INTEGRATION=1 to run real Docker integration tests")
	}
	docker := os.Getenv("KIRO_CODE_EXECUTOR_DOCKER")
	if docker == "" {
		docker = "docker"
	}
	runner, err := NewExecRunner(docker)
	if err != nil {
		t.Fatal("Docker runner initialization failed")
	}
	t.Cleanup(func() { _ = runner.Close() })
	recorded := &fakeRunner{run: func(ctx context.Context, args []string, code string, out, errOut io.Writer) error {
		return runner.Run(ctx, args, strings.NewReader(code), out, errOut)
	}}
	executor, err := New(Config{Token: "integration-test-only-token", Image: os.Getenv("KIRO_CODE_EXECUTOR_IMAGE"), ExecutionTimeout: timeout}, recorded)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(executor.Close)
	return executor, recorded
}

func realDockerAssertContainersRemoved(t *testing.T, executor *Executor, recorded *fakeRunner) {
	t.Helper()
	for _, call := range recorded.snapshot() {
		if len(call.args) < 4 || call.args[0] != "run" {
			continue
		}
		name := call.args[3]
		out, errOut := newBoundedBuffer(4096, nil), newBoundedBuffer(4096, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := executor.runner.Run(ctx, []string{"ps", "-aq", "--filter", "name=^/" + name + "$"}, nil, out, errOut)
		cancel()
		left, exceeded := out.snapshot()
		if err != nil || exceeded || strings.TrimSpace(left) != "" {
			t.Fatal("execution container was not verified absent after cleanup")
		}
	}
}

func TestRealDockerModesHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, mode, code, stdout, stderr, errorCode string
		status, exit                                int
		timeout                                     time.Duration
	}{
		{name: "default-python-stdlib", code: "import json\nprint('python-stdlib:' + json.dumps(sum(range(11))))\n", stdout: "python-stdlib:55\n", status: 200},
		{name: "bash-pipeline-subprocess", mode: ModeBash, code: "printf 'gamma\\nalpha\\nbeta\\n' | sort\npython3 -I -S -u - <<'PY'\nimport math\nprint('subprocess:' + str(math.isqrt(81)))\nPY\n", stdout: "alpha\nbeta\ngamma\nsubprocess:9\n", status: 200},
		{name: "bash-actual-nonzero", mode: ModeBash, code: "printf 'before-exit\\n'\nprintf 'stderr-line\\n' >&2\nexit 7\n", stdout: "before-exit\n", stderr: "stderr-line\n", status: 200, exit: 7},
		{name: "bash-reserved-exit", mode: ModeBash, code: "exit 127\n", status: 200, exit: 127},
		{name: "python-reserved-exit", mode: ModePython, code: "raise SystemExit(127)\n", status: 200, exit: 127},
		{name: "bash-isolation", mode: ModeBash, code: "if { printf x > /kiro-worker-check; } 2>/dev/null; then exit 91; fi\nprintf 'uid:%s\\n' \"$(id -u)\"\nprintf 'temporary\\n' > /tmp/worker-check\ncat /tmp/worker-check\npython3 -I -S -u - <<'PY'\nimport socket\ns = socket.socket()\ns.settimeout(0.5)\ntry:\n s.connect(('203.0.113.1',443))\nexcept OSError:\n print('network-blocked')\nelse:\n raise SystemExit(92)\nfinally:\n s.close()\nPY\n", stdout: "uid:65534\ntemporary\nnetwork-blocked\n", status: 200},
		{name: "bash-stdout-limit", mode: ModeBash, code: "python3 -I -S -u - <<'PY'\nimport sys\nsys.stdout.write('x'*20000)\nPY\n", status: 422, exit: -1, errorCode: "output_limit_exceeded"},
		{name: "bash-stderr-limit", mode: ModeBash, code: "python3 -I -S -u - <<'PY'\nimport sys\nsys.stderr.write('x'*20000)\nPY\n", status: 422, exit: -1, errorCode: "output_limit_exceeded"},
		{name: "bash-timeout-ignores-term", mode: ModeBash, code: "trap '' TERM\nwhile :; do sleep 1; done\n", status: 504, exit: -1, errorCode: "execution_timeout", timeout: 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor, recorded := realDockerExecutor(t, tc.timeout)
			server := httptest.NewServer(executor)
			defer server.Close()
			body, _ := json.Marshal(map[string]string{"code": tc.code, "mode": tc.mode})
			request, _ := http.NewRequest(http.MethodPost, server.URL+"/execute", strings.NewReader(string(body)))
			request.Header.Set("Authorization", "Bearer integration-test-only-token")
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var result Response
			if err := json.NewDecoder(io.LimitReader(response.Body, 256<<10)).Decode(&result); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != tc.status || result.ReturnCode != tc.exit || result.ErrorCode != tc.errorCode {
				t.Fatalf("HTTP=%d result=%+v", response.StatusCode, result)
			}
			if tc.errorCode == "" && (result.Stdout != tc.stdout || result.Stderr != tc.stderr) {
				t.Fatalf("actual output=%+v", result)
			}
			if len(result.Stdout) > MaxOutputBytes || len(result.Stderr) > MaxOutputBytes {
				t.Fatal("actual output exceeded fixed limit")
			}
			realDockerAssertContainersRemoved(t, executor, recorded)
			if tc.errorCode == "" {
				t.Logf("HTTP=%d stdout=%q stderr=%q exit=%d; container removed", response.StatusCode, result.Stdout, result.Stderr, result.ReturnCode)
			} else {
				t.Logf("HTTP=%d error=%s exit=%d stdout_bytes=%d stderr_bytes=%d; container removed", response.StatusCode, result.ErrorCode, result.ReturnCode, len(result.Stdout), len(result.Stderr))
			}
		})
	}
}

type realDockerNotifyWriter struct {
	target  io.Writer
	started chan struct{}
}

func (w realDockerNotifyWriter) Write(data []byte) (int, error) {
	n, err := w.target.Write(data)
	if n > 0 {
		select {
		case w.started <- struct{}{}:
		default:
		}
	}
	return n, err
}

func TestRealDockerModesClientCancellation(t *testing.T) {
	executor, recorded := realDockerExecutor(t, 0)
	underlying := recorded.run
	started := make(chan struct{}, 1)
	recorded.run = func(ctx context.Context, args []string, code string, out, errOut io.Writer) error {
		if args[0] == "run" {
			out = realDockerNotifyWriter{target: out, started: started}
		}
		return underlying(ctx, args, code, out, errOut)
	}
	server := httptest.NewServer(executor)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body, _ := json.Marshal(map[string]string{"mode": ModeBash, "code": "trap '' TERM\nprintf 'started\\n'\nwhile :; do sleep 1; done\n"})
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/execute", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer integration-test-only-token")
	done := make(chan struct{})
	go func() {
		defer close(done)
		response, err := server.Client().Do(request)
		if err == nil {
			response.Body.Close()
			t.Error("cancelled request returned an HTTP response")
		}
	}()
	select {
	case <-started:
	case <-time.After(8 * time.Second):
		t.Fatal("Bash did not reach actual container execution")
	}
	cancel()
	<-done
	// Close waits for the HTTP handler's detached rm -f, not just client return.
	executor.Close()
	realDockerAssertContainersRemoved(t, executor, recorded)
	t.Log("client cancellation after actual Bash stdout; container removed")
}
