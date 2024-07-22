package kubevirt

//go:generate mockgen -source=./machine_factory.go -destination=./mock/machine_factory_generated.go -package=mock_kubevirt

import (
	gocontext "context"
	"time"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/cluster-api-provider-kubevirt/pkg/context"
	"sigs.k8s.io/cluster-api-provider-kubevirt/pkg/ssh"
	"sigs.k8s.io/cluster-api-provider-kubevirt/pkg/workloadcluster"
)

// MachineInterface abstracts the functions that the kubevirt.machine interface implements.

type MachineInterface interface {
	// Create creates a new VM for this machine.
	Create(ctx gocontext.Context) error
	// Delete deletes VM for this machine.
	Delete() error
	// Exists checks if the VM has been provisioned already.
	Exists() bool
	// IsReady checks if the VM is ready
	IsReady() bool
	// Address returns the IP address of the VM.
	Address() string
	// SupportsCheckingIsBootstrapped checks if we have a method of checking
	// that this bootstrapper has completed.
	SupportsCheckingIsBootstrapped() bool
	// IsBootstrapped checks if the VM is bootstrapped with Kubernetes.
	IsBootstrapped() bool
	// GenerateProviderID generates the KubeVirt provider ID to be used for the NodeRef
	GenerateProviderID() (string, error)
	// IsTerminal reports back if a VM is in a permanent terminal state
	IsTerminal() (bool, string, error)

	DrainNodeIfNeeded(workloadcluster.WorkloadCluster) (time.Duration, error)

	// GetVMUnscheduledReason returns the reason and message for the condition, if the VM is not ready
	GetVMNotReadyReason() (string, string)
}

// MachineFactory allows creating new instances of kubevirt.machine

type MachineFactory interface {
	// NewMachine returns a new Machine service for the given context.
	NewMachine(ctx *context.MachineContext, client client.Client, namespace string, sshKeys *ssh.ClusterNodeSshKeys, serviceAccountSecret *corev1.Secret, networkDataSecret *corev1.Secret) (MachineInterface, error)
}

// DefaultMachineFactory is the default implementation of MachineFactory
type DefaultMachineFactory struct {
}

// NewMachine creates a new kubevirt.machine
func (defaultMachineFactory DefaultMachineFactory) NewMachine(ctx *context.MachineContext, client client.Client, namespace string, sshKeys *ssh.ClusterNodeSshKeys, serviceAccountSecret *corev1.Secret, networkDataSecret *corev1.Secret) (MachineInterface, error) {
	externalMachine, err := NewMachine(ctx, client, namespace, sshKeys, serviceAccountSecret, networkDataSecret)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create helper for managing the externalMachine")
	}
	return externalMachine, nil
}
