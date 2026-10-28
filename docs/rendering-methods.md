# Rendering Methods

Choose a rendering method with the `--render-method` flag.

## 1. CLI (`cli`)

The `cli` method is the most traditional way to generate manifests.

- **How it works:** It executes the `argocd app manifests` command via the Argo CD CLI binary against the ephemeral cluster for every application. 
- **Characteristics:** Highly reliable, because it authenticates and manages connections automatically. However, it is more restrictive, as it will throw the exact same errors users are accustomed to seeing in the Argo CD UI. It is also slower because it must wait for all applications to reach the "OutOfSync" state before it can render the manifests.
- **Note:** The Argo CD CLI binary is **not** included in the Docker image. If you want to use this render method, you must install the `argocd` binary yourself (either by extending the Docker image or by running the `argocd-diff-preview` binary directly in your pipeline).

## 2. Server API (`server-api`) - Default

The `server-api` method is the default rendering method. It improves performance by communicating directly with Argo CD's APIs.

- **How it works:** It communicates directly with the Argo CD API server over persistent gRPC connections to request manifest rendering.
- **Characteristics:** Faster than the CLI method because it does not wait for the Argo CD Application controller reconciliation loop. It also provides more detailed error messages when rendering fails, since it surfaces errors directly from the API server. However, it is slightly more fragile because the tool must manage connections and authentication manually.
- **Lockdown mode:** Compatible with [lockdown mode](reusing-clusters/lockdown-mode.md) (namespace-scoped Argo CD).

## 3. Repo Server API (`repo-server-api`) - 🧪 Experimental

The `repo-server-api` method is an experimental fast-path that bypasses the cluster's reconciliation loop entirely.

- **How it works:** It connects directly to the Argo CD `repo-server` component via gRPC, asking it to generate the manifests synchronously. 
- **Characteristics:** The fastest method available. No cluster-side Application objects are created, and no polling of the reconciliation loop is needed. 
- **Lockdown mode:** Compatible with [lockdown mode](reusing-clusters/lockdown-mode.md) (namespace-scoped Argo CD).

## API only (`server-api-only`) — Prototype

This mode connects to a dedicated Argo CD instance over verified HTTPS. Preview
creates temporary `diff-preview-*` Applications, requests their manifests, then
deletes the Applications with `cascade=false`. It does not use Kubernetes,
port-forwarding, local Helm downloads, or Argo CD repository Secrets.

```sh
# Set ARGOCD_AUTH_TOKEN through the CI secret mechanism.
argocd-diff-preview \
  --render-method=server-api-only \
  --create-cluster=false \
  --argocd-server=https://preview.example.com \
  --argocd-project=preview \
  --argocd-preview-destination=https://discovery.example.com \
  --base-branch=BASE_COMMIT --target-branch=TARGET_COMMIT \
  --repo=example/gitops
```

Use `--argocd-ca-file` for a private CA. HTTP, unverified TLS, and redirects are
not supported. The normal `base-branch` and `target-branch` folders are still
needed to discover Applications. Workload sources must be committed and
accessible to Argo CD; local workload edits are not uploaded. Use immutable
commit IDs to avoid a branch changing during a run.

Configure an API account with these permissions in the Argo CD namespace:

```csv
p, role:preview, applications, get, preview/diff-preview-*, allow
p, role:preview, applications, create, preview/diff-preview-*, allow
p, role:preview, applications, delete, preview/diff-preview-*, allow
g, preview, role:preview
```

ApplicationSet generation also needs `applicationsets, create` for the required
project and ApplicationSet names. The project must permit the source repositories
and the preview destination. The API server needs Kubernetes permissions to
create and delete these Application objects. CI does not need those permissions.

Use a dedicated instance with controllers stopped and no workload-cluster
credentials. The manifests API still queries destination discovery, even when
Helm has explicit Kubernetes capabilities. A mock registration alone is not
enough: its endpoint must serve Kubernetes version and API-resource discovery.
It can reject all resource reads and writes. This client does not provision that
endpoint or change instance permissions.

Limits of this prototype:

- Repository credentials remain inside Argo CD and still reach repo-server.
  This does not isolate the renderer from its credentials or files. Rendered
  output can still disclose files readable by the renderer.
- CI still needs its repository checkout credentials and the limited Argo CD
  API token. Remove old Kubernetes credentials and their retrieval permissions
  separately when adopting this mode.
- App-of-apps traversal is not supported. Raw manifest namespaces are preserved;
  namespace normalization and discovery-based deduplication are not performed.
- Rendering errors report HTTP status only, to avoid copying private file
  contents from error responses into CI logs. Inspect trusted server logs for details.
- Interrupted runs or lost create responses can leave temporary Applications.
  The error reports their names. There is no cleanup of other runs' Applications.
