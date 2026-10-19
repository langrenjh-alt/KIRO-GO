package codeexec

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedCommand struct {
	args         []string
	input        string
	contextError error
}
type fakeRunner struct {
	mu    sync.Mutex
	calls []recordedCommand
	run   func(context.Context, []string, string, io.Writer, io.Writer) error
}

func (f *fakeRunner) Run(ctx context.Context, args []string, input io.Reader, stdout, stderr io.Writer) error {
	var code string
	if input != nil {
		data, err := io.ReadAll(input)
		if err != nil {
			return err
		}
		code = string(data)
	}
	f.mu.Lock()
	f.calls = append(f.calls, recordedCommand{append([]string(nil), args...), code, ctx.Err()})
	f.mu.Unlock()
	if f.run != nil {
		return f.run(ctx, args, code, stdout, stderr)
	}
	if args[0] == "inspect" {
		_, _ = io.WriteString(stdout, `{"Status":"exited","Running":false,"OOMKilled":false,"ExitCode":0,"Error":"","StartedAt":"2026-10-19T00:00:00Z"}`)
	}
	return nil
}

func (f *fakeRunner) snapshot() []recordedCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedCommand(nil), f.calls...)
}

func testExecutor(t *testing.T, runner Runner, timeout time.Duration) *Executor {
	t.Helper()
	executor, err := New(Config{Token: "test-shared-token", ExecutionTimeout: timeout}, runner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(executor.Close)
	return executor
}

func testRequest(executor *Executor, body string, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:12346/execute", strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	executor.ServeHTTP(w, r)
	return w
}

func decodeResponse(t *testing.T, w *httptest.ResponseRecorder) Response {
	t.Helper()
	var result Response
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON response=%s err=%v", w.Body, err)
	}
	return result
}

func stateJSON(exit int, oom bool) string {
	data, _ := json.Marshal(map[string]any{"Status": "exited", "Running": false, "OOMKilled": oom, "ExitCode": exit, "Error": "", "StartedAt": "2026-10-19T00:00:00Z"})
	return string(data)
}

type fakeExit int

func (e fakeExit) Error() string { return "command exited" }
func (e fakeExit) ExitCode() int { return int(e) }

