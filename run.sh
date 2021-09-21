#!/bin/sh

# Don't use `set -e` because `make undeploy` might fail if the operator doesn't exist yet
# set -e

TARGETCONTAINER="${TARGETCONTAINER}"
TARGETNAMESPACE="${TARGETNAMESPACE}"

if [ "${TARGETCONTAINER}" = "" ]; then
  /bin/echo -n "Target container name: "
  read TARGETCONTAINER
  if [ "${TARGETCONTAINER}" = "" ]; then
    echo "Target container name required."
    exit 1
  fi
fi

if [ "${TARGETNAMESPACE}" = "" ]; then
  /bin/echo -n "Target container's namespace': "
  read TARGETNAMESPACE
  if [ "${TARGETNAMESPACE}" = "" ]; then
    echo "Target container's namespace required."
    exit 1
  fi
fi

make undeploy
make docker-build docker-push IMG="kgibm/containerdiagoperator:$(awk '/const OperatorVersion/ { gsub(/"/, ""); print $NF; }' controllers/containerdiagnostic_controller.go)" && \
  make deploy IMG="kgibm/containerdiagoperator:$(awk '/const OperatorVersion/ { gsub(/"/, ""); print $NF; }' controllers/containerdiagnostic_controller.go)" && \
  kubectl get pods --namespace=containerdiagoperator-system && \
  sleep 30 && \
  kubectl get pods --namespace=containerdiagoperator-system && \
  printf '{"apiVersion": "diagnostic.ibm.com/v1", "kind": "ContainerDiagnostic", "metadata": {"name": "%s", "namespace": "%s"}, "spec": {"command": "%s", "arguments": %s, "targetObjects": %s, "steps": %s}}' diag1 containerdiagoperator-system script '[]' "$(printf '[{"kind": "Pod", "name": "%s", "namespace": "%s"}]' "${TARGETCONTAINER}" "${TARGETNAMESPACE}")" '[{"command": "install", "arguments": ["top ps netstat df date echo vmstat sleep dmesg gzip rm"]}, {"command": "execute", "arguments": ["vmstat -tw 1 3"]}, {"command": "uninstall"}]' | kubectl create -f - && \
  sleep 20 && \
  kubectl describe ContainerDiagnostic diag1 --namespace=containerdiagoperator-system && \
  echo "" && \
  kubectl logs --container=manager --namespace=containerdiagoperator-system $(kubectl get pods --namespace=containerdiagoperator-system | awk '/containerdiagoperator/ {print $1;}') && \
  echo "" && \
  kubectl get ContainerDiagnostic diag1 --namespace=containerdiagoperator-system
