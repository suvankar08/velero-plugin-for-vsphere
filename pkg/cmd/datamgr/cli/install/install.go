/*
Copyright 2020 the Velero contributors.

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

package install

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/vmware-tanzu/velero-plugin-for-vsphere/pkg/cmd"
	"github.com/vmware-tanzu/velero-plugin-for-vsphere/pkg/install"
	"github.com/vmware-tanzu/velero-plugin-for-vsphere/pkg/utils"
	"github.com/vmware-tanzu/velero/pkg/client"
	"github.com/vmware-tanzu/velero/pkg/cmd/util/flag"
	"github.com/vmware-tanzu/velero/pkg/cmd/util/output"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"io/ioutil"
	"os"
	"path/filepath"

	kubeutil "github.com/vmware-tanzu/velero/pkg/util/kube"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type InstallOptions struct {
	Namespace      string
	Image          string
	BucketName     string
	Prefix         string
	ProviderName   string
	PodAnnotations flag.Map
	PodCPURequest  string
	PodMemRequest  string
	PodCPULimit    string
	PodMemLimit    string
	SecretFile     string
	NoSecret       bool
	DryRun         bool
}

func (o *InstallOptions) BindFlags(flags *pflag.FlagSet) {
	flags.StringVar(&o.ProviderName, "provider", o.ProviderName, "provider name for backup and volume storage")
	flags.StringVar(&o.BucketName, "bucket", o.BucketName, "name of the object storage bucket where backups should be stored")
	flags.StringVar(&o.SecretFile, "secret-file", o.SecretFile, "file containing credentials for backup and volume provider. If not specified, --no-secret must be used for confirmation. Optional.")
	flags.BoolVar(&o.NoSecret, "no-secret", o.NoSecret, "flag indicating if a secret should be created. Must be used as confirmation if --secret-file is not provided. Optional.")
	flags.StringVar(&o.Image, "image", o.Image, "image to use for the Velero and datamgr server pods. Optional.")
	flags.StringVar(&o.Prefix, "prefix", o.Prefix, "prefix under which all Velero data should be stored within the bucket. Optional.")
	flags.Var(&o.PodAnnotations, "pod-annotations", "annotations to add to the Velero and restic pods. Optional. Format is key1=value1,key2=value2")
	flags.StringVar(&o.PodCPURequest, "datamgr-pod-cpu-request", o.PodCPURequest, `CPU request for Datamgr pod. A value of "0" is treated as unbounded. Optional.`)
	flags.StringVar(&o.PodMemRequest, "datamgr-pod-mem-request", o.PodMemRequest, `memory request for Datamgr pod. A value of "0" is treated as unbounded. Optional.`)
	flags.StringVar(&o.PodCPULimit, "datamgr-pod-cpu-limit", o.PodCPULimit, `CPU limit for Datamgr pod. A value of "0" is treated as unbounded. Optional.`)
	flags.StringVar(&o.PodMemLimit, "datamgr-pod-mem-limit", o.PodMemLimit, `memory limit for Datamgr pod. A value of "0" is treated as unbounded. Optional.`)
	flags.BoolVar(&o.DryRun, "dry-run", o.DryRun, "generate resources, but don't send them to the cluster. Use with -o. Optional.")
}

func NewInstallOptions() *InstallOptions {
	return &InstallOptions{
		Namespace:      "velero",
		Image:          install.DefaultDatamgrImage,
		PodAnnotations: flag.NewMap(),
		PodCPURequest:  install.DefaultDatamgrPodCPURequest,
		PodMemRequest:  install.DefaultDatamgrPodMemRequest,
		PodCPULimit:    install.DefaultDatamgrPodCPULimit,
		PodMemLimit:    install.DefaultDatamgrPodMemLimit,
	}
}

// AsDatamgrOptions translates the values provided at the command line into values used to instantiate Kubernetes resources
func (o *InstallOptions) AsDatamgrOptions() (*install.PodOptions, error) {
	var secretData []byte
	if o.SecretFile != "" && !o.NoSecret {
		realPath, err := filepath.Abs(o.SecretFile)
		if err != nil {
			return nil, err
		}
		secretData, err = ioutil.ReadFile(realPath)
		if err != nil {
			return nil, err
		}
	}

	podResources, err := kubeutil.ParseResourceRequirements(o.PodCPURequest, o.PodMemRequest, o.PodCPULimit, o.PodMemLimit)
	if err != nil {
		return nil, err
	}

	return &install.PodOptions{
		Namespace:      o.Namespace,
		Image:          o.Image,
		ProviderName:   o.ProviderName,
		Bucket:         o.BucketName,
		Prefix:         o.Prefix,
		PodAnnotations: o.PodAnnotations.Data(),
		PodResources:   podResources,
		SecretData:     secretData,
		SecretAdd:      true,
	}, nil
}

func NewCommand(f client.Factory) *cobra.Command {
	o := NewInstallOptions()

	c := &cobra.Command{
		Use:   "install",
		Short: "Install data manager",
		Long:  "Install data manager",
		Run: func(c *cobra.Command, args []string) {
			cmd.CheckError(o.Complete(args, f))
			cmd.CheckError(o.Run(c, f))
		},
	}

	o.BindFlags(c.Flags())
	output.BindFlags(c.Flags())
	output.ClearOutputFlagDefault(c)

	return c
}

func (o *InstallOptions) Run(c *cobra.Command, f client.Factory) error {
	skipDataMgr := false
	// In case of Guest or Supervisor cluster, skip installing data manager
	clusterFlavor, _ := utils.GetClusterFlavor(nil)
	if clusterFlavor == utils.TkgGuest || clusterFlavor == utils.Supervisor {
		fmt.Printf("The Cluster Flavor: %s\n. Skipping data manager installation.", clusterFlavor)
		skipDataMgr = true
	}
	isLocalMode := utils.GetBool(install.DefaultDatamgrImageLocalMode, false)
	fmt.Printf("The Image LocalMode: %v\n", isLocalMode)
	if isLocalMode || skipDataMgr {
		fmt.Println("Local mode set, skipping data manager installation")
		return nil
	}

	// Check vSphere CSI driver version
	isCSIInstalled, isVersionOk, err := cmd.CheckCSIInstalled(f)
	if err != nil {
		fmt.Println("CSI driver check failed")
		isCSIInstalled = false
		isVersionOk = false
	}
	if !isCSIInstalled {
		fmt.Println("Velero Plug-in for vSphere requires vSphere CSI/CNS and vSphere 6.7U3 to function. Please install the vSphere CSI/CNS driver")
	}
	if !isVersionOk {
		fmt.Printf("vSphere CSI driver version is prior to %s. Velero Plug-in for vSphere requires CSI driver version to be %s or above\n", utils.CsiMinVersion, utils.CsiMinVersion)
	}

	// Check velero version
	veleroVersion, err := cmd.GetVeleroVersion(f)
	if err != nil || veleroVersion == "" {
		fmt.Println("Failed to get velero version.")
	} else {
		if cmd.CompareVersion(veleroVersion, utils.VeleroMinVersion) == -1 {
			fmt.Printf("WARNING: Velero version %s is prior to %s. Velero Plug-in for vSphere requires velero version to be %s or above.\n", veleroVersion, utils.VeleroMinVersion, utils.VeleroMinVersion)
		}
	}

	// Check velero vsphere plugin image repo
	err = o.CheckPluginImageRepo(f)
	if err != nil {
		fmt.Printf("Failed to check plugin image repo, error msg: %s. Using default image %s\n", err.Error(), o.Image)
	} else {
		fmt.Printf("Using image %s.\n", o.Image)
	}

	vo, err := o.AsDatamgrOptions()
	if err != nil {
		return err
	}

	var resources *unstructured.UnstructuredList
	resources, err = install.AllDatamgrResources(vo, true)
	if err != nil {
		return err
	}

	if _, err := output.PrintWithFormat(c, resources); err != nil {
		return err
	}

	if o.DryRun {
		return nil
	}
	dynamicClient, err := f.DynamicClient()
	if err != nil {
		return err
	}
	factory := client.NewDynamicFactory(dynamicClient)

	nNodes, err := o.getNumberOfNodes(f)
	if err != nil {
		return errors.Wrap(err, "Error while getting number of nodes in kubernetes cluster")
	}

	errorMsg := fmt.Sprintf("\n\nError installing data manager. Use `kubectl logs daemonset/%s -n %s` to check the logs",
		utils.DataManagerForPlugin, o.Namespace)

	err = install.Install(factory, resources, os.Stdout)
	if err != nil {
		return errors.Wrap(err, errorMsg)
	}

	fmt.Println("Waiting for data manager daemonset to be ready.")

	if _, err = install.DaemonSetIsReady(factory, o.Namespace, nNodes); err != nil {
		return errors.Wrap(err, errorMsg)
	}

	fmt.Printf("data manager is installed! ⛵ Use 'kubectl logs daemonset/%s -n %s' to view the status.\n",
		utils.DataManagerForPlugin, o.Namespace)

	return nil
}

func (o *InstallOptions) CheckPluginImageRepo(f client.Factory) error {
	clientset, err := f.KubeClient()
	if err != nil {
		errMsg := fmt.Sprint("Failed to get clientset.")
		return errors.New(errMsg)
	}
	deployment, err := clientset.AppsV1().Deployments(o.Namespace).Get(utils.VeleroDeployment, metav1.GetOptions{})
	if err != nil {
		errMsg := fmt.Sprintf("Failed to get velero deployment in namespace %s", o.Namespace)
		return errors.New(errMsg)
	}

	repo := ""
	tag := ""
	image := ""
	for _, container := range deployment.Spec.Template.Spec.InitContainers {
		if strings.Contains(container.Image, utils.VeleroPluginForVsphere) {
			image = container.Image
			repo = utils.GetRepo(container.Image)
			tag = strings.Split(container.Image, ":")[1]
			break
		}
	}

	if repo != "" && tag != "" {
		o.Image = repo + "/" + utils.DataManagerForPlugin + ":" + tag
		return nil
	} else {
		errMsg := fmt.Sprintf("Failed to get repo and tag from velero plugin image %s.", image)
		return errors.New(errMsg)
	}
}

//Complete completes options for a command.
func (o *InstallOptions) Complete(args []string, f client.Factory) error {
	fileName := "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

	_, err := os.Stat(fileName)
	if os.IsNotExist(err) {
		// If the file isn't there, just return an empty map
		fmt.Printf("No namespace specified in the namespace file, %v\n", fileName)
		return nil
	}
	if err != nil {
		// For any other Stat() error, return it
		return errors.WithStack(err)
	}

	content, err := ioutil.ReadFile(fileName)
	if err != nil {
		return errors.WithStack(err)
	}
	namespace := string(content)

	config, err := rest.InClusterConfig()
	if err != nil {
		fmt.Println("Failed to get k8s inClusterConfig")
		return errors.WithStack(err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Println("Failed to get k8s clientset with the given config")
		return errors.WithStack(err)
	}

	_, err = clientset.CoreV1().Namespaces().Get(namespace, metav1.GetOptions{})
	if err != nil {
		fmt.Printf("Failed to get the specified namespace, %v, for velero in the current k8s cluster\n", namespace)
		return errors.WithStack(err)
	}
	o.Namespace = namespace
	fmt.Printf("velero is running in the namespace, %v\n", namespace)

	return nil
}

func (o *InstallOptions) getNumberOfNodes(f client.Factory) (int, error) {
	clientset, err := f.KubeClient()
	if err != nil {
		return 0, err
	}

	nodeList, err := clientset.CoreV1().Nodes().List(metav1.ListOptions{})
	if err != nil {
		return 0, err
	}

	return len(nodeList.Items), nil
}
