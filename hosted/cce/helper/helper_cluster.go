package helper

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rancher-sandbox/ele-testhelpers/tools"

	"github.com/rancher/hosted-providers-e2e/hosted/helpers"

	"github.com/rancher/shepherd/clients/rancher"
	management "github.com/rancher/shepherd/clients/rancher/generated/management/v3"
	"github.com/rancher/shepherd/extensions/clusters"
	"github.com/rancher/shepherd/extensions/clusters/cce"
	"github.com/rancher/shepherd/pkg/config"
	namegen "github.com/rancher/shepherd/pkg/namegenerator"
)

// CreateCCEHostedCluster is a helper function that creates an CCE hosted cluster
func CreateCCEHostedCluster(client *rancher.Client, displayName, cloudCredentialID, kubernetesVersion, region string, id int64, updateFunc func(clusterConfig *cce.ClusterConfig)) (*management.Cluster, error) {
	var cceClusterConfig cce.ClusterConfig
	config.LoadConfig(cce.CCEClusterConfigConfigurationFileKey, &cceClusterConfig)

	cceClusterConfig.Name = displayName
	cceClusterConfig.Version = kubernetesVersion
	cceClusterConfig.RegionID = region
	cceClusterConfig.Tags = helpers.GetCommonMetadataLabels()
	cceClusterConfig.ContainerNetwork.CIDR = fmt.Sprintf("10.%v.0.0/16", id%255)

	if updateFunc != nil {
		updateFunc(&cceClusterConfig)
	}
	ginkgo.GinkgoLogr.Info(fmt.Sprintf("Creating CCE cluster version %v ClusterCIDR %v", kubernetesVersion, cceClusterConfig.ContainerNetwork.CIDR))

	return cce.CreateCCEHostedCluster(client, displayName, cloudCredentialID, cceClusterConfig, false, false, false, false, nil)
}

func ImportCCEHostedCluster(client *rancher.Client, displayName, cloudCredentialID, region string) (*management.Cluster, error) {
	cluster := &management.Cluster{
		DockerRootDir: "/var/lib/docker",
		CCEConfig: &management.CCEClusterConfigSpec{
			HuaweiCredentialSecret: cloudCredentialID,
			Name:                   displayName,
			Imported:               true,
			RegionID:               region,
		},
		Name: displayName,
	}

	clusterResp, err := client.Management.Cluster.Create(cluster)
	if err != nil {
		return nil, err
	}
	return clusterResp, err
}

func DeleteCCEHostClusterNodeEIPs(cluster *management.Cluster, client *rancher.Client) {
	Eventually(func() bool {
		ginkgo.GinkgoLogr.Info("Cleaning up CCE Cluster Node EIPs...")
		ok, err := cce.CleanupNodeEIP(client, cluster.ID)
		Expect(err).To(BeNil())
		return ok
	}, tools.SetTimeout(5*time.Minute), 10*time.Second).Should(BeTrue())
	ginkgo.GinkgoLogr.Info("Done Cleanup CCE Cluster Node EIPs")
}

// DeleteCCEHostCluster deletes the CCE cluster
func DeleteCCEHostCluster(cluster *management.Cluster, client *rancher.Client) error {
	return client.Management.Cluster.Delete(cluster)
}

func WaitCCEClusterNodeIP(client *rancher.Client, cluster *management.Cluster) {
	Eventually(func() bool {
		ginkgo.GinkgoLogr.Info("Waiting for CCE Cluster Nodes setup EIP...")
		ok, err := cce.UpdateNodePublicIP(client, cluster.ID)
		Expect(err).To(BeNil())
		return ok
	}, tools.SetTimeout(15*time.Minute), 30*time.Second).Should(BeTrue())
	ginkgo.GinkgoLogr.Info("Done Waiting for CCE Cluster Nodes")
}

