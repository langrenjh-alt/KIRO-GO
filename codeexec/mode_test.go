package codeexec

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHTTPExecutionModesUseFixedInterpreterAndVerbatimStdin(t *testing.T) {
	for _, mode := range []string{"omitted", "", ModePython, ModeBash} {
		t.Run(mode, func(t *testing.T) {
			runner := &fakeRunner{}
			executor := testExecutor(t, runner, 0)
			code := "printf '%s\\n' \"$(literal)\" '--privileged -v /:/host' '$HOME'\n"
			request := map[string]string{"code": code}
			if mode != "omitted" {
				request["mode"] = mode
			}
			body, _ := json.Marshal(request)
			w := testRequest(executor, string(body), "test-shared-token")
			if w.Code != http.StatusOK || decodeResponse(t, w).ReturnCode != 0 {
				t.Fatalf("mode %q = %d %s", mode, w.Code, w.Body)
			}
			calls := runner.snapshot()
			if len(calls) != 3 || calls[0].input != code || calls[1].args[0] != "inspect" || calls[2].args[0] != "rm" {
				t.Fatalf("execution sequence/stdin = %+v", calls)
			}
			args := calls[0].args
			want := []string{DefaultImage, "python3", "-I", "-S", "-u", "-"}
			if mode == ModeBash {
				want = []string{DefaultImage, "bash", "--noprofile", "--norc", "-p", "-s", "--"}
			}
			if !reflect.DeepEqual(args[len(args)-len(want):], want) {
				t.Fatalf("mode invocation = %v", args)
			}
			// Every language uses the identical isolation prefix and cleanup
			// label, including the default Python path covered by prior tests.
			python := executor.dockerArguments(args[3], ModePython)
			if !reflect.DeepEqual(args[:len(args)-len(want)+1], python[:len(python)-5]) {
				t.Fatalf("mode changed isolation: %v != %v", args, python)
			}
			for _, arg := range args {
				if arg == code || arg == "-c" || arg == "--privileged" || arg == "--mount" || arg == "--volume" || arg == "-v" || arg == "--env" || arg == "-e" {
					t.Fatalf("request controlled a host argument: %q", arg)
				}
			}
		})
	}
}

func TestInvalidExecutionModesNeverReachDocker(t *testing.T) {
	runner := &fakeRunner{}
	executor := testExecutor(t, runner, 0)
	for _, mode := range []string{`null`, `1`, `false`, `[]`, `{}`} {
		w := testRequest(executor, `{"code":"x","mode":`+mode+`}`, "test-shared-token")
		if w.Code != 400 || decodeResponse(t, w).ErrorCode != "invalid_request" {
			t.Fatalf("non-string mode %s = %d %s", mode, w.Code, w.Body)
		}
	}
	for _, mode := range []string{"sh", "BASH", " bash", "bash ", "python3", "bash\n", "bash --privileged", "$(bash)"} {
		body, _ := json.Marshal(map[string]string{"code": "x", "mode": mode})
		w := testRequest(executor, string(body), "test-shared-token")
		if w.Code != 400 || decodeResponse(t, w).ErrorCode != "invalid_mode" {
			t.Fatalf("unknown mode %q = %d %s", mode, w.Code, w.Body)
		}
		result, status := executor.ExecuteMode(context.Background(), "x", mode)
		if status != 400 || result.ErrorCode != "invalid_mode" {
			t.Fatalf("direct unknown mode %q = %d %+v", mode, status, result)
		}
	}
	if len(runner.snapshot()) != 0 {
		t.Fatal("invalid modes reached Docker")
	}
}

func TestBothModesShareRequestAndCodeLimits(t *testing.T) {
	for _, mode := range []string{ModePython, ModeBash} {
		t.Run(mode, func(t *testing.T) {
			runner := &fakeRunner{}
			executor := testExecutor(t, runner, 0)
			body, _ := json.Marshal(map[string]string{"code": strings.Repeat("x", MaxCodeBytes+1), "mode": mode})
			w := testRequest(executor, string(body), "test-shared-token")
			if w.Code != 413 || decodeResponse(t, w).ErrorCode != "code_too_large" {
				t.Fatalf("code limit = %d %s", w.Code, w.Body)
			}
			body, _ = json.Marshal(map[string]string{"code": "x", "mode": mode})
			w = testRequest(executor, string(body)+strings.Repeat(" ", MaxRequestBytes), "test-shared-token")
			if w.Code != 413 || decodeResponse(t, w).ErrorCode != "request_too_large" {
				t.Fatalf("body limit = %d %s", w.Code, w.Body)
			}
			if len(runner.snapshot()) != 0 {
				t.Fatal("oversized request reached Docker")
			}
		})
	}
}

