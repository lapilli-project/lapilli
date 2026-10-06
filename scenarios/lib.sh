# Sourced by every scenario script.
#
# These scripts create and delete namespaces, and on one of them change files on the nodes. They
# therefore run against one cluster only — the kind cluster made from kind.yaml — through a kubeconfig
# that is named explicitly. The machine they were written on had a production cluster as the current
# context of ~/.kube/config; a bare `kubectl apply` there would have gone to it.

SCENARIO_CLUSTER=rcabench

if [ -z "${KUBECONFIG:-}" ]; then
  echo "refusing to run: KUBECONFIG is not set, and the default kubeconfig is never used. Create the cluster with" >&2
  echo "  kind create cluster --name $SCENARIO_CLUSTER --config scenarios/kind.yaml --kubeconfig <file>" >&2
  echo "and export KUBECONFIG=<file>." >&2
  exit 64
fi
case "$KUBECONFIG" in
  *:*) echo "refusing to run: KUBECONFIG must name one file, not a list" >&2; exit 64 ;;
esac
context=$(command kubectl --kubeconfig "$KUBECONFIG" config current-context 2>/dev/null || true)
if [ "$context" != "kind-$SCENARIO_CLUSTER" ]; then
  echo "refusing to run: the current context of $KUBECONFIG is '${context:-none}', not kind-$SCENARIO_CLUSTER" >&2
  exit 64
fi

# Every kubectl below this line is pinned to that file and that context, whatever the caller's shell says.
kubectl() { command kubectl --kubeconfig "$KUBECONFIG" --context "kind-$SCENARIO_CLUSTER" "$@"; }
