package codeexec

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
)

type Response struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ReturnCode int    `json:"return_code"`
	ErrorCode  string `json:"error_code,omitempty"`
}

// Executor is both an HTTP handler and a bounded container executor. Close
// cancels active jobs, waits for forced container cleanup and rejects new jobs.
type Executor struct {
	cfg       Config
	runner    Runner
	tokenHash [32]byte
	root      context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	closed    bool
	slots     chan struct{}
	wg        sync.WaitGroup
	newName   func() (string, error)
}

func New(cfg Config, runner Runner) (*Executor, error) {
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	if runner == nil {
		return nil, errors.New("a trusted Docker runner is required")
	}
	root, cancel := context.WithCancel(context.Background())
	return &Executor{cfg: cfg, runner: runner, tokenHash: sha256.Sum256([]byte(cfg.Token)), root: root, cancel: cancel, slots: make(chan struct{}, MaxConcurrent), newName: randomContainerName}, nil
}

func randomContainerName() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "kiro-code-" + hex.EncodeToString(value[:]), nil
}

func (e *Executor) Close() {
	e.mu.Lock()
	e.closed = true
	e.cancel()
	e.mu.Unlock()
	e.wg.Wait()
}

func failure(code, message string) Response {
	return Response{Stderr: message, ReturnCode: -1, ErrorCode: code}
}

func (e *Executor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	respond := func(status int, result Response) { w.WriteHeader(status); _ = json.NewEncoder(w).Encode(result) }
	if !e.authorized(r) {
		respond(http.StatusUnauthorized, failure("unauthorized", "Unauthorized request."))
		return
	}
	if r.URL.Path != "/execute" {
		respond(http.StatusNotFound, failure("not_found", "Unknown executor endpoint."))
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		respond(http.StatusMethodNotAllowed, failure("method_not_allowed", "POST is required."))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request struct {
		Code *string         `json:"code"`
		Mode json.RawMessage `json:"mode"`
	}
	decodeErr := decoder.Decode(&request)
	if decodeErr == nil {
		var extra any
		decodeErr = decoder.Decode(&extra)
		if decodeErr == io.EOF {
			decodeErr = nil
		} else if decodeErr == nil {
			decodeErr = errors.New("multiple JSON values")
		}
	}
	if decodeErr != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(decodeErr, &tooLarge) {
			respond(http.StatusRequestEntityTooLarge, failure("request_too_large", "Request body exceeds 96 KiB."))
		} else {
			respond(http.StatusBadRequest, failure("invalid_request", "Expected one JSON object containing a code string."))
		}
		return
	}
	if request.Code == nil {
		respond(http.StatusBadRequest, failure("invalid_request", "A code string is required."))
		return
	}
	var mode string
	if len(request.Mode) > 0 {
		if string(request.Mode) == "null" || json.Unmarshal(request.Mode, &mode) != nil {
			respond(http.StatusBadRequest, failure("invalid_request", "Mode must be a string."))
			return
		}
	}
	result, status := e.ExecuteMode(r.Context(), *request.Code, mode)
	respond(status, result)
}

func (e *Executor) authorized(r *http.Request) bool {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \r\n\t") {
		return false
	}
	hash := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(hash[:], e.tokenHash[:]) == 1
}

func (e *Executor) acquire() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return http.StatusServiceUnavailable
	}
	select {
	case e.slots <- struct{}{}:
		e.wg.Add(1)
		return 0
	default:
		return http.StatusTooManyRequests
	}
}

// Execute preserves the original Python-only API. ExecuteMode explicitly selects
// a fixed container interpreter; submitted code is never a host command argument.
func (e *Executor) Execute(parent context.Context, code string) (result Response, status int) {
	return e.ExecuteMode(parent, code, ModePython)
}

