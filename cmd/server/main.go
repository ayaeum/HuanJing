package main

import (
	"context"
	"github.com/ayaeum/HuanJing/internal/app"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func main() {
	workers, e := strconv.Atoi(env("WORKERS", "4"))
	if e != nil {
		log.Fatal(e)
	}
	a, e := app.Open(env("DB_PATH", "data/system.db"), workers)
	if e != nil {
		log.Fatal(e)
	}
	defer a.DB.Close()
	if e = a.ConfigureAIFromEnv(); e != nil {
		log.Fatal(e)
	}
	for _, t := range strings.Split(env("SEED_TENANTS", "demo"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			if e := a.Seed(t); e != nil {
				log.Fatal(e)
			}
		}
	}
	a.WebDir = env("WEB_DIR", "web")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a.Start(ctx)
	s := &http.Server{Addr: env("ADDR", "127.0.0.1:8080"), Handler: a.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Shutdown(shutdown)
	}()
	log.Printf("%s listening on %s; moderation, media perception, decisions and comment generation remain simulated", a.AIMode, s.Addr)
	if e = s.ListenAndServe(); e != nil && e != http.ErrServerClosed {
		log.Fatal(e)
	}
}
