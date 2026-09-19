package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Labels stamped on every credential Secret, so they're discoverable and attributable
// with kubectl (`-l app.kubernetes.io/managed-by=booth-storage`) without ever exposing
// their contents.
const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelWorkspace = "booth.projectbooth.io/workspace"
	labelBackendID = "booth.projectbooth.io/storage-backend"
	managedByValue = "booth-storage"
)

// KubernetesCredentials is the CredentialStore backed by Kubernetes Secrets (ADR 0020):
// one Secret per backend, in booth-storage's own namespace. booth-storage is the
// "granting module" ADR 0020 names — it provisions the Secret at registration time
// rather than serving credentials on demand to other modules.
//
// It needs get/create/update/delete on Secrets in that one namespace (the chart's
// namespaced Role) and nothing cluster-wide.
type KubernetesCredentials struct {
	client    kubernetes.Interface
	namespace string
}

func NewKubernetesCredentials(client kubernetes.Interface, namespace string) *KubernetesCredentials {
	return &KubernetesCredentials{client: client, namespace: namespace}
}

// SecretName derives the Secret's name from (workspace, id). It is a hash rather than a
// concatenation because concatenation is ambiguous ("a-b"+"c" vs "a"+"b-c") and two
// different backends must never share a Secret; the NUL separator makes the hashed
// input unambiguous.
func SecretName(workspace, id string) string {
	sum := sha256.Sum256([]byte(workspace + "\x00" + id))
	return "booth-storage-cred-" + hex.EncodeToString(sum[:])[:24]
}

func (k *KubernetesCredentials) Put(ctx context.Context, workspace, id string, creds map[string]string) error {
	data := make(map[string][]byte, len(creds))
	for key, v := range creds {
		data[key] = []byte(v)
	}
	labels := map[string]string{labelManagedBy: managedByValue, labelWorkspace: workspace, labelBackendID: id}
	name := SecretName(workspace, id)
	secrets := k.client.CoreV1().Secrets(k.namespace)

	existing, err := secrets.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: k.namespace, Labels: labels},
			Type:       corev1.SecretTypeOpaque,
			Data:       data,
		}, metav1.CreateOptions{})
		return wrap("creating", name, err)
	case err != nil:
		return wrap("reading", name, err)
	}

	// Replace wholesale (not merge): an update that drops a key must actually drop it.
	existing.Data, existing.StringData, existing.Labels = data, nil, labels
	_, err = secrets.Update(ctx, existing, metav1.UpdateOptions{})
	return wrap("updating", name, err)
}

func (k *KubernetesCredentials) Get(ctx context.Context, workspace, id string) (map[string]string, error) {
	name := SecretName(workspace, id)
	s, err := k.client.CoreV1().Secrets(k.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, ErrNoCredentials
	}
	if err != nil {
		return nil, wrap("reading", name, err)
	}
	out := make(map[string]string, len(s.Data))
	for key, v := range s.Data {
		out[key] = string(v)
	}
	return out, nil
}

func (k *KubernetesCredentials) Delete(ctx context.Context, workspace, id string) error {
	name := SecretName(workspace, id)
	err := k.client.CoreV1().Secrets(k.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return wrap("deleting", name, err)
}

// wrap adds the operation and Secret name to a Kubernetes error. It never includes
// Secret data — client-go errors don't carry it, and this must stay that way.
func wrap(op, name string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s secret %s: %w", op, name, err)
}
