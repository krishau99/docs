# Images

Two images the operator needs but does not ship.

| Directory | Purpose | Referenced by |
|---|---|---|
| `zensical-builder/` | The upstream zensical image plus GNU gettext, so the build init container has `envsubst` | `spec.images.zensical` |
| `httpd/` | `ubi10/httpd-24` with CA trust added, serving `/var/www/html` on 8080 | `spec.serving.image` |

`dev/kind-dev-deploy.sh` builds both and loads them into the kind cluster, so
the local environment has them before any DocsPage is created. For anything
else, build and push them yourself:

```bash
podman build -t registry.internal/docspage-zensical:local images/zensical-builder
podman build -t registry.internal/docspage-httpd:local   images/httpd
```

Both take `BASE_REGISTRY`, `BASE_IMAGE` and `BASE_TAG` build arguments so the
base can be pulled from a mirror in an airgapped build:

```bash
podman build --build-arg BASE_REGISTRY=registry.internal -t ... images/httpd
```

## Why the builder image exists

The operator's `zensical-build` init container runs `envsubst` over the clone
before building. `envsubst` comes from GNU gettext, and the upstream
`zensical/zensical` image is Alpine with no gettext, so that container exits 127
with `sh: envsubst: not found` and the pod never leaves `Init:Error`. Until the
operator stops depending on `envsubst`, every build-mode DocsPage needs
`spec.images.zensical` pointed at an image like this one.

## Why the serving image exists

Less urgently: `ubi10/httpd-24` already listens on 8080 as uid 1001 and serves
`/var/www/html`, so it works as `spec.serving.image` untouched. This image adds
the CA trust bundle and the `httpd-cfg` change from the production
Containerfile, so what the kind environment serves with matches what production
serves with.
