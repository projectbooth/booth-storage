// Command storage is booth-storage's entrypoint: the storage-backend registry and the
// generic read/write/list API other modules call (ADR 0013, 0035, 0036).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/projectbooth/booth-storage/internal/api"
	"github.com/projectbooth/booth-storage/internal/auth"
	"github.com/projectbooth/booth-storage/internal/config"
	"github.com/projectbooth/booth-storage/internal/registry"
)

const shutdownTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	meta, creds, closeStores, err := openStores(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeStores()

	if len(cfg.FilesystemRoots) == 0 {
		log.Print("no filesystem roots configured; the filesystem backend kind is disabled (set BOOTH_STORAGE_FILESYSTEM_ROOTS to enable it)")
	} else {
		log.Printf("filesystem backends may be registered under: %v", cfg.FilesystemRoots)
	}

	verifier, err := auth.NewVerifier(ctx, cfg.OIDC)
	if err != nil {
		return fmt.Errorf("creating OIDC verifier: %w", err)
	}

	server := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: api.NewRouter(api.Deps{
			Verifier:       verifier,
			Registry:       registry.NewService(meta, creds, registry.FilesystemPolicy{Roots: cfg.FilesystemRoots}),
			MaxUploadBytes: cfg.MaxUploadBytes,
		}),
		// Bound how long a client may take to send headers. Deliberately no overall
		// Read/WriteTimeout: those would cut off large object uploads and downloads.
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf("booth-storage listening on %s", cfg.HTTPAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// openStores builds the metadata and credential stores: PostgreSQL + Kubernetes Secrets
// in a real deployment (ADR 0014, ADR 0020), or in-process memory in explicit dev mode.
func openStores(ctx context.Context, cfg config.Config) (registry.MetadataStore, registry.CredentialStore, func(), error) {
	if cfg.DevMemory {
		log.Print("WARNING: BOOTH_STORAGE_DEV_MEMORY=true — backend metadata AND credentials are held only in this process's memory and are lost on restart. Local development only.")
		return registry.NewMemoryStore(), registry.NewMemoryCredentials(), func() {}, nil
	}

	pg, err := registry.NewPostgresStore(ctx, cfg.PostgresDSN)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("opening metadata store: %w", err)
	}

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		pg.Close()
		return nil, nil, nil, fmt.Errorf("loading in-cluster Kubernetes config (credentials are stored as Secrets, ADR 0020): %w", err)
	}
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		pg.Close()
		return nil, nil, nil, fmt.Errorf("creating Kubernetes client: %w", err)
	}
	log.Printf("storing backend credentials as Secrets in namespace %q", cfg.Namespace)

	return pg, registry.NewKubernetesCredentials(client, cfg.Namespace), pg.Close, nil
}
