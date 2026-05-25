package helper

import (
	"fmt"
	"maps"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rancher-sandbox/ele-testhelpers/tools"

	"github.com/rancher/hosted-providers-e2e/hosted/helpers"

	"github.com/epinio/epinio/acceptance/helpers/proc"
	"github.com/pkg/errors"
	"github.com/rancher/shepherd/clients/rancher"
	management "github.com/rancher/shepherd/clients/rancher/generated/management/v3"
	"github.com/rancher/shepherd/extensions/clusters"
	"github.com/rancher/shepherd/extensions/clusters/eks"
	"github.com/rancher/shepherd/pkg/config"
	namegen "github.com/rancher/shepherd/pkg/namegenerator"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/pointer"
)

// CreateEKSHostedCluster is a helper function that creates an EKS hosted cluster
func CreateEKSHostedCluster(client *rancher.Client, displayName, cloudCredentialID, kubernetesVersion, region string, updateFunc func(clusterConfig *eks.ClusterConfig)) (*management.Cluster, error) {
	var eksClusterConfig eks.ClusterConfig
	config.LoadConfig(eks.EKSClusterConfigConfigurationFileKey, &eksClusterConfig)
	eksClusterConfig.Region = region
	eksClusterConfig.Tags = helpers.GetCommonMetadataLabels()
	eksClusterConfig.KubernetesVersion = &kubernetesVersion

	if updateFunc != nil {
		updateFunc(&eksClusterConfig)
	}
	return eks.CreateEKSHostedCluster(client, displayName, cloudCredentialID, eksClusterConfig, false, false, false, false, nil)
}

func ImportEKSHostedCluster(client *rancher.Client, displayName, cloudCredentialID, region string) (*management.Cluster, error) {
	cluster := &management.Cluster{
		DockerRootDir: "/var/lib/docker",
		EKSConfig: &management.EKSClusterConfigSpec{
			AmazonCredentialSecret: cloudCredentialID,
			DisplayName:            displayName,
			Imported:               true,
			Region:                 region,
		},
		Name: displayName,
	}

	clusterResp, err := client.Management.Cluster.Create(cluster)
	if err != nil {
		return nil, err
	}
	return clusterResp, err
}

// DeleteEKSHostCluster deletes the EKS cluster
func DeleteEKSHostCluster(cluster *management.Cluster, client *rancher.Client) error {
	return client.Management.Cluster.Delete(cluster)

}

// UpgradeClusterKubernetesVersion upgrades the k8s version to the value defined by upgradeToVersion.
// if checkClusterConfig is set to true, it will validate that the cluster control plane has been upgrade successfully
func UpgradeClusterKubernetesVersion(cluster *management.Cluster, upgradeToVersion string, client *rancher.Client, checkClusterConfig bool) (*management.Cluster, error) {
	upgradedCluster := cluster
	currentVersion := *cluster.EKSConfig.KubernetesVersion
	upgradedCluster.EKSConfig.KubernetesVersion = &upgradeToVersion

	cluster, err := client.Management.Cluster.Update(cluster, &upgradedCluster)
	Expect(err).To(BeNil())

	if checkClusterConfig {
		// Check if the desired config is set correctly
		Expect(*cluster.EKSConfig.KubernetesVersion).To(Equal(upgradeToVersion))
		// ensure nodegroup version is still the same when config is applied
		for _, ng := range *cluster.EKSConfig.NodeGroups {
			Expect(*ng.Version).To(Equal(currentVersion))
		}

		// Check if the desired config has been applied in Rancher
		// Check if EKSConfig has correct KubernetesVersion after upgrade (Ref: eks-operator/issues/668)
		Eventually(func() bool {
			ginkgo.GinkgoLogr.Info("Waiting for k8s upgrade to appear in EKSStatus.UpstreamSpec & EKSConfig ...")
			cluster, err = client.Management.Cluster.ByID(cluster.ID)
			Expect(err).To(BeNil())
			return *cluster.EKSStatus.UpstreamSpec.KubernetesVersion == upgradeToVersion && *cluster.EKSConfig.KubernetesVersion == upgradeToVersion
		}, tools.SetTimeout(15*time.Minute), 30*time.Second).Should(BeTrue())

		// ensure nodegroup version is same in Rancher
		for _, ng := range *cluster.EKSStatus.UpstreamSpec.NodeGroups {
			Expect(*ng.Version).To(Equal(currentVersion))
		}
	}
	return cluster, nil
}

