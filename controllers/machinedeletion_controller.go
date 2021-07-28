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
	"context"
	"fmt"
	"github.com/go-logr/logr"
	"github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"

	"github.com/medik8s/machine-deletion/api/v1alpha1"
)

const (
	machineAnnotationOpenshift = "machine.openshift.io/machine"
	machineKind                = "Machine"
	machineSetKind             = "MachineSet"
)

// MachineDeletionReconciler reconciles a MachineDeletion object
type MachineDeletionReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=machine-deletion.medik8s.io,resources=machinedeletions,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=machine-deletion.medik8s.io,resources=machinedeletions/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=machine-deletion.medik8s.io,resources=machinedeletions/finalizers,verbs=update
//+kubebuilder:rbac:groups=machine.openshift.io,resources=machines,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// the MachineDeletion object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *MachineDeletionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("machinedeletion", req.NamespacedName)

	log.Info("reconciling...")

	//fetch the remediation
	var remediation *v1alpha1.MachineDeletion
	if remediation = r.getRemediation(ctx, req); remediation == nil {
		return ctrl.Result{}, nil
	}

	var machine *unstructured.Unstructured
	//Health check was done by NHC
	if node, err := r.getNodeFromMdr(remediation); err == nil {
		if machine, err = r.buildMachineFromNode(node); err != nil {
			return ctrl.Result{}, err
		}
		if isMachineBelongToMasterNode(machine) {
			return ctrl.Result{}, nil
		}

	} else { //Failed both in fetching the machine and in fetching the node
		return ctrl.Result{}, err
	}

	//delete the machine
	if err := r.deleteMachine(ctx, machine); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func isMachineBelongToMasterNode(machine *unstructured.Unstructured) bool {
	refs := machine.GetOwnerReferences()
	for _, ref := range refs {
		if ref.Kind == machineSetKind {
			return false
		}
	}
	return true
}

// SetupWithManager sets up the controller with the Manager.
func (r *MachineDeletionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.MachineDeletion{}).
		Complete(r)
}

func (r *MachineDeletionReconciler) getRemediation(ctx context.Context, req ctrl.Request) *v1alpha1.MachineDeletion {
	remediation := new(v1alpha1.MachineDeletion)
	key := client.ObjectKey{Name: req.Name, Namespace: req.Namespace}
	if err := r.Client.Get(ctx, key, remediation); err != nil {
		if !errors.IsNotFound(err) {
			r.Log.Error(err, "error retrieving remediation %s in namespace %s: %v", req.Name, req.Namespace)
		}
		return nil
	}
	return remediation
}

func (r *MachineDeletionReconciler) deleteMachine(ctx context.Context, machine *unstructured.Unstructured) error {
	if err := r.Client.Delete(ctx, machine); err != nil {
		if !errors.IsNotFound(err) {
			r.Log.Error(err, "error deleting machine %s in namespace %s: %v", machine.GetName(), machine.GetNamespace())
			return err
		}
		r.Log.Info("machine: %s in namespace: %s is deleted , but remediation still exist", machine.GetName(), machine.GetNamespace())
	}
	return nil
}

func (r *MachineDeletionReconciler) getNodeFromMdr(mdr *v1alpha1.MachineDeletion) (*v1.Node, error) {
	node := &v1.Node{}
	key := client.ObjectKey{
		Name: mdr.Name,
	}

	if err := r.Get(context.TODO(), key, node); err != nil {
		return nil, err
	}
	return node, nil
}

func (r *MachineDeletionReconciler) buildMachineFromNode(node *v1.Node) (*unstructured.Unstructured, error) {

	var nodeAnnotations map[string]string
	if nodeAnnotations = node.Annotations; nodeAnnotations == nil {
		return nil, fmt.Errorf("failed to find machine annotation on node name: %s", node.Name)
	}
	var machineNameNamespace, machineName string

	//OpenShift Machine
	if machineNameNamespace = nodeAnnotations[machineAnnotationOpenshift]; len(machineNameNamespace) == 0 {
		return nil, fmt.Errorf("failed to find openshift machine annotation on node name: %s", node.Name)
	}

	machineName, machineNamespace, err := extractNameAndNamespace(machineNameNamespace, node.Name)
	if err != nil {
		return nil, err
	}

	machine := new(unstructured.Unstructured)
	machine.SetKind(machineKind)
	machine.SetAPIVersion(v1beta1.SchemeGroupVersion.String())

	key := client.ObjectKey{
		Name:      machineName,
		Namespace: machineNamespace,
	}

	if err := r.Get(context.TODO(), key, machine); err != nil {
		return nil, err
	}
	return machine, nil
}

func extractNameAndNamespace(nameNamespace string, nodeName string) (string, string, error) {
	if nameNamespaceSlice := strings.Split(nameNamespace, "/"); len(nameNamespaceSlice) == 2 {
		return nameNamespaceSlice[1], nameNamespaceSlice[0], nil
	}
	return "", "", fmt.Errorf("failed to extract Machine Name and Machine Namespace from machine annotation on the node for node name: %s", nodeName)
}
