package main

import (
	"fmt"
	"html/template"
	"log"
	"log/slog"
	"net/http"
	"os"

	"octopus/internal/config"
)

var helloTmpl = template.Must(template.New("hello").Parse(`<!doctype html>
<html>
<head><title>Octopus</title></head>
<body>
<h1>Octopus is running.</h1>
<p>Status: {{.Status}}</p>
</body>
</html>
`))

func main() {
	configPath := os.Getenv("OCTOPUS_CONFIG")
	if configPath == "" {
		configPath = "config.yaml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/", handleHello)

	addr := fmt.Sprintf(":%d", cfg.Port)
	slog.Info("octopus starting", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func handleHello(w http.ResponseWriter, r *http.Request) {
	if err := helloTmpl.Execute(w, map[string]string{"Status": "booted"}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