// UpgradeNodeKubernetesVersion upgrades the k8s version of nodegroup to the value defined by upgradeToVersion.
// if wait is set to true, it will wait until the cluster finishes upgrading;
// if checkClusterConfig is set to true, it will validate that nodegroup has been upgraded successfully
// if useEksctl is set to true, nodegroup will be upgraded using eksctl utility instead of updating it from Rancher
func UpgradeNodeKubernetesVersion(cluster *management.Cluster, upgradeToVersion string, client *rancher.Client, wait, checkClusterConfig, useEksctl bool) (*management.Cluster, error) {
	var err error

	if !useEksctl {
		upgradedCluster := cluster
		configNodeGroups := *upgradedCluster.EKSConfig.NodeGroups
		for i := range configNodeGroups {
			configNodeGroups[i].Version = &upgradeToVersion
		}

		cluster, err = client.Management.Cluster.Update(cluster, &upgradedCluster)
		Expect(err).To(BeNil())

		if wait {
			err = clusters.WaitClusterToBeUpgraded(client, cluster.ID)
			Expect(err).To(BeNil())
		}
	} else {
		// Upgrade Nodegroup using eksctl due to custom Launch template
		for _, ng := range *cluster.EKSConfig.NodeGroups {
			err = UpgradeEKSNodegroupOnAWS(helpers.GetEKSRegion(), cluster.EKSConfig.DisplayName, *ng.NodegroupName, upgradeToVersion)
			Expect(err).To(BeNil())
		}
	}

	if checkClusterConfig {
		Eventually(func() bool {
			// Check if the desired config has been applied
			cluster, err = client.Management.Cluster.ByID(cluster.ID)
			Expect(err).To(BeNil())
			ginkgo.GinkgoLogr.Info("waiting for the nodegroup upgrade to appear in EKSStatus.UpstreamSpec ...")
			for _, ng := range *cluster.EKSStatus.UpstreamSpec.NodeGroups {
				if ng.Version == nil || *ng.Version != upgradeToVersion {
					return false
				}
			}
			return true
		}, tools.SetTimeout(15*time.Minute), 30*time.Second).Should(BeTrue())
	}

	// Ensure nodegroup version is correct in Rancher after upgrade
	for _, ng := range *cluster.EKSConfig.NodeGroups {
		Expect(*ng.Version).To(Equal(upgradeToVersion))
	}

	return cluster, nil
}

// AddNodeGroup adds a nodegroup to the list; it uses the nodegroup template defined in CATTLE_TEST_CONFIG file
// if checkClusterConfig is set to true, it will validate that nodegroup has been added successfully
func AddNodeGroup(cluster *management.Cluster, increaseBy int, client *rancher.Client, wait, checkClusterConfig bool) (*management.Cluster, error) {
	upgradedCluster := cluster
	currentNodeGroupNumber := len(*cluster.EKSConfig.NodeGroups)

	// Workaround for eks-operator/issues/406
	// We use management.EKSClusterConfigSpec instead of the usual eks.ClusterConfig to unmarshal the data without the need of a lot of post-processing.
	var eksClusterConfig management.EKSClusterConfigSpec
	config.LoadConfig(eks.EKSClusterConfigConfigurationFileKey, &eksClusterConfig)
	nodeGroups := *eksClusterConfig.NodeGroups
	ngTemplate := nodeGroups[0]

	updateNodeGroupsList := *cluster.EKSConfig.NodeGroups
	for i := 1; i <= increaseBy; i++ {
		newNodeGroup := management.NodeGroup{
			NodegroupName: pointer.String(namegen.AppendRandomString("ng")),
			DesiredSize:   ngTemplate.DesiredSize,
			DiskSize:      ngTemplate.DiskSize,
			InstanceType:  ngTemplate.InstanceType,
			MaxSize:       ngTemplate.MaxSize,
			MinSize:       ngTemplate.MinSize,
		}
		updateNodeGroupsList = append([]management.NodeGroup{newNodeGroup}, updateNodeGroupsList...)
	}
	upgradedCluster.EKSConfig.NodeGroups = &updateNodeGroupsList

	cluster, err := client.Management.Cluster.Update(cluster, &upgradedCluster)
	Expect(err).To(BeNil())

	if checkClusterConfig {
		// Check if the desired config is set correctly
		Expect(len(*cluster.EKSConfig.NodeGroups)).Should(BeNumerically("==", currentNodeGroupNumber+increaseBy))
		for i, ng := range *cluster.EKSConfig.NodeGroups {
			Expect(ng.NodegroupName).To(Equal(updateNodeGroupsList[i].NodegroupName))
		}
	}

	if wait {
		err = clusters.WaitClusterToBeUpgraded(client, cluster.ID)
		Expect(err).To(BeNil())
	}

	if checkClusterConfig {
		// Check if the desired config has been applied in Rancher
		Eventually(func() int {
			ginkgo.GinkgoLogr.Info("Waiting for the total nodegroup count to increase in EKSStatus.UpstreamSpec ...")
			cluster, err = client.Management.Cluster.ByID(cluster.ID)
			Expect(err).To(BeNil())
			return len(*cluster.EKSStatus.UpstreamSpec.NodeGroups)
		}, tools.SetTimeout(15*time.Minute), 10*time.Second).Should(BeNumerically("==", currentNodeGroupNumber+increaseBy))

		for i, ng := range *cluster.EKSStatus.UpstreamSpec.NodeGroups {
			Expect(ng.NodegroupName).To(Equal(updateNodeGroupsList[i].NodegroupName))
		}
	}

	return cluster, nil
}

