package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dlorenc/docstore/internal/server"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := server.New()

	httpServer := &http.Server{
		Addr:         ":" + port,
		Handler:      srv,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in background.
	go func() {
		log.Printf("docstore listening on :%s", port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// Wait for interrupt signal, then gracefully shut down.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Fatalf("shutdown error: %v", err)
	}
	log.Println("stopped")
}
