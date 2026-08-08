package config

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const configProcessLockHelperEnv = "KIROGO_CONFIG_PROCESS_LOCK_HELPER"

func TestConfigProcessLockRejectsSecondProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = Close() })

	cmd := exec.Command(os.Args[0], "-test.run=^TestConfigProcessLockHelperProcess$", "-test.v")
	cmd.Env = append(os.Environ(),
		configProcessLockHelperEnv+"=1",
		"KIROGO_CONFIG_PROCESS_LOCK_PATH="+path,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper process unexpectedly failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "PASS") {
		t.Fatalf("helper process did not report a successful lock conflict check: %s", output)
	}
}

func TestConfigProcessLockHelperProcess(t *testing.T) {
	if os.Getenv(configProcessLockHelperEnv) != "1" {
		return
	}
	err := Init(os.Getenv("KIROGO_CONFIG_PROCESS_LOCK_PATH"))
	if !errors.Is(err, ErrConfigPathInUse) {
		t.Fatalf("Init error = %v, want ErrConfigPathInUse", err)
	}
	t.Log("PASS: configuration path lock rejected the second process")
}