// AddNodeGroupToConfig adds a nodegroup to the list; it uses the nodegroup template defined in CATTLE_TEST_CONFIG file
func AddNodeGroupToConfig(eksClusterConfig eks.ClusterConfig, ngCount int) (eks.ClusterConfig, error) {

	var updateNodeGroupsList []eks.NodeGroupConfig
	ngTemplate := *eksClusterConfig.NodeGroupsConfig

	for i := 1; i <= ngCount; i++ {
		newNodeGroup := ngTemplate[0]
		newNodeGroup.NodegroupName = pointer.String(namegen.AppendRandomString(*ngTemplate[0].NodegroupName))
		updateNodeGroupsList = append([]eks.NodeGroupConfig{newNodeGroup}, updateNodeGroupsList...)
	}
	eksClusterConfig.NodeGroupsConfig = &updateNodeGroupsList

	return eksClusterConfig, nil
}

// DeleteNodeGroup deletes a nodegroup from the list
// if checkClusterConfig is set to true, it will validate that nodegroup has been deleted successfully
// TODO: Modify this method to delete a custom qty of DeleteNodeGroup, perhaps by adding an `decreaseBy int` arg
func DeleteNodeGroup(cluster *management.Cluster, client *rancher.Client, wait, checkClusterConfig bool) (*management.Cluster, error) {
	upgradedCluster := cluster
	currentNodeGroupNumber := len(*cluster.EKSConfig.NodeGroups)
	configNodeGroups := *cluster.EKSConfig.NodeGroups
	updateNodeGroupsList := configNodeGroups[:1]
	upgradedCluster.EKSConfig.NodeGroups = &updateNodeGroupsList

	cluster, err := client.Management.Cluster.Update(cluster, &upgradedCluster)
	Expect(err).To(BeNil())

	if checkClusterConfig {
		// Check if the desired config is set correctly
		Expect(len(*cluster.EKSConfig.NodeGroups)).Should(BeNumerically("==", currentNodeGroupNumber-1))
		for i, ng := range *cluster.EKSConfig.NodeGroups {
			Expect(ng.NodegroupName).To(Equal(updateNodeGroupsList[i].NodegroupName))
		}
	}
	if wait {
		err = clusters.WaitClusterToBeUpgraded(client, cluster.ID)
		Expect(err).To(BeNil())
	}
	if checkClusterConfig {

		// Check if the desired config has been applied in Rancher
		Eventually(func() int {
			ginkgo.GinkgoLogr.Info("Waiting for the total nodegroup count to decrease in EKSStatus.UpstreamSpec ...")
			cluster, err = client.Management.Cluster.ByID(cluster.ID)
			Expect(err).To(BeNil())
			return len(*cluster.EKSStatus.UpstreamSpec.NodeGroups)
		}, tools.SetTimeout(15*time.Minute), 10*time.Second).Should(BeNumerically("==", currentNodeGroupNumber-1))
		for i, ng := range *cluster.EKSStatus.UpstreamSpec.NodeGroups {
			Expect(ng.NodegroupName).To(Equal(updateNodeGroupsList[i].NodegroupName))
		}
	}
	return cluster, nil
}

// ScaleNodeGroup modifies the number of initialNodeCount of all the nodegroups as defined by nodeCount
// if wait is set to true, it will wait until the cluster finishes updating;
// if checkClusterConfig is set to true, it will validate that nodegroup has been scaled successfully
func ScaleNodeGroup(cluster *management.Cluster, client *rancher.Client, nodeCount int64, wait, checkClusterConfig bool) (*management.Cluster, error) {
	upgradedCluster := cluster
	configNodeGroups := *upgradedCluster.EKSConfig.NodeGroups
	for i := range configNodeGroups {
		configNodeGroups[i].DesiredSize = pointer.Int64(nodeCount)
		configNodeGroups[i].MaxSize = pointer.Int64(nodeCount)
	}

	cluster, err := client.Management.Cluster.Update(cluster, &upgradedCluster)
	Expect(err).To(BeNil())

	if checkClusterConfig {
		// Check if the desired config is set correctly
		configNodeGroups = *cluster.EKSConfig.NodeGroups
		for i := range configNodeGroups {
			Expect(*configNodeGroups[i].DesiredSize).To(BeNumerically("==", nodeCount))
		}
	}

	if wait {
		err = clusters.WaitClusterToBeUpgraded(client, cluster.ID)
		Expect(err).To(BeNil())
	}

	if checkClusterConfig {
		// check that the desired config is applied on Rancher
		Eventually(func() bool {
			ginkgo.GinkgoLogr.Info("Waiting for the node count change to appear in EKSStatus.UpstreamSpec ...")
			cluster, err = client.Management.Cluster.ByID(cluster.ID)
			Expect(err).To(BeNil())
			upstreamNodeGroups := *cluster.EKSStatus.UpstreamSpec.NodeGroups
			for i := range upstreamNodeGroups {
				if ng := upstreamNodeGroups[i]; *ng.DesiredSize != nodeCount {
					return false
				}
			}
			return true
		}, tools.SetTimeout(15*time.Minute), 10*time.Second).Should(BeTrue())
	}

	return cluster, nil
}

