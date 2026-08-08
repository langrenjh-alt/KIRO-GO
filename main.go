// Package main provides the entry point for Kiro API Proxy.
//
// Kiro API Proxy is a reverse proxy service that translates Kiro API requests
// into OpenAI and Anthropic (Claude) compatible formats. Key features include:
//   - Multi-account pool with round-robin load balancing
//   - Automatic OAuth token refresh
//   - Streaming response support for real-time AI interactions
//   - Admin panel for account and configuration management
//
// The service exposes the following endpoints:
//   - /v1/messages - Claude API compatible endpoint
//   - /v1/chat/completions - OpenAI API compatible endpoint
//   - /admin - Web-based administration panel
package main

import (
	"context"
	"errors"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"kiro-go/proxy"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	// 配置文件路径，支持环境变量覆盖
	configPath := "data/config.json"
	if envPath := os.Getenv("CONFIG_PATH"); envPath != "" {
		configPath = envPath
	}

	// 确保数据目录存在
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		log.Fatalf("Failed to create data directory: %v", err)
	}

	// 加载配置
	if err := config.Init(configPath); err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	// Initialize log level: LOG_LEVEL env var takes priority over config, defaulting to "info".
	logger.Init(config.GetLogLevel())
	usageFlusher := config.StartApiKeyUsageFlusher(config.DefaultApiKeyUsageFlushInterval, func(err error) {
		logger.Warnf("API key usage flush failed: %v", err)
	})

	// 环境变量覆盖密码
	if envPassword := os.Getenv("ADMIN_PASSWORD"); envPassword != "" {
		config.SetPassword(envPassword)
	}

	// 初始化账号池
	pool.GetPool()

	// 创建 HTTP 处理器（包含后台刷新任务）
	handler := proxy.NewHandler()

	// 启动服务器
	addr := fmt.Sprintf("%s:%d", config.GetHost(), config.GetPort())
	logger.Infof("Kiro-Go starting on http://%s (log level: %s)", addr, logger.LevelName(logger.GetLevel()))
	logger.Infof("Admin panel: http://%s/admin", addr)
	logger.Infof("Claude API: http://%s/v1/messages", addr)
	logger.Infof("OpenAI API: http://%s/v1/chat/completions", addr)

	// WriteTimeout intentionally 0: SSE streams can run for minutes while the
	// upstream model produces tokens. ReadHeaderTimeout + ReadTimeout still
	// guard against slowloris-style header/body stalls.
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.ListenAndServe()
	}()

	shutdownSignal := make(chan os.Signal, 1)
	signal.Notify(shutdownSignal, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(shutdownSignal)

	var err error
	select {
	case err = <-serverErr:
	case sig := <-shutdownSignal:
		logger.Infof("Received %s, shutting down", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		shutdownErr := srv.Shutdown(ctx)
		cancel()
		handler.Close()
		err = <-serverErr
		if shutdownErr != nil {
			if flushErr := usageFlusher.Close(); flushErr != nil {
				logger.Errorf("Final API key usage flush failed: %v", flushErr)
			}
			logger.Fatalf("Server shutdown failed: %v", shutdownErr)
		}
	}

	// ListenAndServe returns ErrServerClosed after a graceful shutdown.
	handler.Close()
	if flushErr := usageFlusher.Close(); flushErr != nil {
		logger.Errorf("Final API key usage flush failed: %v", flushErr)
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatalf("Server failed: %v", err)
	}
}