func TestHTTPAuthorizationAndRouting(t *testing.T) {
	runner := &fakeRunner{}
	executor := testExecutor(t, runner, 0)
	for _, token := range []string{"", "incorrect", "test-shared-token extra"} {
		w := testRequest(executor, `{"code":"print(1)"}`, token)
		result := decodeResponse(t, w)
		if w.Code != 401 || result.ErrorCode != "unauthorized" || result.ReturnCode != -1 {
			t.Fatalf("auth response=%d %+v", w.Code, result)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/execute", strings.NewReader(`{"code":""}`))
	request.Header.Add("Authorization", "Bearer test-shared-token")
	request.Header.Add("Authorization", "Bearer test-shared-token")
	w := httptest.NewRecorder()
	executor.ServeHTTP(w, request)
	if w.Code != 401 {
		t.Fatal("duplicate authorization headers accepted")
	}
	for _, test := range []struct {
		method, path string
		status       int
	}{{"GET", "/execute", 405}, {"POST", "/other", 404}} {
		r := httptest.NewRequest(test.method, test.path, strings.NewReader(`{"code":""}`))
		r.Header.Set("Authorization", "Bearer test-shared-token")
		w := httptest.NewRecorder()
		executor.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("%s %s=%d", test.method, test.path, w.Code)
		}
	}
	if len(runner.snapshot()) != 0 {
		t.Fatal("unauthorized or unroutable requests reached Docker")
	}
	w = testRequest(executor, `{"code":""}`, "test-shared-token")
	if w.Code != 200 || decodeResponse(t, w).ReturnCode != 0 {
		t.Fatalf("authorized response=%d %s", w.Code, w.Body)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("execution response may be cached")
	}
}

func TestRequestAndCodeLimitsRejectBeforeDocker(t *testing.T) {
	runner := &fakeRunner{}
	executor := testExecutor(t, runner, 0)
	for _, body := range []string{`{}`, `null`, `{"code":null}`, `{"code":123}`, `{"code":"x","other":1}`, `{"code":"x"}{"code":"y"}`, `{"code":`} {
		w := testRequest(executor, body, "test-shared-token")
		if w.Code != 400 || decodeResponse(t, w).ErrorCode != "invalid_request" {
			t.Fatalf("invalid body=%q status=%d result=%s", body, w.Code, w.Body)
		}
	}
	body, _ := json.Marshal(map[string]string{"code": strings.Repeat("x", MaxCodeBytes+1)})
	w := testRequest(executor, string(body), "test-shared-token")
	if w.Code != 413 || decodeResponse(t, w).ErrorCode != "code_too_large" {
		t.Fatalf("code limit=%d %s", w.Code, w.Body)
	}
	w = testRequest(executor, `{"code":"x"}`+strings.Repeat(" ", MaxRequestBytes), "test-shared-token")
	if w.Code != 413 || decodeResponse(t, w).ErrorCode != "request_too_large" {
		t.Fatalf("body limit=%d %s", w.Code, w.Body)
	}
	if len(runner.snapshot()) != 0 {
		t.Fatal("invalid input executed")
	}
}

func TestDockerArgumentsContainOnlyIsolatedContainerAndCodeUsesStdin(t *testing.T) {
	runner := &fakeRunner{}
	executor := testExecutor(t, runner, 0)
	code := "print('literal --privileged -v /:/host $HOME')"
	result, status := executor.Execute(context.Background(), code)
	if status != 200 || result.ReturnCode != 0 {
		t.Fatalf("execution=%d %+v", status, result)
	}
	calls := runner.snapshot()
	if len(calls) != 3 || calls[0].args[0] != "run" || calls[1].args[0] != "inspect" || calls[2].args[0] != "rm" {
		t.Fatalf("command sequence=%+v", calls)
	}
	if calls[0].input != code {
		t.Fatal("code was not passed verbatim through stdin")
	}
	args := calls[0].args
	for _, required := range [][]string{{"--network", "none"}, {"--user", "65534:65534"}, {"--cap-drop", "ALL"}, {"--security-opt", "no-new-privileges"}, {"--pids-limit", "64"}, {"--memory", "128m"}, {"--memory-swap", "128m"}, {"--cpus", "0.5"}, {"--ulimit", "nofile=64:64"}, {"--tmpfs", "/tmp:rw,noexec,nosuid,size=16m"}, {"--log-driver", "none"}} {
		found := false
		for i := 0; i+len(required) <= len(args); i++ {
			if reflect.DeepEqual(args[i:i+len(required)], required) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing isolation arguments %v", required)
		}
	}
	for _, arg := range args {
		if arg == "--privileged" || arg == "-v" || arg == "--volume" || arg == "--mount" || arg == "-e" || arg == "--env" || strings.Contains(arg, code) {
			t.Fatalf("unsafe Docker argument=%q", arg)
		}
	}
	if !strings.Contains(strings.Join(args, " "), "--read-only") {
		t.Fatal("root filesystem is writable")
	}
	if want := []string{DefaultImage, "python3", "-I", "-S", "-u", "-"}; !reflect.DeepEqual(args[len(args)-len(want):], want) {
		t.Fatalf("Python invocation=%v", args)
	}
	name := args[3]
	if len(name) != len("kiro-code-")+32 || !strings.HasPrefix(name, "kiro-code-") {
		t.Fatalf("unsafe container identity=%q", name)
	}
	if !reflect.DeepEqual(calls[2].args, []string{"rm", "-f", name}) {
		t.Fatalf("cleanup=%v", calls[2].args)
	}
	if _, status := executor.Execute(context.Background(), ""); status != 200 {
		t.Fatal("second execution failed")
	}
	if second := runner.snapshot()[3].args[3]; second == name {
		t.Fatal("container identity was reused")
	}
}

func TestActualExitCodesIncludingDockerReservedCodesArePreserved(t *testing.T) {
	for _, exit := range []int{0, 1, 7, 125, 126, 127} {
		t.Run(string(rune('a'+exit%26)), func(t *testing.T) {
			runner := &fakeRunner{run: func(_ context.Context, args []string, _ string, stdout, stderr io.Writer) error {
				switch args[0] {
				case "run":
					_, _ = io.WriteString(stdout, "actual stdout\n")
					_, _ = io.WriteString(stderr, "actual stderr\n")
					if exit != 0 {
						return fakeExit(exit)
					}
				case "inspect":
					_, _ = io.WriteString(stdout, stateJSON(exit, false))
				}
				return nil
			}}
			executor := testExecutor(t, runner, 0)
			result, status := executor.Execute(context.Background(), "raise SystemExit(...)")
			if status != 200 || result.ReturnCode != exit || result.ErrorCode != "" || result.Stdout != "actual stdout\n" || result.Stderr != "actual stderr\n" {
				t.Fatalf("exit%d=%d %+v", exit, status, result)
			}
		})
	}
}

func TestOutputCapsKillAndCleanupWithoutUnboundedBuffers(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			runner := &fakeRunner{run: func(ctx context.Context, args []string, _ string, stdout, stderr io.Writer) error {
				if args[0] == "run" {
					target := stdout
					if stream == "stderr" {
						target = stderr
					}
					_, _ = io.WriteString(target, strings.Repeat("x", MaxOutputBytes+1024))
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}}
			executor := testExecutor(t, runner, time.Second)
			result, status := executor.Execute(context.Background(), "while True: print('x')")
			if status != 422 || result.ErrorCode != "output_limit_exceeded" || result.ReturnCode != -1 {
				t.Fatalf("overflow=%d %+v", status, result)
			}
			if len(result.Stdout) > MaxOutputBytes || len(result.Stderr) > MaxOutputBytes {
				t.Fatal("response output exceeded cap")
			}
			calls := runner.snapshot()
			if len(calls) != 2 || calls[1].args[0] != "rm" || calls[1].contextError != nil {
				t.Fatalf("overflow cleanup=%+v", calls)
			}
		})
	}
}

