/*
Copyright 2018 The Kubernetes Authors.
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

package google

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"reflect"
	"strings"
	"time"

	"github.com/golang/glog"
	"golang.org/x/net/context"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"regexp"

	"encoding/base64"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/cluster-api/cloud/google/clients"
	gceconfigv1 "sigs.k8s.io/cluster-api/cloud/google/gceproviderconfig/v1alpha1"
	"sigs.k8s.io/cluster-api/cloud/google/machinesetup"
	apierrors "sigs.k8s.io/cluster-api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
	"sigs.k8s.io/cluster-api/pkg/cert"
	client "sigs.k8s.io/cluster-api/pkg/client/clientset_generated/clientset/typed/cluster/v1alpha1"
	"sigs.k8s.io/cluster-api/util"
)

const (
	ProjectAnnotationKey = "gcp-project"
	ZoneAnnotationKey    = "gcp-zone"
	NameAnnotationKey    = "gcp-name"

	BootstrapLabelKey = "boostrap"

	// This file is a yaml that will be used to create the machine-setup configmap on the machine controller.
	// It contains the supported machine configurations along with the startup scripts and OS image paths that correspond to each supported configuration.
	MachineSetupConfigsFilename = "machine_setup_configs.yaml"
)

type SshCreds struct {
	user           string
	privateKeyPath string
}

type GCEClientMachineSetupConfigGetter interface {
	GetMachineSetupConfig() (machinesetup.MachineSetupConfig, error)
}

type GCEClientComputeService interface {
	ImagesGet(project string, image string) (*compute.Image, error)
	ImagesGetFromFamily(project string, family string) (*compute.Image, error)
	InstancesDelete(project string, zone string, targetInstance string) (*compute.Operation, error)
	InstancesGet(project string, zone string, instance string) (*compute.Instance, error)
	InstancesInsert(project string, zone string, instance *compute.Instance) (*compute.Operation, error)
	ZoneOperationsGet(project string, zone string, operation string) (*compute.Operation, error)
}

type GCEClient struct {
	certificateAuthority     *cert.CertificateAuthority
	computeService           GCEClientComputeService
	gceProviderConfigCodec   *gceconfigv1.GCEProviderConfigCodec
	scheme                   *runtime.Scheme
	kubeadmToken             string
	sshCreds                 SshCreds
	machineClient            client.MachineInterface
	machineSetupConfigGetter GCEClientMachineSetupConfigGetter
}

type MachineActuatorParams struct {
	CertificateAuthority     *cert.CertificateAuthority
	ComputeService           GCEClientComputeService
	KubeadmToken             string
	MachineClient            client.MachineInterface
	MachineSetupConfigGetter GCEClientMachineSetupConfigGetter
}

const (
	gceTimeout   = time.Minute * 10
	gceWaitSleep = time.Second * 5
)

func NewMachineActuator(params MachineActuatorParams) (*GCEClient, error) {
	computeService, err := getOrNewComputeService(params)
	if err != nil {
		return nil, err
	}

	scheme, err := gceconfigv1.NewScheme()
	if err != nil {
		return nil, err
	}
	codec, err := gceconfigv1.NewCodec()
	if err != nil {
		return nil, err
	}

	// Only applicable if it's running inside machine controller pod.
	var privateKeyPath, user string
	if _, err := os.Stat("/etc/sshkeys/private"); err == nil {
		privateKeyPath = "/etc/sshkeys/private"

		b, err := ioutil.ReadFile("/etc/sshkeys/user")
		if err == nil {
			user = string(b)
		} else {
			return nil, err
		}
	}

	return &GCEClient{
		certificateAuthority:   params.CertificateAuthority,
		computeService:         computeService,
		scheme:                 scheme,
		gceProviderConfigCodec: codec,
		kubeadmToken:           params.KubeadmToken,
		sshCreds: SshCreds{
			privateKeyPath: privateKeyPath,
			user:           user,
		},
		machineClient:            params.MachineClient,
		machineSetupConfigGetter: params.MachineSetupConfigGetter,
	}, nil
}

func (gce *GCEClient) CreateMachineController(cluster *clusterv1.Cluster, initialMachines []*clusterv1.Machine, clientSet kubernetes.Clientset) error {
	if gce.machineSetupConfigGetter == nil {
		return errors.New("a valid machineSetupConfigGetter is required")
	}
	if err := gce.CreateMachineControllerServiceAccount(cluster, initialMachines); err != nil {
		return err
	}

	// Setup SSH access to master VM
	if err := gce.setupSSHAccess(cluster, util.GetMaster(initialMachines)); err != nil {
		return err
	}

	if err := CreateExtApiServerRoleBinding(); err != nil {
		return err
	}

	machineSetupConfig, err := gce.machineSetupConfigGetter.GetMachineSetupConfig()
	if err != nil {
		return err
	}
	yaml, err := machineSetupConfig.GetYaml()
	if err != nil {
		return err
	}
	configMap := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-setup"},
		Data: map[string]string{
			MachineSetupConfigsFilename: yaml,
		},
	}
	configMaps := clientSet.CoreV1().ConfigMaps(corev1.NamespaceDefault)
	if _, err := configMaps.Create(&configMap); err != nil {
		return err
	}

	if err := CreateApiServerAndController(gce.kubeadmToken); err != nil {
		return err
	}
	return nil
}

func (gce *GCEClient) ProvisionClusterDependencies(cluster *clusterv1.Cluster, initialMachines []*clusterv1.Machine) error {
	err := gce.CreateWorkerNodeServiceAccount(cluster, initialMachines)
	if err != nil {
		return err
	}

	return gce.CreateMasterNodeServiceAccount(cluster, initialMachines)
}

func (gce *GCEClient) Create(cluster *clusterv1.Cluster, machine *clusterv1.Machine) error {
	if gce.machineSetupConfigGetter == nil {
		return errors.New("a valid machineSetupConfigGetter is required")
	}
	machineConfig, err := gce.machineproviderconfig(machine.Spec.ProviderConfig)
	if err != nil {
		return gce.handleMachineError(machine, apierrors.InvalidMachineConfiguration(
			"Cannot unmarshal machine's providerConfig field: %v", err))
	}
	clusterConfig, err := gce.clusterproviderconfig(cluster.Spec.ProviderConfig)
	if err != nil {
		return gce.handleMachineError(machine, apierrors.InvalidMachineConfiguration(
			"Cannot unmarshal cluster's providerConfig field: %v", err))
	}

	if verr := gce.validateMachine(machine, machineConfig); verr != nil {
		return gce.handleMachineError(machine, verr)
	}

	configParams := &machinesetup.ConfigParams{
		OS:       machineConfig.OS,
		Roles:    machine.Spec.Roles,
		Versions: machine.Spec.Versions,
	}
	machineSetupConfigs, err := gce.machineSetupConfigGetter.GetMachineSetupConfig()
	if err != nil {
		return err
	}
	image, err := machineSetupConfigs.GetImage(configParams)
	if err != nil {
		return err
	}
	imagePath := gce.getImagePath(image)
	metadata, err := gce.getMetadata(cluster, machine, clusterConfig, configParams)
	if err != nil {
		return err
	}

	instance, err := gce.instanceIfExists(cluster, machine)
	if err != nil {
		return err
	}

	name := machine.ObjectMeta.Name
	project := clusterConfig.Project
	zone := machineConfig.Zone

	if instance == nil {
		labels := map[string]string{}
		if gce.machineClient == nil {
			labels[BootstrapLabelKey] = "true"
		}

		op, err := gce.computeService.InstancesInsert(project, zone, &compute.Instance{
			Name:         name,
			MachineType:  fmt.Sprintf("zones/%s/machineTypes/%s", zone, machineConfig.MachineType),
			CanIpForward: true,
			NetworkInterfaces: []*compute.NetworkInterface{
				{
					Network: "global/networks/default",
					AccessConfigs: []*compute.AccessConfig{
						{
							Type: "ONE_TO_ONE_NAT",
							Name: "External NAT",
						},
					},
				},
			},
			Disks: newDisks(machineConfig, zone, imagePath, int64(30)),
			Metadata: metadata,
			Tags: &compute.Tags{
				Items: []string{
					"https-server",
					fmt.Sprintf("%s-worker", cluster.Name)},
			},
			Labels: labels,
			ServiceAccounts: []*compute.ServiceAccount{
				{
					Email: gce.GetDefaultServiceAccountForMachine(cluster, machine),
					Scopes: []string{
						compute.CloudPlatformScope,
					},
				},
			},
		})

		if err == nil {
			err = gce.waitForOperation(clusterConfig, op)
		}

		if err != nil {
			return gce.handleMachineError(machine, apierrors.CreateMachine(
				"error creating GCE instance: %v", err))
		}

		// If we have a machineClient, then annotate the machine so that we
		// remember exactly what VM we created for it.
		if gce.machineClient != nil {
			return gce.updateAnnotations(cluster, machine)
		}
	} else {
		glog.Infof("Skipped creating a VM that already exists.\n")
	}

	return nil
}

func (gce *GCEClient) Delete(cluster *clusterv1.Cluster, machine *clusterv1.Machine) error {
	instance, err := gce.instanceIfExists(cluster, machine)
	if err != nil {
		return err
	}

	if instance == nil {
		glog.Infof("Skipped deleting a VM that is already deleted.\n")
		return nil
	}

	machineConfig, err := gce.machineproviderconfig(machine.Spec.ProviderConfig)
	if err != nil {
		return gce.handleMachineError(machine,
			apierrors.InvalidMachineConfiguration("Cannot unmarshal machine's providerConfig field: %v", err))
	}

	clusterConfig, err := gce.clusterproviderconfig(cluster.Spec.ProviderConfig)
	if err != nil {
		return gce.handleMachineError(machine,
			apierrors.InvalidMachineConfiguration("Cannot unmarshal cluster's providerConfig field: %v", err))
	}

	if verr := gce.validateMachine(machine, machineConfig); verr != nil {
		return gce.handleMachineError(machine, verr)
	}

	var project, zone, name string

	if machine.ObjectMeta.Annotations != nil {
		project = machine.ObjectMeta.Annotations[ProjectAnnotationKey]
		zone = machine.ObjectMeta.Annotations[ZoneAnnotationKey]
		name = machine.ObjectMeta.Annotations[NameAnnotationKey]
	}

	// If the annotations are missing, fall back on providerConfig
	if project == "" || zone == "" || name == "" {
		project = clusterConfig.Project
		zone = machineConfig.Zone
		name = machine.ObjectMeta.Name
	}

	op, err := gce.computeService.InstancesDelete(project, zone, name)
	if err == nil {
		err = gce.waitForOperation(clusterConfig, op)
	}
	if err != nil {
		return gce.handleMachineError(machine, apierrors.DeleteMachine(
			"error deleting GCE instance: %v", err))
	}

	if gce.machineClient != nil {
		// Remove the finalizer
		machine.ObjectMeta.Finalizers = util.Filter(machine.ObjectMeta.Finalizers, clusterv1.MachineFinalizer)
		_, err = gce.machineClient.Update(machine)
	}

	return err
}

func (gce *GCEClient) PostCreate(cluster *clusterv1.Cluster, machines []*clusterv1.Machine) error {
	err := CreateDefaultStorageClass()
	if err != nil {
		return fmt.Errorf("error creating default storage class: %v", err)
	}

	err = gce.CreateIngressControllerServiceAccount(cluster, machines)
	if err != nil {
		return fmt.Errorf("error creating service account for ingress controller: %v", err)
	}

	clusterConfig, err := gce.clusterproviderconfig(cluster.Spec.ProviderConfig)
	if err != nil {
		return fmt.Errorf("Cannot unmarshal cluster's providerConfig field: %v", err)
	}
	err = CreateIngressController(clusterConfig.Project, cluster.Name)
	if err != nil {
		return fmt.Errorf("error creating ingress controller: %v", err)
	}

	return nil
}

func (gce *GCEClient) PostDelete(cluster *clusterv1.Cluster, machines []*clusterv1.Machine) error {
	if err := gce.DeleteMasterNodeServiceAccount(cluster, machines); err != nil {
		return fmt.Errorf("error deleting master node service account: %v", err)
	}
	if err := gce.DeleteWorkerNodeServiceAccount(cluster, machines); err != nil {
		return fmt.Errorf("error deleting worker node service account: %v", err)
	}
	if err := gce.DeleteIngressControllerServiceAccount(cluster, machines); err != nil {
		return fmt.Errorf("error deleting ingress controller service account: %v", err)
	}
	if err := gce.DeleteMachineControllerServiceAccount(cluster, machines); err != nil {
		return fmt.Errorf("error deleting machine controller service account: %v", err)
	}
	return nil
}

func (gce *GCEClient) Update(cluster *clusterv1.Cluster, goalMachine *clusterv1.Machine) error {
	// Before updating, do some basic validation of the object first.
	config, err := gce.machineproviderconfig(goalMachine.Spec.ProviderConfig)
	if err != nil {
		return gce.handleMachineError(goalMachine,
			apierrors.InvalidMachineConfiguration("Cannot unmarshal machine's providerConfig field: %v", err))
	}
	if verr := gce.validateMachine(goalMachine, config); verr != nil {
		return gce.handleMachineError(goalMachine, verr)
	}

	status, err := gce.instanceStatus(goalMachine)
	if err != nil {
		return err
	}

	currentMachine := (*clusterv1.Machine)(status)
	if currentMachine == nil {
		instance, err := gce.instanceIfExists(cluster, goalMachine)
		if err != nil {
			return err
		}
		if instance != nil && instance.Labels[BootstrapLabelKey] != "" {
			glog.Infof("Populating current state for boostrap machine %v", goalMachine.ObjectMeta.Name)
			return gce.updateAnnotations(cluster, goalMachine)
		} else {
			return fmt.Errorf("Cannot retrieve current state to update machine %v", goalMachine.ObjectMeta.Name)
		}
	}

	if !gce.requiresUpdate(currentMachine, goalMachine) {
		return nil
	}

	if util.IsMaster(currentMachine) {
		glog.Infof("Doing an in-place upgrade for master.\n")
		// TODO: should we support custom CAs here?
		err = gce.updateMasterInplace(cluster, currentMachine, goalMachine)
		if err != nil {
			glog.Errorf("master inplace update failed: %v", err)
		}
	} else {
		glog.Infof("re-creating machine %s for update.", currentMachine.ObjectMeta.Name)
		err = gce.Delete(cluster, currentMachine)
		if err != nil {
			glog.Errorf("delete machine %s for update failed: %v", currentMachine.ObjectMeta.Name, err)
		} else {
			err = gce.Create(cluster, goalMachine)
			if err != nil {
				glog.Errorf("create machine %s for update failed: %v", goalMachine.ObjectMeta.Name, err)
			}
		}
	}
	if err != nil {
		return err
	}
	err = gce.updateInstanceStatus(goalMachine)
	return err
}

func (gce *GCEClient) Exists(cluster *clusterv1.Cluster, machine *clusterv1.Machine) (bool, error) {
	i, err := gce.instanceIfExists(cluster, machine)
	if err != nil {
		return false, err
	}
	return (i != nil), err
}

func (gce *GCEClient) GetIP(cluster *clusterv1.Cluster, machine *clusterv1.Machine) (string, error) {
	machineConfig, err := gce.machineproviderconfig(machine.Spec.ProviderConfig)
	if err != nil {
		return "", err
	}

	clusterConfig, err := gce.clusterproviderconfig(cluster.Spec.ProviderConfig)
	if err != nil {
		return "", err
	}

	instance, err := gce.computeService.InstancesGet(clusterConfig.Project, machineConfig.Zone, machine.ObjectMeta.Name)
	if err != nil {
		return "", err
	}

	var publicIP string

	for _, networkInterface := range instance.NetworkInterfaces {
		if networkInterface.Name == "nic0" {
			for _, accessConfigs := range networkInterface.AccessConfigs {
				publicIP = accessConfigs.NatIP
			}
		}
	}
	return publicIP, nil
}

func (gce *GCEClient) GetKubeConfig(cluster *clusterv1.Cluster, master *clusterv1.Machine) (string, error) {
	machineConfig, err := gce.machineproviderconfig(master.Spec.ProviderConfig)
	if err != nil {
		return "", err
	}

	clusterConfig, err := gce.clusterproviderconfig(cluster.Spec.ProviderConfig)
	if err != nil {
		return "", err
	}

	command := "sudo cat /etc/kubernetes/admin.conf"
	result := strings.TrimSpace(util.ExecCommand(
		"gcloud", "compute", "ssh", "--project", clusterConfig.Project,
		"--zone", machineConfig.Zone, master.ObjectMeta.Name, "--command", command, "--", "-q"))
	return result, nil
}

func (gce *GCEClient) updateAnnotations(cluster *clusterv1.Cluster, machine *clusterv1.Machine) error {
	machineConfig, err := gce.machineproviderconfig(machine.Spec.ProviderConfig)
	name := machine.ObjectMeta.Name
	zone := machineConfig.Zone
	if err != nil {
		return gce.handleMachineError(machine,
			apierrors.InvalidMachineConfiguration("Cannot unmarshal machine's providerConfig field: %v", err))
	}

	clusterConfig, err := gce.clusterproviderconfig(cluster.Spec.ProviderConfig)
	project := clusterConfig.Project
	if err != nil {
		return gce.handleMachineError(machine,
			apierrors.InvalidMachineConfiguration("Cannot unmarshal cluster's providerConfig field: %v", err))
	}

	if machine.ObjectMeta.Annotations == nil {
		machine.ObjectMeta.Annotations = make(map[string]string)
	}
	machine.ObjectMeta.Annotations[ProjectAnnotationKey] = project
	machine.ObjectMeta.Annotations[ZoneAnnotationKey] = zone
	machine.ObjectMeta.Annotations[NameAnnotationKey] = name
	_, err = gce.machineClient.Update(machine)
	if err != nil {
		return err
	}
	err = gce.updateInstanceStatus(machine)
	return err
}

// The two machines differ in a way that requires an update
func (gce *GCEClient) requiresUpdate(a *clusterv1.Machine, b *clusterv1.Machine) bool {
	// Do not want status changes. Do want changes that impact machine provisioning
	return !reflect.DeepEqual(a.Spec.ObjectMeta, b.Spec.ObjectMeta) ||
		!reflect.DeepEqual(a.Spec.ProviderConfig, b.Spec.ProviderConfig) ||
		!reflect.DeepEqual(a.Spec.Roles, b.Spec.Roles) ||
		!reflect.DeepEqual(a.Spec.Versions, b.Spec.Versions) ||
		a.ObjectMeta.Name != b.ObjectMeta.Name
}

// Gets the instance represented by the given machine
func (gce *GCEClient) instanceIfExists(cluster *clusterv1.Cluster, machine *clusterv1.Machine) (*compute.Instance, error) {
	identifyingMachine := machine

	// Try to use the last saved status locating the machine
	// in case instance details like the proj or zone has changed
	status, err := gce.instanceStatus(machine)
	if err != nil {
		return nil, err
	}

	if status != nil {
		identifyingMachine = (*clusterv1.Machine)(status)
	}

	// Get the VM via specified location and name
	machineConfig, err := gce.machineproviderconfig(identifyingMachine.Spec.ProviderConfig)
	if err != nil {
		return nil, err
	}

	clusterConfig, err := gce.clusterproviderconfig(cluster.Spec.ProviderConfig)
	if err != nil {
		return nil, err
	}

	instance, err := gce.computeService.InstancesGet(clusterConfig.Project, machineConfig.Zone, identifyingMachine.ObjectMeta.Name)
	if err != nil {
		// TODO: Use formal way to check for error code 404
		if strings.Contains(err.Error(), "Error 404") {
			return nil, nil
		}
		return nil, err
	}

	return instance, nil
}

func (gce *GCEClient) machineproviderconfig(providerConfig clusterv1.ProviderConfig) (*gceconfigv1.GCEMachineProviderConfig, error) {
	var config gceconfigv1.GCEMachineProviderConfig
	err := gce.gceProviderConfigCodec.DecodeFromProviderConfig(providerConfig, &config)
	if err != nil {
		return nil, err
	}
	return &config, nil
}

func (gce *GCEClient) clusterproviderconfig(providerConfig clusterv1.ProviderConfig) (*gceconfigv1.GCEClusterProviderConfig, error) {
	var config gceconfigv1.GCEClusterProviderConfig
	err := gce.gceProviderConfigCodec.DecodeFromProviderConfig(providerConfig, &config)
	if err != nil {
		return nil, err
	}
	return &config, nil
}

func (gce *GCEClient) waitForOperation(c *gceconfigv1.GCEClusterProviderConfig, op *compute.Operation) error {
	glog.Infof("Wait for %v %q...", op.OperationType, op.Name)
	defer glog.Infof("Finish wait for %v %q...", op.OperationType, op.Name)

	start := time.Now()
	ctx, cf := context.WithTimeout(context.Background(), gceTimeout)
	defer cf()

	var err error
	for {
		if err = gce.checkOp(op, err); err != nil || op.Status == "DONE" {
			return err
		}
		glog.V(1).Infof("Wait for %v %q: %v (%d%%): %v", op.OperationType, op.Name, op.Status, op.Progress, op.StatusMessage)
		select {
		case <-ctx.Done():
			return fmt.Errorf("gce operation %v %q timed out after %v", op.OperationType, op.Name, time.Since(start))
		case <-time.After(gceWaitSleep):
		}
		op, err = gce.getOp(c, op)
	}
}

// getOp returns an updated operation.
func (gce *GCEClient) getOp(c *gceconfigv1.GCEClusterProviderConfig, op *compute.Operation) (*compute.Operation, error) {
	return gce.computeService.ZoneOperationsGet(c.Project, path.Base(op.Zone), op.Name)
}

func (gce *GCEClient) checkOp(op *compute.Operation, err error) error {
	if err != nil || op.Error == nil || len(op.Error.Errors) == 0 {
		return err
	}

	var errs bytes.Buffer
	for _, v := range op.Error.Errors {
		errs.WriteString(v.Message)
		errs.WriteByte('\n')
	}
	return errors.New(errs.String())
}

func (gce *GCEClient) updateMasterInplace(cluster *clusterv1.Cluster, oldMachine *clusterv1.Machine, newMachine *clusterv1.Machine) error {
	if oldMachine.Spec.Versions.ControlPlane != newMachine.Spec.Versions.ControlPlane {
		cmd := fmt.Sprintf(
			"curl -sSL https://dl.k8s.io/release/v%s/bin/linux/amd64/kubeadm | sudo tee /usr/bin/kubeadm > /dev/null; " +
			"sudo chmod a+rx /usr/bin/kubeadm", newMachine.Spec.Versions.ControlPlane)
		_, err := gce.remoteSshCommand(cluster, newMachine, cmd)
		if err != nil {
			glog.Infof("remotesshcomand error: %v", err)
			return err
		}

		// TODO: We might want to upgrade kubeadm if the target control plane version is newer.
		// Upgrade control plan.
		cmd = fmt.Sprintf("sudo kubeadm upgrade apply %s -y", "v"+newMachine.Spec.Versions.ControlPlane)
		_, err = gce.remoteSshCommand(cluster, newMachine, cmd)
		if err != nil {
			glog.Infof("remotesshcomand error: %v", err)
			return err
		}
	}

	// Upgrade kubelet.
	if oldMachine.Spec.Versions.Kubelet != newMachine.Spec.Versions.Kubelet {
		cmd := fmt.Sprintf("sudo kubectl drain %s --kubeconfig /etc/kubernetes/admin.conf --ignore-daemonsets", newMachine.Name)
		// The errors are intentionally ignored as master has static pods.
		gce.remoteSshCommand(cluster, newMachine, cmd)
		// Upgrade kubelet to desired version.
		cmd = fmt.Sprintf("sudo apt-get install kubelet=%s", newMachine.Spec.Versions.Kubelet+"-00")
		_, err := gce.remoteSshCommand(cluster, newMachine, cmd)
		if err != nil {
			glog.Infof("remotesshcomand error: %v", err)
			return err
		}
		cmd = fmt.Sprintf("sudo kubectl uncordon %s --kubeconfig /etc/kubernetes/admin.conf", newMachine.Name)
		_, err = gce.remoteSshCommand(cluster, newMachine, cmd)
		if err != nil {
			glog.Infof("remotesshcomand error: %v", err)
			return err
		}
	}

	return nil
}

func (gce *GCEClient) validateMachine(machine *clusterv1.Machine, config *gceconfigv1.GCEMachineProviderConfig) *apierrors.MachineError {
	if machine.Spec.Versions.Kubelet == "" {
		return apierrors.InvalidMachineConfiguration("spec.versions.kubelet can't be empty")
	}
	return nil
}

// If the GCEClient has a client for updating Machine objects, this will set
// the appropriate reason/message on the Machine.Status. If not, such as during
// cluster installation, it will operate as a no-op. It also returns the
// original error for convenience, so callers can do "return handleMachineError(...)".
func (gce *GCEClient) handleMachineError(machine *clusterv1.Machine, err *apierrors.MachineError) error {
	if gce.machineClient != nil {
		reason := err.Reason
		message := err.Message
		machine.Status.ErrorReason = &reason
		machine.Status.ErrorMessage = &message
		gce.machineClient.UpdateStatus(machine)
	}

	glog.Errorf("Machine error: %v", err.Message)
	return err
}

func (gce *GCEClient) getImagePath(img string) (imagePath string) {
	defaultImg := "projects/ubuntu-os-cloud/global/images/family/ubuntu-1710"

	// A full image path must match the regex format. If it doesn't, we will fall back to a default base image.
	matches := regexp.MustCompile("projects/(.+)/global/images/(family/)*(.+)").FindStringSubmatch(img)
	if matches != nil {
		// Check to see if the image exists in the given path. The presence of "family" in the path dictates which API call we need to make.
		project, family, name := matches[1], matches[2], matches[3]
		var err error
		if family == "" {
			_, err = gce.computeService.ImagesGet(project, name)
		} else {
			_, err = gce.computeService.ImagesGetFromFamily(project, name)
		}

		if err == nil {
			return img
		}
	}

	// Otherwise, fall back to the base image.
	glog.Infof("Could not find image at %s. Defaulting to %s.", img, defaultImg)
	return defaultImg
}

func newDisks(config *gceconfigv1.GCEMachineProviderConfig, zone string, imagePath string, minDiskSizeGb int64) []*compute.AttachedDisk {
	var disks []*compute.AttachedDisk
	for idx, disk := range config.Disks {
		diskSizeGb := disk.InitializeParams.DiskSizeGb
		d := compute.AttachedDisk{
			AutoDelete: true,
			InitializeParams: &compute.AttachedDiskInitializeParams{
				DiskSizeGb:  diskSizeGb,
				DiskType:    fmt.Sprintf("zones/%s/diskTypes/%s", zone, disk.InitializeParams.DiskType),
			},
		}
		if idx == 0 {
			d.InitializeParams.SourceImage = imagePath
			d.Boot = true
			if diskSizeGb < minDiskSizeGb {
				glog.Info("increasing disk size to %v gb, the supplied disk size of %v gb is below the minimum", minDiskSizeGb, diskSizeGb)
				d.InitializeParams.DiskSizeGb = minDiskSizeGb
			}
		}
		disks = append(disks, &d)
	}
	return disks
}

// Just a temporary hack to grab a single range from the config.
func getSubnet(netRange clusterv1.NetworkRanges) string {
	if len(netRange.CIDRBlocks) == 0 {
		return ""
	}
	return netRange.CIDRBlocks[0]
}

func getOrNewComputeService(params MachineActuatorParams) (GCEClientComputeService, error) {
	if params.ComputeService != nil {
		return params.ComputeService, nil
	}
	// The default GCP client expects the environment variable
	// GOOGLE_APPLICATION_CREDENTIALS to point to a file with service credentials.
	client, err := google.DefaultClient(context.TODO(), compute.ComputeScope)
	if err != nil {
		return nil, err
	}
	computeService, err := clients.NewComputeService(client)
	if err != nil {
		return nil, err
	}
	return computeService, nil
}

func (gce *GCEClient) getMetadata(cluster *clusterv1.Cluster, machine *clusterv1.Machine, clusterConfig *gceconfigv1.GCEClusterProviderConfig, configParams *machinesetup.ConfigParams) (*compute.Metadata, error) {
	var metadataMap map[string]string
	if machine.Spec.Versions.Kubelet == "" {
		return nil, errors.New("invalid master configuration: missing Machine.Spec.Versions.Kubelet")
	}
	machineSetupConfigs, err := gce.machineSetupConfigGetter.GetMachineSetupConfig()
	if err != nil {
		return nil, err
	}
	machineSetupMetadata, err := machineSetupConfigs.GetMetadata(configParams)
	if err != nil {
		return nil, err
	}
	if util.IsMaster(machine) {
		if machine.Spec.Versions.ControlPlane == "" {
			return nil, gce.handleMachineError(machine, apierrors.InvalidMachineConfiguration(
				"invalid master configuration: missing Machine.Spec.Versions.ControlPlane"))
		}
		var err error
		metadataMap, err = masterMetadata(gce.kubeadmToken, cluster, machine, clusterConfig.Project, &machineSetupMetadata)
		if err != nil {
			return nil, err
		}
		ca := gce.certificateAuthority
		if ca != nil {
			metadataMap["ca-cert"] = base64.StdEncoding.EncodeToString(ca.Certificate)
			metadataMap["ca-key"] = base64.StdEncoding.EncodeToString(ca.PrivateKey)
		}
	} else {
		if len(cluster.Status.APIEndpoints) == 0 {
			return nil, errors.New("invalid cluster state: cannot create a Kubernetes node without an API endpoint")
		}
		var err error
		metadataMap, err = nodeMetadata(gce.kubeadmToken, cluster, machine, clusterConfig.Project, &machineSetupMetadata)
		if err != nil {
			return nil, err
		}
	}
	var metadataItems []*compute.MetadataItems
	for k, v := range metadataMap {
		v := v // rebind scope to avoid loop aliasing below
		metadataItems = append(metadataItems, &compute.MetadataItems{
			Key:   k,
			Value: &v,
		})
	}
	metadata := compute.Metadata{
		Items: metadataItems,
	}
	return &metadata, nil
}

// TODO: We need to change this when we create dedicated service account for apiserver/controller
// pod.
//
func CreateExtApiServerRoleBinding() error {
	return run("kubectl", "create", "rolebinding",
		"-n", "kube-system", "machine-controller", "--role=extension-apiserver-authentication-reader",
		"--serviceaccount=default:default")
}
