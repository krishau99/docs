# Local development environment

`dev/kind-dev-deploy.sh` creates a minimal single-node kind cluster and runs
the DocsPage operator in it. Nothing else is installed — no git server, no
ingress controller, no certificates. Pods reach the internet through the node,
so a `DocsPage` can clone straight from GitHub.

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
./dev/kind-dev-deploy.sh                 # create cluster, build, deploy
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

The image is loaded through a `docker-archive` rather than
`kind load docker-image`, because that path behaves identically under Podman
and Docker.

Against GitHub, the operator's git smart-HTTP polling works, but its Gitea API
fallback will not — that is fine, the fallback only runs if the primary path
fails, and a 404 from GitHub there is expected in the logs.

Ingress and a custom CA are not set up. Both need decisions that are easier to
make later: `extraPortMappings` cannot be added to an existing kind cluster, so
enabling ingress means editing `dev/kind-config.yaml` and recreating.