// UpdateLogging updates the logging of a EKS cluster, Types: api, audit, authenticator, controllerManager, scheduler
// if checkClusterConfig is true, it validates the update
func UpdateLogging(cluster *management.Cluster, client *rancher.Client, loggingTypes []string, checkClusterConfig bool) (*management.Cluster, error) {
	upgradedCluster := cluster
	upgradedCluster.EKSConfig.LoggingTypes = &loggingTypes

	cluster, err := client.Management.Cluster.Update(cluster, &upgradedCluster)
	Expect(err).To(BeNil())

	if checkClusterConfig {
		// Check if the desired config is set correctly
		Expect(*upgradedCluster.EKSConfig.LoggingTypes).Should(HaveExactElements(loggingTypes))

		Eventually(func() []string {
			ginkgo.GinkgoLogr.Info("Waiting for the logging changes to appear in EKSStatus.UpstreamSpec ...")
			cluster, err = client.Management.Cluster.ByID(cluster.ID)
			Expect(err).To(BeNil())
			return *cluster.EKSStatus.UpstreamSpec.LoggingTypes
		}, tools.SetTimeout(10*time.Minute), 15*time.Second).Should(HaveExactElements(loggingTypes))
	}
	return cluster, nil
}

// UpdateAccess updates the network access of a EKS cluster, Types: publicAccess, privateAccess
// if checkClusterConfig is true, it validates the update
func UpdateAccess(cluster *management.Cluster, client *rancher.Client, publicAccess, privateAccess bool, checkClusterConfig bool) (*management.Cluster, error) {
	upgradedCluster := cluster
	upgradedCluster.EKSConfig.PublicAccess = &publicAccess
	upgradedCluster.EKSConfig.PrivateAccess = &privateAccess

	cluster, err := client.Management.Cluster.Update(cluster, &upgradedCluster)

	if checkClusterConfig {
		Eventually(func() bool {
			// Check if the desired config is set correctly
			Expect(*upgradedCluster.EKSConfig.PublicAccess).Should(Equal(publicAccess))
			Expect(*upgradedCluster.EKSConfig.PrivateAccess).Should(Equal(privateAccess))

			ginkgo.GinkgoLogr.Info("Waiting for the access changes to appear in EKSStatus.UpstreamSpec ...")
			cluster, err = client.Management.Cluster.ByID(cluster.ID)
			Expect(err).To(BeNil())
			return *cluster.EKSStatus.UpstreamSpec.PublicAccess == publicAccess && *cluster.EKSStatus.UpstreamSpec.PrivateAccess == privateAccess
		}, tools.SetTimeout(10*time.Minute), 15*time.Second).Should(BeTrue())
	}
	return cluster, err
}

// UpdatePublicAccessSources updates the network access sources of a EKS cluster
// if checkClusterConfig is true, it validates the update
func UpdatePublicAccessSources(cluster *management.Cluster, client *rancher.Client, publicAccessSources []string, checkClusterConfig bool) (*management.Cluster, error) {
	upgradedCluster := cluster
	*upgradedCluster.EKSConfig.PublicAccessSources = append(*upgradedCluster.EKSConfig.PublicAccessSources, publicAccessSources...)
	cluster, err := client.Management.Cluster.Update(cluster, &upgradedCluster)

	if checkClusterConfig {
		// Check if the desired config is set correctly
		Expect(*upgradedCluster.EKSConfig.PublicAccessSources).Should(ContainElements(publicAccessSources))

		Eventually(func() []string {
			ginkgo.GinkgoLogr.Info("Waiting for the publicaccess sources changes to appear in EKSStatus.UpstreamSpec ...")
			cluster, err = client.Management.Cluster.ByID(cluster.ID)
			Expect(err).To(BeNil())
			return *cluster.EKSStatus.UpstreamSpec.PublicAccessSources
		}, tools.SetTimeout(10*time.Minute), 15*time.Second).Should(ContainElements(publicAccessSources))
	}
	return cluster, nil
}

// UpdateClusterTags updates the tags of a EKS cluster;
// the given tag list will replace the existing tags; this is required to be able to delete tag removal using this function
// if wait is set to true, it waits until the update is complete; if checkClusterConfig is true, it validates the update
func UpdateClusterTags(cluster *management.Cluster, client *rancher.Client, tags map[string]string, checkClusterConfig bool) (*management.Cluster, error) {
	upgradedCluster := cluster
	upgradedCluster.EKSConfig.Tags = &tags

	cluster, err := client.Management.Cluster.Update(cluster, &upgradedCluster)
	Expect(err).To(BeNil())

	if checkClusterConfig {
		// Check if the desired config is set correctly
		for key, value := range tags {
			Expect(*cluster.EKSConfig.Tags).Should(HaveKeyWithValue(key, value))
		}
		Eventually(func() bool {
			ginkgo.GinkgoLogr.Info("Waiting for the cluster tag changes to appear in EKSStatus.UpstreamSpec ...")
			cluster, err = client.Management.Cluster.ByID(cluster.ID)
			Expect(err).To(BeNil())
			return maps.Equal(tags, *cluster.EKSStatus.UpstreamSpec.Tags)
		}, tools.SetTimeout(10*time.Minute), 15*time.Second).Should(BeTrue())
	}
	return cluster, nil
}

