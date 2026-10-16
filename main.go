package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"hark2api/internal/app"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8789", "local dashboard address")
	data := flag.String("data", "", "local settings directory")
	cert := flag.String("tls-cert", "", "TLS certificate file")
	key := flag.String("tls-key", "", "TLS private key file")
	proxy := flag.String("upstream-proxy", "http://127.0.0.1:7897", "HTTP proxy for Hark upstream requests")
	idle := flag.Duration("project-idle", 30*time.Minute, "archive Projects after this idle duration")
	flag.Parse()
	if *idle < time.Minute {
		log.Fatal("project-idle must be at least 1m")
	}
	if *data == "" {
		exe, err := os.Executable()
		if err != nil {
			log.Fatal(err)
		}
		*data = filepath.Join(filepath.Dir(exe), "data")
	}
	s, err := app.New(*data)
	if err != nil {
		log.Fatal(err)
	}
	if err := s.SetUpstreamProxy(*proxy); err != nil {
		log.Fatal(err)
	}
	go s.RunProjectCleanup(context.Background(), *idle)
	if (*cert == "") != (*key == "") {
		log.Fatal("tls-cert and tls-key must be set together")
	}
	scheme := "http"
	if *cert != "" {
		scheme = "https"
	}
	fmt.Printf("Hark2API dashboard: %s://%s\n", scheme, *addr)
	server := &http.Server{Addr: *addr, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	if *cert != "" {
		log.Fatal(server.ListenAndServeTLS(*cert, *key))
	}
	log.Fatal(server.ListenAndServe())
}