// UpgradeClusterKubernetesVersion upgrades the k8s version to the value defined by upgradeToVersion.
// if checkClusterConfig is set to true, it will validate that the cluster control plane has been upgrade successfully
func UpgradeClusterKubernetesVersion(cluster *management.Cluster, upgradeToVersion string, client *rancher.Client, checkClusterConfig bool) (*management.Cluster, error) {
	upgradedCluster := cluster
	upgradedCluster.CCEConfig.Version = upgradeToVersion

	cluster, err := client.Management.Cluster.Update(cluster, &upgradedCluster)
	Expect(err).To(BeNil())

	if checkClusterConfig {
		// Check if the desired config is set correctly
		Expect(cluster.CCEConfig.Version).To(Equal(upgradeToVersion))

		// Check if the desired config has been applied in Rancher
		// Check if CCEConfig has correct KubernetesVersion after upgrade
		Eventually(func() bool {
			ginkgo.GinkgoLogr.Info("Waiting for k8s upgrade to appear in CCEStatus.UpstreamSpec & CCEConfig ...")
			cluster, err = client.Management.Cluster.ByID(cluster.ID)
			Expect(err).To(BeNil())
			ginkgo.GinkgoLogr.Info(fmt.Sprintf("UpstreamSpec.Version: %v, CCEConfig.Version %v, upgradeToVersion %v",
				cluster.CCEStatus.UpstreamSpec.Version, cluster.CCEConfig.Version, upgradeToVersion))
			return cluster.CCEStatus.UpstreamSpec.Version == upgradeToVersion && cluster.CCEConfig.Version == upgradeToVersion
		}, tools.SetTimeout(15*time.Minute), 30*time.Second).Should(BeTrue())
		ginkgo.GinkgoLogr.Info("Done Waiting for k8s upgrade to appear in CCEStatus.UpstreamSpec & CCEConfig")
	}
	return cluster, nil
}

// AddNodePool adds a nodepool to the list; it uses the nodepool template defined in CATTLE_TEST_CONFIG file
// if checkClusterConfig is set to true, it will validate that nodepool has been added successfully
func AddNodePool(cluster *management.Cluster, increaseBy int, client *rancher.Client, wait, checkClusterConfig bool) (*management.Cluster, error) {
	upgradedCluster := cluster
	currentNodeGroupNumber := len(cluster.CCEConfig.NodePools)

	// We use management.CCEClusterConfigSpec instead of the usual cce.ClusterConfig to unmarshal the data without the need of a lot of post-processing.
	var cceClusterConfig management.CCEClusterConfigSpec
	config.LoadConfig(cce.CCEClusterConfigConfigurationFileKey, &cceClusterConfig)
	nodePools := cceClusterConfig.NodePools
	ngTemplate := nodePools[0]

	updateNodeGroupsList := cluster.CCEConfig.NodePools
	for i := 1; i <= increaseBy; i++ {
		newNodeGroup := management.CCENodePool{
			CustomSecurityGroups: ngTemplate.CustomSecurityGroups,
			Type:                 ngTemplate.Type,
			InitialNodeCount:     ngTemplate.InitialNodeCount,
			Name:                 namegen.AppendRandomString("ng"),
			NodeTemplate:         ngTemplate.NodeTemplate,
			PodSecurityGroups:    ngTemplate.PodSecurityGroups,
		}
		updateNodeGroupsList = append(updateNodeGroupsList, newNodeGroup)
	}
	upgradedCluster.CCEConfig.NodePools = updateNodeGroupsList

	cluster, err := client.Management.Cluster.Update(cluster, &upgradedCluster)
	Expect(err).To(BeNil())

	if checkClusterConfig {
		// Check if the desired config is set correctly
		Expect(len(cluster.CCEConfig.NodePools)).Should(BeNumerically("==", currentNodeGroupNumber+increaseBy))
		for i, ng := range cluster.CCEConfig.NodePools {
			Expect(ng.Name).To(Equal(updateNodeGroupsList[i].Name))
		}
	}

	if wait {
		err = clusters.WaitClusterToBeUpgraded(client, cluster.ID)
		Expect(err).To(BeNil())
	}

	if checkClusterConfig {
		// Check if the desired config has been applied in Rancher
		Eventually(func() int {
			ginkgo.GinkgoLogr.Info("Waiting for the total nodepool count to increase in CCEStatus.UpstreamSpec ...")
			cluster, err = client.Management.Cluster.ByID(cluster.ID)
			Expect(err).To(BeNil())
			return len(cluster.CCEStatus.UpstreamSpec.NodePools)
		}, tools.SetTimeout(15*time.Minute), 10*time.Second).Should(BeNumerically("==", currentNodeGroupNumber+increaseBy))
		ginkgo.GinkgoLogr.Info("Done waiting for the total nodepool count to increase in CCEStatus.UpstreamSpec")

		for i, ng := range cluster.CCEStatus.UpstreamSpec.NodePools {
			Expect(ng.Name).To(Equal(updateNodeGroupsList[i].Name))
		}
	}

	return cluster, nil
}

