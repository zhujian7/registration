package integration_test

import (
	"context"
	"path"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"

	"open-cluster-management.io/registration/pkg/spoke"
	"open-cluster-management.io/registration/test/integration/util"

	"github.com/openshift/library-go/pkg/controller/controllercmd"
)

var _ = ginkgo.Describe("Certificate Rotation", func() {
	ginkgo.It("Certificate should be automatically rotated when it is about to expire", func() {
		var err error

		managedClusterName := "rotationtest-spokecluster"
		hubKubeconfigSecret := "rotationtest-hub-kubeconfig-secret"
		hubKubeconfigDir := path.Join(util.TestDir, "rotationtest", "hub-kubeconfig")

		// run registration agent
		go func() {
			spokeAgent := spoke.NewSpokeAgent(
				spoke.WithClusterName(managedClusterName),
				spoke.WithBootstrapKubeconfig(bootstrapKubeConfigFile),
				spoke.WithHubKubeconfigSecret(hubKubeconfigSecret),
				spoke.WithHubKubeconfigDir(hubKubeconfigDir),
				spoke.WithClusterHealthCheckPeriod(1*time.Minute),
				spoke.WithSpokeKubeConfig(spokeCfg),
			)
			err := spokeAgent.Run(context.Background(), &controllercmd.ControllerContext{
				KubeConfig:    spokeCfg,
				EventRecorder: util.NewIntegrationTestEventRecorder("rotationtest"),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()

		// after bootstrap the spokecluster and csr should be created
		gomega.Eventually(func() bool {
			if _, err := util.GetManagedCluster(clusterClient, managedClusterName); err != nil {
				return false
			}
			return true
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.BeTrue())

		gomega.Eventually(func() bool {
			if _, err := util.FindUnapprovedSpokeCSR(kubeClient, managedClusterName); err != nil {
				return false
			}
			return true
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.BeTrue())

		// simulate hub cluster admin approve the csr with a short time certificate
		err = util.ApproveSpokeClusterCSR(kubeClient, managedClusterName, time.Second*20)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// simulate hub cluster admin accept the spokecluster
		err = util.AcceptManagedCluster(clusterClient, managedClusterName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// the hub kubeconfig secret should be filled after the csr is approved
		gomega.Eventually(func() bool {
			if _, err := util.GetFilledHubKubeConfigSecret(kubeClient, testNamespace, hubKubeconfigSecret); err != nil {
				return false
			}
			return true
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.BeTrue())

		// the agent should rotate the certificate because the certificate with a short valid time
		// the hub controller should auto approve it
		gomega.Eventually(func() bool {
			if _, err := util.FindAutoApprovedSpokeCSR(kubeClient, managedClusterName); err != nil {
				return false
			}
			return true
		}, eventuallyTimeout, eventuallyInterval).Should(gomega.BeTrue())
	})
})