// ExecuteMode returns return_code=-1 only when no trustworthy interpreter exit
// status exists. An actual nonzero script exit is not a worker failure.
func (e *Executor) ExecuteMode(parent context.Context, code, mode string) (result Response, status int) {
	if mode == "" {
		mode = ModePython
	}
	if mode != ModePython && mode != ModeBash {
		return failure("invalid_mode", "Mode must be python or bash."), http.StatusBadRequest
	}
	if len(code) > MaxCodeBytes {
		return failure("code_too_large", "Code exceeds 64 KiB."), http.StatusRequestEntityTooLarge
	}
	if parent == nil {
		parent = context.Background()
	}
	if parent.Err() != nil {
		return failure("execution_cancelled", "Execution was cancelled."), http.StatusRequestTimeout
	}
	if rejected := e.acquire(); rejected != 0 {
		if rejected == http.StatusTooManyRequests {
			return failure("executor_busy", "Both execution slots are occupied."), rejected
		}
		return failure("executor_stopping", "Executor is stopping."), rejected
	}
	defer func() { <-e.slots; e.wg.Done() }()
	name, err := e.newName()
	if err != nil {
		return failure("internal_error", "Container identity generation failed."), http.StatusInternalServerError
	}
	// rm uses its own context: cancellation of a caller or a timeout must not
	// cancel the cleanup command that kills and removes its execution container.
	defer func() {
		if !e.cleanup(name) {
			message := "Container cleanup failed."
			if len(result.Stderr)+len(message)+1 <= MaxOutputBytes {
				if result.Stderr != "" {
					result.Stderr += "\n"
				}
				result.Stderr += message
			}
			if result.ErrorCode == "" {
				result.ErrorCode = "cleanup_failed"
				status = http.StatusBadGateway
			}
		}
	}()
	ctx, cancel := context.WithTimeout(parent, e.cfg.ExecutionTimeout)
	defer cancel()
	stop := context.AfterFunc(e.root, cancel)
	defer stop()
	stdout, stderr := newBoundedBuffer(MaxOutputBytes, cancel), newBoundedBuffer(MaxOutputBytes, cancel)
	runErr := e.runner.Run(ctx, e.dockerArguments(name, mode), strings.NewReader(code), stdout, stderr)
	result.Stdout, _ = stdout.snapshot()
	result.Stderr, _ = stderr.snapshot()
	result.ReturnCode = -1
	_, stdoutExceeded := stdout.snapshot()
	_, stderrExceeded := stderr.snapshot()
	if stdoutExceeded || stderrExceeded {
		result.ErrorCode = "output_limit_exceeded"
		return result, http.StatusUnprocessableEntity
	}
	if ctx.Err() != nil {
		if parent.Err() != nil || e.root.Err() != nil {
			result.ErrorCode = "execution_cancelled"
			return result, http.StatusRequestTimeout
		}
		result.ErrorCode = "execution_timeout"
		return result, http.StatusGatewayTimeout
	}
	state, inspectErr := e.inspect(ctx, name)
	if inspectErr != nil {
		if ctx.Err() != nil {
			if parent.Err() != nil || e.root.Err() != nil {
				result.ErrorCode = "execution_cancelled"
				return result, http.StatusRequestTimeout
			}
			result.ErrorCode = "execution_timeout"
			return result, http.StatusGatewayTimeout
		}
		if errors.Is(runErr, exec.ErrNotFound) || errors.Is(runErr, os.ErrNotExist) {
			return failure("runtime_unavailable", "Container runtime is unavailable."), http.StatusServiceUnavailable
		}
		return failure("runtime_error", "Container execution could not be verified."), http.StatusBadGateway
	}
	if state.Running || state.Status != "exited" || state.ExitCode == nil || *state.ExitCode < 0 || state.Error != "" || state.StartedAt == "" || strings.HasPrefix(state.StartedAt, "0001-") {
		return failure("runtime_error", "Container execution did not complete normally."), http.StatusBadGateway
	}
	if runErr != nil {
		var exit interface{ ExitCode() int }
		if !errors.As(runErr, &exit) || exit.ExitCode() != *state.ExitCode {
			return failure("runtime_error", "Container output could not be collected reliably."), http.StatusBadGateway
		}
	}
	result.ReturnCode = *state.ExitCode
	if state.OOMKilled {
		result.ErrorCode = "memory_limit_exceeded"
		return result, http.StatusUnprocessableEntity
	}
	return result, http.StatusOK
}

func (e *Executor) dockerArguments(name, mode string) []string {
	args := []string{"run", "--pull=never", "--name", name, "--label", "kiro.code-executor=true", "--network", "none", "--read-only", "--user", "65534:65534", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "64", "--memory", "128m", "--memory-swap", "128m", "--cpus", "0.5", "--ulimit", "nofile=64:64", "--tmpfs", "/tmp:rw,noexec,nosuid,size=16m", "--workdir", "/tmp", "--log-driver", "none", "-i", e.cfg.Image}
	if mode == ModeBash {
		// -p ignores BASH_ENV/ENV and exported shell functions. The process
		// remains UID 65534 with no capabilities; this does not grant privilege.
		return append(args, "bash", "--noprofile", "--norc", "-p", "-s", "--")
	}
	return append(args, "python3", "-I", "-S", "-u", "-")
}

type containerState struct {
	Status    string `json:"Status"`
	Running   bool   `json:"Running"`
	OOMKilled bool   `json:"OOMKilled"`
	ExitCode  *int   `json:"ExitCode"`
	Error     string `json:"Error"`
	StartedAt string `json:"StartedAt"`
}

func (e *Executor) inspect(ctx context.Context, name string) (containerState, error) {
	stdout, stderr := newBoundedBuffer(4096, nil), newBoundedBuffer(4096, nil)
	err := e.runner.Run(ctx, []string{"inspect", "--format", "{{json .State}}", name}, nil, stdout, stderr)
	if err != nil {
		return containerState{}, err
	}
	data, exceeded := stdout.snapshot()
	if exceeded {
		return containerState{}, errors.New("container state exceeds limit")
	}
	var state containerState
	if err := json.Unmarshal([]byte(data), &state); err != nil {
		return containerState{}, err
	}
	return state, nil
}

func (e *Executor) cleanup(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.CleanupTimeout)
	defer cancel()
	stdout, stderr := newBoundedBuffer(4096, nil), newBoundedBuffer(4096, nil)
	err := e.runner.Run(ctx, []string{"rm", "-f", name}, nil, stdout, stderr)
	if err == nil {
		return true
	}
	message, _ := stderr.snapshot()
	message = strings.ToLower(message)
	return strings.Contains(message, "no such container")
}
