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
)

func run() error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	s, err := openStore(c, time.Now)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", ":"+c.Port)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Handler: s, ReadHeaderTimeout: 5*time.Second, ReadTimeout: 15*time.Second, IdleTimeout: time.Minute}
	generatorDone := make(chan struct{})
	go func() {
		defer close(generatorDone)
		s.schedule(ctx)
	}()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		timeout, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = server.Shutdown(timeout)
	case err = <-serveDone:
		stop()
	}
	<-generatorDone
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
