package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	vsv1alpha "github.com/coreweave/virtual-server/api/v1alpha1"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"kubevirt.io/client-go/kubecli"
)

type ReadyResponse string

const (
	// VSReady indicates that the VirtualServer is ready
	VSReady ReadyResponse = "Ready"
	// VSStopped indicates that the VirtualServer is stopped
	VSStopped ReadyResponse = "Stopped"
	// VSUnknown indicates that the VirtualServer has unknown state or unacceptable by the Ready function
	VSUnknown ReadyResponse = "Unknown"
)

// Ready waits until Virtual Server reach expected status
func Ready(namespace, name string, c client.Client) ReadyResponse {
	for {
		vs := &vsv1alpha.VirtualServer{}
		err := c.Get(context.Background(), client.ObjectKey{
			Namespace: namespace,
			Name:      name,
		}, vs)

		// VirtualServer has yet to receive any status
		if err != nil {
			continue
		}

		cond := vs.GetReadyStatus()
		if cond == nil {
			return VSUnknown
		} else if cond.Reason == string(vsv1alpha.VSConditionReasonReady) &&
			cond.Type == string(vsv1alpha.VSConditionTypeReady) &&
			cond.Status == "True" {
			fmt.Printf("Network:\n\tinternalIP: %s\n\texternal IP: %s\n\tfloating IPs:",
				vs.Status.InternalIP(),
				vs.Status.ExternalIP(),
			)
			for service, ip := range vs.Status.FloatingIPs() {
				fmt.Printf("\t\t%s: %s\n", service, ip)
			}
			fmt.Printf("\n")
			return VSReady
		} else if cond.Reason == string(vsv1alpha.VSConditionReasonStopped) &&
			cond.Type == string(vsv1alpha.VSConditionTypeReady) &&
			cond.Status == "False" {
			return VSStopped
		}
		time.Sleep(2)
	}
}