func TestExecutionTimeoutAndClientCancellationAlwaysUseFreshCleanupContext(t *testing.T) {
	for _, cancelClient := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "cancel"}[cancelClient], func(t *testing.T) {
			started := make(chan struct{})
			runner := &fakeRunner{run: func(ctx context.Context, args []string, _ string, stdout, _ io.Writer) error {
				if args[0] == "run" {
					close(started)
					_, _ = io.WriteString(stdout, "before interrupt\n")
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}}
			executor := testExecutor(t, runner, 30*time.Millisecond)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cancelClient {
				go func() { <-started; cancel() }()
			}
			result, status := executor.Execute(ctx, "import time; time.sleep(60)")
			wantStatus, wantCode := 504, "execution_timeout"
			if cancelClient {
				wantStatus, wantCode = 408, "execution_cancelled"
			}
			if status != wantStatus || result.ErrorCode != wantCode || result.ReturnCode != -1 || result.Stdout != "before interrupt\n" {
				t.Fatalf("interrupt=%d %+v", status, result)
			}
			calls := runner.snapshot()
			if len(calls) != 2 || calls[1].args[0] != "rm" || calls[1].contextError != nil {
				t.Fatalf("interruption skipped detached cleanup: %+v", calls)
			}
		})
	}
}

func TestConcurrencyReturns429AndShutdownCancelsBothJobs(t *testing.T) {
	started := make(chan struct{}, MaxConcurrent)
	runner := &fakeRunner{run: func(ctx context.Context, args []string, _ string, _, _ io.Writer) error {
		if args[0] == "run" {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}}
	executor := testExecutor(t, runner, time.Second)
	var wg sync.WaitGroup
	for i := 0; i < MaxConcurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, _ := executor.Execute(context.Background(), "pass")
			if result.ErrorCode != "execution_cancelled" {
				t.Errorf("shutdown result=%+v", result)
			}
		}()
	}
	for i := 0; i < MaxConcurrent; i++ {
		<-started
	}
	w := testRequest(executor, `{"code":"pass"}`, "test-shared-token")
	if w.Code != 429 || decodeResponse(t, w).ErrorCode != "executor_busy" {
		t.Fatalf("third execution=%d %s", w.Code, w.Body)
	}
	executor.Close()
	wg.Wait()
	if result, status := executor.Execute(context.Background(), "pass"); status != 503 || result.ErrorCode != "executor_stopping" {
		t.Fatalf("closed executor=%d %+v", status, result)
	}
	runs, removals := 0, 0
	for _, call := range runner.snapshot() {
		if call.args[0] == "run" {
			runs++
		}
		if call.args[0] == "rm" {
			removals++
			if call.contextError != nil {
				t.Fatal("shutdown cancelled cleanup")
			}
		}
	}
	if runs != 2 || removals != 2 {
		t.Fatalf("concurrency cleanup runs=%d removals=%d", runs, removals)
	}
}