func TestBothModesPreserveVerifiedScriptExitCodes(t *testing.T) {
	for _, mode := range []string{ModePython, ModeBash} {
		for _, exit := range []int{0, 7, 125, 126, 127} {
			t.Run(fmt.Sprintf("%s-%d", mode, exit), func(t *testing.T) {
				runner := &fakeRunner{run: func(_ context.Context, args []string, _ string, out, errOut io.Writer) error {
					switch args[0] {
					case "run":
						_, _ = io.WriteString(out, "actual stdout\n")
						_, _ = io.WriteString(errOut, "actual stderr\n")
						if exit != 0 {
							return fakeExit(exit)
						}
					case "inspect":
						_, _ = io.WriteString(out, stateJSON(exit, false))
					}
					return nil
				}}
				result, status := testExecutor(t, runner, 0).ExecuteMode(context.Background(), "exit code", mode)
				if status != 200 || result.ReturnCode != exit || result.ErrorCode != "" || result.Stdout != "actual stdout\n" || result.Stderr != "actual stderr\n" {
					t.Fatalf("verified exit = %d %+v", status, result)
				}
			})
		}
	}
}

func TestBothModesBoundFailuresAndAlwaysCleanup(t *testing.T) {
	for _, mode := range []string{ModePython, ModeBash} {
		for _, scenario := range []string{"timeout", "cancel", "stdout", "stderr", "oom", "missing-interpreter", "inspect-failure", "cleanup-failure"} {
			t.Run(mode+"-"+scenario, func(t *testing.T) {
				parent, cancel := context.WithCancel(context.Background())
				defer cancel()
				runner := &fakeRunner{run: func(ctx context.Context, args []string, _ string, out, errOut io.Writer) error {
					switch args[0] {
					case "run":
						switch scenario {
						case "stdout", "stderr":
							target := out
							if scenario == "stderr" {
								target = errOut
							}
							_, _ = io.WriteString(target, strings.Repeat("x", MaxOutputBytes+1))
							<-ctx.Done()
							return ctx.Err()
						case "cancel":
							cancel()
							fallthrough
						case "timeout":
							<-ctx.Done()
							return ctx.Err()
						case "oom":
							return fakeExit(137)
						case "missing-interpreter":
							return fakeExit(127)
						}
					case "inspect":
						switch scenario {
						case "oom":
							_, _ = io.WriteString(out, stateJSON(137, true))
						case "missing-interpreter":
							_, _ = io.WriteString(out, `{"Status":"created","Running":false,"ExitCode":127,"Error":"interpreter missing","StartedAt":"0001-01-01T00:00:00Z"}`)
						case "inspect-failure":
							return fakeExit(1)
						default:
							_, _ = io.WriteString(out, stateJSON(0, false))
						}
					case "rm":
						if ctx.Err() != nil {
							t.Error("cleanup inherited cancelled context")
						}
						if scenario == "cleanup-failure" {
							return fakeExit(1)
						}
					}
					return nil
				}}
				result, status := testExecutor(t, runner, 20*time.Millisecond).ExecuteMode(parent, "literal stdin", mode)
				wantCode := map[string]string{"timeout": "execution_timeout", "cancel": "execution_cancelled", "stdout": "output_limit_exceeded", "stderr": "output_limit_exceeded", "oom": "memory_limit_exceeded", "missing-interpreter": "runtime_error", "inspect-failure": "runtime_error", "cleanup-failure": "cleanup_failed"}[scenario]
				wantStatus := map[string]int{"timeout": 504, "cancel": 408, "stdout": 422, "stderr": 422, "oom": 422, "missing-interpreter": 502, "inspect-failure": 502, "cleanup-failure": 502}[scenario]
				if status != wantStatus || result.ErrorCode != wantCode || len(result.Stdout) > MaxOutputBytes || len(result.Stderr) > MaxOutputBytes {
					t.Fatalf("failure = %d %+v", status, result)
				}
				calls := runner.snapshot()
				if calls[len(calls)-1].args[0] != "rm" {
					t.Fatal("failure left container cleanup unattempted")
				}
			})
		}
	}
}

func TestPythonAndBashShareConcurrencyAndShutdown(t *testing.T) {
	started := make(chan struct{}, 2)
	runner := &fakeRunner{run: func(ctx context.Context, args []string, _ string, _, _ io.Writer) error {
		if args[0] == "run" {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}}
	executor := testExecutor(t, runner, time.Second)
	done := make(chan struct{}, 2)
	for _, mode := range []string{ModePython, ModeBash} {
		go func(mode string) {
			defer func() { done <- struct{}{} }()
			if result, _ := executor.ExecuteMode(context.Background(), "stdin", mode); result.ErrorCode != "execution_cancelled" {
				t.Errorf("shutdown result = %+v", result)
			}
		}(mode)
	}
	<-started
	<-started
	for _, mode := range []string{ModePython, ModeBash} {
		if result, status := executor.ExecuteMode(context.Background(), "stdin", mode); status != 429 || result.ErrorCode != "executor_busy" {
			t.Fatalf("separate language slot pool: %d %+v", status, result)
		}
	}
	executor.Close()
	<-done
	<-done
	removals := 0
	for _, call := range runner.snapshot() {
		if call.args[0] == "rm" {
			removals++
		}
	}
	if removals != 2 {
		t.Fatalf("shutdown cleanup count = %d", removals)
	}
}
