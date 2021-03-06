# containerdiagoperator

The goal of this operator is to automate running diagnostics on a container without restarting the container. This works by uploading diagnostic binaries (e.g. `top`) and their dependent shared libraries into a temporary folder in the container and then executing them. This is all packaged into an operator for ease-of-use. Note that [Kubernetes requires the existence of `tar` in the running container for uploading files to it](https://github.com/kubernetes/kubernetes/issues/58512) and the container must have a writable directory (whether ephemeral or attached storage).

While we welcome any [bug reports or suggestions](https://github.com/kgibm/containerdiagoperator/issues/new), this is not supported and is provided on an "as-is" basis without warranty of any kind.

### ContainerDiagnostic custom resource

#### Example execution

##### YAML Example (top -H)

```
apiVersion: diagnostic.ibm.com/v1
kind: ContainerDiagnostic
metadata:
  name: example1
spec:
  command: script
  targetLabelSelectors:
  - matchLabels:
      app: liberty1
  steps:
  - command: install
    arguments:
    - top
  - command: execute
    arguments:
    - top -b -H -d 5 -n 2
  - command: clean
```

##### YAML Example (Liberty linperf.sh)

```
apiVersion: diagnostic.ibm.com/v1
kind: ContainerDiagnostic
metadata:
  name: example2
spec:
  command: script
  targetLabelSelectors:
  - matchLabels:
      app: liberty1
  steps:
  - command: install
    arguments:
    - linperf.sh
  - command: execute
    arguments:
    - linperf.sh
  - command: package
    arguments:
    - linperf_RESULTS.tar.gz
    - /output/javacore*
    - /logs/
    - /config/
  - command: clean
    arguments:
    - /output/javacore*
```

#### Showing ContainerDiagnostic resources

Get:

```
$ kubectl get ContainerDiagnostic diag1 --namespace=containerdiagoperator-system
NAME    COMMAND   STATUSMESSAGE   RESULT                                 DOWNLOAD
diag1   script    success         Successfully finished on 1 container   kubectl cp containerdiagoperator-controller-manager-6bbc6b4644-pk4s6:/tmp/containerdiagoutput/containerdiag_20211013_185845_tmp1687502014560622238.zip containerdiag_20211013_185845_tmp1687502014560622238.zip --container=manager --namespace=containerdiagoperator-system
```

Describe:

```
$ kubectl describe ContainerDiagnostic diag1 --namespace=containerdiagoperator-system
[...]
Status:
  Download:            kubectl cp containerdiagoperator-controller-manager-6bbc6b4644-pk4s6:/tmp/containerdiagoutput/containerdiag_20211013_185845_tmp1687502014560622238.zip containerdiag_20211013_185845_tmp1687502014560622238.zip --container=manager --namespace=containerdiagoperator-system
  Download Container:  manager
  Download File Name:  containerdiag_20211013_185845_tmp1687502014560622238.zip
  Download Namespace:  containerdiagoperator-system
  Download Path:       /tmp/containerdiagoutput/containerdiag_20211013_185845_tmp1687502014560622238.zip
  Download Pod:        containerdiagoperator-controller-manager-6bbc6b4644-pk4s6
  Log:                 
  Result:              Successfully finished on 1 container
  Status Code:         2
  Status Message:      success
Events:
  Type    Reason         Age    From                 Message
  ----    ------         ----   ----                 -------
  Normal  Informational  2m27s  containerdiagnostic  Status update (processing): Started processing with version 0.185.20211013 @ 2021-10-13T18:58:16.413
  Normal  Informational  118s   containerdiagnostic  Download: kubectl cp containerdiagoperator-controller-manager-6bbc6b4644-pk4s6:/tmp/containerdiagoutput/containerdiag_20211013_185845_tmp1687502014560622238.zip containerdiag_20211013_185845_tmp1687502014560622238.zip --container=manager --namespace=containerdiagoperator-system
  Normal  Informational  118s   containerdiagnostic  Status update (success): Successfully finished on 1 container @ 2021-10-13T18:58:45.775
```

#### Deleting ContainerDiagnostic resources

```
kubectl delete ContainerDiagnostic diag1 --namespace=containerdiagoperator-system
```

### ContainerDiagnostic

Describe the API resource:

```
$ kubectl explain ContainerDiagnostic
KIND:     ContainerDiagnostic
VERSION:  diagnostic.ibm.com/v1

DESCRIPTION:
     ContainerDiagnostic is the Schema for the containerdiagnostics API

FIELDS:
[...]
   spec	<Object>
     ContainerDiagnosticSpec defines the desired state of ContainerDiagnostic

   status	<Object>
     ContainerDiagnosticStatus defines the observed state of ContainerDiagnostic
```

Describe the spec:

```
$ kubectl explain ContainerDiagnostic.spec       
KIND:     ContainerDiagnostic
VERSION:  diagnostic.ibm.com/v1

RESOURCE: spec <Object>

DESCRIPTION:
     ContainerDiagnosticSpec defines the desired state of ContainerDiagnostic

DESCRIPTION:
     ContainerDiagnosticSpec defines the desired state of ContainerDiagnostic

FIELDS:
   arguments	<[]string>
     Optional. Arguments for the specified Command.

   command	<string>
     Command is one of: version, script

   directory	<string>
     Optional. Target directory for diagnostic files. Must end in trailing
     slash. Defaults to /tmp/containerdiag/.

   steps	<[]Object>
     A list of steps to perform for the specified Command.

   targetLabelSelectors	<[]Object>
     Optional. A list of LabelSelectors. See
     https://kubernetes.io/docs/reference/kubernetes-api/common-definitions/label-selector/

   targetObjects	<[]Object>
     Optional. A list of ObjectReferences. See
     https://kubernetes.io/docs/reference/kubernetes-api/common-definitions/object-reference/

   useuuid	<boolean>
     Optional. Whether or not to use a unique identifier in the directory name
     of each execution. Defaults to true.
```

## Development

Built with [Operator SDK](https://sdk.operatorframework.io/docs/building-operators/golang/quickstart/). The main operator controller code is in [containerdiagnostic_controller.go](https://github.com/kgibm/containerdiagoperator/blob/main/controllers/containerdiagnostic_controller.go).

### Local Build

1. Installation pre-requisities:
    1. [git](https://git-scm.com/downloads)
    1. [go](https://golang.org/dl/)
    1. [operator-sdk](https://sdk.operatorframework.io/docs/installation/)
1. Update the version in `controllers/containerdiagnostic_controller.go`. For example (e.g. set `YYYYMMDD` to the current date, and increment `X` for each build):
   ```
   const OperatorVersion = "0.X.YYYYMMDD"
   ```
1. If you updated `api/v1/*_types.go`, then:
   ```
   make generate
   ```
    1. [General guidelines](https://sdk.operatorframework.io/docs/building-operators/golang/tutorial/#define-the-api)
    1. [kubebuilder validation](https://book.kubebuilder.io/reference/markers/crd-validation.html)
1. If you updated [controller RBAC manifests](https://sdk.operatorframework.io/docs/building-operators/golang/tutorial/#specify-permissions-and-generate-rbac-manifests), then:
   ```
   make manifests
   ```
1. If you want to build without pushing:
   ```
   make build
   ```
1. The default build engine is `podman`. If using `docker`:
   ```
   export CONTAINER_ENGINE=docker
   ```
1. If pushing to the [Quay.io registry at quay.io/kgibm/containerdiagoperator](https://quay.io/repository/kgibm/containerdiagoperator):
    1. If using `podman`:
       ```
       podman login quay.io
       ```
    1. If using `docker`:
       ```
       docker login quay.io
       ```
    1. Build and push:
       ```
       make docker-build docker-push IMG="quay.io/kgibm/containerdiagoperator:$(awk '/const OperatorVersion/ { gsub(/"/, ""); print $NF; }' controllers/containerdiagnostic_controller.go)"
       ```
1. Deploy to the [currently configured cluster](https://publib.boulder.ibm.com/httpserv/cookbook/Containers-Kubernetes.html#Containers-Kubernetes-kubectl-Cluster_Context). For example:
   ```
   make deploy IMG="quay.io/kgibm/containerdiagoperator:$(awk '/const OperatorVersion/ { gsub(/"/, ""); print $NF; }' controllers/containerdiagnostic_controller.go)"
   ```
1. List operator pods:
   ```
   $ kubectl get pods --namespace=containerdiagoperator-system
   NAME                                                       READY   STATUS    RESTARTS   AGE
   containerdiagoperator-controller-manager-5c65d5b66-zc4v4   2/2     Running   0          22s
   ```
1. Show operator logs:
   ```
   $ kubectl logs --container=manager --namespace=containerdiagoperator-system $(kubectl get pods --namespace=containerdiagoperator-system | awk '/containerdiagoperator/ {print $1;}')
   2021-06-23T16:40:15.930Z	INFO	controller-runtime.metrics	metrics server is starting to listen	{"addr": "127.0.0.1:8080"}
   2021-06-23T16:40:15.931Z	INFO	setup	starting manager 0.4.20210803
   ```

#### Uninstall local build

```
make undeploy
```

### Build Operator

This will install into your [current namespace](https://publib.boulder.ibm.com/httpserv/cookbook/Containers-Kubernetes.html#Containers-Kubernetes-Namespace-Change_current_namespace) which can be anything.

1. If you've done a local build above, first `make undeploy` it.
1. `export VERSION="$(awk '/const OperatorVersion/ { gsub(/"/, ""); print $NF; }' controllers/containerdiagnostic_controller.go)"`
1. `export IMG=quay.io/kgibm/containerdiagoperator:${VERSION}`
1. `export BUNDLE_IMG=quay.io/kgibm/containerdiagoperatorbundle:$VERSION`
1. `make build`
1. `make docker-build docker-push`
1. `make bundle`
1. `make bundle-build bundle-push`
1. `operator-sdk bundle validate $BUNDLE_IMG`
1. `operator-sdk run bundle $BUNDLE_IMG`

#### Uninstall Operator 

1. Delete all CRs
1. OpenShift web console } Operators } Installed Operators } Container Diagnostic Operator } Actions } Uninstall
1. `kubectl delete catalogsources.operators.coreos.com containerdiagoperator-catalog`

### Notes

* Education:
    * [Building Operators](https://book.kubebuilder.io/introduction.html)
    * [Using the client](https://sdk.operatorframework.io/docs/building-operators/golang/references/client/)
* APIs:
    * [Pod](https://pkg.go.dev/k8s.io/api/core/v1#Pod)
    * [Container](https://pkg.go.dev/k8s.io/api/core/v1#Container)
    * [Clientset](https://pkg.go.dev/k8s.io/client-go/kubernetes)
    * [ctrl.Manager](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/manager#Manager)
    * [Logger](https://pkg.go.dev/github.com/go-logr/logr)
        * "This package restricts the logging API to just 2 types of logs: info and error."
        * "To write log lines that are more verbose, Logger has a V() method. The higher the V-level of a log line, the less critical it is considered. Log-lines with V-levels that are not enabled (as per the LogSink) will not be written. Level V(0) is the default, and logger.V(0).Info() has the same meaning as logger.Info(). Negative V-levels have the same meaning as V(0)."
        * If `V()` is omitted, that's equivalent to `V(0)`.
        * Fine debug: `logger.V(1).Info(fmt.Sprintf(""))`
        * Finer debug: `logger.V(2).Info(fmt.Sprintf(""))`
        * The default log level is `V(1)` and below.
* Add Go module dependency (example): `GO111MODULE=on go get github.com/...`
* On OpenShift, if runng into https://github.com/operator-framework/operator-sdk/issues/4684 then change `gcr.io/kubebuilder/kube-rbac-proxy:v0.8.0` to `registry.redhat.io/openshift4/ose-kube-rbac-proxy:v4.7` in `manager_auth_proxy_patch.yaml`
  ```
  sed -i.bak 's/gcr.io\/kubebuilder\/kube-rbac-proxy:v0.8.0/registry.redhat.io\/openshift4\/ose-kube-rbac-proxy:v4.7/g' config/default/manager_auth_proxy_patch.yaml && rm config/default/manager_auth_proxy_patch.yaml.bak
  ```
* Development:
    * Remote into a running manager container:
      ```
      kubectl exec $(kubectl get pods --namespace=containerdiagoperator-system | awk '/containerdiagoperator/ {print $1;}') --namespace containerdiagoperator-system --container=manager -it -- sh
      ```

## Appendix

##### YAML Example (top -H) (targetObjects)

```
apiVersion: diagnostic.ibm.com/v1
kind: ContainerDiagnostic
metadata:
  name: example
spec:
  command: script
  targetObjects:
  - kind: Pod
    name: $PODNAME
    namespace: $PODNAMESPACE
  steps:
  - command: install
    arguments:
    - top
  - command: execute
    arguments:
    - top -b -H -d 5 -n 2
  - command: clean
```

### JSON Example (top -H)

`printf '{"apiVersion": "diagnostic.ibm.com/v1", "kind": "ContainerDiagnostic", "metadata": {"name": "%s", "namespace": "%s"}, "spec": {"command": "%s", "arguments": %s, "targetObjects": %s, "steps": %s}}' diag1 containerdiagoperator-system script '[]' '[{"kind": "Pod", "name": "liberty1-774c5fccc6-f7mjt", "namespace": "testns1"}]' '[{"command": "install", "arguments": ["top"]}, {"command": "execute", "arguments": ["top -b -H -d 5 -n 6"]}, {"command": "clean"}]' | kubectl create -f -`

### JSON Example (Liberty linperf.sh)

`printf '{"apiVersion": "diagnostic.ibm.com/v1", "kind": "ContainerDiagnostic", "metadata": {"name": "%s", "namespace": "%s"}, "spec": {"command": "%s", "arguments": %s, "targetObjects": %s, "steps": %s}}' diag1 containerdiagoperator-system script '[]' '[{"kind": "Pod", "name": "liberty1-774c5fccc6-f7mjt", "namespace": "testns1"}]' '[{"command": "install", "arguments": ["linperf.sh"]}, {"command": "execute", "arguments": ["linperf.sh"]}, {"command": "package", "arguments": ["/output/javacore*", "/logs/", "/config/"]} , {"command": "clean", "arguments": ["/output/javacore*"]}]' | kubectl create -f -`

### Using podman

Most commonly with `podman machine start` or [CodeReady Containers](https://developers.redhat.com/products/codeready-containers/overview) with `crc start --cpus 4 --memory 12000 && eval $(crc podman-env)`

### IBM Cloud Container Registry

Listing images:

```
$ ibmcloud cr image-list
Listing images...

Repository                                   Tag              Digest         Namespace       Created          Size     Security status   
icr.io/containerdiag/containerdiagoperator   0.177.20211012   4a45b147a880   containerdiag   11 minutes ago   188 MB   Scanning...   
```

### Building to other registries

1. If pushing to the [DockerHub registry at docker.io/kgibm/containerdiagoperator](https://hub.docker.com/r/kgibm/containerdiagoperator):
    1. If using `podman`:
       ```
       podman login docker.io
       ```
    1. If using `docker`:
       ```
       docker login
       ```
    1. Build and push:
       ```
       make docker-build docker-push IMG="docker.io/kgibm/containerdiagoperator:$(awk '/const OperatorVersion/ { gsub(/"/, ""); print $NF; }' controllers/containerdiagnostic_controller.go)"
       ```
1. If pushing to the IBM Container Registry at `icr.io/containerdiag/containerdiagoperator`:
    1. Log into IBM Cloud with [`ibmcloud`](https://cloud.ibm.com/docs/cli?topic=cli-getting-started) in the `us-east` region:
       ```
       ibmcloud login --sso -r us-east
       ```
    1. If using `podman`, ensure `docker` doesn't exist and then create a symlink to `podman`:
       ```
       ln -s $(which podman) $(dirname $(which podman))/docker
       ```
    1. Log into the IBM Container Registry:
       ```
       ibmcloud cr login
       ```
    1. Build and push:
       ```
       make docker-build docker-push IMG="icr.io/containerdiag/containerdiagoperator:$(awk '/const OperatorVersion/ { gsub(/"/, ""); print $NF; }' controllers/containerdiagnostic_controller.go)"
       ```
