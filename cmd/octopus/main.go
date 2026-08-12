package main

import (
	"fmt"
	"html/template"
	"log"
	"log/slog"
	"net/http"
	"os"

	"golang.org/x/crypto/bcrypt"

	"octopus/internal/config"
)

// Version is overridden at build time via -ldflags "-X main.Version=x.y.z",
// kept in sync with the repo-root VERSION file (see PLAN.md §9). "dev"
// covers plain `go run`/`go build` with no ldflags.
var Version = "dev"

var helloTmpl = template.Must(template.New("hello").Parse(`<!doctype html>
<html>
<head><title>Octopus</title></head>
<body>
<h1>Octopus is running.</h1>
<p>Status: {{.Status}} (v{{.Version}})</p>
</body>
</html>
`))

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hash-password" {
		runHashPassword(os.Args[2:])
		return
	}

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
	slog.Info("octopus starting", "addr", addr, "version", Version)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// runHashPassword implements `octopus hash-password [password]`, used to
// generate AuthConfig.AdminPasswordHash without ever writing a plaintext
// password into config.yaml. If no argument is given it reads stdin, so the
// password doesn't linger in shell history either.
func runHashPassword(args []string) {
	var password string
	if len(args) > 0 {
		password = args[0]
	} else {
		fmt.Fprint(os.Stderr, "Password: ")
		var input string
		if _, err := fmt.Scanln(&input); err != nil {
			log.Fatalf("reading password: %v", err)
		}
		password = input
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("hashing password: %v", err)
	}
	fmt.Println(string(hash))
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func handleHello(w http.ResponseWriter, r *http.Request) {
	data := map[string]string{"Status": "booted", "Version": Version}
	if err := helloTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
