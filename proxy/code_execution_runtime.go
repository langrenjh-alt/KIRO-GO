package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	maxCodeExecutionCodeBytes     = 64 << 10
	maxCodeExecutionResponseBytes = 256 << 10
	maxCodeExecutionOutputBytes   = 16 << 10
	codeExecutionTimeout          = 15 * time.Second
	maxCodeExecutionRounds        = 3
)

// CodeExecutor runs code in a separately managed isolation boundary. The API
// process never interprets generated code or launches a local Python process.
type CodeExecutor interface {
	Execute(context.Context, string) (CodeExecutionResult, error)
}

type CodeExecutionResult struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ReturnCode int    `json:"return_code"`
	ErrorCode  string `json:"error_code,omitempty"`
}

type httpCodeExecutor struct {
	url    string
	token  string
	client *http.Client
}

func configuredCodeExecutor() (CodeExecutor, error) {
	rawURL := strings.TrimSpace(os.Getenv("KIRO_CODE_EXECUTOR_URL"))
	if rawURL == "" {
		return nil, nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("code executor URL must be a loopback HTTP URL")
	}
	ip := net.ParseIP(parsed.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("code executor URL must use a literal loopback address")
	}
	if parsed.Path != "" && parsed.Path != "/" && parsed.Path != "/execute" {
		return nil, errors.New("code executor URL path must be /execute")
	}
	parsed.Path, parsed.RawPath = "/execute", ""
	tokenFile := strings.TrimSpace(os.Getenv("KIRO_CODE_EXECUTOR_TOKEN_FILE"))
	if tokenFile == "" {
		return nil, errors.New("code executor token file is required")
	}
	file, err := os.Open(tokenFile)
	if err != nil {
		return nil, errors.New("code executor token file could not be read")
	}
	defer file.Close()
	rawToken, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil || len(rawToken) > 8192 {
		return nil, errors.New("code executor token file is invalid")
	}
	token := strings.TrimSpace(string(rawToken))
	if token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return nil, errors.New("code executor token file is invalid")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Never disclose the worker credential to an environment proxy.
	transport.DisableKeepAlives = true
	return &httpCodeExecutor{
		url: parsed.String(), token: token,
		client: &http.Client{
			Transport: transport, Timeout: codeExecutionTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (executor *httpCodeExecutor) Execute(ctx context.Context, code string) (CodeExecutionResult, error) {
	failure := CodeExecutionResult{ReturnCode: -1, ErrorCode: "unavailable"}
	if len(code) == 0 || len(code) > maxCodeExecutionCodeBytes {
		failure.ErrorCode = "invalid_tool_input"
		return failure, errors.New("code must contain between 1 and 65536 bytes")
	}
	ctx, cancel := context.WithTimeout(ctx, codeExecutionTimeout)
	defer cancel()
	body, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		return failure, errors.New("code executor request could not be encoded")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, executor.url, bytes.NewReader(body))
	if err != nil {
		return failure, errors.New("code executor request could not be constructed")
	}
	req.Header.Set("Authorization", "Bearer "+executor.token)
	req.Header.Set("Content-Type", "application/json")
	response, err := executor.client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			failure.ErrorCode = "execution_timeout"
		}
		return failure, errors.New("code executor request failed")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxCodeExecutionResponseBytes+1))
	if err != nil || len(raw) > maxCodeExecutionResponseBytes {
		return failure, errors.New("code executor response exceeded its limit or could not be read")
	}
	// Presence checks distinguish an actual successful exit from a malformed
	// response whose missing return_code would otherwise silently become zero.
	var wire struct {
		Stdout     *string `json:"stdout"`
		Stderr     *string `json:"stderr"`
		ReturnCode *int    `json:"return_code"`
		ErrorCode  string  `json:"error_code"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decodeErr := decoder.Decode(&wire)
	var extra interface{}
	trailingErr := decoder.Decode(&extra)
	if decodeErr != nil || trailingErr != io.EOF || wire.Stdout == nil || wire.Stderr == nil || wire.ReturnCode == nil {
		if response.StatusCode == http.StatusTooManyRequests {
			failure.ErrorCode = "too_many_requests"
		}
		return failure, errors.New("code executor returned an invalid response")
	}
	result := CodeExecutionResult{Stdout: *wire.Stdout, Stderr: *wire.Stderr, ReturnCode: *wire.ReturnCode, ErrorCode: wire.ErrorCode}
	if len(result.Stdout) > maxCodeExecutionOutputBytes || len(result.Stderr) > maxCodeExecutionOutputBytes {
		return failure, errors.New("code executor output exceeded its decoded limit")
	}
	if response.StatusCode != http.StatusOK {
		if result.ErrorCode == "" {
			switch response.StatusCode {
			case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
				result.ErrorCode = "invalid_tool_input"
			case http.StatusTooManyRequests:
				result.ErrorCode = "too_many_requests"
			case http.StatusGatewayTimeout:
				result.ErrorCode = "execution_timeout"
			default:
				result.ErrorCode = "unavailable"
			}
		}
		return result, fmt.Errorf("code executor returned HTTP %d", response.StatusCode)
	}
	if result.ErrorCode != "" {
		return result, errors.New("code executor reported an execution failure")
	}
	return result, nil
}

func publicCodeExecutionError(code string) string {
	switch code {
	case "invalid_tool_input", "code_too_large", "request_too_large":
		return "invalid_tool_input"
	case "too_many_requests", "executor_busy":
		return "too_many_requests"
	case "execution_timeout", "execution_time_exceeded":
		return "execution_time_exceeded"
	default:
		return "unavailable"
	}
}
