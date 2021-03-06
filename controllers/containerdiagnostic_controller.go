/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/go-logr/logr"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	diagnosticv1 "github.com/kgibm/containerdiagoperator/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/tools/remotecommand"

	"math/rand"
	"path/filepath"
	"strconv"

	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"encoding/json"
)

const OperatorVersion = "0.247.20211115"

// Setting this to false doesn't work because of errors such as:
//   symbol lookup error: .../lib64/libc.so.6: undefined symbol: _dl_catch_error_ptr, version GLIBC_PRIVATE
// This is because the ld-linux in the image may not match what the binaries need (e.g. specific glibc),
// So we need to use the ld-linux used by the containerdiagsmall image (see GetExecutionCommand).
// Thus we have to launch with an explicit call to ld-linux.
// See https://www.kernel.org/doc/man-pages/online/pages/man8/ld-linux.so.8.html
const UseLdLinuxDirect = true

const ResultProcessing = "Processing..."

const FinalizerName = "diagnostic.ibm.com/finalizer"

type StatusEnum int

const (
	StatusUninitialized StatusEnum = iota
	StatusProcessing
	StatusSuccess
	StatusError
	StatusMixed
)

var StatusEnumNames = []string{
	"uninitialized",
	"processing",
	"success",
	"error",
	"mixed",
}

func (se StatusEnum) ToString() string {
	return StatusEnumNames[se]
}

func (se StatusEnum) Value() int {
	return int(se)
}

// ContainerDiagnosticReconciler reconciles a ContainerDiagnostic object
type ContainerDiagnosticReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Config        *rest.Config
	EventRecorder record.EventRecorder
}

type ContextTracker struct {
	visited                 int
	successes               int
	localPermanentDirectory string
}

type CustomLogger struct {
	logger     logr.Logger
	outputFile *os.File
	buffer     string
}

func (l *CustomLogger) Info(str string) {
	l.logger.Info(str)
	l.AppendToLocalFile(str)
}

func (l *CustomLogger) Error(err error, str string) {
	l.logger.Error(err, str)
	l.AppendToLocalFile(str)
	l.AppendToLocalFile(fmt.Sprintf("Error: %+v", err))
}

func (l *CustomLogger) Debug1(str string) {
	l.logger.V(1).Info(str)
	l.AppendToLocalFile(str)
}

func (l *CustomLogger) Debug2(str string) {
	l.logger.V(2).Info(str)
	l.AppendToLocalFile(str)
}

func (l *CustomLogger) Debug3(str string) {
	l.logger.V(3).Info(str)
	l.AppendToLocalFile(str)
}

func (l *CustomLogger) OpenLocalFile(fileName string) error {
	outputFile, err := os.OpenFile(fileName, os.O_CREATE|os.O_WRONLY, os.ModePerm)

	if err == nil {

		l.outputFile = outputFile
		if len(l.buffer) > 0 {
			l.outputFile.WriteString(l.buffer)
			l.outputFile.Sync()
			l.buffer = ""
		}
	}

	return err
}

func (l *CustomLogger) CloseLocalFile() {
	if l.outputFile != nil {
		l.outputFile.Close()
		l.outputFile = nil
	}
}

func (l *CustomLogger) AppendToLocalFile(str string) {
	t := "[" + CurrentTimeAsString() + "] "
	if l.outputFile != nil {
		l.outputFile.WriteString(t + str + "\n")
		l.outputFile.Sync()
	} else {
		l.buffer += t + str + "\n"
	}
}

