# Local development environment

`dev/kind-dev-deploy.sh` creates a minimal single-node kind cluster, builds the
three images a build-mode `DocsPage` needs, loads them, and runs the DocsPage
operator. Nothing else is installed — no git server, no ingress controller, no
certificates. Pods reach the internet through the node, so a `DocsPage` can
clone straight from GitHub.

## Images

| Image | Built from | Used as |
|---|---|---|
| `localhost/docspage-operator:local` | `Dockerfile` | the operator Deployment |
| `localhost/docspage-zensical:local` | `images/zensical-builder` | `spec.images.zensical` |
| `localhost/docspage-httpd:local` | `images/httpd` | `spec.serving.image` |

The last two are built here rather than left to the reader because without them
a build-mode DocsPage cannot start: the upstream `zensical/zensical` image has
no `envsubst`, which the operator's build step requires, and the operator has no
default serving image by design. Both are loaded before the script finishes, so
`dev/manifests/docspage-sample.yaml` can be applied as-is.

They are tagged `:local` deliberately. A `:latest` tag would flip the default
pull policy to `Always` and the kubelet would ignore the loaded copy and try to
pull from a registry.

## Prerequisites

`podman` (or `docker`), `kind`, `kubectl`.

With Podman, the script exports `KIND_EXPERIMENTAL_PROVIDER=podman` for you.
On macOS the podman machine has to be running first (`podman machine start`).
Rootless Podman on Linux additionally needs cgroup v2 with `cpu` delegation;
see [kind's rootless notes](https://kind.sigs.k8s.io/docs/user/rootless/). The
script checks both and says which one is wrong rather than letting kind fail
with an opaque error.

## Quickstart

```bash
./dev/kind-dev-deploy.sh                 # create cluster, build images, deploy
./dev/kind-dev-deploy.sh --skip-build    # redeploy without rebuilding
./dev/kind-dev-deploy.sh --recreate      # start from a clean cluster
./dev/kind-dev-deploy.sh --engine docker # if you are not on Podman
kind delete cluster --name docspage      # tear down
```

The script stops once the operator is up and has self-installed its CRD.
Creating a `DocsPage` is a separate, deliberate step:

```bash
cp dev/manifests/docspage-sample.yaml /tmp/docspage.yaml
$EDITOR /tmp/docspage.yaml     # set spec.repo.url to your docs repo
kubectl apply -f /tmp/docspage.yaml

kubectl get docspage -A
kubectl logs -n docs deploy/docspage-operator -f
```

## Notes

Images are loaded through a `docker-archive` rather than
`kind load docker-image`, because that path behaves identically under Podman
and Docker.

The httpd image pulls `registry.access.redhat.com/ubi10/httpd-24`, which needs
no credentials but does need outbound network at build time. Both image
directories take `BASE_REGISTRY`, `BASE_IMAGE` and `BASE_TAG` build arguments if
you need to point them at a mirror.

Against GitHub, the operator's git smart-HTTP polling works, but its Gitea API
fallback will not — that is fine, the fallback only runs if the primary path
fails, and a 404 from GitHub there is expected in the logs.

Ingress and a custom CA are not set up. Both need decisions that are easier to
make later: `extraPortMappings` cannot be added to an existing kind cluster, so
enabling ingress means editing `dev/kind-config.yaml` and recreating.