// AddNodePoolToConfig adds a nodepool to the list; it uses the nodepool template defined in CATTLE_TEST_CONFIG file
func AddNodePoolToConfig(cceClusterConfig cce.ClusterConfig, ngCount int) (cce.ClusterConfig, error) {

	var updateNodeGroupsList []cce.NodePool
	ngTemplate := cceClusterConfig.NodePools

	for i := 1; i <= ngCount; i++ {
		newNodeGroup := ngTemplate[0]
		newNodeGroup.Name = namegen.AppendRandomString(ngTemplate[0].Name)
		updateNodeGroupsList = append(updateNodeGroupsList, newNodeGroup)
	}
	cceClusterConfig.NodePools = updateNodeGroupsList

	return cceClusterConfig, nil
}

// DeleteNodePool deletes a nodepool from the list
// if checkClusterConfig is set to true, it will validate that nodepool has been deleted successfully
func DeleteNodePool(cluster *management.Cluster, client *rancher.Client, wait, checkClusterConfig bool) (*management.Cluster, error) {
	upgradedCluster := cluster
	currentNodeGroupNumber := len(cluster.CCEConfig.NodePools)
	configNodePools := cluster.CCEConfig.NodePools
	updateNodeGroupsList := configNodePools[:1]
	upgradedCluster.CCEConfig.NodePools = updateNodeGroupsList

	cluster, err := client.Management.Cluster.Update(cluster, &upgradedCluster)
	Expect(err).To(BeNil())

	if checkClusterConfig {
		// Check if the desired config is set correctly
		Expect(len(cluster.CCEConfig.NodePools)).Should(BeNumerically("==", currentNodeGroupNumber-1))
		for i, ng := range cluster.CCEConfig.NodePools {
			Expect(ng.Name).To(Equal(updateNodeGroupsList[i].Name))
		}
	}
	if wait {
		err = clusters.WaitClusterToBeUpgraded(client, cluster.ID)
		Expect(err).To(BeNil())
	}
	if checkClusterConfig {

		// Check if the desired config has been applied in Rancher
		Eventually(func() int {
			ginkgo.GinkgoLogr.Info("Waiting for the total nodepool count to decrease in CCEStatus.UpstreamSpec ...")
			cluster, err = client.Management.Cluster.ByID(cluster.ID)
			Expect(err).To(BeNil())
			return len(cluster.CCEStatus.UpstreamSpec.NodePools)
		}, tools.SetTimeout(15*time.Minute), 10*time.Second).Should(BeNumerically("==", currentNodeGroupNumber-1))
		for i, ng := range cluster.CCEStatus.UpstreamSpec.NodePools {
			Expect(ng.Name).To(Equal(updateNodeGroupsList[i].Name))
		}
		ginkgo.GinkgoLogr.Info("Done waiting for the total nodepool count to decrease in CCEStatus.UpstreamSpec")
	}
	return cluster, nil
}