// UpdateNodegroupMetadata updates the tags & labels of a EKS Node groups
// the given tags and labels will replace the existing counterparts
// if wait is set to true, it waits until the update is complete; if checkClusterConfig is true, it validates the update
func UpdateNodegroupMetadata(cluster *management.Cluster, client *rancher.Client, tags, labels map[string]string, checkClusterConfig bool) (*management.Cluster, error) {
	upgradedCluster := cluster
	configNodeGroups := *upgradedCluster.EKSConfig.NodeGroups
	for i := range configNodeGroups {
		*configNodeGroups[i].Tags = tags
		*configNodeGroups[i].Labels = labels
	}

	var err error
	cluster, err = client.Management.Cluster.Update(cluster, &upgradedCluster)
	Expect(err).To(BeNil())

	if checkClusterConfig {
		// Check if the desired config is set correctly
		for _, ng := range *cluster.EKSConfig.NodeGroups {
			for key, value := range tags {
				Expect(*ng.Tags).Should(HaveKeyWithValue(key, value))
			}
			for key, value := range labels {
				Expect(*ng.Labels).Should(HaveKeyWithValue(key, value))
			}
		}

		Eventually(func() bool {
			ginkgo.GinkgoLogr.Info("Waiting for the nodegroup metadata changes to appear in EKSStatus.UpstreamSpec ...")
			cluster, err = client.Management.Cluster.ByID(cluster.ID)
			Expect(err).To(BeNil())

			for _, ng := range *cluster.EKSStatus.UpstreamSpec.NodeGroups {
				if maps.Equal(tags, *ng.Tags) && maps.Equal(labels, *ng.Labels) {
					return true
				}
			}
			return false
		}, tools.SetTimeout(10*time.Minute), 15*time.Second).Should(BeTrue())
	}
	return cluster, nil
}

// UpdateCluster is a generic function to update a cluster
func UpdateCluster(cluster *management.Cluster, client *rancher.Client, updateFunc func(*management.Cluster)) (*management.Cluster, error) {
	upgradedCluster := cluster

	updateFunc(upgradedCluster)

	return client.Management.Cluster.Update(cluster, &upgradedCluster)
}

// ListEKSAvailableVersions lists all the available and UI supported EKS versions for cluster upgrade.
// this function is a fork of r/shepherd ListEKSAvailableVersions
func ListEKSAvailableVersions(client *rancher.Client, cluster *management.Cluster) (availableVersions []string, err error) {
	currentVersion, err := semver.NewVersion(cluster.Version.GitVersion)
	if err != nil {
		return
	}
	var validMasterVersions []*semver.Version
	allAvailableVersions, err := ListEKSAllVersions(client)
	if err != nil {
		return
	}
	for _, version := range allAvailableVersions {
		v, err := semver.NewVersion(version)
		if err != nil {
			continue
		}
		validMasterVersions = append(validMasterVersions, v)
	}
	for _, v := range validMasterVersions {
		if v.Minor()-1 > currentVersion.Minor() || v.Compare(currentVersion) == 0 || v.Compare(currentVersion) == -1 {
			continue
		}
		version := fmt.Sprintf("%v.%v", v.Major(), v.Minor())
		availableVersions = append(availableVersions, version)
	}

	sort.SliceStable(availableVersions, func(i, j int) bool { return i > j })
	return helpers.FilterUIUnsupportedVersions(availableVersions, client), nil
}

// ListEKSAllVersions lists all the versions supported by UI;
// this is a separate static list maintained by hosted-providers-e2e, similar to the  UI lists:
// https://raw.githubusercontent.com/rancher/dashboard/refs/heads/master/pkg/eks/assets/data/eks-versions.js and
// https://raw.githubusercontent.com/rancher/ui/master/lib/shared/addon/utils/amazon.js
// the static list only contains officially supported EKS versions, i.e. 1.24 to 1.32;
// refer: https://docs.aws.amazon.com/eks/latest/userguide/kubernetes-versions.html.
func ListEKSAllVersions(client *rancher.Client) (allVersions []string, err error) {
	serverVersion, err := helpers.GetRancherServerVersion(client)
	if err != nil {
		return
	}

	allVersions = []string{"1.35", "1.34", "1.33"}

	if strings.Contains(serverVersion, "2.13") || strings.Contains(serverVersion, "2.12") {
		allVersions = []string{"1.33", "1.32", "1.31"}
	} else if strings.Contains(serverVersion, "2.11") {
		allVersions = []string{"1.32", "1.31", "1.30"}
	} else if strings.Contains(serverVersion, "2.10") {
		allVersions = []string{"1.31", "1.30", "1.29", "1.28"}
	} else if strings.Contains(serverVersion, "2.9") {
		allVersions = []string{"1.30", "1.29", "1.28", "1.27"}
	} else if strings.Contains(serverVersion, "2.8") {
		allVersions = []string{"1.28", "1.27", "1.26", "1.25"}
	} else if strings.Contains(serverVersion, "2.7") {
		allVersions = []string{"1.27", "1.26", "1.25", "1.24"}
	}

	// as a safety net, we ensure all the versions are UI supported
	return helpers.FilterUIUnsupportedVersions(allVersions, client), nil
}

