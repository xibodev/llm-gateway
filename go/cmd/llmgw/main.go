// Command llmgw runs the standalone multi-provider LLM gateway.
//
//	llmgw serve   # run the HTTP server (default)
//
// Config comes from the environment (LLMGW_* prefix) and a YAML config file
// (default ~/.llmgw/config.yaml, override with LLMGW_CONFIG). Providers,
// categories, minted keys, and provider secrets are managed at /admin.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"llmgw/internal/api"
	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/operations"
)

const usage = `Usage: llmgw [serve|health|version|backup]

Commands:
  serve   Run the gateway HTTP server (default when no command is given).
  health  Probe the local server's /health endpoint; exit non-zero on failure
           (for the distroless image's Docker/compose healthcheck).
  backup create [archive]       Create an offline, verified state backup.
  backup inspect <archive>      Validate and summarize a backup.
  backup restore <archive> --force
                                Replace offline state from a verified backup.

Environment:
  LLMGW_HOST=127.0.0.1  LLMGW_PORT=8787
  LLMGW_CONFIG=<path to yaml>  LLMGW_API_KEY=<bearer>  LLMGW_ALLOW_UNAUTHENTICATED_API=0
  LLMGW_STATE_DIR=<dir for config/keys/secrets/db>
  LLMGW_LOG_REQUESTS=0  (1 -> append request metadata JSONL to <state>/requests.jsonl)
  LLMGW_LOG_REQUEST_BODIES=0  (1 -> also capture sensitive request/response bodies)
`

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve", "run":
		serve()
	case "health":
		healthCheck()
	case "-h", "--help", "help":
		fmt.Print(usage)
	case "version", "--version":
		fmt.Println("llm-gateway " + config.Version)
	case "backup":
		if err := backupCommand(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "backup:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "llmgw: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
}

// healthCheck probes the local server's /health endpoint and exits non-zero on
// failure. It exists so the distroless image (which has no shell) can define a
// Docker/compose healthcheck as `llmgw health`.
func healthCheck() {
	port := getenv("LLMGW_PORT", "8787")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/health")
	if err != nil {
		fmt.Fprintln(os.Stderr, "health: "+err.Error())
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "health: status %d\n", resp.StatusCode)
		os.Exit(1)
	}
}

func serve() {
	lock, err := operations.AcquireStateLock()
	if err != nil {
		log.Fatal(err)
	}
	defer lock.Release()
	config.Load()
	if migrated, err := iam.Initialize(); err != nil {
		log.Fatalf("initialize IAM control plane: %v", err)
	} else if migrated.Keys > 0 {
		log.Printf(
			"migrated %d legacy API keys into gateway.db (%d projects, %d principals)",
			migrated.Keys, migrated.Projects, migrated.Principals,
		)
	}

	// Local providers are surfaced via the /admin "Detect local" button, not
	// hardwired. Opt in to silent auto-add on startup with LLMGW_AUTODISCOVER_LOCAL=1.
	if truthy(os.Getenv("LLMGW_AUTODISCOVER_LOCAL")) {
		func() {
			defer func() { _ = recover() }()
			config.AutodetectProviders(true)
		}()
	}

	host := getenv("LLMGW_HOST", "127.0.0.1")
	port := getenv("LLMGW_PORT", "8787")
	if _, err := strconv.Atoi(port); err != nil {
		port = "8787"
	}
	addr := host + ":" + port

	srv := &http.Server{
		Addr:              addr,
		Handler:           api.NewServer(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("llm-gateway %s listening on http://%s (admin at /admin)", config.Version, addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Print("shutting down…")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func backupCommand(args []string, output io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("expected create, inspect, or restore")
	}
	switch args[0] {
	case "create":
		if len(args) > 2 {
			return fmt.Errorf("usage: llmgw backup create [archive]")
		}
		path := ""
		if len(args) == 2 {
			path = args[1]
		}
		inspection, err := operations.CreateBackup(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(output, "backup created: %s\n", inspection.Path)
		return printInspection(output, inspection)
	case "inspect":
		if len(args) != 2 {
			return fmt.Errorf("usage: llmgw backup inspect <archive>")
		}
		inspection, err := operations.InspectBackup(args[1])
		if err != nil {
			return err
		}
		return printInspection(output, inspection)
	case "restore":
		if len(args) != 3 || args[2] != "--force" {
			return fmt.Errorf("usage: llmgw backup restore <archive> --force")
		}
		inspection, err := operations.RestoreBackup(args[1])
		if err != nil {
			return err
		}
		fmt.Fprintln(output, "backup restored")
		return printInspection(output, inspection)
	default:
		return fmt.Errorf("unknown backup command %q", args[0])
	}
}

func printInspection(output io.Writer, inspection operations.BackupInspection) error {
	fmt.Fprintf(output, "format: %d\ncreated: %s\nschema: %d\n", inspection.Format, inspection.CreatedAt, inspection.SchemaVersion)
	fmt.Fprintf(output, "files: %s\n", strings.Join(inspection.Files, ", "))
	for _, name := range []string{"projects", "principals", "api_keys", "provider_connections"} {
		fmt.Fprintf(output, "%s: %d\n", name, inspection.Counts[name])
	}
	return nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
