package argocd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/dag-andersen/argocd-diff-preview/pkg/vars"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// APIOnlyClient uses only the public Argo CD API. It deliberately has no
// Kubernetes client, repository credentials, or repo-server connection.
type APIOnlyClient struct {
	server, token, namespace, project, destination string
	httpClient                                     *http.Client
}

// NewAPIOnly connects to a dedicated preview instance over verified HTTPS.
func NewAPIOnly(server, token, namespace, project, destination, caFile string) (*ArgoCDInstallation, error) {
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("argocd-server must be an HTTPS URL without credentials, query, or fragment")
	}
	if token == "" || namespace == "" || project == "" || destination == "" {
		return nil, fmt.Errorf("API-only mode requires an auth token, namespace, project, and preview destination")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read Argo CD CA: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("load system CAs: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("argo CD CA file contains no certificates")
		}
		tlsConfig.RootCAs = pool
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	c := &APIOnlyClient{
		server: strings.TrimRight(server, "/"), token: token, namespace: namespace,
		project: project, destination: destination,
		httpClient: &http.Client{Transport: transport, Timeout: 60 * time.Second,
			// Never forward an API token through a redirect, including to a
			// different path on the same host.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	return &ArgoCDInstallation{Namespace: namespace, renderMode: vars.RenderMethodServerAPIOnly, operations: c}, nil
}

func (a *ArgoCDInstallation) APIOnly() *APIOnlyClient {
	c, _ := a.operations.(*APIOnlyClient)
	return c
}

func (c *APIOnlyClient) request(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.server+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("argo CD %s request: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A renderer error may contain file contents or credentials. Do not
		// copy an untrusted response body into CI logs.
		return fmt.Errorf("argo CD %s %s returned HTTP %d", method, req.URL.Path, resp.StatusCode)
	}
	if output == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(output); err != nil {
		return fmt.Errorf("decode Argo CD response: %w", err)
	}
	return nil
}

func (c *APIOnlyClient) Login() error {
	var info struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if err := c.request(context.Background(), http.MethodGet, "/api/v1/session/userinfo", nil, &info); err != nil {
		return err
	}
	if !info.LoggedIn {
		return fmt.Errorf("argo CD token is not authenticated")
	}
	return nil
}

func (c *APIOnlyClient) CheckVersionCompatibility() error {
	var result struct {
		Version string `json:"Version"`
	}
	if err := c.request(context.Background(), http.MethodGet, "/api/version", nil, &result); err != nil {
		return err
	}
	return checkServerVersionCompatibility(result.Version)
}

func (c *APIOnlyClient) Cleanup()                              { c.httpClient.CloseIdleConnections() }
func (c *APIOnlyClient) IsExpectedError(string) (bool, string) { return false, "" }
func (c *APIOnlyClient) AddSourceNamespaceToDefaultAppProject() error {
	return fmt.Errorf("API-only mode does not change AppProjects")
}

func (c *APIOnlyClient) AppsetGenerate(resource *unstructured.Unstructured, _ string) ([]unstructured.Unstructured, error) {
	appset := resource.DeepCopy()
	if err := unstructured.SetNestedField(appset.Object, c.project, "spec", "template", "spec", "project"); err != nil {
		return nil, err
	}
	var result struct {
		Applications []map[string]any `json:"applications"`
	}
	if err := c.request(context.Background(), http.MethodPost, "/api/v1/applicationsets/generate", map[string]any{"applicationSet": appset.Object}, &result); err != nil {
		return nil, err
	}
	apps := make([]unstructured.Unstructured, 0, len(result.Applications))
	for _, object := range result.Applications {
		app := unstructured.Unstructured{Object: object}
		app.SetAPIVersion("argoproj.io/v1alpha1")
		app.SetKind("Application")
		apps = append(apps, app)
	}
	return apps, nil
}

// PrepareApplication discards lifecycle metadata and any operation supplied by
// the repository. Only this dedicated instance's project/destination is used.
func (c *APIOnlyClient) PrepareApplication(app *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	spec, found, err := unstructured.NestedMap(app.Object, "spec")
	if err != nil || !found {
		return nil, fmt.Errorf("application has no valid spec")
	}
	delete(spec, "syncPolicy")
	delete(spec, "sourceHydrator")
	spec["project"] = c.project
	dest, _ := spec["destination"].(map[string]any)
	namespace, _ := dest["namespace"].(string)
	spec["destination"] = map[string]any{"server": c.destination, "namespace": namespace}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": app.GetName(), "namespace": c.namespace,
			"annotations": map[string]any{"argocd.argoproj.io/skip-reconcile": "true"}},
		"spec": spec,
	}}, nil
}

func (c *APIOnlyClient) CreateApplication(ctx context.Context, app *unstructured.Unstructured) error {
	// No upsert. Keep server-side validation and AppProject checks enabled.
	return c.request(ctx, http.MethodPost, "/api/v1/applications", app.Object, nil)
}

func (c *APIOnlyClient) applicationPath(name, suffix string) string {
	q := url.Values{"appNamespace": {c.namespace}, "project": {c.project}}
	if suffix == "" {
		q.Set("cascade", "false")
	}
	return "/api/v1/applications/" + url.PathEscape(name) + suffix + "?" + q.Encode()
}

func (c *APIOnlyClient) DeleteApplication(ctx context.Context, name string) error {
	return c.request(ctx, http.MethodDelete, c.applicationPath(name, ""), nil, nil)
}

func (c *APIOnlyClient) Manifests(ctx context.Context, name string) ([]unstructured.Unstructured, error) {
	var result struct {
		Manifests []string `json:"manifests"`
	}
	if err := c.request(ctx, http.MethodGet, c.applicationPath(name, "/manifests"), nil, &result); err != nil {
		return nil, err
	}
	manifests := make([]unstructured.Unstructured, 0, len(result.Manifests))
	for _, raw := range result.Manifests {
		var obj unstructured.Unstructured
		if err := json.Unmarshal([]byte(raw), &obj.Object); err != nil {
			return nil, fmt.Errorf("invalid manifest JSON")
		}
		if obj.GetAPIVersion() != "" && obj.GetKind() != "" && obj.GetName() != "" {
			manifests = append(manifests, obj)
		}
	}
	return manifests, nil
}

func (c *APIOnlyClient) GetManifests(name string) ([]unstructured.Unstructured, bool, error) {
	m, err := c.Manifests(context.Background(), name)
	return m, err == nil, err
}