// <==============================EKS CLI==============================>

// Create AWS EKS cluster using EKS CLI
func CreateEKSClusterOnAWS(region string, clusterName string, k8sVersion string, nodes string, tags map[string]string, extraArgs ...string) error {
	currentKubeconfig := os.Getenv("KUBECONFIG")
	defer os.Setenv("KUBECONFIG", currentKubeconfig)

	helpers.SetTempKubeConfig(clusterName)

	formattedTags := k8slabels.SelectorFromSet(tags).String()
	fmt.Println("Creating EKS cluster ...")
	args := []string{"create", "cluster", "--region=" + region, "--name=" + clusterName, "--version=" + k8sVersion, "--nodegroup-name", "ranchernodes", "--nodes", nodes, "--tags", formattedTags}
	if len(extraArgs) != 0 {
		args = append(args, extraArgs...)
	}
	fmt.Printf("Running command: eksctl %v\n", args)
	out, err := proc.RunW("eksctl", args...)
	if err != nil {
		return errors.Wrap(err, "Failed to create cluster: "+out)
	}
	fmt.Println("Created EKS cluster: ", clusterName)

	return nil
}

// Upgrade EKS cluster using EKS CLI
func UpgradeEKSClusterOnAWS(region string, clusterName string, upgradeToVersion string) error {

	fmt.Println("Upgrading EKS cluster controlplane ...")
	args := []string{"upgrade", "cluster", "--region=" + region, "--name=" + clusterName, "--version=" + upgradeToVersion, "--approve"}
	fmt.Printf("Running command: eksctl %v\n", args)
	out, err := proc.RunW("eksctl", args...)
	if err != nil {
		return errors.Wrap(err, "Failed to upgrade cluster: "+out)
	}

	fmt.Println("Upgraded EKS cluster controlplane: ", clusterName)
	return nil
}

// AddNodeGroupOnAWS adds nodegroup ot a cluster using EKS CLI
func AddNodeGroupOnAWS(nodeName, clusterName, region string, extraArgs ...string) error {
	fmt.Println("Adding nodegroup to EKS cluster ...")
	args := []string{"create", "nodegroup", "--region=" + region, "--cluster", clusterName, "--name", nodeName}
	if len(extraArgs) != 0 {
		args = append(args, extraArgs...)
	}
	fmt.Printf("Running command: eksctl %v\n", args)
	out, err := proc.RunW("eksctl", args...)
	if err != nil {
		return errors.Wrap(err, "Failed to add nodegroup: "+out)
	}
	fmt.Println("Added nodegroup: ", nodeName)
	return nil

}

// ScaleNodeGroupOnAWS scales nodegroup of a cluster using EKS CLI
func ScaleNodeGroupOnAWS(ngName, clusterName, region string, numOfNodes, maxCount, minCount int64, extraArgs ...string) error {
	fmt.Println("Scaling nodegroup of EKS cluster ...")
	args := []string{"scale", "nodegroup", "--region", region, "--name", ngName, "--cluster", clusterName, "--nodes", fmt.Sprintf("%d", numOfNodes), "--nodes-max", fmt.Sprintf("%d", maxCount), "--nodes-min", fmt.Sprintf("%d", minCount), "--wait"}
	if len(extraArgs) != 0 {
		args = append(args, extraArgs...)
	}
	fmt.Printf("Running command: eksctl %v\n", args)
	out, err := proc.RunW("eksctl", args...)
	if err != nil {
		return errors.Wrap(err, "Failed to scale nodegroup: "+out)
	}
	fmt.Println("Scaled nodegroup: ", ngName)
	return nil
}

// UpdateNodeGroupLabelsOnAWS deletes or add/updates labels on nodegroup of a cluster;
func UpdateNodeGroupLabelsOnAWS(clusterName, nodegroupName, region string, addOrUpdatelabels map[string]string, removeLabels []string, extraArgs ...string) error {
	if len(addOrUpdatelabels) == 0 && len(removeLabels) == 0 {
		return fmt.Errorf("no labels provided to remove or update/add")
	}
	fmt.Println("Updating labels of nodegroup on EKS cluster ...")
	args := []string{"eks", "update-nodegroup-config", "--cluster-name", clusterName, "--nodegroup-name", nodegroupName, "--region", region}
	var addLabel, removeLabel string

	if len(addOrUpdatelabels) > 0 {
		formattedLabels := k8slabels.FormatLabels(addOrUpdatelabels)
		addLabel = fmt.Sprintf("addOrUpdateLabels={%s}", formattedLabels)
	}
	if len(removeLabels) > 0 {
		removeLabel = fmt.Sprintf("removeLabels=%s", strings.Join(removeLabels, ","))
	}

	var label string
	if addLabel != "" && removeLabel != "" {
		label = fmt.Sprintf("%s,%s", addLabel, removeLabel)
	} else if addLabel != "" {
		label = fmt.Sprintf("%s", addLabel)
	} else {
		label = fmt.Sprintf("%s", removeLabel)
	}

	args = append(args, "--labels", label)
	if len(extraArgs) != 0 {
		args = append(args, extraArgs...)
	}

	fmt.Printf("Running command: aws %v\n", args)
	out, err := proc.RunW("aws", args...)
	if err != nil {
		return errors.Wrap(err, "Failed to update labels to nodegroup: "+out)
	}
	fmt.Println("Updated labels to nodegroup: ", nodegroupName, "\n", out)
	return nil
}

