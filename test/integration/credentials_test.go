//go:build integration

// Package integration holds layer-3 tests (contracts/testing-strategy.md): they need a
// real Kubernetes API server, so they only run under `-tags integration` in
// .github/workflows/integration.yml's kind cluster, never in the fast per-push CI layer.
package integration

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/projectbooth/booth-storage/internal/registry"
)

func clientset(t *testing.T) (kubernetes.Interface, string) {
	t.Helper()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatalf("loading kubeconfig (KUBECONFIG=%q): %v", os.Getenv("KUBECONFIG"), err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ns := os.Getenv("BOOTH_IT_NAMESPACE")
	if ns == "" {
		ns = "booth-storage"
	}
	return client, ns
}

// TestCredentialSecretsProvisionOnARealAPIServer verifies ADR 0020 end to end against a
// real API server: registering credentials creates a real Secret with the expected
// shape, updates replace it wholesale, and deleting removes it. The unit tests cover the
// same logic against a fake clientset; this proves the real server accepts what we send
// (name validity, label syntax, RBAC-independent object shape).
func TestCredentialSecretsProvisionOnARealAPIServer(t *testing.T) {
	client, ns := clientset(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store := registry.NewKubernetesCredentials(client, ns)
	workspace, id := "it-ws", fmt.Sprintf("it-backend-%d", time.Now().UnixNano()%1_000_000)
	name := registry.SecretName(workspace, id)
	t.Cleanup(func() {
		_ = client.CoreV1().Secrets(ns).Delete(context.Background(), name, metav1.DeleteOptions{})
	})

	// Create.
	if err := store.Put(ctx, workspace, id, map[string]string{"accessKeyId": "AKIA", "secretAccessKey": "one", "sessionToken": "tok"}); err != nil {
		t.Fatalf("Put (create): %v", err)
	}
	secret, err := client.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Secret not created on the API server: %v", err)
	}
	if secret.Type != corev1.SecretTypeOpaque || string(secret.Data["secretAccessKey"]) != "one" {
		t.Errorf("unexpected Secret: type=%s data-keys=%d", secret.Type, len(secret.Data))
	}
	if secret.Labels["app.kubernetes.io/managed-by"] != "booth-storage" ||
		secret.Labels["booth.projectbooth.io/workspace"] != workspace ||
		secret.Labels["booth.projectbooth.io/storage-backend"] != id {
		t.Errorf("labels = %v", secret.Labels)
	}

	// Replace wholesale: the dropped key must really be gone on a real server too.
	if err := store.Put(ctx, workspace, id, map[string]string{"accessKeyId": "AKIA2", "secretAccessKey": "two"}); err != nil {
		t.Fatalf("Put (replace): %v", err)
	}
	got, err := store.Get(ctx, workspace, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, stale := got["sessionToken"]; stale || got["secretAccessKey"] != "two" {
		t.Errorf("after replace got %v", got)
	}

	// Delete, idempotently.
	if err := store.Delete(ctx, workspace, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.Delete(ctx, workspace, id); err != nil {
		t.Errorf("second Delete: %v", err)
	}
	if _, err := client.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("Secret still present after Delete: %v", err)
	}
	if _, err := store.Get(ctx, workspace, id); err != registry.ErrNoCredentials {
		t.Errorf("Get after Delete = %v, want ErrNoCredentials", err)
	}
}
