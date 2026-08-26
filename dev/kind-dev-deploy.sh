#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

CLUSTER_NAME="${KIND_CLUSTER_NAME:-docspage}"
OPERATOR_NAMESPACE="${DOCSPAGE_OPERATOR_NAMESPACE:-docs}"
OPERATOR_IMAGE="${DOCSPAGE_OPERATOR_IMAGE:-localhost/docspage-operator:local}"
ENGINE="${DOCSPAGE_CONTAINER_ENGINE:-podman}"

SKIP_BUILD=false
RECREATE=false

usage() {
  cat <<EOF
Usage: $0 [options]

Creates or reuses a minimal single-node kind cluster, builds the DocsPage
operator image, loads it into the cluster, and applies deploy/.

Nothing else is installed. Pods reach the internet through the node, so a
DocsPage can point straight at a GitHub repository.

Options:
  --cluster-name NAME   kind cluster name       (default: ${CLUSTER_NAME})
  --image IMAGE         operator image tag      (default: ${OPERATOR_IMAGE})
  --engine ENGINE       podman or docker        (default: ${ENGINE})
  --skip-build          reuse an already-built image
  --recreate            delete the cluster first
  -h, --help            show this message

Environment overrides:
  KIND_CLUSTER_NAME            default: ${CLUSTER_NAME}
  DOCSPAGE_OPERATOR_IMAGE      default: ${OPERATOR_IMAGE}
  DOCSPAGE_OPERATOR_NAMESPACE  default: ${OPERATOR_NAMESPACE}
  DOCSPAGE_CONTAINER_ENGINE    default: ${ENGINE}

Tear down with:
  kind delete cluster --name ${CLUSTER_NAME}
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --cluster-name) CLUSTER_NAME="$2"; shift 2 ;;
    --image)        OPERATOR_IMAGE="$2"; shift 2 ;;
    --engine)       ENGINE="$2"; shift 2 ;;
    --skip-build)   SKIP_BUILD=true; shift ;;
    --recreate)     RECREATE=true; shift ;;
    -h|--help)      usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

require_command "${ENGINE}"
require_command kind
require_command kubectl

if [[ "${ENGINE}" == "podman" ]]; then
  export KIND_EXPERIMENTAL_PROVIDER=podman
fi

# ---------------------------------------------------------------------------
# Preflight
#
# kind's rootless Podman support is documented for Linux hosts. On macOS the
# podman CLI is a remote client talking to a Linux VM, so the checks that
# matter differ; both cases are detected here rather than assumed, because the
# failure mode otherwise is an opaque "exit status 125" from kind.
# ---------------------------------------------------------------------------

echo "==> Checking ${ENGINE}"
if ! "${ENGINE}" info >/dev/null 2>&1; then
  echo "${ENGINE} is installed but not responding." >&2
  if [[ "${ENGINE}" == "podman" && "$(uname -s)" == "Darwin" ]]; then
    echo "On macOS the podman machine has to be running. Try:" >&2
    echo "  podman machine start" >&2
  fi
  exit 1
fi

if [[ "${ENGINE}" == "podman" ]]; then
  CGROUP_VERSION="$("${ENGINE}" info --format '{{.Host.CgroupsVersion}}' 2>/dev/null || echo unknown)"
  if [[ "${CGROUP_VERSION}" != "v2" ]]; then
    echo "warning: podman reports cgroups ${CGROUP_VERSION}; kind needs cgroup v2" >&2
  fi

  # Rootless Podman's default log relay can connect too slowly for kind's
  # startup detection. See https://kind.sigs.k8s.io/docs/user/rootless/
  ROOTLESS="$("${ENGINE}" info --format '{{.Host.Security.Rootless}}' 2>/dev/null || echo unknown)"
  if [[ "${ROOTLESS}" == "true" ]]; then
    echo "    rootless podman detected"
    echo "    if cluster creation hangs, set the k8s-file log driver in"
    echo "    ~/.config/containers/containers.conf and retry"
  fi
fi

# ---------------------------------------------------------------------------
# Cluster
# ---------------------------------------------------------------------------

if [[ "${RECREATE}" == "true" ]]; then
  echo "==> Deleting cluster ${CLUSTER_NAME}"
  kind delete cluster --name "${CLUSTER_NAME}"
fi

echo "==> Using kind cluster: ${CLUSTER_NAME}"
if ! kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  kind create cluster --name "${CLUSTER_NAME}" --config "${REPO_ROOT}/dev/kind-config.yaml"
fi

kubectl config use-context "kind-${CLUSTER_NAME}" >/dev/null
kubectl wait --for=condition=Ready node --all --timeout=3m

# ---------------------------------------------------------------------------
# Operator image
# ---------------------------------------------------------------------------

if [[ "${SKIP_BUILD}" == "false" ]]; then
  echo "==> Building ${OPERATOR_IMAGE}"
  "${ENGINE}" build -t "${OPERATOR_IMAGE}" "${REPO_ROOT}"
fi

echo "==> Loading ${OPERATOR_IMAGE} into kind"
# Going through a docker-format archive rather than `kind load docker-image`:
# it is the path that behaves the same under both engines, and it does not
# depend on kind being able to reach the image store directly.
ARCHIVE_DIR="$(mktemp -d)"
trap 'rm -rf "${ARCHIVE_DIR}"' EXIT
"${ENGINE}" save --format docker-archive -o "${ARCHIVE_DIR}/operator.tar" "${OPERATOR_IMAGE}"
kind load image-archive "${ARCHIVE_DIR}/operator.tar" --name "${CLUSTER_NAME}"
rm -rf "${ARCHIVE_DIR}"
trap - EXIT

# ---------------------------------------------------------------------------
# Operator
# ---------------------------------------------------------------------------

echo "==> Applying deploy/"
kubectl apply -f "${REPO_ROOT}/deploy/namespace.yaml"
kubectl apply -f "${REPO_ROOT}/deploy/rbac.yaml"
kubectl apply -f "${REPO_ROOT}/deploy/deployment.yaml"

kubectl set image deployment/docspage-operator \
  -n "${OPERATOR_NAMESPACE}" operator="${OPERATOR_IMAGE}"

kubectl rollout restart deployment/docspage-operator -n "${OPERATOR_NAMESPACE}"
kubectl rollout status deployment/docspage-operator -n "${OPERATOR_NAMESPACE}" --timeout=3m

# The operator installs its own CRD on startup, so this also confirms it came
# up with working RBAC rather than crash-looping quietly.
echo "==> Waiting for the operator to self-install its CRD"
kubectl wait --for=condition=Established crd/docspages.docspage.io --timeout=2m

echo
kubectl get deployment,pods -n "${OPERATOR_NAMESPACE}"
echo
echo "Operator is running. Point a DocsPage at a GitHub repository with:"
echo
echo "  cp dev/manifests/docspage-sample.yaml /tmp/docspage.yaml"
echo "  \$EDITOR /tmp/docspage.yaml     # set spec.repo.url"
echo "  kubectl apply -f /tmp/docspage.yaml"
echo
echo "Then watch what happens:"
echo "  kubectl get docspage -A"
echo "  kubectl logs -n ${OPERATOR_NAMESPACE} deploy/docspage-operator -f"
echo
echo "Tear down with: kind delete cluster --name ${CLUSTER_NAME}"