// +kubebuilder:rbac:groups=diagnostic.ibm.com,resources=containerdiagnostics,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=diagnostic.ibm.com,resources=containerdiagnostics/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=diagnostic.ibm.com,resources=containerdiagnostics/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get
// +kubebuilder:rbac:groups=core,resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// Compare the state specified by
// the ContainerDiagnostic object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *ContainerDiagnosticReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	logger := &CustomLogger{logger: log.FromContext(ctx)}

	logger.Info(fmt.Sprintf("ContainerDiagnosticReconciler Reconcile called, version: %s", OperatorVersion))

	containerDiagnostic := &diagnosticv1.ContainerDiagnostic{}
	err := r.Get(ctx, req.NamespacedName, containerDiagnostic)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			logger.Info("ContainerDiagnostic resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		logger.Error(err, "Failed to get ContainerDiagnostic")
		return ctrl.Result{}, err
	}

	logger.Info(fmt.Sprintf("Started reconciling ContainerDiagnostic name: %s, namespace: %s, command: %s, status: %s @ %s", containerDiagnostic.Name, containerDiagnostic.Namespace, containerDiagnostic.Spec.Command, StatusEnum(containerDiagnostic.Status.StatusCode).ToString(), CurrentTimeAsString()))

	logger.Info(fmt.Sprintf("Details of the ContainerDiagnostic: %+v", containerDiagnostic))

	// Check if we are finalizing
	isMarkedToBeDeleted := containerDiagnostic.GetDeletionTimestamp() != nil
	if isMarkedToBeDeleted {
		logger.Info(fmt.Sprintf("Marked to be deleted"))
		if controllerutil.ContainsFinalizer(containerDiagnostic, FinalizerName) {
			// Run finalization logic. If the
			// finalization logic fails, don't remove the finalizer so
			// that we can retry during the next reconciliation.
			if err := r.Finalize(logger, containerDiagnostic); err != nil {
				return ctrl.Result{}, err
			}

			// Remove finalizer. Once all finalizers have been
			// removed, the object will be deleted.
			controllerutil.RemoveFinalizer(containerDiagnostic, FinalizerName)
			err := r.Update(ctx, containerDiagnostic)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer for this CR
	if !controllerutil.ContainsFinalizer(containerDiagnostic, FinalizerName) {
		logger.Info(fmt.Sprintf("Adding finalizer"))
		controllerutil.AddFinalizer(containerDiagnostic, FinalizerName)
		err = r.Update(ctx, containerDiagnostic)
		if err != nil {
			logger.Info(fmt.Sprintf("Failed to add finalizer: %+v", err))
			return ctrl.Result{}, err
		} else {
			logger.Info(fmt.Sprintf("Added finalizer"))
			return ctrl.Result{}, nil
		}
	}

	logger.Info(fmt.Sprintf("Started normal processing"))

	// This is just a marker status
	containerDiagnostic.Status.Result = ResultProcessing

	var result ctrl.Result = ctrl.Result{}
	err = nil

	if containerDiagnostic.Status.StatusCode == StatusUninitialized.Value() {

		// We make a quick transition from uninitialized to processing just so
		// that we can show a processing status in the get
		r.SetStatus(StatusProcessing, fmt.Sprintf("Started processing. Operator version %s", OperatorVersion), containerDiagnostic, logger)

	} else if containerDiagnostic.Status.StatusCode == StatusProcessing.Value() {

		switch containerDiagnostic.Spec.Command {
		case "version":
			result, err = r.CommandVersion(ctx, req, containerDiagnostic, logger)
		case "script":
			result, err = r.CommandScript(ctx, req, containerDiagnostic, logger)
		}

	}

	return r.ProcessResult(result, err, ctx, containerDiagnostic, logger)
}

func (r *ContainerDiagnosticReconciler) SetStatus(status StatusEnum, message string, containerDiagnostic *diagnosticv1.ContainerDiagnostic, logger *CustomLogger) {
	r.RecordEventInfo(fmt.Sprintf("Status update (%s): %s @ %s", status.ToString(), message, CurrentTimeAsString()), containerDiagnostic, logger)
	if IsInitialStatus(containerDiagnostic) {
		containerDiagnostic.Status.StatusCode = int(status)
		containerDiagnostic.Status.StatusMessage = status.ToString()
		containerDiagnostic.Status.Result = message
	} else {
		containerDiagnostic.Status.StatusCode = int(StatusMixed)
		containerDiagnostic.Status.StatusMessage = StatusMixed.ToString()
		containerDiagnostic.Status.Result = "Mixed results; describe and review Events"
	}
}

func (r *ContainerDiagnosticReconciler) Finalize(logger *CustomLogger, containerDiagnostic *diagnosticv1.ContainerDiagnostic) error {

	// If the download file still exists, then delete it
	if len(containerDiagnostic.Status.DownloadPath) > 0 {
		err := os.Remove(containerDiagnostic.Status.DownloadPath)
		if err == nil {
			logger.Info(fmt.Sprintf("Successfully deleted %s", containerDiagnostic.Status.DownloadPath))
		} else {
			// We don't even bother to return this error though
			logger.Info(fmt.Sprintf("Failed to delete %s: %v", containerDiagnostic.Status.DownloadPath, err))
		}
	}

	r.RecordEventInfo(fmt.Sprintf("Finalized and deleted @ %s", CurrentTimeAsString()), containerDiagnostic, logger)

	logger.Info("Successfully finalized")
	return nil
}

func IsInitialStatus(containerDiagnostic *diagnosticv1.ContainerDiagnostic) bool {
	if strings.HasPrefix(containerDiagnostic.Status.Result, ResultProcessing) {
		return true
	} else {
		return false
	}
}

func CurrentTimeAsString() string {
	return time.Now().Format("2006-01-02T15:04:05.000")
}

func (r *ContainerDiagnosticReconciler) ProcessResult(result ctrl.Result, err error, ctx context.Context, containerDiagnostic *diagnosticv1.ContainerDiagnostic, logger *CustomLogger) (ctrl.Result, error) {
	if err == nil {
		logger.Info(fmt.Sprintf("Finished reconciling @ %s", CurrentTimeAsString()))
	} else {
		r.SetStatus(StatusError, fmt.Sprintf("Error: %s", err.Error()), containerDiagnostic, logger)
		r.RecordEventWarning(err, fmt.Sprintf("Finished reconciling with error %v @ %s", err, CurrentTimeAsString()), containerDiagnostic, logger)
	}

	if !strings.HasPrefix(containerDiagnostic.Status.Result, ResultProcessing) {
		statusErr := r.Status().Update(ctx, containerDiagnostic)
		if statusErr != nil {
			logger.Error(statusErr, fmt.Sprintf("Failed to update ContainerDiagnostic status: %v", statusErr))
			if err == nil {
				return ctrl.Result{}, statusErr
			} else {
				// If we're already processing an error, don't override that
				// with the status update error
			}
		}
	}

	return result, err
}

func (r *ContainerDiagnosticReconciler) RecordEventInfo(message string, containerDiagnostic *diagnosticv1.ContainerDiagnostic, logger *CustomLogger) {
	logger.Info(message)

	// https://pkg.go.dev/k8s.io/client-go/tools/record#EventRecorder
	r.EventRecorder.Event(containerDiagnostic, corev1.EventTypeNormal, "Informational", message)
}

func (r *ContainerDiagnosticReconciler) RecordEventWarning(err error, message string, containerDiagnostic *diagnosticv1.ContainerDiagnostic, logger *CustomLogger) {
	logger.Error(err, message)

	// k8s only has normal and warning event types
	// https://pkg.go.dev/k8s.io/client-go/tools/record#EventRecorder
	r.EventRecorder.Event(containerDiagnostic, corev1.EventTypeWarning, "Warning", message)
}

func (r *ContainerDiagnosticReconciler) CommandVersion(ctx context.Context, req ctrl.Request, containerDiagnostic *diagnosticv1.ContainerDiagnostic, logger *CustomLogger) (ctrl.Result, error) {
	logger.Info("Processing command: version")

	r.SetStatus(StatusSuccess, fmt.Sprintf("Version %s", OperatorVersion), containerDiagnostic, logger)

	return ctrl.Result{}, nil
}

func (r *ContainerDiagnosticReconciler) CommandScript(ctx context.Context, req ctrl.Request, containerDiagnostic *diagnosticv1.ContainerDiagnostic, logger *CustomLogger) (ctrl.Result, error) {
	logger.Info("Processing command: script")

	if len(containerDiagnostic.Spec.Steps) == 0 {
		r.SetStatus(StatusError, fmt.Sprintf("You must specify an array of steps to perform for the script command"), containerDiagnostic, logger)
		return ctrl.Result{}, nil
	}

	// Create a permanent directory for this run
	// user is 'nobody' so /tmp is really the only place
	uuid := GetUniqueIdentifier()
	localPermanentDirectory := filepath.Join("/tmp/containerdiagoutput", uuid)
	err := os.MkdirAll(localPermanentDirectory, os.ModePerm)
	if err != nil {
		r.SetStatus(StatusError, fmt.Sprintf("Could not create local permanent output space in %s: %+v", localPermanentDirectory, err), containerDiagnostic, logger)
		return ctrl.Result{}, err
	}

	localPermanentDirectoryCluster := filepath.Join(localPermanentDirectory, "cluster")
	err = os.MkdirAll(localPermanentDirectoryCluster, os.ModePerm)
	if err != nil {
		r.SetStatus(StatusError, fmt.Sprintf("Could not create local permanent output space in %s: %+v", localPermanentDirectoryCluster, err), containerDiagnostic, logger)
		return ctrl.Result{}, err
	}

	err = logger.OpenLocalFile(filepath.Join(localPermanentDirectoryCluster, "trace.txt"))
	if err != nil {
		r.SetStatus(StatusError, fmt.Sprintf("Could not create local debug file in %s: %+v", localPermanentDirectoryCluster, err), containerDiagnostic, logger)
		return ctrl.Result{}, err
	}

	contextTracker := ContextTracker{localPermanentDirectory: localPermanentDirectory}

	if containerDiagnostic.Spec.TargetObjects == nil && containerDiagnostic.Spec.TargetLabelSelectors == nil {
		r.SetStatus(StatusError, fmt.Sprintf("You must specify targetLabelSelectors and/or targetObjects to target a set of pods"), containerDiagnostic, logger)
		return ctrl.Result{}, nil
	}

	if containerDiagnostic.Spec.TargetObjects != nil {
		for _, targetObject := range containerDiagnostic.Spec.TargetObjects {

			logger.Info(fmt.Sprintf("targetObject: %+v", targetObject))

			pod := &corev1.Pod{}
			err := r.Get(ctx, client.ObjectKey{
				Namespace: targetObject.Namespace,
				Name:      targetObject.Name,
			}, pod)

			if err == nil {
				logger.Debug1(fmt.Sprintf("found pod: %+v", pod))
				r.RunScriptOnPod(ctx, req, containerDiagnostic, logger, pod, &contextTracker)
			} else {
				if k8serrors.IsNotFound(err) {
					r.SetStatus(StatusError, fmt.Sprintf("Pod not found: name: %s namespace: %s", targetObject.Name, targetObject.Namespace), containerDiagnostic, logger)
				} else {
					logger.Error(err, "Failed to get targetObject")
					return ctrl.Result{}, err
				}
			}
		}
	}

	if containerDiagnostic.Spec.TargetLabelSelectors != nil {
		clientset, err := kubernetes.NewForConfig(r.Config)
		if err != nil {
			r.SetStatus(StatusError, fmt.Sprintf("Could not create client: %+v", err), containerDiagnostic, logger)
			return ctrl.Result{}, err
		}

		for _, targetSelector := range containerDiagnostic.Spec.TargetLabelSelectors {
			logger.Info(fmt.Sprintf("targetSelector: %+v", targetSelector))

			selector := metav1.FormatLabelSelector(&targetSelector)

			if err != nil {
				r.SetStatus(StatusError, fmt.Sprintf("Could not process LabelSelector: %+v", err), containerDiagnostic, logger)
				return ctrl.Result{}, err
			}

			// https://github.com/kubernetes/client-go/blob/master/kubernetes/typed/core/v1/pod.go#L43
			allpods, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{LabelSelector: selector})

			if err != nil {
				r.SetStatus(StatusError, fmt.Sprintf("Could not list all pods: %+v", err), containerDiagnostic, logger)
				return ctrl.Result{}, err
			}

			for _, pod := range allpods.Items {
				r.RunScriptOnPod(ctx, req, containerDiagnostic, logger, &pod, &contextTracker)
			}
		}
	}

	if IsInitialStatus(containerDiagnostic) && contextTracker.visited == 0 {
		r.SetStatus(StatusError, fmt.Sprintf("The specified targetLabelSelectors and/or targetObjects did not evaluate to any pods"), containerDiagnostic, logger)
		return ctrl.Result{}, nil
	}

	logger.Info("CommandScript: walking " + localPermanentDirectory)

	checkForCompressedFiles := true

	for checkForCompressedFiles {
		// Walk our perm dir and uncompress anything in place
		var filesToUncompress []string = []string{}

		err = filepath.Walk(localPermanentDirectory,
			func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if strings.HasSuffix(info.Name(), "zip") || strings.HasSuffix(info.Name(), "tar") || strings.HasSuffix(info.Name(), "tar.gz") {
					// path is the absolute path since we call Walk with an absolute path
					// https://pkg.go.dev/path/filepath#WalkFunc
					filesToUncompress = append(filesToUncompress, path)
				}
				return nil
			})
		if err != nil {
			r.SetStatus(StatusError, fmt.Sprintf("Could not walk local permanent output space in %s: %+v", localPermanentDirectory, err), containerDiagnostic, logger)
			return ctrl.Result{}, err
		}

		if len(filesToUncompress) == 0 {
			break
		}

		logger.Info(fmt.Sprintf("CommandScript: uncompressing files: %+v", filesToUncompress))

		for _, fileToUncompress := range filesToUncompress {
			logger.Info(fmt.Sprintf("Uncompressing %s", fileToUncompress))

			var command string
			var arguments []string

			if strings.HasSuffix(fileToUncompress, "zip") {
				command = "unzip"
				arguments = []string{fileToUncompress, "-d", filepath.Dir(fileToUncompress)}
			} else if strings.HasSuffix(fileToUncompress, "tar") {
				command = "tar"
				arguments = []string{"-C", filepath.Dir(fileToUncompress), "-xvf", fileToUncompress}
			} else if strings.HasSuffix(fileToUncompress, "tar.gz") {
				command = "tar"
				arguments = []string{"-C", filepath.Dir(fileToUncompress), "-xzvf", fileToUncompress}
			}

			outputBytes, err := r.ExecuteLocalCommand(logger, containerDiagnostic, command, arguments...)
			var outputStr string = string(outputBytes[:])
			if err != nil {
				r.SetStatus(StatusError, fmt.Sprintf("Could not uncompress %s: %+v %s", fileToUncompress, err, outputStr), containerDiagnostic, logger)
				return ctrl.Result{}, err
			}

			logger.Debug1(fmt.Sprintf("CommandScript uncompress output: %v", outputStr))

			os.Remove(fileToUncompress)
		}
	}

	logger.Info("CommandScript: Finished pre-processing zip for download.")

	logger.CloseLocalFile()

	// Manager container name
	hostnameBytes, err := ioutil.ReadFile("/etc/hostname")
	if err != nil {
		r.SetStatus(StatusError, fmt.Sprintf("Could not read /etc/hostname: %+v", err), containerDiagnostic, logger)
		return ctrl.Result{}, err
	}

	managerPodName := string(hostnameBytes)
	managerPodName = strings.ReplaceAll(managerPodName, "\n", "")
	managerPodName = strings.ReplaceAll(managerPodName, "\r", "")

	logger.Info("CommandScript: processing all pods")

	// Find the namespace of the manager container pod
	clientset, err := kubernetes.NewForConfig(r.Config)
	if err != nil {
		r.SetStatus(StatusError, fmt.Sprintf("Could not create client: %+v", err), containerDiagnostic, logger)
		return ctrl.Result{}, err
	}

	// https://github.com/kubernetes/client-go/blob/master/kubernetes/typed/core/v1/pod.go#L43
	allpods, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})

	if err != nil {
		r.SetStatus(StatusError, fmt.Sprintf("Could not list all pods: %+v", err), containerDiagnostic, logger)
		return ctrl.Result{}, err
	}

	// We're searching for our manager pod namespace, but while we're at it, let's note
	// down all pods and their details

	podsFile, err := os.OpenFile(filepath.Join(localPermanentDirectoryCluster, "pods.txt"), os.O_CREATE|os.O_WRONLY, os.ModePerm)
	if err != nil {
		r.SetStatus(StatusError, fmt.Sprintf("Could not create pods file in %s: %+v", localPermanentDirectoryCluster, err), containerDiagnostic, logger)
		return ctrl.Result{}, err
	}

	for podIndex, pod := range allpods.Items {
		jsonBytes, err := json.MarshalIndent(pod, "  ", "  ")
		if err != nil {
			r.SetStatus(StatusError, fmt.Sprintf("Could not generate JSON for %s: %+v", pod.Name, err), containerDiagnostic, logger)
			return ctrl.Result{}, err
		}
		podsFile.WriteString(fmt.Sprintf("Pod %d:\n%s\n", (podIndex + 1), string(jsonBytes)))
		if pod.Name == managerPodName {
			containerDiagnostic.Status.DownloadNamespace = pod.Namespace
		}
	}

	podsFile.Close()

	logger.Info(fmt.Sprintf("manager pod namespace: %s", containerDiagnostic.Status.DownloadNamespace))

	logger.Info("CommandScript: creating final zip")

	// Finally, zip up the files for final user download
	finalZip := filepath.Join("/tmp/containerdiagoutput", fmt.Sprintf("containerdiag_%s_%s.zip", time.Now().Format("20060102_150405"), uuid))
	outputBytes, err := r.ExecuteLocalCommand(logger, containerDiagnostic, "sh", "-c", fmt.Sprintf("cd %s; zip -r %s .", localPermanentDirectory, finalZip))
	var outputStr string = string(outputBytes[:])
	if err != nil {
		r.SetStatus(StatusError, fmt.Sprintf("Could not zip %s: %+v %s", finalZip, err, outputStr), containerDiagnostic, logger)
		return ctrl.Result{}, err
	}

	logger.Debug1(fmt.Sprintf("CommandScript zip output: %v", outputStr))

	// Now that we've created the zip, we can delete the actual directory to save space
	os.RemoveAll(localPermanentDirectory)

	containerDiagnostic.Status.DownloadPath = finalZip
	containerDiagnostic.Status.DownloadFileName = filepath.Base(finalZip)
	containerDiagnostic.Status.DownloadPod = managerPodName
	containerDiagnostic.Status.DownloadContainer = "manager"

	if containerDiagnostic.Status.DownloadNamespace == "" {
		containerDiagnostic.Status.Download = fmt.Sprintf("kubectl cp %s:%s %s --container=%s", containerDiagnostic.Status.DownloadPod, containerDiagnostic.Status.DownloadPath, containerDiagnostic.Status.DownloadFileName, containerDiagnostic.Status.DownloadContainer)
	} else {
		containerDiagnostic.Status.Download = fmt.Sprintf("kubectl cp %s:%s %s --container=%s --namespace=%s", containerDiagnostic.Status.DownloadPod, containerDiagnostic.Status.DownloadPath, containerDiagnostic.Status.DownloadFileName, containerDiagnostic.Status.DownloadContainer, containerDiagnostic.Status.DownloadNamespace)
	}

	r.RecordEventInfo(fmt.Sprintf("Download: %s", containerDiagnostic.Status.Download), containerDiagnostic, logger)

	if contextTracker.visited > 0 {
		if contextTracker.successes > 0 {
			var containerText string
			if contextTracker.successes == 1 {
				containerText = "container"
			} else {
				containerText = "containers"
			}

			r.SetStatus(StatusSuccess, fmt.Sprintf("Successfully finished on %d %s", contextTracker.successes, containerText), containerDiagnostic, logger)
		}
	} else {
		// If none were visited and there's already an error, then just leave that as probably a pod wasn't found
		if IsInitialStatus(containerDiagnostic) {
			r.SetStatus(StatusError, "No pods/containers specified", containerDiagnostic, logger)
		}
	}

	return ctrl.Result{}, nil
}

