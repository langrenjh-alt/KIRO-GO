package codeexec

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvironmentRequiresTokenAndHidesItsPath(t *testing.T) {
	t.Setenv("KIRO_CODE_EXECUTOR_TOKEN_FILE", "")
	if _, err := ConfigFromEnvironment(); err == nil {
		t.Fatal("missing token was accepted")
	}
	path := filepath.Join(t.TempDir(), "private-token")
	t.Setenv("KIRO_CODE_EXECUTOR_TOKEN_FILE", path)
	if _, err := ConfigFromEnvironment(); err == nil || strings.Contains(err.Error(), path) {
		t.Fatalf("file-open error=%v", err)
	}
	for _, token := range []string{"", " \n", "not a token", strings.Repeat("x", maxTokenBytes+1)} {
		if err := os.WriteFile(path, []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ConfigFromEnvironment(); err == nil {
			t.Fatalf("invalid token length %d accepted", len(token))
		}
	}
	if err := os.WriteFile(path, []byte("test-shared-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIRO_CODE_EXECUTOR_LISTEN", "")
	t.Setenv("KIRO_CODE_EXECUTOR_IMAGE", "")
	t.Setenv("KIRO_CODE_EXECUTOR_DOCKER", "")
	cfg, err := ConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "test-shared-token" || cfg.ListenAddress != DefaultListenAddress || cfg.Image != DefaultImage || cfg.DockerBinary != "docker" || cfg.ExecutionTimeout != DefaultExecutionTimeout || cfg.CleanupTimeout != DefaultCleanupTimeout {
		t.Fatalf("unexpected defaults: address=%s image=%s", cfg.ListenAddress, cfg.Image)
	}
}

func TestBindingRejectsWildcardNamesAndNonLoopback(t *testing.T) {
	for _, address := range []string{"0.0.0.0:12346", ":12346", "[::]:12346", "localhost:12346", "192.0.2.1:12346", "example.test:12346", "127.0.0.1:65536", "127.0.0.1:-1", "127.0.0.1:bad", "unix:/tmp/executor.sock"} {
		if err := ValidateListenAddress(address); err == nil {
			t.Errorf("accepted address %q", address)
		}
		if listener, err := Listen(address); err == nil {
			listener.Close()
			t.Errorf("opened forbidden address %q", address)
		}
	}
	for _, address := range []string{DefaultListenAddress, "127.0.0.2:0", "[::1]:12346"} {
		if err := ValidateListenAddress(address); err != nil {
			t.Errorf("rejected %q: %v", address, err)
		}
	}
	listener, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	host, _, err := net.SplitHostPort(listener.Addr().String())
	if err != nil || !net.ParseIP(host).IsLoopback() {
		t.Fatalf("actual bound address=%s", listener.Addr())
	}
}

func TestConfigurationRejectsOptionInjectionAndUnsafeTimeouts(t *testing.T) {
	for _, cfg := range []Config{{Token: "test", Image: "--privileged"}, {Token: "test", Image: "python:3.12 alpine"}, {Token: "test", ExecutionTimeout: DefaultExecutionTimeout + 1}, {Token: "test", ExecutionTimeout: -1}, {Token: "test", CleanupTimeout: -1}, {Token: "test", DockerBinary: "docker\nsh"}, {Token: "test", ListenAddress: "0.0.0.0:12346"}, {Token: ""}} {
		if _, err := normalizeConfig(cfg); err == nil {
			t.Fatalf("accepted invalid config: listen=%s image=%s", cfg.ListenAddress, cfg.Image)
		}
	}
	if _, err := normalizeConfig(Config{Token: "test", Image: "python@sha256:a6e34c598f2467ed0e9a8d349809fcd8b5c603269512df273a0bb1784edc11b1"}); err != nil {
		t.Fatal(err)
	}
}

func TestExecRunnerUsesIsolatedConfigurationAndEnvironment(t *testing.T) {
	t.Setenv("HOST_SECRET_TOKEN", "not-inherited")
	t.Setenv("DOCKER_HOST", "tcp://remote.invalid:2375")
	t.Setenv("HTTP_PROXY", "http://secret.invalid")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewExecRunner(binary)
	if err != nil {
		t.Fatal(err)
	}
	directory := runner.directory
	if _, err := os.Stat(directory); err != nil {
		t.Fatal(err)
	}
	for _, entry := range runner.env {
		if strings.Contains(entry, "HOST_SECRET_TOKEN") || strings.HasPrefix(entry, "DOCKER_HOST=") || strings.HasPrefix(entry, "HTTP_PROXY=") || strings.Contains(entry, "not-inherited") {
			t.Fatalf("host environment leaked: %s", entry)
		}
	}
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("private CLI directory remains: %v", err)
	}
}