// AddClusterTagsOnAWS adds label to cluster using AWS cli
func AddClusterTagsOnAWS(clusterName, region string, tags map[string]string, extraArgs ...string) error {
	arn, err := GetFromEKS(region, clusterName, "cluster", ".[].Arn")
	if err != nil {
		return fmt.Errorf("failed to get ARN for cluster %s: %v", clusterName, err)
	}
	return UpdateResoureTagsOnAWS(arn, clusterName, region, tags, extraArgs...)
}

// RemoveClusterTagsOnAWS removes label from cluster using AWS cli
func RemoveClusterTagsOnAWS(clusterName, region string, tags []string, extraArgs ...string) error {
	arn, err := GetFromEKS(region, clusterName, "cluster", ".[].Arn")
	if err != nil {
		return fmt.Errorf("failed to get ARN for cluster %s: %v", clusterName, err)
	}
	return RemoveResourceTagsOnAWS(arn, clusterName, region, tags, extraArgs...)
}

// UpdateClusterTagOnAWS tags resource using AWS CLI
func UpdateResoureTagsOnAWS(resourceArn, clusterName, region string, tags map[string]string, extraArgs ...string) error {
	formattedTags := k8slabels.SelectorFromSet(tags).String()
	fmt.Println("Updating tag on EKS cluster ...")
	args := []string{"eks", "tag-resource", "--resource-arn", resourceArn, "--tags", formattedTags, "--region", region}
	if len(extraArgs) != 0 {
		args = append(args, extraArgs...)
	}
	fmt.Printf("Running command: aws %v\n", args)
	out, err := proc.RunW("aws", args...)
	if err != nil {
		return errors.Wrap(err, "Failed to update tag: "+out)
	}
	fmt.Println("Updated tag on EKS cluster: ", clusterName, "\n", out)
	return nil
}

// RemoveResourceTagsOnAWS untags the resources using AWS CLI
func RemoveResourceTagsOnAWS(resourceArn, clusterName, region string, tags []string, extraArgs ...string) error {
	fmt.Println("Removing tag on EKS cluster ...")
	args := []string{"eks", "untag-resource", "--resource-arn", resourceArn, "--region", region}
	args = append(args, "--tag-keys")
	for _, tag := range tags {
		args = append(args, tag)
	}
	if len(extraArgs) != 0 {
		args = append(args, extraArgs...)
	}
	fmt.Printf("Running command: aws %v\n", args)
	out, err := proc.RunW("aws", args...)
	if err != nil {
		return errors.Wrap(err, "Failed to remove tag: "+out)
	}
	fmt.Println("Removed tag on EKS cluster: ", clusterName, "\n", out)
	return nil
}

// UpdateLoggingOnAWS enabled and disabled the logging of a cluster
// types: all, api, audit, authenticator, controllerManager, scheduler
func UpdateLoggingOnAWS(clusterName, region string, enableLoggingTypes, disableLoggingTypes []string, extraArgs ...string) error {
	fmt.Println("Updating Logging of EKS cluster ...")
	args := []string{"utils", "update-cluster-logging", "--region", region, "--cluster", clusterName, "--approve"}
	if len(enableLoggingTypes) != 0 {
		args = append(args, "--enable-types")
		args = append(args, strings.Join(enableLoggingTypes, ","))
	}
	if len(disableLoggingTypes) != 0 {
		args = append(args, "--disable-types")
		args = append(args, strings.Join(disableLoggingTypes, ","))
	}
	if len(extraArgs) != 0 {
		args = append(args, extraArgs...)
	}
	fmt.Printf("Running command: eksctl %v\n", args)
	out, err := proc.RunW("eksctl", args...)
	if err != nil {
		return errors.Wrap(err, "Failed to update logging: "+out)
	}
	fmt.Println("Updated logging of EKS cluster: ", clusterName, "\n", out)
	return nil
}

func UpdateVPCAccess(clusterName, region string, enablePublic, enablePrivate bool, publicAccessCIDR []string, extraArgs ...string) error {
	fmt.Println("Updating VPC access of control plane ...")
	args := []string{"utils", "update-cluster-vpc-config", "--region", region, "--cluster", clusterName, "--approve"}
	if enablePublic {
		args = append(args, "--public-access")
	}
	if enablePrivate {
		args = append(args, "--private-access")
	}
	if len(publicAccessCIDR) != 0 {
		args = append(args, "--public-access-cidrs", strings.Join(publicAccessCIDR, ","))
	}
	if len(extraArgs) != 0 {
		args = append(args, extraArgs...)
	}
	fmt.Printf("Running command: eksctl %v\n", args)
	out, err := proc.RunW("eksctl", args...)
	if err != nil {
		return errors.Wrap(err, "Failed to update VPC access: "+out)
	}
	fmt.Println("Updated VPC access: ", clusterName)
	return nil
}