func (r *ContainerDiagnosticReconciler) RunScriptOnPod(ctx context.Context, req ctrl.Request, containerDiagnostic *diagnosticv1.ContainerDiagnostic, logger *CustomLogger, pod *corev1.Pod, contextTracker *ContextTracker) {
	logger.Info(fmt.Sprintf("RunScriptOnPod containers: %d", len(pod.Spec.Containers)))
	for _, container := range pod.Spec.Containers {
		logger.Info(fmt.Sprintf("RunScriptOnPod container: %+v", container))
		r.RunScriptOnContainer(ctx, req, containerDiagnostic, logger, pod, container, contextTracker)
	}
}

func GetUniqueIdentifier() string {
	// We don't use a UUID because it contains letters and
	// that may accidentally contain a command such as "df"
	// which is then incorrectly replaced again when building
	// some ld-linux command execution lines
	// import "github.com/google/uuid"
	// uuid.New().String()

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	return "tmp" + strconv.FormatInt(r.Int63(), 10)
}

func (r *ContainerDiagnosticReconciler) RunScriptOnContainer(ctx context.Context, req ctrl.Request, containerDiagnostic *diagnosticv1.ContainerDiagnostic, logger *CustomLogger, pod *corev1.Pod, container corev1.Container, contextTracker *ContextTracker) {
	logger.Info(fmt.Sprintf("RunScriptOnContainer pod: %s, container: %s", pod.Name, container.Name))

	contextTracker.visited++

	uuid := GetUniqueIdentifier()

	logger.Info(fmt.Sprintf("RunScriptOnContainer UUID = %s", uuid))

	// First create a local scratchspace
	localScratchSpaceDirectory := filepath.Join("/tmp/", uuid)
	err := os.MkdirAll(localScratchSpaceDirectory, os.ModePerm)
	if err != nil {
		r.SetStatus(StatusError, fmt.Sprintf("Could not create local scratchspace in %s: %+v", localScratchSpaceDirectory, err), containerDiagnostic, logger)

		// We don't stop processing other pods/containers, just return. If this is the
		// only error, status will show as error; otherwise, as mixed
		return
	}

	logger.Info(fmt.Sprintf("RunScriptOnContainer Created local scratch space: %s", localScratchSpaceDirectory))

	containerTmpFilesPrefix, ok := r.EnsureDirectoriesOnContainer(ctx, req, containerDiagnostic, logger, pod, container, contextTracker, uuid)
	if !ok {
		// The error will have been logged within the above function.
		// We don't stop processing other pods/containers, just return. If this is the
		// only error, status will show as error; otherwise, as mixed
		Cleanup(logger, localScratchSpaceDirectory)
		return
	}

	// Now loop through the steps to figure out all the files we'll need to upload
	localTarFile := filepath.Join(localScratchSpaceDirectory, "files.tar")
	remoteFilesToPackage := make(map[string]bool)
	remoteFilesToClean := make(map[string]bool)
	remoteFilesToClean[containerTmpFilesPrefix] = true

	// Include some commonly useful Linux files, especially related to containers
	remoteFilesToPackage["/proc/cpuinfo"] = true
	remoteFilesToPackage["/proc/meminfo"] = true
	remoteFilesToPackage["/proc/version"] = true
	remoteFilesToPackage["/proc/loadavg"] = true

	remoteFilesToPackage["/etc/hostname"] = true
	remoteFilesToPackage["/etc/os-release"] = true
	remoteFilesToPackage["/etc/system-release"] = true
	remoteFilesToPackage["/etc/system-release-cpe"] = true
	remoteFilesToPackage["/etc/redhat-release"] = true

	remoteFilesToPackage["/proc/pressure/cpu"] = true
	remoteFilesToPackage["/proc/pressure/memory"] = true
	remoteFilesToPackage["/proc/pressure/io"] = true
	remoteFilesToPackage["/sys/fs/cgroup/cpuset/cpuset.memory_pressure"] = true
	remoteFilesToPackage["/sys/fs/cgroup/cpu.pressure"] = true
	remoteFilesToPackage["/sys/fs/cgroup/memory.pressure"] = true
	remoteFilesToPackage["/sys/fs/cgroup/io.pressure"] = true

	remoteFilesToPackage["/proc/sys/kernel/core_pattern"] = true
	remoteFilesToPackage["/proc/sys/vm/swappiness"] = true

	remoteFilesToPackage["/sys/fs/cgroup/cpu/cpu.cfs_period_us"] = true
	remoteFilesToPackage["/sys/fs/cgroup/cpu/cpu.cfs_quota_us"] = true
	remoteFilesToPackage["/sys/fs/cgroup/cpu/cpu.shares"] = true
	remoteFilesToPackage["/sys/fs/cgroup/cpu/cpu.stat"] = true

	remoteFilesToPackage["/sys/fs/cgroup/cpuacct/cpuacct.stat"] = true
	remoteFilesToPackage["/sys/fs/cgroup/cpuacct/cpuacct.usage"] = true
	remoteFilesToPackage["/sys/fs/cgroup/cpuacct/cpuacct.usage_all"] = true
	remoteFilesToPackage["/sys/fs/cgroup/cpuacct/cpuacct.usage_percpu"] = true
	remoteFilesToPackage["/sys/fs/cgroup/cpuacct/cpuacct.usage_percpu_sys"] = true
	remoteFilesToPackage["/sys/fs/cgroup/cpuacct/cpuacct.usage_percpu_user"] = true
	remoteFilesToPackage["/sys/fs/cgroup/cpuacct/cpuacct.usage_sys"] = true
	remoteFilesToPackage["/sys/fs/cgroup/cpuacct/cpuacct.usage_user"] = true

	remoteFilesToPackage["/sys/fs/cgroup/cpuset/cpuset.cpus"] = true
	remoteFilesToPackage["/sys/fs/cgroup/cpuset/cpuset.effective_cpus"] = true

	remoteFilesToPackage["/sys/fs/cgroup/memory/memory.failcnt"] = true
	remoteFilesToPackage["/sys/fs/cgroup/memory/memory.kmem.failcnt"] = true
	remoteFilesToPackage["/sys/fs/cgroup/memory/memory.kmem.limit_in_bytes"] = true
	remoteFilesToPackage["/sys/fs/cgroup/memory/memory.kmem.max_usage_in_bytes"] = true
	remoteFilesToPackage["/sys/fs/cgroup/memory/memory.kmem.tcp.failcnt"] = true
	remoteFilesToPackage["/sys/fs/cgroup/memory/memory.kmem.tcp.limit_in_bytes"] = true
	remoteFilesToPackage["/sys/fs/cgroup/memory/memory.kmem.tcp.max_usage_in_bytes"] = true
	remoteFilesToPackage["/sys/fs/cgroup/memory/memory.kmem.tcp.usage_in_bytes"] = true
	remoteFilesToPackage["/sys/fs/cgroup/memory/memory.limit_in_bytes"] = true
	remoteFilesToPackage["/sys/fs/cgroup/memory/memory.max_usage_in_bytes"] = true
	remoteFilesToPackage["/sys/fs/cgroup/memory/memory.soft_limit_in_bytes"] = true
	remoteFilesToPackage["/sys/fs/cgroup/memory/memory.stat"] = true
	remoteFilesToPackage["/sys/fs/cgroup/memory/memory.swappiness"] = true
	remoteFilesToPackage["/sys/fs/cgroup/memory/memory.usage_in_bytes"] = true

	filesToTar := make(map[string]bool)

	// "--dereference" not needed because we tar up the symlink targets too
	var tarArguments []string = []string{"-cv", "-f", localTarFile}

	// First add in some basic commands that we'll always need
	for _, command := range []string{
		"/usr/bin/cp",
		"/usr/bin/date",
		"/usr/bin/echo",
		"/usr/bin/pgrep",
		"/usr/bin/pwd",
		"/usr/bin/tee",
		"/usr/bin/rm",
		"/usr/bin/sleep",
		"/usr/bin/zip",
		"/usr/bin/df",
		"/usr/bin/awk",
		"/usr/bin/ldd",
		"/usr/bin/bash",
		"/usr/bin/sh",
		"/usr/bin/kill",
		"/usr/bin/pkill",
		"/usr/bin/ls",
	} {
		ok := r.ProcessInstallCommand(command, filesToTar, containerDiagnostic, logger)
		if !ok {
			// The error will have been logged within the above function.
			// We don't stop processing other pods/containers, just return. If this is the
			// only error, status will show as error; otherwise, as mixed
			Cleanup(logger, localScratchSpaceDirectory)
			return
		}
	}

	// Now add in any commands that the user has specified
	for _, step := range containerDiagnostic.Spec.Steps {
		if step.Command == "install" {
			for _, commandLine := range step.Arguments {
				for _, command := range strings.Split(commandLine, " ") {

					// Specifically known and pre-packaged scripts
					if command == "linperf.sh" {
						// Add prereqs that might not be installed above
						for _, command := range []string{
							"/usr/bin/whoami",
							"/usr/bin/netstat",
							"/usr/bin/top",
							"/usr/bin/expr",
							"/usr/bin/vmstat",
							"/usr/bin/ps",
							"/usr/bin/kill",
							"/usr/bin/dmesg",
							"/usr/bin/df",
							"/usr/bin/gzip",
							"/usr/bin/tput",
						} {
							ok := r.ProcessInstallCommand(command, filesToTar, containerDiagnostic, logger)
							if !ok {
								// The error will have been logged within the above function.
								// We don't stop processing other pods/containers, just return. If this is the
								// only error, status will show as error; otherwise, as mixed
								Cleanup(logger, localScratchSpaceDirectory)
								return
							}
						}

						// Now we need to copy the script over to our local scratch space to modify the command executions
						localScript := filepath.Join(localScratchSpaceDirectory, command)
						localScriptFile, err := os.OpenFile(localScript, os.O_CREATE|os.O_WRONLY, os.ModePerm)

						if err != nil {
							r.SetStatus(StatusError, fmt.Sprintf("Error writing local script file %s error: %+v", command, err), containerDiagnostic, logger)

							// We don't stop processing other pods/containers, just return. If this is the
							// only error, status will show as error; otherwise, as mixed
							Cleanup(logger, localScratchSpaceDirectory)
							return
						}

						sourceScript := "/usr/local/bin/" + command
						sourceScriptFile, err := os.Open(sourceScript)
						if err != nil {
							r.SetStatus(StatusError, fmt.Sprintf("Error reading file %s error: %+v", sourceScript, err), containerDiagnostic, logger)

							// We don't stop processing other pods/containers, just return. If this is the
							// only error, status will show as error; otherwise, as mixed
							Cleanup(logger, localScratchSpaceDirectory)
							return
						}

						sourceScriptFileScanner := bufio.NewScanner(sourceScriptFile)

						// For some reason linperf.sh (including at the main Drupal page) doesn't have
						// a shabang line, so add that in
						localScriptFile.WriteString("#!/bin/sh\n")

						for sourceScriptFileScanner.Scan() {
							line := sourceScriptFileScanner.Text()

							if !strings.Contains(line, "$(tput") {
								if UseLdLinuxDirect {
									logger.Debug3(fmt.Sprintf("RunScriptOnContainer processing %s line: %s", command, line))

									if !strings.HasPrefix(line, "#") && !strings.Contains(line, "FILES_STRING=") && !strings.Contains(line, "TEMP_STRING=") {

										line = strings.ReplaceAll(line, "MustGather>>", "MustGather:")

										i := strings.Index(line, ">")
										line_end := ""
										if i != -1 {
											line_end = line[i:]
											line = line[:i]
										} else {
											i := strings.Index(line, "#")
											if i != -1 {
												line_end = line[i:]
												line = line[:i]
											}
										}

										replacements := []string{
											"echo",
											"date",
											"tee",
											"rm",
											"sleep",
											"whoami",
											"netstat",
											"top",
											"expr",
											"vmstat",
											"ps",
											"kill",
											"dmesg",
											"df",
											"gzip",
											"tput",
											"tar",
										}

										if strings.Contains(line, "echo $(date)") && strings.Contains(line, "\"MustGather") {
											replacements = []string{
												"echo",
												"date",
												"tee",
											}
										}

										for _, replaceCommand := range replacements {
											if replaceCommand == "tar" && strings.Contains(line, ".tar") {
											} else {
												logger.Debug3(fmt.Sprintf("RunScriptOnContainer before replacing %s in line: %s", replaceCommand, line))
												line = strings.ReplaceAll(line, replaceCommand, GetExecutionCommand(containerTmpFilesPrefix, replaceCommand, ""))
												logger.Debug3(fmt.Sprintf("RunScriptOnContainer after replacing %s in line: %s", replaceCommand, line))
											}
										}

										line = line + line_end
									}
									logger.Debug3(fmt.Sprintf("RunScriptOnContainer writing line: %s", line))
								}
								localScriptFile.WriteString(line + "\n")
							}
						}

						localScriptFile.Close()

						sourceScriptFile.Close()

						os.Chmod(localScript, os.ModePerm)

						// Now that the local file is written, add it to the files to transfer over:
						filesToTar[localScript] = true

						if containerDiagnostic.Spec.Debug {
							remoteFilesToPackage[filepath.Join(containerTmpFilesPrefix, localScratchSpaceDirectory, "linperf.sh")] = true
						}

					} else {

						// First try sbin because it's more likely in bin and if both don't exist
						// then that will be the error message

						commandPath := "/usr/sbin/" + command
						commandExists, _ := DoesFileExist(commandPath)
						if !commandExists {
							commandPath = "/usr/bin/" + command
						}

						ok := r.ProcessInstallCommand(commandPath, filesToTar, containerDiagnostic, logger)
						if !ok {
							// The error will have been logged within the above function.
							// We don't stop processing other pods/containers, just return. If this is the
							// only error, status will show as error; otherwise, as mixed
							Cleanup(logger, localScratchSpaceDirectory)
							return
						}
					}
				}
			}
		}
	}

	// Create the execute script(s)
	for stepIndex, step := range containerDiagnostic.Spec.Steps {
		if step.Command == "execute" {

			if step.Arguments == nil || len(step.Arguments) == 0 {
				r.SetStatus(StatusError, fmt.Sprintf("Run command must have arguments including the binary name"), containerDiagnostic, logger)

				// We don't stop processing other pods/containers, just return. If this is the
				// only error, status will show as error; otherwise, as mixed
				Cleanup(logger, localScratchSpaceDirectory)
				return
			}

			remoteOutputFile := filepath.Join(containerTmpFilesPrefix, fmt.Sprintf("containerdiag_%s_%d.txt", time.Now().Format("20060102_150405"), (stepIndex+1)))

			remoteFilesToPackage[remoteOutputFile] = true

			executionScriptName := fmt.Sprintf("execute_%d.sh", (stepIndex + 1))
			localExecuteScript := filepath.Join(localScratchSpaceDirectory, executionScriptName)

			if containerDiagnostic.Spec.Debug {
				remoteFilesToPackage[filepath.Join(containerTmpFilesPrefix, localScratchSpaceDirectory, executionScriptName)] = true
			}

			localExecuteFile, err := os.OpenFile(localExecuteScript, os.O_CREATE|os.O_WRONLY, os.ModePerm)

			if err != nil {
				r.SetStatus(StatusError, fmt.Sprintf("Error writing local execute.sh file %s error: %+v", localExecuteScript, err), containerDiagnostic, logger)

				// We don't stop processing other pods/containers, just return. If this is the
				// only error, status will show as error; otherwise, as mixed
				Cleanup(logger, localScratchSpaceDirectory)
				return
			}

			// Script header
			localExecuteFile.WriteString("#!/bin/sh\n")

			// Change directory to the temp directory in case any command needs to use the current working directory for scratch files
			localExecuteFile.WriteString(fmt.Sprintf("cd %s\n", containerTmpFilesPrefix))

			if !UseLdLinuxDirect {
				AddDirectCallEnvars(localExecuteFile, containerTmpFilesPrefix)
			}

			// Echo outputfile directly to stdout without redirecting to the output file because a user executing this script wants to know where the output goes
			WriteExecutionLine(localExecuteFile, containerTmpFilesPrefix, "echo", fmt.Sprintf("\"Writing output to %s\"", remoteOutputFile), false, "", false)

			// Build the command execution with arguments
			command := step.Arguments[0]
			arguments := ""

			spaceIndex := strings.Index(strings.TrimSpace(command), " ")
			if spaceIndex != -1 {
				arguments = command[spaceIndex+1:]
				command = command[:spaceIndex]
			}

			background := false

			for index, arg := range step.Arguments {
				if index > 0 {
					if len(arguments) > 0 {
						arguments += " "
					}
					arguments += arg
				}
			}

			if strings.HasSuffix(arguments, " &") {
				background = true
				arguments = arguments[:len(arguments)-2]
			}

			// Echo a simple prolog to the output file including free disk space
			WriteExecutionLine(localExecuteFile, containerTmpFilesPrefix, "date", "", true, remoteOutputFile, false)
			WriteExecutionLine(localExecuteFile, containerTmpFilesPrefix, "echo", fmt.Sprintf("\"containerdiag: Started execution of %s in $(%s)\"", command, GetExecutionCommand(containerTmpFilesPrefix, "pwd", "")), true, remoteOutputFile, false)
			WriteExecutionLine(localExecuteFile, containerTmpFilesPrefix, "echo", "\"\"", true, remoteOutputFile, false)
			WriteExecutionLine(localExecuteFile, containerTmpFilesPrefix, "df", "--block-size=MiB --print-type", false, "", false)

			// The first thing we do is check disk space in our target directory and bail if there isn't enough
			dfcmd := GetExecutionCommand(containerTmpFilesPrefix, "df", "")
			awkcmd := GetExecutionCommand(containerTmpFilesPrefix, "awk", "")
			echocmd := GetExecutionCommand(containerTmpFilesPrefix, "echo", "")

			localExecuteFile.WriteString(fmt.Sprintf("DFOUTPUT=\"$(%s --block-size=MiB --output=avail %s | %s 'BEGIN {avail = -1;} NR == 2 {gsub(/M.*/, \"\"); avail = 0 + $1;} END {printf(\"%%s\", avail);}')\"\necho \"Disk space free in %s: ${DFOUTPUT} MB\"\n", dfcmd, containerTmpFilesPrefix, awkcmd, containerTmpFilesPrefix))

			localExecuteFile.WriteString(fmt.Sprintf("if [ \"${DFOUTPUT}\" = \"\" ]; then\n  DFOUTPUT=\"0\";\nfi\nif [ \"${DFOUTPUT}\" -lt \"%d\" ]; then\n  %s \"ERROR: The available disk space in %s of ${DFOUTPUT} MB is insufficient (%d MB required).\";\n  exit 1;\nfi\n", containerDiagnostic.Spec.MinDiskSpaceFreeMB, echocmd, containerTmpFilesPrefix, containerDiagnostic.Spec.MinDiskSpaceFreeMB))

			// Execute the command with arguments
			if command == "linperf.sh" {
				executionScript := filepath.Join(containerTmpFilesPrefix, localScratchSpaceDirectory, command)
				localExecuteFile.WriteString(fmt.Sprintf("%s $(%s) >> %s 2>&1\n", executionScript, GetExecutionCommand(containerTmpFilesPrefix, "pgrep", "java"), remoteOutputFile))

				remoteFilesToPackage[filepath.Join(containerTmpFilesPrefix, "linperf_RESULTS.tar.gz")] = true
			} else {
				WriteExecutionLine(localExecuteFile, containerTmpFilesPrefix, command, arguments, true, remoteOutputFile, background)
			}

			// Echo a simple epilog to the output file
			WriteExecutionLine(localExecuteFile, containerTmpFilesPrefix, "echo", "\"\"", true, remoteOutputFile, false)
			WriteExecutionLine(localExecuteFile, containerTmpFilesPrefix, "date", "", true, remoteOutputFile, false)
			WriteExecutionLine(localExecuteFile, containerTmpFilesPrefix, "echo", fmt.Sprintf("\"containerdiag: Finished execution of %s\"", command), true, remoteOutputFile, false)

			localExecuteFile.Close()

			os.Chmod(localExecuteScript, os.ModePerm)

			// Now that the local file is written, add it to the files to transfer over:
			filesToTar[localExecuteScript] = true
		} else if step.Command == "package" {
			logger.Info(fmt.Sprintf("RunScriptOnContainer running 'package' step"))

			if step.Arguments == nil || len(step.Arguments) == 0 {
				r.SetStatus(StatusError, fmt.Sprintf("Package command must have arguments including the files to package"), containerDiagnostic, logger)

				// We don't stop processing other pods/containers, just return. If this is the
				// only error, status will show as error; otherwise, as mixed
				Cleanup(logger, localScratchSpaceDirectory)
				return
			}

			for _, arg := range step.Arguments {
				remoteFilesToPackage[arg] = true
			}

			logger.Info(fmt.Sprintf("RunScriptOnContainer finished 'package' step"))
		} else if step.Command == "clean" {
			logger.Info(fmt.Sprintf("RunScriptOnContainer running 'clean' step"))

			for _, arg := range step.Arguments {
				remoteFilesToClean[arg] = true
			}

			logger.Info(fmt.Sprintf("RunScriptOnContainer finished 'clean' step"))
		}
	}

	// Create the zip script
	zipFileName := fmt.Sprintf("containerdiag_%s.zip", time.Now().Format("20060102_150405"))
	remoteZipFile := filepath.Join(containerTmpFilesPrefix, zipFileName)

	localZipScript := filepath.Join(localScratchSpaceDirectory, "zip.sh")
	localZipScriptFile, err := os.OpenFile(localZipScript, os.O_CREATE|os.O_WRONLY, os.ModePerm)
	if err != nil {
		r.SetStatus(StatusError, fmt.Sprintf("Error writing local zip.sh file %s error: %+v", localZipScript, err), containerDiagnostic, logger)

		// We don't stop processing other pods/containers, just return. If this is the
		// only error, status will show as error; otherwise, as mixed
		Cleanup(logger, localScratchSpaceDirectory)
		return
	}

	localZipScriptFile.WriteString("#!/bin/sh\n")
	if !UseLdLinuxDirect {
		AddDirectCallEnvars(localZipScriptFile, containerTmpFilesPrefix)
	}
	localZipScriptFile.WriteString(fmt.Sprintf("%s", GetExecutionCommand(containerTmpFilesPrefix, "zip", "-r")))
	localZipScriptFile.WriteString(fmt.Sprintf(" %s", remoteZipFile))
	for remoteFileToPackage := range remoteFilesToPackage {
		logger.Info(fmt.Sprintf("RunScriptOnContainer packaging %s", remoteFileToPackage))
		localZipScriptFile.WriteString(fmt.Sprintf(" %s", remoteFileToPackage))
	}
	localZipScriptFile.WriteString("\n")

	// Some files (e.g. in /proc) might not exist or we don't have permission but we don't want to fail because of that, so just return true
	localZipScriptFile.WriteString("exit 0\n")

	localZipScriptFile.Close()

	os.Chmod(localZipScript, os.ModePerm)

	filesToTar[localZipScript] = true

	if containerDiagnostic.Spec.Debug {
		remoteFilesToPackage[filepath.Join(containerTmpFilesPrefix, localScratchSpaceDirectory, "zip.sh")] = true
	}

	// Create the clean script
	localCleanScript := filepath.Join(localScratchSpaceDirectory, "clean.sh")
	localCleanScriptFile, err := os.OpenFile(localCleanScript, os.O_CREATE|os.O_WRONLY, os.ModePerm)
	if err != nil {
		r.SetStatus(StatusError, fmt.Sprintf("Error writing local clean.sh file %s error: %+v", localCleanScript, err), containerDiagnostic, logger)

		// We don't stop processing other pods/containers, just return. If this is the
		// only error, status will show as error; otherwise, as mixed
		Cleanup(logger, localScratchSpaceDirectory)
		return
	}

	localCleanScriptFile.WriteString("#!/bin/sh\n")
	if !UseLdLinuxDirect {
		AddDirectCallEnvars(localCleanScriptFile, containerTmpFilesPrefix)
	}
	localCleanScriptFile.WriteString(fmt.Sprintf("%s", GetExecutionCommand(containerTmpFilesPrefix, "rm", "-rf")))
	for remoteFileToClean := range remoteFilesToClean {
		logger.Info(fmt.Sprintf("RunScriptOnContainer cleaning %s", remoteFileToClean))
		localCleanScriptFile.WriteString(fmt.Sprintf(" %s", remoteFileToClean))
	}
	localCleanScriptFile.WriteString("\n")

	// We don't want to fail if there are errors cleaning files
	localCleanScriptFile.WriteString("exit 0\n")

	localCleanScriptFile.Close()

	os.Chmod(localCleanScript, os.ModePerm)

	filesToTar[localCleanScript] = true

	if containerDiagnostic.Spec.Debug {
		remoteFilesToPackage[filepath.Join(containerTmpFilesPrefix, localScratchSpaceDirectory, "clean.sh")] = true
	}

	// Upload any files that are needed
	if len(filesToTar) > 0 {
		for key := range filesToTar {
			tarArguments = append(tarArguments, key)
		}

		logger.Info(fmt.Sprintf("RunScriptOnContainer creating local tar..."))

		outputBytes, err := r.ExecuteLocalCommand(logger, containerDiagnostic, "tar", tarArguments...)
		if err != nil {
			// The error will have been logged within the above function.
			// We don't stop processing other pods/containers, just return. If this is the
			// only error, status will show as error; otherwise, as mixed
			Cleanup(logger, localScratchSpaceDirectory)
			return
		}

		var outputStr string = string(outputBytes[:])
		logger.Debug2(fmt.Sprintf("RunScriptOnContainer local tar output: %v", outputStr))

		file, err := os.Open(localTarFile)
		if err != nil {
			r.SetStatus(StatusError, fmt.Sprintf("Error reading tar file %s error: %+v", localTarFile, err), containerDiagnostic, logger)

			// We don't stop processing other pods/containers, just return. If this is the
			// only error, status will show as error; otherwise, as mixed
			Cleanup(logger, localScratchSpaceDirectory)
			return
		}

		fileReader := bufio.NewReader(file)
		logger.Info(fmt.Sprintf("RunScriptOnContainer local tar file binary size: %d", fileReader.Size()))

		var tarStdout, tarStderr bytes.Buffer
		err = r.ExecInContainer(pod, container, []string{"tar", "-xmf", "-", "-C", containerTmpFilesPrefix}, &tarStdout, &tarStderr, fileReader, nil)

		file.Close()

		logger.Debug1(fmt.Sprintf("ExecInContainer results: stdout: %s\n\nstderr: %s\n", tarStdout.String(), tarStderr.String()))

		if err != nil {
			r.SetStatus(StatusError, fmt.Sprintf("Error uploading tar file to pod: %s container: %s error: %+v", pod.Name, container.Name, err), containerDiagnostic, logger)

			// We don't stop processing other pods/containers, just return. If this is the
			// only error, status will show as error; otherwise, as mixed
			Cleanup(logger, localScratchSpaceDirectory)
			return
		}

		logger.Debug2(fmt.Sprintf("RunScriptOnContainer tar results: stdout: %v stderr: %v", tarStdout.String(), tarStderr.String()))
	}

	// Run any executions
	for stepIndex, step := range containerDiagnostic.Spec.Steps {
		if step.Command == "execute" {

			logger.Info(fmt.Sprintf("RunScriptOnContainer running 'execute' step"))

			remoteExecutionScript := filepath.Join(containerTmpFilesPrefix, localScratchSpaceDirectory, fmt.Sprintf("execute_%d.sh", (stepIndex+1)))

			logger.Info(fmt.Sprintf("RunScriptOnContainer Running script %v", remoteExecutionScript))

			var stdout, stderr bytes.Buffer
			err := r.ExecInContainer(pod, container, []string{remoteExecutionScript}, &stdout, &stderr, nil, nil)

			logger.Debug1(fmt.Sprintf("ExecInContainer results: err: %v, stdout: %s\n\nstderr: %s\n", err, stdout.String(), stderr.String()))

			if err != nil {

				stdout := stdout.String()
				stderr := stderr.String()

				var log string
				if len(stdout) == 0 {
					log = stderr
				} else if len(stderr) == 0 {
					log = stdout
				} else {
					log = stderr + "\n\n" + stdout
				}

				containerDiagnostic.Status.Log += log

				r.SetStatus(StatusError, fmt.Sprintf("Error running 'execute' step on pod (review Status Log): %s container: %s error: %+v", pod.Name, container.Name, err), containerDiagnostic, logger)

				// TODO run clean.sh

				// We don't stop processing other pods/containers, just return. If this is the
				// only error, status will show as error; otherwise, as mixed
				Cleanup(logger, localScratchSpaceDirectory)
				return
			}

			stdoutStr := stdout.String()
			stderrStr := stderr.String()
			if len(stderrStr) == 0 {
				logger.Info(fmt.Sprintf("RunScriptOnContainer stdout:\n%s\n", stdoutStr))
			} else {
				logger.Info(fmt.Sprintf("RunScriptOnContainer stdout:\n%s\n\nstderr:\n%s\n", stdoutStr, stderrStr))
			}

			logger.Info(fmt.Sprintf("RunScriptOnContainer finished 'execute' step"))
		}
	}

	// Execute the final zip

	var zipStdout, zipStderr bytes.Buffer

	zipScript := filepath.Join(containerTmpFilesPrefix, localScratchSpaceDirectory, "zip.sh")
	logger.Info(fmt.Sprintf("RunScriptOnContainer zipping up remote files: %s", zipScript))

	err = r.ExecInContainer(pod, container, []string{zipScript}, &zipStdout, &zipStderr, nil, nil)
	logger.Debug1(fmt.Sprintf("ExecInContainer results: stdout: %s\n\nstderr: %s\n", zipStdout.String(), zipStderr.String()))

	if err != nil {
		r.SetStatus(StatusError, fmt.Sprintf("Error running 'zip' step on pod: %s container: %s error: %+v", pod.Name, container.Name, err), containerDiagnostic, logger)

		// We don't stop processing other pods/containers, just return. If this is the
		// only error, status will show as error; otherwise, as mixed
		Cleanup(logger, localScratchSpaceDirectory)
		return
	}

	// Download the files locally
	localDownloadedTarFile := filepath.Join(localScratchSpaceDirectory, strings.ReplaceAll(zipFileName, ".zip", ".tar"))
	localZipFile := filepath.Join(localScratchSpaceDirectory, zipFileName)

	logger.Info(fmt.Sprintf("RunScriptOnContainer Downloading file to: %s", localDownloadedTarFile))

	file, err := os.OpenFile(localDownloadedTarFile, os.O_CREATE|os.O_WRONLY, os.ModePerm)
	if err != nil {
		r.SetStatus(StatusError, fmt.Sprintf("Error opening tar file %s error: %+v", localDownloadedTarFile, err), containerDiagnostic, logger)

		// We don't stop processing other pods/containers, just return. If this is the
		// only error, status will show as error; otherwise, as mixed
		Cleanup(logger, localScratchSpaceDirectory)
		return
	}

	fileWriter := bufio.NewWriter(file)

	var tarStderr bytes.Buffer
	args := []string{"tar", "-C", filepath.Dir(remoteZipFile), "-cf", "-", filepath.Base(remoteZipFile)}
	err = r.ExecInContainer(pod, container, args, nil, &tarStderr, nil, fileWriter)

	fileWriter.Flush()
	file.Close()

	logger.Debug1(fmt.Sprintf("ExecInContainer results: stderr: %s\n", tarStderr.String()))

	if err != nil {
		r.SetStatus(StatusError, fmt.Sprintf("Error downloading %s from pod: %s container: %s error: %+v for %v", remoteZipFile, pod.Name, container.Name, err, args), containerDiagnostic, logger)

		// We don't stop processing other pods/containers, just return. If this is the
		// only error, status will show as error; otherwise, as mixed
		Cleanup(logger, localScratchSpaceDirectory)
		return
	}

	// Now untar the tar file which will expand the zip file
	logger.Info(fmt.Sprintf("RunScriptOnContainer Untarring downloaded file: %s", localDownloadedTarFile))

	outputBytes, err := r.ExecuteLocalCommand(logger, containerDiagnostic, "tar", "-C", localScratchSpaceDirectory, "-xvf", localDownloadedTarFile)
	var outputStr string = string(outputBytes[:])
	if err != nil {
		r.SetStatus(StatusError, fmt.Sprintf("Could not untar %s: %+v %s", localDownloadedTarFile, err, outputStr), containerDiagnostic, logger)

		// We don't stop processing other pods/containers, just return. If this is the
		// only error, status will show as error; otherwise, as mixed
		Cleanup(logger, localScratchSpaceDirectory)
		return
	}

	logger.Debug1(fmt.Sprintf("RunScriptOnContainer untar output: %v", outputStr))

	// Delete the tar file
	os.Remove(localDownloadedTarFile)

	fileInfo, err := os.Stat(localZipFile)
	if err != nil {
		r.SetStatus(StatusError, fmt.Sprintf("Could not find local zip file: %s error: %+v", localZipFile, err), containerDiagnostic, logger)

		// We don't stop processing other pods/containers, just return. If this is the
		// only error, status will show as error; otherwise, as mixed
		Cleanup(logger, localScratchSpaceDirectory)
		return
	}

	logger.Info(fmt.Sprintf("RunScriptOnContainer Finished downloading zip file, size: %d", fileInfo.Size()))

	// Now move the zip over to the permanent space
	permdir := filepath.Join(contextTracker.localPermanentDirectory, "namespaces", pod.Namespace, "pods", pod.Name, "containers", container.Name, uuid)
	err = os.MkdirAll(permdir, os.ModePerm)
	if err != nil {
		r.SetStatus(StatusError, fmt.Sprintf("Could not create permanent output space in %s: %+v", permdir, err), containerDiagnostic, logger)

		// We don't stop processing other pods/containers, just return. If this is the
		// only error, status will show as error; otherwise, as mixed
		Cleanup(logger, localScratchSpaceDirectory)
		return
	}

	// Finally copy the zip file over
	err = CopyFile(localZipFile, filepath.Join(permdir, zipFileName))
	if err != nil {
		r.SetStatus(StatusError, fmt.Sprintf("Could not copy file file to permanent directory %s: %+v", permdir, err), containerDiagnostic, logger)

		// We don't stop processing other pods/containers, just return. If this is the
		// only error, status will show as error; otherwise, as mixed
		Cleanup(logger, localScratchSpaceDirectory)
		return
	}

	logger.Info(fmt.Sprintf("RunScriptOnContainer Copied zip file to: %s", permdir))

	// Cleanup if requested
	for _, step := range containerDiagnostic.Spec.Steps {
		if step.Command == "clean" {

			cleanScript := filepath.Join(containerTmpFilesPrefix, localScratchSpaceDirectory, "clean.sh")
			logger.Info(fmt.Sprintf("RunScriptOnContainer running 'clean' step"))

			var stdout, stderr bytes.Buffer
			err := r.ExecInContainer(pod, container, []string{cleanScript}, &stdout, &stderr, nil, nil)

			logger.Debug1(fmt.Sprintf("ExecInContainer results: stdout: %s\n\nstderr: %s\n", stdout.String(), stderr.String()))

			if err != nil {
				r.SetStatus(StatusError, fmt.Sprintf("Error running clean step on pod: %s container: %s error: %+v", pod.Name, container.Name, err), containerDiagnostic, logger)

				// We don't stop processing other pods/containers, just return. If this is the
				// only error, status will show as error; otherwise, as mixed
				Cleanup(logger, localScratchSpaceDirectory)
				return
			}

			logger.Info(fmt.Sprintf("RunScriptOnContainer finished 'clean' step"))
		}
	}

	contextTracker.successes++

	Cleanup(logger, localScratchSpaceDirectory)
}

