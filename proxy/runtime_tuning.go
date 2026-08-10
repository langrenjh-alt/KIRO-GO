package proxy

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultKiroResponseHeaderTimeout = 45 * time.Second
	// Streaming generations remain active until upstream completion or downstream
	// cancellation. Positive environment overrides can opt into local deadlines.
	defaultKiroStreamBodyIdleTimeout = 0
	defaultKiroGenerationTimeout     = 0
	// Recovery chains are bounded by retry counts. A positive environment
	// override can opt into an additional whole-chain cap.
	defaultKiroRecoveryChainTimeout = 0
	// Agent continuations use the normal generation budget by default. A positive
	// environment override can opt back into a shorter continuation deadline.
	defaultKiroAgentContinueTimeout = 0
)

type kiroTimeoutPolicy struct {
	responseHeaderTimeout time.Duration
	bodyIdleTimeout       time.Duration
	generationTimeout     time.Duration
	recoveryChainTimeout  time.Duration
	agentContinueTimeout  time.Duration
}

func kiroTimeoutPolicyFromEnv() kiroTimeoutPolicy {
	return kiroTimeoutPolicy{
		responseHeaderTimeout: time.Duration(positiveEnvInt(
			"KIRO_RESPONSE_HEADER_TIMEOUT_SECONDS",
			int(defaultKiroResponseHeaderTimeout/time.Second),
		)) * time.Second,
		bodyIdleTimeout: time.Duration(nonNegativeEnvInt(
			"KIRO_STREAM_BODY_IDLE_TIMEOUT_SECONDS",
			int(defaultKiroStreamBodyIdleTimeout/time.Second),
		)) * time.Second,
		generationTimeout: time.Duration(nonNegativeEnvInt(
			"KIRO_GENERATION_TIMEOUT_SECONDS",
			int(defaultKiroGenerationTimeout/time.Second),
		)) * time.Second,
		recoveryChainTimeout: time.Duration(nonNegativeEnvInt(
			"KIRO_RECOVERY_CHAIN_TIMEOUT_SECONDS",
			int(defaultKiroRecoveryChainTimeout/time.Second),
		)) * time.Second,
		agentContinueTimeout: time.Duration(nonNegativeEnvInt(
			"KIRO_AGENT_CONTINUATION_TIMEOUT_SECONDS",
			int(defaultKiroAgentContinueTimeout/time.Second),
		)) * time.Second,
	}
}

func positiveEnvInt(name string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func nonNegativeEnvInt(name string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

func earlyStreamPreludeEnabled() bool {
	value, ok := os.LookupEnv("KIRO_EARLY_STREAM_PRELUDE")
	if !ok || strings.TrimSpace(value) == "" {
		// Match kiro.rs: once the upstream response headers are accepted, send
		// the downstream stream envelope before waiting for the first token.
		return true
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(value))
	return err == nil && enabled
}