// Upgrade EKS cluster nodegroup using EKS CLI
func UpgradeEKSNodegroupOnAWS(region string, clusterName string, ngName string, upgradeToVersion string) error {
	fmt.Println("Upgrading EKS cluster nodegroup ...")
	args := []string{"upgrade", "nodegroup", "--region=" + region, "--name=" + ngName, "--cluster=" + clusterName, "--kubernetes-version=" + upgradeToVersion}
	fmt.Printf("Running command: eksctl %v\n", args)
	out, err := proc.RunW("eksctl", args...)
	if err != nil {
		return errors.Wrap(err, "Failed to upgrade nodegroup: "+out)
	}

	fmt.Println("Upgraded EKS cluster nodegroup: ", clusterName)
	return nil
}

func GetFromEKS(region string, clusterName string, cmd string, query string, extraArgs ...string) (out string, err error) {
	clusterArgs := []string{"eksctl", "get", "cluster", "--region=" + region, "--name=" + clusterName, "-ojson"}
	ngArgs := []string{"eksctl", "get", "nodegroup", "--region=" + region, "--cluster=" + clusterName, "-ojson"}
	queryArgs := []string{"|", "jq", "-r", query}

	if cmd == "cluster" {
		// extraArgs must be appended before queryArgs
		if len(extraArgs) != 0 {
			clusterArgs = append(clusterArgs, extraArgs...)
		}
		clusterArgs = append(clusterArgs, queryArgs...)
		cmd = strings.Join(clusterArgs, " ")
	} else {
		// extraArgs must be appended before queryArgs
		if len(extraArgs) != 0 {
			ngArgs = append(ngArgs, extraArgs...)
		}
		ngArgs = append(ngArgs, queryArgs...)
		cmd = strings.Join(ngArgs, " ")
	}

	fmt.Printf("Running command: %s\n", cmd)
	out, err = proc.RunW("bash", "-c", cmd)
	return strings.TrimSpace(out), err
}

// Creates/Deletes EKS cluster nodegroup using EKS CLI
func ModifyEKSNodegroupOnAWS(region string, clusterName string, ngName string, operation string, extraArgs ...string) error {
	args := []string{operation, "nodegroup", "--region=" + region, "--name=" + ngName, "--cluster=" + clusterName}
	if operation == "delete" {
		args = append(args, "--disable-eviction")
	}
	args = append(args, extraArgs...)
	fmt.Printf("Running command: eksctl %v\n", args)
	out, err := proc.RunW("eksctl", args...)
	if err != nil {
		return errors.Wrap(err, "Failed to modify nodegroup: "+out)
	}
	return nil
}

// Complete cleanup steps for Amazon EKS
func DeleteEKSClusterOnAWS(region string, clusterName string) error {
	currentKubeconfig := os.Getenv("KUBECONFIG")
	downstreamKubeconfig := os.Getenv(helpers.DownstreamKubeconfig(clusterName))
	defer func() {
		_ = os.Setenv("KUBECONFIG", currentKubeconfig)
		_ = os.Remove(downstreamKubeconfig) // clean up
	}()
	_ = os.Setenv("KUBECONFIG", downstreamKubeconfig)

	fmt.Println("Deleting all nodegroups ...")
	ngNames, err := GetFromEKS(region, clusterName, "nodegroup", ".[].Name")
	if err != nil {
		return errors.Wrap(err, "Failed to list nodegroup for deletion")
	}

	if ngNames != "" {
		for _, ngName := range strings.Split(ngNames, "\n") {
			err = ModifyEKSNodegroupOnAWS(region, clusterName, ngName, "delete", "--wait")
			if err != nil {
				return errors.Wrap(err, "Failed to delete nodegroup")
			}
		}
	}

	fmt.Println("Deleting EKS cluster ...")

	args := []string{"delete", "cluster", "--region=" + region, "--name=" + clusterName}
	fmt.Printf("Running command: eksctl %v\n", args)
	out, err := proc.RunW("eksctl", args...)
	if err != nil {
		return errors.Wrap(err, "Failed to delete cluster: "+out)
	}

	fmt.Println("Deleted EKS cluster: ", clusterName)

	return nil
}

// <==============================EKS CLI(end)==============================>

// GetK8sVersion returns the k8s version to be used by the test;
// this value can either be a variant of envvar DOWNSTREAM_K8S_MINOR_VERSION or the highest available version
// or second-highest minor version in case of upgrade scenarios
func GetK8sVersion(client *rancher.Client, forUpgrade bool) (string, error) {
	if k8sVersion := helpers.DownstreamK8sMinorVersion; k8sVersion != "" {
		return k8sVersion, nil
	}
	allVariants, err := ListEKSAllVersions(client)
	if err != nil {
		return "", err
	}

	return helpers.DefaultK8sVersion(allVariants, forUpgrade)
}