func CopyFile(src string, dest string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	outFile, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer outFile.Close()

	_, err = io.Copy(outFile, srcFile)
	return err
}

func WriteExecutionLine(fileWriter *os.File, containerTmpFilesPrefix string, command string, arguments string, redirectOutput bool, outputFile string, background bool) {
	var redirectStr string = ""
	var backgroundStr string = ""
	if redirectOutput {
		redirectStr = fmt.Sprintf(" >> %s 2>&1", outputFile)
	}
	if background {
		backgroundStr = " &"
	}
	fileWriter.WriteString(fmt.Sprintf("%s%s%s\n", GetExecutionCommand(containerTmpFilesPrefix, command, arguments), redirectStr, backgroundStr))
}

func GetExecutionCommand(containerTmpFilesPrefix string, command string, arguments string) string {
	// See https://www.kernel.org/doc/man-pages/online/pages/man8/ld-linux.so.8.html
	var result string
	if UseLdLinuxDirect {

		usrFolder := "bin"
		commandExists, _ := DoesFileExist("/usr/bin/" + command)
		if !commandExists {
			usrFolder = "sbin"
		}

		result = fmt.Sprintf("%s --inhibit-cache --library-path %s %s", filepath.Join(containerTmpFilesPrefix, "lib64", "ld-linux-x86-64.so.2"), filepath.Join(containerTmpFilesPrefix, "lib64"), filepath.Join(containerTmpFilesPrefix, "usr", usrFolder, command))
	} else {
		result = command
	}
	if len(arguments) > 0 {
		result += " " + arguments
	}
	return result
}