func TestRuntimeErrorsAndCleanupFailuresRemainExplicitAndDoNotLeakHostDetails(t *testing.T) {
	runner := &fakeRunner{run: func(_ context.Context, args []string, _ string, _, stderr io.Writer) error {
		if args[0] == "rm" {
			_, _ = io.WriteString(stderr, "Error: No such container: name")
			return fakeExit(1)
		}
		_, _ = io.WriteString(stderr, "cannot connect /private/host/docker.sock token=host-secret")
		return errors.New("host-private error")
	}}
	executor := testExecutor(t, runner, 0)
	result, status := executor.Execute(context.Background(), "print(1)")
	if status != 502 || result.ErrorCode != "runtime_error" || result.ReturnCode != -1 {
		t.Fatalf("runtime unavailable result=%d %+v", status, result)
	}
	if strings.Contains(result.Stderr, "/private") || strings.Contains(result.Stderr, "host-secret") {
		t.Fatal("raw runtime diagnostic leaked")
	}
	if len(runner.snapshot()) != 3 {
		t.Fatal("runtime failure skipped inspect/cleanup")
	}

	cleanupRunner := &fakeRunner{run: func(_ context.Context, args []string, _ string, stdout, stderr io.Writer) error {
		switch args[0] {
		case "inspect":
			_, _ = io.WriteString(stdout, stateJSON(0, false))
		case "rm":
			_, _ = io.WriteString(stderr, "private daemon failure")
			return fakeExit(1)
		}
		return nil
	}}
	cleanupExecutor := testExecutor(t, cleanupRunner, 0)
	result, status = cleanupExecutor.Execute(context.Background(), "pass")
	if status != 502 || result.ErrorCode != "cleanup_failed" || result.ReturnCode != 0 || !strings.Contains(result.Stderr, "Container cleanup failed") || strings.Contains(result.Stderr, "private daemon") {
		t.Fatalf("cleanup failure=%d %+v", status, result)
	}
}

func TestMemoryKillAndUnverifiableStateAreNotSuccessfulResults(t *testing.T) {
	for _, test := range []struct {
		name, state string
		status      int
		code        string
		exit        int
	}{{"oom", stateJSON(137, true), 422, "memory_limit_exceeded", 137}, {"not-started", `{"Status":"created","Running":false,"ExitCode":0,"StartedAt":"0001-01-01T00:00:00Z"}`, 502, "runtime_error", -1}, {"missing-exit", `{"Status":"exited","StartedAt":"2026-10-19T00:00:00Z"}`, 502, "runtime_error", -1}, {"invalid-json", `invalid`, 502, "runtime_error", -1}} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{run: func(_ context.Context, args []string, _ string, stdout, _ io.Writer) error {
				if args[0] == "run" && test.exit == 137 {
					return fakeExit(137)
				}
				if args[0] == "inspect" {
					_, _ = io.WriteString(stdout, test.state)
				}
				return nil
			}}
			executor := testExecutor(t, runner, 0)
			result, status := executor.Execute(context.Background(), "pass")
			if status != test.status || result.ErrorCode != test.code || result.ReturnCode != test.exit {
				t.Fatalf("state=%d %+v", status, result)
			}
			if calls := runner.snapshot(); calls[len(calls)-1].args[0] != "rm" {
				t.Fatal("state failure skipped cleanup")
			}
		})
	}
}

func TestHTTPClientCancellationStillRemovesContainer(t *testing.T) {
	started, removed := make(chan struct{}), make(chan struct{})
	runner := &fakeRunner{run: func(ctx context.Context, args []string, _ string, _, _ io.Writer) error {
		switch args[0] {
		case "run":
			close(started)
			<-ctx.Done()
			return ctx.Err()
		case "rm":
			if ctx.Err() != nil {
				t.Error("cancelled cleanup context")
			}
			close(removed)
		}
		return nil
	}}
	executor := testExecutor(t, runner, time.Second)
	server := httptest.NewServer(executor)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/execute", strings.NewReader(`{"code":"pass"}`))
	request.Header.Set("Authorization", "Bearer test-shared-token")
	done := make(chan struct{})
	go func() {
		defer close(done)
		response, err := server.Client().Do(request)
		if err == nil {
			response.Body.Close()
			t.Error("cancelled HTTP request returned success")
		}
	}()
	<-started
	cancel()
	<-done
	select {
	case <-removed:
	case <-time.After(time.Second):
		t.Fatal("client disconnect left its container alive")
	}
}
