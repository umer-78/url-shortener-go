// Command shortener runs the URL shortener: an HTTP API, redirects and a page.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so the binary is static

	"github.com/umer-78/url-shortener-go/internal/shortener"
	"github.com/umer-78/url-shortener-go/internal/web"
)

func main() {
	addr := flag.String("addr", envOr("ADDR", ":8080"), "address to listen on")
	dbPath := flag.String("db", envOr("DB", "links.db"), "SQLite file")
	baseURL := flag.String("base-url", envOr("BASE_URL", ""), "public base URL, e.g. https://s.example.com")
	flag.Parse()

	db, err := sql.Open("sqlite", *dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		log.Fatalf("opening %s: %v", *dbPath, err)
	}
	defer db.Close()

	store, err := shortener.NewStore(db)
	if err != nil {
		log.Fatalf("preparing the database: %v", err)
	}

	base := *baseURL
	if base == "" {
		// ":8080" means every interface; a link has to name a host, so use localhost
		if strings.HasPrefix(*addr, ":") {
			base = "http://localhost" + *addr
		} else {
			base = "http://" + *addr
		}
	}
	server := &http.Server{
		Addr:              *addr,
		Handler:           shortener.NewServer(store, base, web.Page),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Expired links are deleted hourly rather than at read time, so the table
	// does not grow for ever with links nobody can use.
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if n, err := store.PurgeExpired(); err != nil {
					log.Printf("purge failed: %v", err)
				} else if n > 0 {
					log.Printf("purged %d expired links", n)
				}
			case <-stop:
				return
			}
		}
	}()

	go func() {
		log.Printf("listening on %s, serving links as %s/<code>", *addr, base)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	<-signals
	close(stop)
	log.Println("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