func AddDirectCallEnvars(localFile *os.File, containerTmpFilesPrefix string) {
	localFile.WriteString(fmt.Sprintf("export PATH=%s\n", filepath.Join(containerTmpFilesPrefix, "usr", "bin")))
	localFile.WriteString(fmt.Sprintf("export LD_LIBRARY_PATH=%s\n", filepath.Join(containerTmpFilesPrefix, "lib64")))
}

func DoesFileExist(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else {
		return false, err
	}
}

func (r *ContainerDiagnosticReconciler) ProcessInstallCommand(fullCommand string, filesToTar map[string]bool, containerDiagnostic *diagnosticv1.ContainerDiagnostic, logger *CustomLogger) bool {

	fullCommand = filepath.Clean(fullCommand)

	logger.Debug1(fmt.Sprintf("RunScriptOnContainer Processing install command: %s", fullCommand))

	fileExists, err := DoesFileExist(fullCommand)
	if !fileExists || err != nil {
		r.SetStatus(StatusError, fmt.Sprintf("Tool %s does not exist", fullCommand), containerDiagnostic, logger)
		return false
	}

	filesToTar[fullCommand] = true

	// We can't run ldd on itself
	if strings.HasSuffix(fullCommand, "ldd") {
		return true
	}

	ProcessSymlinks(fullCommand, filesToTar, logger)

	lines, ok := r.FindSharedLibraries(logger, containerDiagnostic, fullCommand)
	if !ok {
		// The error will have been logged within the above function.
		return false
	}

	for _, line := range lines {
		logger.Debug2(fmt.Sprintf("RunScriptOnContainer ldd file: %v", line))
		filesToTar[line] = true
		ProcessSymlinks(line, filesToTar, logger)
	}

	return true
}

