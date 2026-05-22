package p0_test

import (
	"fmt"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "github.com/rancher-sandbox/qase-ginkgo"

	"github.com/rancher/shepherd/clients/rancher"
	management "github.com/rancher/shepherd/clients/rancher/generated/management/v3"
	namegen "github.com/rancher/shepherd/pkg/namegenerator"

	"github.com/rancher/hosted-providers-e2e/hosted/helpers"
	tkehelper "github.com/rancher/hosted-providers-e2e/hosted/tke/helper"
)

const (
	increaseBy = 1 // 节点池扩缩容步长
)

var (
	ctx         helpers.RancherContext
	cluster     *management.Cluster
	clusterName string
	testCaseID  int64
)

// go test 入口：注册断言失败处理并启动 Ginkgo
func TestP0(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "TKE P0 Suite")
}

var _ = SynchronizedBeforeSuite(func() []byte {
	helpers.CommonSynchronizedBeforeSuite()
	return nil
}, func() {
	ctx = helpers.CommonBeforeSuite()
})

var _ = BeforeEach(func() {
	cluster = nil
	clusterName = namegen.AppendRandomString(helpers.ClusterNamePrefix)
})

var _ = ReportBeforeEach(func(_ SpecReport) { testCaseID = -1 })
var _ = ReportAfterEach(func(report SpecReport) { Qase(testCaseID, report) })

func p0UpgradeK8sVersionChecks(c *management.Cluster, client *rancher.Client, name string) {
	helpers.ClusterIsReadyChecks(c, client, name)

	upgradeTo, err := tkehelper.GetK8sVersion(client, false)
	Expect(err).To(BeNil())
	GinkgoLogr.Info(fmt.Sprintf("Upgrading TKE cluster to version %s", upgradeTo))

	By("upgrading the TKE cluster", func() {
		c, err = tkehelper.UpgradeClusterKubernetesVersion(c, upgradeTo, client, true)
		Expect(err).To(BeNil())
	})
}

func p0NodesChecks(c *management.Cluster, client *rancher.Client, name string) {
	helpers.ClusterIsReadyChecks(c, client, name)

	cfgPools := c.TKEConfig.NodePoolList
	initial := cfgPools[0].AutoScalingGroupPara.DesiredCapacity

	By("scaling up the NodePool", func() {
		var err error
		c, err = tkehelper.ScaleNodeGroup(c, client, initial+increaseBy, true, true)
		Expect(err).To(BeNil())
	})

	By("scaling down the NodePool", func() {
		var err error
		c, err = tkehelper.ScaleNodeGroup(c, client, initial, true, true)
		Expect(err).To(BeNil())
	})

	By("adding a NodePool", func() {
		var err error
		c, err = tkehelper.AddNodePool(c, increaseBy, client, true, true)
		Expect(err).To(BeNil())
	})

	By("deleting the NodePool", func() {
		var err error
		c, err = tkehelper.DeleteNodePool(c, client, true, true)
		Expect(err).To(BeNil())
	})
}
