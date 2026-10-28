package argocd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestAPIOnlyHTTPS(t *testing.T) {
	for _, endpoint := range []string{"http://localhost", "https://user:pass@localhost", "https://localhost?token=secret", "https://localhost#fragment"} {
		_, err := NewAPIOnly(endpoint, "dummy", "argocd", "preview", "https://discovery.invalid", "")
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "user:pass")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("untrusted server must not receive the token")
	}))
	defer server.Close()
	installation, err := NewAPIOnly(server.URL, "dummy", "argocd", "preview", "https://discovery.invalid", "")
	require.NoError(t, err)
	defer installation.Cleanup()
	require.Error(t, installation.APIOnly().Login())
	badCA := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(badCA, []byte("not a certificate"), 0600))
	_, err = NewAPIOnly(server.URL, "dummy", "argocd", "preview", "https://discovery.invalid", badCA)
	require.ErrorContains(t, err, "contains no certificates")
}

func TestAPIOnlyDoesNotFollowRedirectOrLogResponse(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/leak" {
					t.Error("followed redirect with token")
				}
				w.Header().Set("Location", "/leak")
				w.WriteHeader(status)
				_, _ = w.Write([]byte("sensitive-renderer-file-contents"))
			}))
			defer server.Close()
			installation, err := NewAPIOnly(server.URL, "dummy", "argocd", "preview", "https://discovery.invalid", "")
			require.NoError(t, err)
			defer installation.Cleanup()
			client := installation.APIOnly()
			client.httpClient.Transport = server.Client().Transport
			err = client.Login()
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "sensitive-renderer-file-contents")
		})
	}
}

func TestAPIOnlyPreparesApplication(t *testing.T) {
	installation, err := NewAPIOnly("https://preview.invalid", "dummy", "argocd-preview", "preview", "https://discovery.invalid", "")
	require.NoError(t, err)
	defer installation.Cleanup()
	app := &unstructured.Unstructured{Object: map[string]any{
		"metadata":  map[string]any{"name": "diff-preview-test", "namespace": "production", "finalizers": []any{"custom-finalizer"}, "ownerReferences": []any{map[string]any{"name": "parent"}}},
		"operation": map[string]any{"sync": map[string]any{}},
		"status":    map[string]any{"phase": "Succeeded"},
		"spec": map[string]any{"project": "admin", "syncPolicy": map[string]any{"automated": map[string]any{}},
			"destination": map[string]any{"name": "production", "namespace": "workload"},
			"sources": []any{
				map[string]any{"repoURL": "https://charts.invalid", "chart": "demo", "targetRevision": "1.0.0", "helm": map[string]any{"valueFiles": []any{"$values/config.yaml"}}},
				map[string]any{"repoURL": "https://git.invalid/repo", "targetRevision": "commit-sha", "ref": "values"},
			},
		},
	}}
	prepared, err := installation.APIOnly().PrepareApplication(app)
	require.NoError(t, err)
	assert.Equal(t, "production", app.GetNamespace(), "input must remain unchanged")
	assert.Equal(t, "argocd-preview", prepared.GetNamespace())
	assert.Empty(t, prepared.GetFinalizers())
	assert.Empty(t, prepared.GetOwnerReferences())
	assert.NotContains(t, prepared.Object, "operation")
	assert.NotContains(t, prepared.Object, "status")
	spec := prepared.Object["spec"].(map[string]any)
	assert.NotContains(t, spec, "syncPolicy")
	assert.Equal(t, "preview", spec["project"])
	assert.Equal(t, map[string]any{"server": "https://discovery.invalid", "namespace": "workload"}, spec["destination"])
	assert.Equal(t, app.Object["spec"].(map[string]any)["sources"], spec["sources"])
}

func TestAPIOnlyAppsetAndEmptyManifests(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer dummy", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/api/v1/applicationsets/generate":
			var payload map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
			project, _, _ := unstructured.NestedString(payload, "applicationSet", "spec", "template", "spec", "project")
			assert.Equal(t, "preview", project)
			_, _ = w.Write([]byte(`{"applications":[{"metadata":{"name":"demo"},"spec":{}}]}`))
		case "/api/v1/applications/diff-preview-test/manifests":
			assert.Equal(t, "argocd", r.URL.Query().Get("appNamespace"))
			assert.Equal(t, "preview", r.URL.Query().Get("project"))
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
	}))
	defer server.Close()
	installation, err := NewAPIOnly(server.URL, "dummy", "argocd", "preview", "https://discovery.invalid", "")
	require.NoError(t, err)
	defer installation.Cleanup()
	client := installation.APIOnly()
	client.httpClient.Transport = server.Client().Transport
	apps, err := client.AppsetGenerate(&unstructured.Unstructured{Object: map[string]any{}}, "unused")
	require.NoError(t, err)
	require.Len(t, apps, 1)
	assert.Equal(t, "Application", apps[0].GetKind())
	manifests, err := client.Manifests(context.Background(), "diff-preview-test")
	require.NoError(t, err)
	assert.Empty(t, manifests)
}