func ProcessSymlinks(check string, filesToTar map[string]bool, logger *CustomLogger) {
	// Follow any symlinks and add those
	var last string = check
	var count int = 0

	for count < 10 {
		logger.Debug2(fmt.Sprintf("ProcessSymlinks checking for symlinks: %s", last))
		fileInfo, err := os.Lstat(last)
		if err == nil {
			if fileInfo.Mode()&os.ModeSymlink != 0 {
				checkLink, err := os.Readlink(last)
				logger.Debug2(fmt.Sprintf("ProcessSymlinks found symlink: %s", checkLink))
				if err == nil {
					if checkLink != last {

						if !filepath.IsAbs(checkLink) {
							checkLink = filepath.Clean(filepath.Join(filepath.Dir(last), checkLink))
						} else {
							checkLink = filepath.Clean(checkLink)
						}

						logger.Debug2(fmt.Sprintf("ProcessSymlinks after cleaning: %s", checkLink))

						filesToTar[checkLink] = true
						last = checkLink
					} else {
						break
					}
				} else {
					break
				}
			} else {
				break
			}
		} else {
			break
		}

		// Avoid an infinite loop
		count++
	}
}

func Cleanup(logger *CustomLogger, localScratchSpaceDirectory string) {
	err := os.RemoveAll(localScratchSpaceDirectory)
	if err != nil {
		logger.Info(fmt.Sprintf("Could not cleanup %s: %+v", localScratchSpaceDirectory, err))
	}
}

