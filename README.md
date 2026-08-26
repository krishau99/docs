# DocsPage Operator

A Kubernetes operator for managing documentation page deployments using the `DocsPage` custom resource. The operator supports two modes:

- **`build`**: Clone a git repository, substitute variables, build with Zensical, and serve with Apache.
- **`prebuilt`**: Deploy a pre-existing documentation image directly.

The operator is designed for **airgapped environments** with private container registries and custom CA certificates. It **self-installs its CRD on startup** — no separate Helm CRD install step needed.

---

## Table of Contents

- [What the Operator Does](#what-the-operator-does)
- [CRD Reference](#crd-reference)
  - [Mode: build](#mode-build)
  - [Mode: prebuilt](#mode-prebuilt)
  - [Status Fields](#status-fields)
- [Deploying the Operator](#deploying-the-operator)
  - [Airgapped Environments](#airgapped-environments)
- [TLS / CA Certificates](#tls--ca-certificates)
- [Variable Substitution](#variable-substitution)
- [Git Polling](#git-polling)
- [CRD Self-Installation](#crd-self-installation)
- [Flux / GitOps Integration](#flux--gitops-integration)
- [RBAC](#rbac)

---

## What the Operator Does

The DocsPage operator watches `DocsPage` resources across all namespaces and manages the lifecycle of documentation deployments:

1. **Creates/updates a `Deployment`** with the appropriate containers for the configured mode.
2. **Creates/updates a `Service`** (ClusterIP) to expose the deployment.
3. **Polls Gitea** for new commit SHAs on the configured branch (build mode only).
4. **Triggers rolling restarts** when a new commit is detected by updating a pod template annotation.
5. **Reports status** on the `DocsPage` resource including current SHA, last sync time, and readiness conditions.

---

## CRD Reference

### Mode: `build`

Clones a git repository, substitutes variables, builds with Zensical, and serves the output with Apache.

```yaml
apiVersion: docspage.io/v1alpha1
kind: DocsPage
metadata:
  name: docs-page-a
  namespace: docs
spec:
  mode: build

  # Git repository to clone
  repo:
    url: https://gitea.internal/org/docs-a
    branch: main
    credentialsSecret: gitea-deploy-key  # Secret with keys: username, password

  # How often to check for new commits
  pollInterval: 5m

  # Variables substituted in all .md, .toml, .html files via envsubst
  variables:
    DOMAIN: docs-a.internal.example.com
    BASE_URL: https://docs-a.internal.example.com

  # Additional substitution variables
  extraSubstitutions:
    CUSTOM_VAR_1: value1
    CUSTOM_VAR_2: value2

  # Registry URL prefix for all images (git, zensical builder, apache)
  registry:
    url: registry.internal

  # Custom CA certificate for TLS verification (git clone, registry pulls)
  tls:
    caSecret: custom-ca-cert  # Secret with key: ca.crt

  # Serving configuration
  serving:
    replicas: 1
    port: 8080

  # Optional: override default container images used in build mode
  # images:
  #   git: alpine/git:latest
  #   zensical: zensical/zensical:latest
  #   apache: httpd:2.4-alpine
```

**How build mode works:**

The generated Deployment has three containers:

1. **Init container `git-clone`** (`alpine/git:latest` or prefixed with `registry.url`):
   - Clones the repository to `/workspace`
   - Uses `GIT_USERNAME` / `GIT_PASSWORD` env vars from `credentialsSecret`
   - Sets `GIT_SSL_CAINFO` to the mounted CA certificate

2. **Init container `zensical-build`** (`zensical/zensical:latest` or prefixed):
   - Runs `envsubst` on all `.md`, `.toml`, and `.html` files in `/workspace`
   - All `variables` and `extraSubstitutions` are available as environment variables
   - Runs `zensical build` (default output directory is `./site`, i.e. `/workspace/site`)
   - Moves the built files from `/workspace/site` to `/output`

3. **Main container `apache`** (`httpd:2.4-alpine` or prefixed):
   - Serves the built documentation from `/output` via Apache

### Mode: `prebuilt`

Deploys a pre-existing documentation image directly without any build steps.

```yaml
apiVersion: docspage.io/v1alpha1
kind: DocsPage
metadata:
  name: legacy-docs
  namespace: docs
spec:
  mode: prebuilt

  # The pre-built documentation image
  image: registry.internal/docs-legacy:latest

  # Optional: if image doesn't start with a registry host, prepend this
  registry:
    url: registry.internal

  # Custom CA certificate
  tls:
    caSecret: custom-ca-cert

  serving:
    replicas: 1
    port: 8080
```

### Status Fields

```yaml
status:
  currentSHA: "abc123def456"       # Current deployed git SHA (build mode only)
  lastSyncTime: "2026-04-14T10:00:00Z"
  ready: true
  conditions:
    - type: Ready
      status: "True"
      lastTransitionTime: "2026-04-14T10:00:00Z"
      reason: DeploymentReady
      message: "Documentation is being served"
```

---

## Deploying the Operator

### 1. Apply the deployment manifests

```bash
kubectl apply -f deploy/namespace.yaml
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/deployment.yaml
```

The operator will automatically install the `DocsPage` CRD on startup.

### 2. Create a DocsPage resource

```bash
kubectl apply -f examples/build-docspage.yaml
# or
kubectl apply -f examples/prebuilt-docspage.yaml
```

### 3. Check status

```bash
kubectl get docspages -A
kubectl describe docspage docs-page-a -n docs
```

### Airgapped Environments

In airgapped environments:

1. **Mirror the operator image** to your private registry:
   ```bash
   docker pull ghcr.io/3bab5a2843705818/docs-operator:latest
   docker tag ghcr.io/3bab5a2843705818/docs-operator:latest registry.internal/docs-operator:latest
   docker push registry.internal/docs-operator:latest
   ```

2. **Mirror dependent images** (`alpine/git`, `zensical/builder`, `httpd:2.4-alpine`) to your registry.

3. **Update `deploy/deployment.yaml`** to reference your registry.

4. **Set `spec.registry.url`** in your `DocsPage` resources so the operator prefixes all image references.

5. **Mount your CA certificate** into the operator pod and pass `--ca-cert-file /path/to/ca.crt` (or set `CA_CERT_FILE` env var).

---

## TLS / CA Certificates

### Operator-level CA certificate

For the operator itself (e.g., to talk to Gitea for git polling), provide a CA cert via:

- `--ca-cert-file /etc/ssl/certs/custom-ca.crt` flag, or
- `CA_CERT_FILE=/etc/ssl/certs/custom-ca.crt` environment variable

Mount the certificate from a Secret in the operator's Deployment (see `deploy/deployment.yaml` for an example with the `caSecret` commented section).

### Per-DocsPage CA certificate

Set `spec.tls.caSecret` to a Secret containing your CA certificate:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: custom-ca-cert
  namespace: docs
type: Opaque
data:
  ca.crt: <base64-encoded-PEM>
```

The CA certificate is mounted at `/etc/ssl/certs/custom-ca.crt` in all containers that need TLS (git clone, zensical build, and Apache main container). The `SSL_CERT_FILE` and `GIT_SSL_CAINFO` environment variables are set automatically.

---

## Variable Substitution

Variable substitution uses standard `envsubst` syntax: `${VARIABLE_NAME}`.

1. Define variables in `spec.variables` and/or `spec.extraSubstitutions`:
   ```yaml
   spec:
     variables:
       DOMAIN: docs.internal.example.com
       BASE_URL: https://docs.internal.example.com
     extraSubstitutions:
       LOGO_URL: https://docs.internal.example.com/logo.png
   ```

2. Use them in your documentation files and `zensical.toml`:
   ```toml
   # zensical.toml
   [site]
   base_url = "${BASE_URL}"
   title = "Documentation"
   ```
   ```markdown
   # My Docs

   Visit us at ${DOMAIN}.
   ```

3. During the build init container, all `.md`, `.toml`, and `.html` files are processed by `envsubst`.

---

## Git Polling

The operator polls for new commits on a configurable interval (`spec.pollInterval`, default `5m`).

**How it works:**

1. The operator calls the git smart HTTP protocol endpoint (`/info/refs?service=git-upload-pack`) to get the latest commit SHA without doing a full clone.
2. If that fails, it falls back to the Gitea REST API (`/api/v1/repos/{owner}/{repo}/branches/{branch}`).
3. If the latest SHA differs from `status.currentSHA`, the operator updates the pod template annotation `docspage/restartedAt` and `docspage/currentSHA`, triggering a rolling restart.
4. The new SHA is stored in `status.currentSHA`.

**Credentials:** The same `spec.repo.credentialsSecret` used for cloning is also used for the polling HTTP requests (HTTP Basic Auth).

---

## CRD Self-Installation

The operator embeds the CRD YAML directly into its binary using Go's `embed` package:

```go
//go:embed docspage-crd.yaml
var docsPageCRDYAML []byte
```

On startup, `main.go` calls `installCRDs()` which:
1. Checks if the `docspages.docspage.io` CRD already exists.
2. If not, creates it.
3. If it exists, updates it to ensure it matches the embedded definition.

This means you don't need a separate CRD installation step — just deploy the operator and it handles everything.

---

## Flux / GitOps Integration

Use Flux's `HelmRelease` with the [raw chart](https://github.com/deliverybot/helm-charts/tree/master/charts/raw) to manage `DocsPage` resources via GitOps:

```yaml
apiVersion: helm.toolkit.fluxcd.io/v2beta1
kind: HelmRelease
metadata:
  name: docs-page-a
  namespace: flux-system
spec:
  interval: 1h
  chart:
    spec:
      chart: raw
      sourceRef:
        kind: HelmRepository
        name: deliverybot
  values:
    resources:
      - apiVersion: docspage.io/v1alpha1
        kind: DocsPage
        metadata:
          name: docs-page-a
          namespace: docs
        spec:
          mode: build
          repo:
            url: https://gitea.internal/org/docs-a
            branch: main
            credentialsSecret: gitea-deploy-key
          pollInterval: 5m
          variables:
            DOMAIN: docs-a.internal.example.com
            BASE_URL: https://docs-a.internal.example.com
          registry:
            url: registry.internal
          tls:
            caSecret: custom-ca-cert
          serving:
            replicas: 1
            port: 8080
```

Each documentation site becomes a simple `HelmRelease` with its own values — Flux manages the lifecycle, and the DocsPage operator handles the actual deployment.

---

## RBAC

The operator requires these permissions (see `deploy/rbac.yaml`):

| Resource | Verbs |
|---|---|
| `docspages` (docspage.io) | get, list, watch, create, update, patch, delete |
| `docspages/status` | get, update, patch |
| `docspages/finalizers` | update |
| `deployments` (apps) | get, list, watch, create, update, patch, delete |
| `services` | get, list, watch, create, update, patch, delete |
| `secrets` | get, list, watch |
| `customresourcedefinitions` (apiextensions) | get, list, watch, create, update, patch |
