# containerdiagoperator

ContainerDiagnostic CRD and diagnostic controller

## Development

### Update Spec

1. Update `api/v1/*_types.go`
    1. [General guidelines](https://sdk.operatorframework.io/docs/building-operators/golang/tutorial/#define-the-api)
    1. [kubebuilder validation](https://book.kubebuilder.io/reference/markers/crd-validation.html)
1. `make generate`

### Build and Deploy

Built with [Operator SDK](https://sdk.operatorframework.io/docs/building-operators/golang/quickstart/).

1. Increment version in `controllers/containerdiagnostic_controller.go`:
   ```
   const OPERATOR_VERSION = "0.4.20210803"
   ```
1. If you updated `api/v1/*_types.go`, then:
   ```
   make generate
   ```
1. If needed, log into DockerHub:
   ```
   docker login
   ```
1. Build and push to [DockerHub](https://hub.docker.com/r/kgibm/containerdiagoperator) (increment version):
   ```
   make docker-build docker-push IMG="kgibm/containerdiagoperator:0.4.20210803"
   ```
1. Deploy to the [currently configured cluster](https://publib.boulder.ibm.com/httpserv/cookbook/Containers-Kubernetes.html#Containers-Kubernetes-kubectl-Cluster_Context):
   ```
   make deploy IMG="kgibm/containerdiagoperator:0.4.20210803"
   ```
1. List operator pods:
   ```
   $ kubectl get pods --namespace=containerdiagoperator-system
   NAME                                                       READY   STATUS    RESTARTS   AGE
   containerdiagoperator-controller-manager-5c65d5b66-zc4v4   2/2     Running   0          22s
   ```
1. Show operator logs:
   ```
   $ kubectl logs --container=manager --namespace=containerdiagoperator-system containerdiagoperator-controller-manager-5c65d5b66-zc4v4
   2021-06-23T16:40:15.930Z	INFO	controller-runtime.metrics	metrics server is starting to listen	{"addr": "127.0.0.1:8080"}
   2021-06-23T16:40:15.931Z	INFO	setup	starting manager 0.4.20210803
   ```

To destroy the CRD and all CRs:

```
make undeploy
```

### Create ContainerDiagnostic

#### Test using version command

Create:

```
printf '{"apiVersion": "diagnostic.ibm.com/v1", "kind": "ContainerDiagnostic", "metadata": {"name": "%s", "namespace": "%s"}, "spec": {"command": "%s", "arguments": %s}}' diag1 testns1 version '[]' | kubectl create -f -
```

Describe:

```
$ kubectl describe ContainerDiagnostic diag1 --namespace=testns1
[...]
Spec:
  Command:  version
Status:
  Log:             
  Result:          Version 0.12.20210803
  Status Code:     0
  Status Message:  success
Events:            <none>
```

Get:

```
$ kubectl get ContainerDiagnostic diag1 --namespace=testns1
NAME    STARTED   COMMAND   ARGUMENTS   RESULT                  STATUSCODE   STATUSMESSAGE
diag1   7m24s     version               Version 0.12.20210803   0            success
```

Delete:

```
kubectl delete ContainerDiagnostic diag1 --namespace=testns1
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
[..]
   spec	<Object>
     ContainerDiagnosticSpec defines the desired state of ContainerDiagnostic

   status	<map[string]>
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

FIELDS:
   command	<string>
     Command is one of: version
```
