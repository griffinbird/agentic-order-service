package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/griffinbird/agentic-order-service/internal/shippingmcp"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	flags := flag.NewFlagSet("shipping-mcp", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:8081", "loopback address for the MCP server")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("parse listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("shipping MCP server must bind to a loopback address")
	}

	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           shippingmcp.NewHTTPHandler(shippingmcp.NewServer(shippingmcp.NewService())),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown shipping MCP server: %v", err)
		}
	}()

	log.Printf("shipping MCP server listening at http://%s", *listen)
	err = httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