func main() {
	name := "my-test-virtual-server"

	// Get namespace or use default
	namespace, envExist := os.LookupEnv("NAMESPACE")
	if envExist == false {
		namespace = "default"
	}
	// Uses the value of the KUBECONFIG environment variable as a filepath to a kube config file
	c, err := client.New(config.GetConfigOrDie(), client.Options{})
	if err != nil {
		log.Fatalf("Failed to create client\n")
	}

	username, usernameExist := os.LookupEnv("USERNAME")
	password, passwordExist := os.LookupEnv("PASSWORD")

	if !usernameExist || passwordExist {
		log.Fatalf("Required environment variables USERNAME and PASSWORD not found")
	}

	vsv1alpha.AddToScheme(c.Scheme())

	// prepare config for kubevirt client, you need to set env variable, KUBECONFIG=<path-to-kubeconfig>/.kubeconfig
	kubevirtClientConfig := kubecli.DefaultClientConfig(&pflag.FlagSet{})

	// get the kubevirt client, using which kubevirt resources can be managed.
	kubevirtClient, err := kubecli.GetKubevirtClientFromClientConfig(kubevirtClientConfig)
	if err != nil {
		log.Fatalf("Cannot obtain KubeVirt client: %v\n", err)
	}

	// Create a new VirtualServer with the name "my-virtual-server" to be deployed in the "default" namespace
	virtualServer := vsv1alpha.NewVirtualServer(name, namespace)

	// Set the region the VirtualServer will be deployed to
	virtualServer.SetRegion("ORD1")

	// Specify the VirtualServer operating system
	virtualServer.SetOS(vsv1alpha.VirtualServerOSTypeLinux)

	// Set a GPU type request for the VirtualServer
	virtualServer.SetGPUType("Quadro_RTX_4000")

	// Set the number of GPUs to request for the VirtualServer
	virtualServer.SetGPUCount(1)

	// Set the cpu core count for the VirtualServer
	// GPU type and CPU type are mutually exclusive i.e. CPU type cannot be specified when GPU type is selected.
	virtualServer.SetCPUCount(2)

	// CPU type is selected automatically based on GPU type.
	//virtualServer.SetCPUType("amd-epyc-rome")
	// Set the memory request for the VirtualServer
	virtualServer.SetMemory("16Gi")

	// Add user
	virtualServer.AddUser(vsv1alpha.VirtualServerUser{
		Username: username,
		Password: password,
	})

	// Configure the root filesystem of the VirtualServer to clone a preexisting PVC namedubuntu1804-docker-master-20210210-ord1
	// sourced in the vd-images namespace
	err = virtualServer.ConfigureStorageRootWithPVCSource(vsv1alpha.VirtualServerStorageRootPVCSource{
		Size:             "40Gi",
		PVCName:          "ubuntu1804-docker-master-20210210-ord1",
		PVCNamespace:     "vd-images",
		StorageClassName: "ceph-ssd-2-replica",
	})
	if err != nil {
		log.Fatalf("Cound not configure root filesystem\n")
	}
	// Add a floating IP to the VirtualServer
	//virtualServer.AddFloatingIP("my-floating-ip-service")
	// Enable direct balancer
	virtualServer.DirectAttachLoadBalancerIP(true)

	// Expose tcp ports 22 and 443 on the VirtualServer
	virtualServer.ExposeTCPPorts([]int32{22, 443})

	// Expose a single udp port 4172 on the VirtualServer
	virtualServer.ExposeUDPPort(4172)

	// Expose service as public
	virtualServer.EnablePublicIP(true)

	// Set the VirtualServer to start as soon as it is created
	virtualServer.InitializeRunning(true)

	// Create an example pvc to be added as an additional file system
	pvc := buildPVC("example-pvc", namespace, resource.MustParse("256Gi"))
	if err := c.Create(context.Background(), pvc); err != nil {
		log.Fatalf("Could not create example pvc\nReason: %s", err.Error())
	}

	// Add the example PVC as a file system to the Virtual Server
	virtualServer.AddPVCFileSystem("example-storage", pvc.Name, false)

	service := buildFloatingIPService("example-floating-ip-service", namespace)
	if err := c.Create(context.Background(), service); err != nil {
		log.Fatalf("Could not create example floatingIP service\nReason: %s", err.Error())
	}

	// Add the example floatingIP service to the VirtualServer
	virtualServer.AddFloatingIP(service.Name)

	// Delete Virtual Server if already exists
	err = c.Delete(context.Background(), virtualServer)
	if err != nil {
		if apierrors.IsNotFound(err) == true {
			fmt.Printf("VirtualServer %s in namespace %s already deleted\n", name, namespace)
		} else {
			log.Fatalf("Failed to create VirtualServer\nReason: %s", err.Error())
		}
	}

	// Create a new Virtual Server
	err = c.Create(context.Background(), virtualServer)
	if err != nil {
		log.Fatalf("Failed to create VirtualServer\nReason: %s", err.Error())
	}

	// Wait until Virtual Server is ready
	fmt.Printf("VirtualServer status: %s\n", Ready(namespace, name, c))

	err = kubevirtClient.VirtualMachine(namespace).Stop(name)
	if err != nil {
		log.Fatalf("Cannot stop virtual sever %s in namespace %s, err: %v\n", name, namespace, err)
	}

	// Wait until Virtual Server is stopped
	fmt.Printf("VirtualServer status: %s\n", Ready(namespace, name, c))

	err = c.Delete(context.Background(), virtualServer)
	if err != nil {
		log.Fatalf("Failed to delete VirtualServer\nReason: %s", err.Error())
	}
}

func buildPVC(name string, namespace string, size resource.Quantity) *corev1.PersistentVolumeClaim {
	pvcVolumeMode := corev1.PersistentVolumeBlock
	pvcStorageClass := "block-nvme-ewr1"
	pvc := corev1.PersistentVolumeClaim{}
	pvc.ObjectMeta = metav1.ObjectMeta{
		Name:      name,
		Namespace: namespace,
	}
	pvc.Spec = corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{
			corev1.ReadWriteOnce,
		},
		VolumeMode:       &pvcVolumeMode,
		StorageClassName: &pvcStorageClass,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceStorage: size,
			},
		},
	}
	return &pvc
}

func buildFloatingIPService(name string, namespace string) *corev1.Service {
	service := corev1.Service{}
	service.ObjectMeta = metav1.ObjectMeta{
		Name:      name,
		Namespace: namespace,
	}
	service.Spec = corev1.ServiceSpec{
		Ports: []corev1.ServicePort{
			{
				Name: "p0",
				Port: 1,
			},
		},
		Type: corev1.ServiceTypeLoadBalancer,
	}
	return &service
}