func (r *ContainerDiagnosticReconciler) EnsureDirectoriesOnContainer(ctx context.Context, req ctrl.Request, containerDiagnostic *diagnosticv1.ContainerDiagnostic, logger *CustomLogger, pod *corev1.Pod, container corev1.Container, contextTracker *ContextTracker, uuid string) (response string, ok bool) {

	containerTmpFilesPrefix := containerDiagnostic.Spec.Directory

	if containerDiagnostic.Spec.UseUUID {
		containerTmpFilesPrefix += uuid + "/"
	}

	logger.Debug1(fmt.Sprintf("RunScriptOnContainer running mkdir: %s", containerTmpFilesPrefix))

	var stdout, stderr bytes.Buffer
	err := r.ExecInContainer(pod, container, []string{"mkdir", "-p", containerTmpFilesPrefix}, &stdout, &stderr, nil, nil)

	logger.Debug1(fmt.Sprintf("ExecInContainer results: stdout: %s\n\nstderr: %s\n", stdout.String(), stderr.String()))

	if err != nil {
		r.SetStatus(StatusError, fmt.Sprintf("Error executing mkdir in container: %+v", err), containerDiagnostic, logger)

		// We don't stop processing other pods/containers, just return. If this is the
		// only error, status will show as error; otherwise, as mixed
		return "", false
	}

	return containerTmpFilesPrefix, true
}

