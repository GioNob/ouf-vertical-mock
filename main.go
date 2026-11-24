package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func run() error {
	if len(os.Args) > 2 || (len(os.Args) == 2 && os.Args[1] != "serve") {
		return fmt.Errorf("usage: ouf-vertical-mock serve")
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	s, err := openStore(c, time.Now)
	if err != nil {
		return err
	}
	defer s.lock.Close()
	listener, err := net.Listen("tcp", ":"+c.Port)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 8 * time.Second, WriteTimeout: 8 * time.Second, IdleTimeout: time.Minute}
	generatorDone := make(chan struct{})
	go func() {
		defer close(generatorDone)
		s.schedule(ctx)
	}()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
	case err = <-serveDone:
		stop()
	}
	deadline, cancel := context.WithTimeout(context.Background(), 9*time.Second)
	defer cancel()
	if shutdownErr := server.Shutdown(deadline); shutdownErr != nil {
		_ = server.Close()
		return shutdownErr
	}
	select {
	case <-generatorDone:
	case <-deadline.Done():
		return deadline.Err()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
