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
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"llmgw/internal/api"
	"llmgw/internal/buildinfo"
	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/operations"
	"llmgw/internal/providers"
	"llmgw/internal/roster"
	"llmgw/internal/router"
)

const usage = `Usage: llmgw [serve|health|version|backup|credentials]

Commands:
  serve   Run the gateway HTTP server (default when no command is given).
  health  Probe the local server's /health endpoint; exit non-zero on failure
           (for the distroless image's Docker/compose healthcheck).
  backup create [archive]       Create an offline, verified state backup.
  backup inspect <archive>      Validate and summarize a backup.
  backup restore <archive> --force
                                Replace offline state from a verified backup.
  credentials rekey             Re-encrypt stored credentials offline from
                                LLMGW_CREDENTIAL_ENCRYPTION_KEY to
                                LLMGW_NEW_CREDENTIAL_ENCRYPTION_KEY.

Environment:
  LLMGW_HOST=127.0.0.1  LLMGW_PORT=8787
  LLMGW_CONFIG=<path to yaml>  LLMGW_API_KEY=<bearer>  LLMGW_ALLOW_UNAUTHENTICATED_API=0
  LLMGW_STATE_DIR=<dir for config/keys/secrets/db>
  LLMGW_ANONYMOUS_PROVIDER_AUTOMATION=0  (1 -> connect and check reviewed no-key providers)
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
		if err := serve(); err != nil {
			log.Fatal(err)
		}
	case "health":
		healthCheck()
	case "-h", "--help", "help":
		fmt.Print(usage)
	case "version", "--version":
		printVersion(os.Stdout)
	case "backup":
		if _, err := config.Load(); err != nil {
			fmt.Fprintln(os.Stderr, "backup:", err)
			os.Exit(1)
		}
		if err := backupCommand(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "backup:", err)
			os.Exit(1)
		}
	case "credentials":
		if _, err := config.Load(); err != nil {
			fmt.Fprintln(os.Stderr, "credentials:", err)
			os.Exit(1)
		}
		if err := credentialsCommand(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "credentials:", err)
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

func serve() error {
	lock, err := operations.AcquireStateLock()
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := operations.RecoverInterruptedRestore(); err != nil {
		return err
	}

	const defaultRosterURL = "https://xibodev.github.io/llm-gateway/roster/payload.json"
	if os.Getenv("LLMGW_PROVIDER_ROSTER_URL") == "" && os.Getenv("LLMGW_PROVIDER_ROSTER_DISABLE") == "" {
		_ = os.Setenv("LLMGW_PROVIDER_ROSTER_URL", defaultRosterURL)
	}

	if _, err := config.Load(); err != nil {
		return err
	}
	if migrated, err := iam.Initialize(); err != nil {
		return fmt.Errorf("initialize IAM control plane: %w", err)
	} else if migrated.Keys > 0 {
		log.Printf(
			"migrated %d legacy API keys into gateway.db (%d projects, %d principals)",
			migrated.Keys, migrated.Projects, migrated.Principals,
		)
	}
	// Deferred here so it runs after the workers below have stopped and
	// before the state lock is released.
	defer func() {
		if err := iam.Close(); err != nil {
			log.Printf("close IAM database: %v", err)
		}
	}()
	// The runtimes own the provider and routing state. Code that does not take
	// them explicitly yet reaches them as the installed ones, so they are
	// installed before anything that could use them starts.
	providerRuntime := providers.NewRuntime()
	providers.Install(providerRuntime)
	routerRuntime := router.NewRuntime(providerRuntime)
	router.Install(routerRuntime)

	host := api.ListenHost()
	port := getenv("LLMGW_PORT", "8787")
	if _, err := strconv.Atoi(port); err != nil {
		port = "8787"
	}
	addr := host + ":" + port
	for _, warning := range api.StartupWarnings(host) {
		log.Printf("warning: %s", warning)
	}
	// Bound before any background worker starts, so a port already in use
	// fails the start before a worker touches state.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	defer listener.Close()
	drain := shutdownTimeout()
	ctx, stopSignals := shutdownOnSignal(drain)
	defer stopSignals()

	externalKeysStop, err := iam.StartExternalKeysFromEnv(context.Background())
	if err != nil {
		return fmt.Errorf("initialize external gateway keys: %w", err)
	}
	defer externalKeysStop()
	retentionStop := startRetention(routerRuntime)
	defer retentionStop()
	rosterStop := roster.Default().Start(context.Background())
	defer rosterStop()
	automationStop := api.StartAnonymousProviderAutomation(context.Background(), providerRuntime)
	defer automationStop()

	// Local providers are surfaced via the /admin "Detect local" button, not
	// hardwired. Opt in to silent auto-add on startup with LLMGW_AUTODISCOVER_LOCAL=1.
	if truthy(os.Getenv("LLMGW_AUTODISCOVER_LOCAL")) {
		func() {
			defer func() { _ = recover() }()
			if _, err := config.AutodetectProviders(true); err != nil {
				log.Printf("warning: local providers were not added: %v", err)
			}
		}()
	}

	srv := newHTTPServer(api.NewServer(api.Runtime{Providers: providerRuntime, Router: routerRuntime}))
	log.Printf("llm-gateway %s (%s) listening on http://%s (admin at /admin)", buildinfo.Version, buildinfo.Commit, addr)
	switch err := serveUntilShutdown(ctx, srv, listener, drain); {
	case errors.Is(err, errDrainExpired):
		// Requests cut short by a stop are its expected cost, not a failure.
		log.Print(err)
		return nil
	case err != nil:
		return fmt.Errorf("server error: %w", err)
	}
	return nil
}

// defaultShutdownTimeout gives a long streamed completion time to finish. A
// container's stop grace period has to be longer, or the runtime kills the
// process mid-drain.
const defaultShutdownTimeout = 25 * time.Second

// shutdownTimeout is how long a stop waits for requests in flight:
// LLMGW_SHUTDOWN_TIMEOUT_SECONDS whole seconds, where zero closes them at once.
func shutdownTimeout() time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(os.Getenv("LLMGW_SHUTDOWN_TIMEOUT_SECONDS")))
	if err != nil || seconds < 0 {
		return defaultShutdownTimeout
	}
	return time.Duration(seconds) * time.Second
}

// shutdownOnSignal returns a context that ends at the first SIGINT or SIGTERM,
// and a function that stops listening for them. A second signal exits at once
// rather than waiting out the drain.
func shutdownOnSignal(drain time.Duration) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go relayShutdownSignals(signals, func() {
		log.Printf("shutting down; draining requests in flight for up to %s", drain)
		cancel()
	}, func() {
		log.Print("second signal; exiting without waiting for the drain")
		os.Exit(1)
	})
	return ctx, func() {
		// Stop guarantees no further delivery, so the channel can be closed.
		signal.Stop(signals)
		close(signals)
		cancel()
	}
}

// relayShutdownSignals calls shutdown on the first signal and force on the
// second. It returns once signals is closed.
func relayShutdownSignals(signals <-chan os.Signal, shutdown, force func()) {
	if _, ok := <-signals; !ok {
		return
	}
	shutdown()
	if _, ok := <-signals; ok {
		force()
	}
}

// errDrainExpired reports a stop that closed requests still in flight when
// the drain ran out.
var errDrainExpired = errors.New("shutdown drain expired; closed the requests still in flight")

// serveUntilShutdown serves srv on listener until ctx is done. It then stops
// accepting connections and gives requests in flight, streams included, up to
// drain to finish and record their usage, and closes whatever is still open
// after that. A serving failure is returned rather than ending the process,
// so the caller still stops its workers and releases its state.
func serveUntilShutdown(ctx context.Context, srv *http.Server, listener net.Listener, drain time.Duration) error {
	served := make(chan error, 1)
	go func() { served <- srv.Serve(listener) }()
	select {
	case err := <-served:
		_ = srv.Close()
		return err
	case <-ctx.Done():
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), drain)
	defer cancel()
	err := srv.Shutdown(drainCtx)
	if errors.Is(err, context.DeadlineExceeded) {
		_ = srv.Close()
		err = errDrainExpired
	}
	<-served
	return err
}

// newHTTPServer returns the gateway's HTTP server for handler.
//
// ReadHeaderTimeout bounds a client that never finishes its request headers.
// IdleTimeout closes keep-alive connections nobody reuses; it outlasts the
// two-minute idle pool of common reverse proxies, Caddy's default included, so
// the proxy retires a pooled connection first and never sends a request into
// one the gateway is closing. There is deliberately no ReadTimeout or
// WriteTimeout: net/http keeps the ReadTimeout deadline on the connection while
// the handler runs, and when it fires the server's background read cancels the
// request context, aborting a long streamed response; a WriteTimeout would cut
// such a stream off as well.
func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       3 * time.Minute,
	}
}

func printVersion(output io.Writer) {
	info := buildinfo.Current()
	fmt.Fprintf(output, "llm-gateway %s\ncommit %s\nbuild_time %s\n", info.Version, info.Commit, info.BuildTime)
}

func startRetention(routes *router.Runtime) func() {
	stop := make(chan struct{})
	run := func() {
		now := time.Now()
		policy := iam.DefaultRetentionPolicy()
		result, err := iam.PruneOperationalHistory(now, policy)
		if err != nil {
			log.Printf("retention: %v", err)
			return
		}
		cutoff := now.AddDate(0, 0, -policy.UsageDays).Unix()
		telemetry, telemetryErr := routes.PruneTelemetryBefore(cutoff)
		savings, savingsErr := routes.PruneSavingsBefore(cutoff)
		backups, backupErr := operations.PruneDefaultBackups()
		if err := errors.Join(telemetryErr, savingsErr, backupErr); err != nil {
			log.Printf("retention: %v", err)
		}
		if result != (iam.RetentionResult{}) || telemetry > 0 || savings > 0 || backups > 0 {
			log.Printf("retention pruned usage=%d audit=%d key_quotas=%d project_quotas=%d outbox=%d telemetry=%d savings=%d backups=%d",
				result.UsageEvents, result.AuditEvents, result.KeyQuotaCounters,
				result.ProjectQuotaCounters, result.DeliveredOutbox,
				telemetry, savings, backups)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		run()
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				run()
			case <-stop:
				return
			}
		}
	}()
	// Stopping waits out a run in progress, so the database it prunes is not
	// closed under it.
	return func() { close(stop); <-done }
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
		if err := printInspection(output, inspection); err != nil {
			return err
		}
		if inspection.CredentialKey == iam.CredentialKeyMismatch {
			fmt.Fprintln(output, "warning: LLMGW_CREDENTIAL_ENCRYPTION_KEY is not the key the restored "+
				"database records; configure that key before starting the gateway")
		}
		return nil
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
	fmt.Fprintf(output, "credential_key: %s\n", inspection.CredentialKey)
	return nil
}

// newCredentialKeyEnv names the key credentials rekey re-encrypts with. It
// is read only by that command, so a gateway never starts with it.
const newCredentialKeyEnv = "LLMGW_NEW_CREDENTIAL_ENCRYPTION_KEY"

func credentialsCommand(args []string, output io.Writer) error {
	if len(args) != 1 || args[0] != "rekey" {
		return fmt.Errorf("usage: llmgw credentials rekey")
	}
	counts, err := operations.RekeyCredentials(config.Get().CredentialEncryptionKey, os.Getenv(newCredentialKeyEnv))
	if err != nil {
		return err
	}
	fmt.Fprintln(output, "credentials re-encrypted; set LLMGW_CREDENTIAL_ENCRYPTION_KEY to the new key before starting the gateway")
	for _, count := range counts {
		fmt.Fprintf(output, "%s: %d\n", count.Table, count.Values)
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