// ScaleNodeGroup modifies the number of initialNodeCount of all the nodegroups as defined by nodeCount
// if wait is set to true, it will wait until the cluster finishes updating;
// if checkClusterConfig is set to true, it will validate that nodepool has been scaled successfully
func ScaleNodeGroup(cluster *management.Cluster, client *rancher.Client, nodeCount int64, wait, checkClusterConfig bool) (*management.Cluster, error) {
	upgradedCluster := cluster
	configNodePools := upgradedCluster.CCEConfig.NodePools
	for i := range configNodePools {
		configNodePools[i].InitialNodeCount = nodeCount
	}

	cluster, err := client.Management.Cluster.Update(cluster, &upgradedCluster)
	Expect(err).To(BeNil())

	if checkClusterConfig {
		// Check if the desired config is set correctly
		configNodePools = cluster.CCEConfig.NodePools
		for i := range configNodePools {
			Expect(configNodePools[i].InitialNodeCount).To(BeNumerically("==", nodeCount))
		}
	}

	if wait {
		err = clusters.WaitClusterToBeUpgraded(client, cluster.ID)
		Expect(err).To(BeNil())
	}

	if checkClusterConfig {
		// check that the desired config is applied on Rancher
		Eventually(func() bool {
			ginkgo.GinkgoLogr.Info("Waiting for the node count change to appear in CCEStatus.UpstreamSpec ...")
			cluster, err = client.Management.Cluster.ByID(cluster.ID)
			Expect(err).To(BeNil())
			upstreamNodeGroups := cluster.CCEStatus.UpstreamSpec.NodePools
			for i := range upstreamNodeGroups {
				if ng := upstreamNodeGroups[i]; ng.InitialNodeCount != nodeCount {
					return false
				}
			}
			return true
		}, tools.SetTimeout(15*time.Minute), 10*time.Second).Should(BeTrue())
		ginkgo.GinkgoLogr.Info("Done waiting for the node count change to appear in CCEStatus.UpstreamSpec")
	}

	return cluster, nil
}

// UpdateCluster is a generic function to update a cluster
func UpdateCluster(cluster *management.Cluster, client *rancher.Client, updateFunc func(*management.Cluster)) (*management.Cluster, error) {
	upgradedCluster := cluster

	updateFunc(upgradedCluster)

	return client.Management.Cluster.Update(cluster, &upgradedCluster)
}

// ListCCEAvailableVersions lists all the available and UI supported CCE versions for cluster upgrade.
// this function is a fork of r/shepherd ListCCEAvailableVersions
func ListCCEAvailableVersions(client *rancher.Client, cluster *management.Cluster) (availableVersions []string, err error) {
	currentVersion, err := semver.NewVersion(cluster.Version.GitVersion)
	if err != nil {
		return
	}
	var validMasterVersions []*semver.Version
	allAvailableVersions, err := ListCCEAllVersions(client)
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
		version := fmt.Sprintf("v%v.%v", v.Major(), v.Minor())
		availableVersions = append(availableVersions, version)
	}

	sort.SliceStable(availableVersions, func(i, j int) bool { return i > j })
	return helpers.FilterUIUnsupportedVersions(availableVersions, client), nil
}

// ListCCEAllVersions lists all the versions supported by UI;
func ListCCEAllVersions(client *rancher.Client) (allVersions []string, err error) {
	serverVersion, err := helpers.GetRancherServerVersion(client)
	if err != nil {
		return
	}

	allVersions = []string{"v1.36", "v1.35", "v1.34"}

	switch {
	case strings.Contains(serverVersion, "2.15"):
		allVersions = []string{"v1.36", "v1.35", "v1.34"}
	case strings.Contains(serverVersion, "2.14"), strings.Contains(serverVersion, "2.13"), strings.Contains(serverVersion, "2.12"):
		allVersions = []string{"v1.33", "v1.32", "v1.31"}
	case strings.Contains(serverVersion, "2.11"):
		allVersions = []string{"v1.32", "v1.31", "v1.30"}
	case strings.Contains(serverVersion, "2.10"):
		allVersions = []string{"v1.31", "v1.30", "v1.29", "v1.28"}
	case strings.Contains(serverVersion, "2.9"):
		allVersions = []string{"v1.30", "v1.29", "v1.28"}
	}

	// as a safety net, we ensure all the versions are UI supported
	return helpers.FilterUIUnsupportedVersions(allVersions, client), nil
}

// GetK8sVersion returns the k8s version to be used by the test;
// this value can either be a variant of envvar DOWNSTREAM_K8S_MINOR_VERSION or the highest available version
// or second-highest minor version in case of upgrade scenarios
func GetK8sVersion(client *rancher.Client, forUpgrade bool) (string, error) {
	if k8sVersion := helpers.DownstreamK8sMinorVersion; k8sVersion != "" {
		return k8sVersion, nil
	}
	allVariants, err := ListCCEAllVersions(client)
	if err != nil {
		return "", err
	}

	return helpers.DefaultK8sVersion(allVariants, forUpgrade)
}
