package main

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Exercise the whole CLI path with an unusable kubeconfig, no kubectl/helm/git
// executables, and an API that rejects every unexpected route. This checks the
// client boundary; a real server is still required to test rendering itself.
func TestRunAPIOnly(t *testing.T) {
	for _, scenario := range []string{"success", "empty", "render-error", "delete-error", "create-error"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			t.Setenv("KUBECONFIG", filepath.Join(dir, "no-kubeconfig"))
			t.Setenv("PATH", dir)
			var mu sync.Mutex
			apps := map[string]string{}
			created, deleted := 0, 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				assert.Equal(t, "Bearer dummy-api-token", r.Header.Get("Authorization"))
				switch {
				case r.URL.Path == "/api/v1/session/userinfo":
					_, _ = w.Write([]byte(`{"loggedIn":true}`))
				case r.URL.Path == "/api/version":
					_, _ = w.Write([]byte(`{"Version":"v3.5.3"}`))
				case r.Method == "POST" && r.URL.Path == "/api/v1/applications":
					if scenario == "create-error" {
						w.WriteHeader(403)
						return
					}
					var app unstructured.Unstructured
					require.NoError(t, json.NewDecoder(r.Body).Decode(&app))
					assert.True(t, strings.HasPrefix(app.GetName(), "diff-preview-"))
					assert.Equal(t, "preview", app.GetNamespace())
					assert.Empty(t, app.GetFinalizers())
					assert.NotContains(t, app.Object, "operation")
					spec := app.Object["spec"].(map[string]any)
					assert.NotContains(t, spec, "syncPolicy")
					assert.Equal(t, "preview-project", spec["project"])
					assert.Equal(t, map[string]any{"server": "https://discovery.invalid", "namespace": "workload"}, spec["destination"])
					rev, _, _ := unstructured.NestedString(app.Object, "spec", "source", "targetRevision")
					apps[app.GetName()] = rev
					created++
					_, _ = w.Write([]byte(`{}`))
				case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/manifests"):
					name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/applications/"), "/manifests")
					assert.Contains(t, apps, name)
					assert.Equal(t, "preview", r.URL.Query().Get("appNamespace"))
					assert.Equal(t, "preview-project", r.URL.Query().Get("project"))
					if scenario == "render-error" {
						w.WriteHeader(500)
						_, _ = w.Write([]byte("private-file-canary"))
						return
					}
					if scenario == "empty" {
						_, _ = w.Write([]byte(`{}`))
						return
					}
					manifest := fmt.Sprintf(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"demo"},"data":{"revision":%q}}`, apps[name])
					require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"manifests": []string{manifest}}))
				case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/api/v1/applications/"):
					assert.Equal(t, "false", r.URL.Query().Get("cascade"))
					name := strings.TrimPrefix(r.URL.Path, "/api/v1/applications/")
					assert.Contains(t, apps, name)
					delete(apps, name)
					deleted++
					if scenario == "delete-error" {
						w.WriteHeader(403)
						return
					}
					_, _ = w.Write([]byte(`{}`))
				default:
					t.Errorf("unexpected API call: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(403)
				}
			}))
			defer server.Close()
			caFile := filepath.Join(dir, "ca.pem")
			require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600))
			for _, branch := range []string{"base", "target"} {
				require.NoError(t, os.Mkdir(branch+"-branch", 0700))
				require.NoError(t, os.WriteFile(filepath.Join(branch+"-branch", "app.yaml"), []byte(`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: demo
  finalizers: [resources-finalizer.argocd.argoproj.io]
operation:
  sync: {}
spec:
  project: privileged
  destination:
    server: https://production.invalid
    namespace: workload
  source:
    repoURL: https://github.com/example/demo
    path: manifests
    targetRevision: HEAD
  syncPolicy:
    automated: {}
`), 0600))
			}
			opts := RawOptions{BaseBranch: "main", TargetBranch: "feature", Repo: "example/demo", RenderMethod: "server-api-only",
				ArgocdServer: server.URL, ArgocdCAFile: caFile, ArgocdAuthToken: "dummy-api-token", ArgocdNamespace: "preview",
				ArgocdProject: "preview-project", ArgocdPreviewDestination: "https://discovery.invalid",
				OutputFolder: "output", Timeout: 10, Concurrency: 2, WatchIfNoWatchPatternFound: true, Debug: true}
			cfg, err := opts.ToConfig()
			require.NoError(t, err)
			err = run(cfg)
			if strings.HasSuffix(scenario, "error") {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "private-file-canary")
			} else {
				require.NoError(t, err)
				output, err := os.ReadFile("output/diff.md")
				require.NoError(t, err)
				if scenario == "success" {
					assert.Contains(t, string(output), "revision: feature")
				}
			}
			if scenario == "create-error" {
				assert.Zero(t, created)
				assert.Zero(t, deleted)
			} else {
				assert.Equal(t, 2, created)
				assert.Equal(t, created, deleted, "cleanup must finish on success and failure")
			}
		})
	}
}

func TestAPIOnlyOptions(t *testing.T) {
	for _, configure := range []func(*RawOptions){
		func(o *RawOptions) { o.CreateCluster = true },
		func(o *RawOptions) { o.ArgocdAuthToken = "" },
		func(o *RawOptions) { o.TraverseAppOfApps = true },
		func(o *RawOptions) { o.RepoServerAddress = "localhost:8081" },
	} {
		opts := RawOptions{BaseBranch: "main", TargetBranch: "feature", Repo: "example/demo", RenderMethod: "server-api-only", ArgocdServer: "https://preview.invalid", ArgocdAuthToken: "dummy"}
		configure(&opts)
		_, err := opts.ToConfig()
		require.Error(t, err)
	}
}