func (r *ContainerDiagnosticReconciler) ExecuteLocalCommand(logger *CustomLogger, containerDiagnostic *diagnosticv1.ContainerDiagnostic, command string, arguments ...string) (output []byte, err error) {

	logger.Debug2(fmt.Sprintf("RunScriptOnContainer ExecuteLocalCommand: %v", command))

	outputBytes, err := exec.Command(command, arguments...).CombinedOutput()
	if err != nil {
		r.SetStatus(StatusError, fmt.Sprintf("Error executing %v %v: %+v", command, arguments, err), containerDiagnostic, logger)

		// We don't stop processing other pods/containers, just return. If this is the
		// only error, status will show as error; otherwise, as mixed
		return outputBytes, err
	}

	logger.Debug2(fmt.Sprintf("RunScriptOnContainer ExecuteLocalCommand results: %v", output))

	return outputBytes, nil
}

func (r *ContainerDiagnosticReconciler) FindSharedLibraries(logger *CustomLogger, containerDiagnostic *diagnosticv1.ContainerDiagnostic, command string) ([]string, bool) {
	outputBytes, err := r.ExecuteLocalCommand(logger, containerDiagnostic, "ldd", command)
	if err != nil {
		// We don't stop processing other pods/containers, just return. If this is the
		// only error, status will show as error; otherwise, as mixed
		return nil, false
	}

	var lines []string
	scanner := bufio.NewScanner(bytes.NewReader(outputBytes))
	for scanner.Scan() {
		var line string = strings.TrimSpace(scanner.Text())
		if strings.Contains(line, "=>") {
			var pieces []string = strings.Split(line, " ")
			lines = append(lines, pieces[2])
		} else if strings.Contains(line, "ld-linux") {
			var pieces []string = strings.Split(line, " ")
			lines = append(lines, pieces[0])
		}
	}

	return lines, true
}

func (r *ContainerDiagnosticReconciler) ExecInContainer(pod *corev1.Pod, container corev1.Container, command []string, stdout *bytes.Buffer, stderr *bytes.Buffer, stdin *bufio.Reader, stdoutWriter *bufio.Writer) error {
	clientset, err := kubernetes.NewForConfig(r.Config)
	if err != nil {
		return err
	}

	restRequest := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec")

	if stdin == nil {
		restRequest.VersionedParams(&corev1.PodExecOptions{
			Command:   command,
			Container: container.Name,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)
	} else {
		restRequest.VersionedParams(&corev1.PodExecOptions{
			Command:   command,
			Container: container.Name,
			Stdout:    true,
			Stderr:    true,
			Stdin:     true,
			TTY:       false,
		}, scheme.ParameterCodec)
	}

	exec, err := remotecommand.NewSPDYExecutor(r.Config, "POST", restRequest.URL())
	if err != nil {
		return err
	}

	if stdin == nil {
		if stdoutWriter == nil {
			err = exec.Stream(remotecommand.StreamOptions{
				Stdout: stdout,
				Stderr: stderr,
				Tty:    false,
			})
		} else {
			err = exec.Stream(remotecommand.StreamOptions{
				Stdout: stdoutWriter,
				Stderr: stderr,
				Tty:    false,
			})
		}
	} else {
		if stdoutWriter == nil {
			err = exec.Stream(remotecommand.StreamOptions{
				Stdout: stdout,
				Stderr: stderr,
				Stdin:  stdin,
				Tty:    false,
			})
		} else {
			err = exec.Stream(remotecommand.StreamOptions{
				Stdout: stdoutWriter,
				Stderr: stderr,
				Stdin:  stdin,
				Tty:    false,
			})
		}
	}

	return err
}

// SetupWithManager sets up the controller with the Manager.
func (r *ContainerDiagnosticReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/builder#Builder
	result := ctrl.NewControllerManagedBy(mgr).
		For(&diagnosticv1.ContainerDiagnostic{}).
		Complete(r)
	r.Config = mgr.GetConfig()
	r.EventRecorder = mgr.GetEventRecorderFor("containerdiagnostic")
	return result
}
