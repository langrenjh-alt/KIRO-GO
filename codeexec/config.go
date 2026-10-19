// Package codeexec runs Python and Bash only inside a restricted Docker container.
// The HTTP worker and Docker CLI are trusted; submitted code is not.
package codeexec

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultListenAddress    = "127.0.0.1:12346"
	DefaultImage            = "python:3.12-slim"
	ModePython              = "python"
	ModeBash                = "bash"
	MaxCodeBytes            = 64 << 10
	MaxRequestBytes         = 96 << 10
	MaxOutputBytes          = 16 << 10
	MaxConcurrent           = 2
	DefaultExecutionTimeout = 10 * time.Second
	DefaultCleanupTimeout   = 2 * time.Second
	maxTokenBytes           = 4096
)

type Config struct {
	ListenAddress    string
	Token            string
	Image            string
	DockerBinary     string
	ExecutionTimeout time.Duration
	CleanupTimeout   time.Duration
}

// ConfigFromEnvironment never logs token contents or token-file paths.
func ConfigFromEnvironment() (Config, error) {
	path := strings.TrimSpace(os.Getenv("KIRO_CODE_EXECUTOR_TOKEN_FILE"))
	if path == "" {
		return Config{}, errors.New("KIRO_CODE_EXECUTOR_TOKEN_FILE is required")
	}
	file, err := os.Open(path)
	if err != nil {
		return Config{}, errors.New("executor token file cannot be opened")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxTokenBytes+1))
	if err != nil || len(data) > maxTokenBytes {
		return Config{}, errors.New("executor token file is invalid")
	}
	cfg := Config{
		ListenAddress: strings.TrimSpace(os.Getenv("KIRO_CODE_EXECUTOR_LISTEN")),
		Token:         strings.TrimSpace(string(data)),
		Image:         strings.TrimSpace(os.Getenv("KIRO_CODE_EXECUTOR_IMAGE")),
		DockerBinary:  strings.TrimSpace(os.Getenv("KIRO_CODE_EXECUTOR_DOCKER")),
	}
	return normalizeConfig(cfg)
}

func normalizeConfig(cfg Config) (Config, error) {
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = DefaultListenAddress
	}
	if cfg.Image == "" {
		cfg.Image = DefaultImage
	}
	if cfg.DockerBinary == "" {
		cfg.DockerBinary = "docker"
	}
	if cfg.ExecutionTimeout == 0 {
		cfg.ExecutionTimeout = DefaultExecutionTimeout
	}
	if cfg.CleanupTimeout == 0 {
		cfg.CleanupTimeout = DefaultCleanupTimeout
	}
	if err := ValidateListenAddress(cfg.ListenAddress); err != nil {
		return Config{}, err
	}
	if cfg.Token == "" || len(cfg.Token) > maxTokenBytes || strings.ContainsAny(cfg.Token, " \t\r\n") {
		return Config{}, errors.New("a nonempty single-line executor token is required")
	}
	if cfg.ExecutionTimeout <= 0 || cfg.ExecutionTimeout > DefaultExecutionTimeout {
		return Config{}, errors.New("execution timeout must be positive and at most 10 seconds")
	}
	if cfg.CleanupTimeout <= 0 || cfg.CleanupTimeout > 3*time.Second {
		return Config{}, errors.New("cleanup timeout must be positive and at most 3 seconds")
	}
	if len(cfg.Image) > 512 || !validImageReference(cfg.Image) {
		return Config{}, errors.New("invalid Python image reference")
	}
	if strings.ContainsAny(cfg.DockerBinary, "\x00\r\n") {
		return Config{}, errors.New("invalid Docker executable")
	}
	return cfg, nil
}

// ValidateListenAddress rejects wildcard bindings, DNS names and non-loopback
// addresses. Port zero is allowed only to support isolated ephemeral listeners.
func ValidateListenAddress(address string) error {
	host, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("executor listen address must be a loopback IP and port")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("executor listener must bind a literal loopback IP")
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 0 || port > 65535 {
		return errors.New("invalid executor listen port")
	}
	return nil
}

func validImageReference(image string) bool {
	if image == "" || !asciiAlphanumeric(image[0]) {
		return false
	}
	for i := 0; i < len(image); i++ {
		b := image[i]
		if !asciiAlphanumeric(b) && !strings.ContainsRune("./:@_-", rune(b)) {
			return false
		}
	}
	return true
}

func asciiAlphanumeric(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// Listen repeats address validation at the actual socket boundary.
func Listen(address string) (net.Listener, error) {
	if err := ValidateListenAddress(address); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("executor loopback listener failed")
	}
	return listener, nil
}
