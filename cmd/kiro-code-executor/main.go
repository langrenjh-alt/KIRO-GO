package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"kiro-go/codeexec"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := codeexec.ConfigFromEnvironment()
	if err != nil {
		return err
	}
	runner, err := codeexec.NewExecRunner(cfg.DockerBinary)
	if err != nil {
		return errors.New("Docker CLI initialization failed")
	}
	defer runner.Close()
	executor, err := codeexec.New(cfg, runner)
	if err != nil {
		return err
	}
	defer executor.Close()
	listener, err := codeexec.Listen(cfg.ListenAddress)
	if err != nil {
		return err
	}
	defer listener.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Handler: executor, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	log.Printf("code executor listening on %s", listener.Addr())
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.New("executor HTTP service failed")
	case <-ctx.Done():
		executor.Close()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return errors.New("executor HTTP shutdown timed out")
		}
		return nil
	}
}
