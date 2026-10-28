package extract

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dag-andersen/argocd-diff-preview/pkg/argoapplication"
	"github.com/dag-andersen/argocd-diff-preview/pkg/argocd"
	"github.com/dag-andersen/argocd-diff-preview/pkg/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestAPIOnlyCleanupAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var deleted atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			_, _ = w.Write([]byte(`{}`))
		case http.MethodGet:
			cancel() // Cancel only after creation has completed.
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
				t.Error("render request was not cancelled")
			}
		case http.MethodDelete:
			assert.Equal(t, "false", r.URL.Query().Get("cascade"))
			deleted.Store(true)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected method: %s", r.Method)
		}
	}))
	defer server.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600))
	installation, err := argocd.NewAPIOnly(server.URL, "dummy", "argocd", "preview", "https://discovery.invalid", ca)
	require.NoError(t, err)
	defer installation.Cleanup()
	app := argoapplication.ArgoResource{Id: "demo", Name: "demo", Branch: git.Base, Kind: argoapplication.Application,
		Yaml: &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"source": map[string]any{"repoURL": "https://git.invalid/demo"}}}}}
	_, err = renderApplicationViaAPI(ctx, installation.APIOnly(), app)
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, deleted.Load(), "cleanup needs a separate context after render cancellation")
}
