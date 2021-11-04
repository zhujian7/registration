package spoke

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"time"

	addonclient "open-cluster-management.io/api/client/addon/clientset/versioned"
	addoninformers "open-cluster-management.io/api/client/addon/informers/externalversions"
	clusterv1client "open-cluster-management.io/api/client/cluster/clientset/versioned"
	clusterv1informers "open-cluster-management.io/api/client/cluster/informers/externalversions"
	"open-cluster-management.io/registration/pkg/clientcert"
	"open-cluster-management.io/registration/pkg/features"
	"open-cluster-management.io/registration/pkg/helpers"
	"open-cluster-management.io/registration/pkg/spoke/addon"
	"open-cluster-management.io/registration/pkg/spoke/managedcluster"

	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"

	"github.com/spf13/pflag"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

const (
	// spokeAgentNameLength is the length of the spoke agent name which is generated automatically
	spokeAgentNameLength = 5
	// defaultSpokeComponentNamespace is the default namespace in which the spoke agent is deployed
	defaultSpokeComponentNamespace = "open-cluster-management"
)

// AddOnLeaseControllerSyncInterval is exposed so that integration tests can crank up the constroller sync speed.
// TODO if we register the lease informer to the lease controller, we need to increase this time
var AddOnLeaseControllerSyncInterval = 30 * time.Second

// SpokeAgentOptions holds configuration for spoke cluster agent
type spokeAgent struct {
	componentNamespace       string
	clusterName              string
	agentName                string
	bootstrapKubeconfig      string
	hubKubeconfigSecret      string
	hubKubeconfigDir         string
	spokeExternalServerURLs  []string
	clusterHealthCheckPeriod time.Duration
	maxCustomClusterClaims   int
	spokeKubeconfigFile      string
	spokeKubeConfig          *rest.Config
}

// SpokeAgentOption defines the functional option type for spokeAgent.
type SpokeAgentOption func(*spokeAgent) *spokeAgent

// WithClusterName limits the spoke agent to a specific cluster
func WithClusterName(clusterName string) SpokeAgentOption {
	return func(s *spokeAgent) *spokeAgent {
		s.clusterName = clusterName
		return s
	}
}

// WithBootstrapKubeconfig sets the path of bootstrap kubeconfig
func WithBootstrapKubeconfig(bootstrapKubeconfig string) SpokeAgentOption {
	return func(s *spokeAgent) *spokeAgent {
		s.bootstrapKubeconfig = bootstrapKubeconfig
		return s
	}
}

// WithHubKubeconfigSecret sets the secret of hub kubeconfig
func WithHubKubeconfigSecret(hubKubeconfigSecret string) SpokeAgentOption {
	return func(s *spokeAgent) *spokeAgent {
		s.hubKubeconfigSecret = hubKubeconfigSecret
		return s
	}
}

// WithClusterName sets the hub kubeconfig directory
func WithHubKubeconfigDir(hubKubeconfigDir string) SpokeAgentOption {
	return func(s *spokeAgent) *spokeAgent {
		s.hubKubeconfigDir = hubKubeconfigDir
		return s
	}
}

// WithSpokeKubeConfig sets the kubeconfig for spoke/managed cluster
func WithSpokeKubeConfig(spokeKubeConfig *rest.Config) SpokeAgentOption {
	return func(s *spokeAgent) *spokeAgent {
		s.spokeKubeConfig = spokeKubeConfig
		return s
	}
}

// WithClusterHealthCheckPeriod sets the period of checking cluster health
func WithClusterHealthCheckPeriod(clusterHealthCheckPeriod time.Duration) SpokeAgentOption {
	return func(s *spokeAgent) *spokeAgent {
		s.clusterHealthCheckPeriod = clusterHealthCheckPeriod
		return s
	}
}

// WithMaxCustomClusterClaims sets the maximum number of cluster claims
func WithMaxCustomClusterClaims(maxCustomClusterClaims int) SpokeAgentOption {
	return func(s *spokeAgent) *spokeAgent {
		s.maxCustomClusterClaims = maxCustomClusterClaims
		return s
	}
}

// NewSpokeAgent returns a SpokeAgent
func NewSpokeAgent(options ...SpokeAgentOption) *spokeAgent {
	agent := &spokeAgent{
		hubKubeconfigSecret:      "hub-kubeconfig-secret",
		hubKubeconfigDir:         "/spoke/hub-kubeconfig",
		clusterHealthCheckPeriod: 1 * time.Minute,
		maxCustomClusterClaims:   20,
	}

	for _, option := range options {
		option(agent)
	}

	return agent
}

// Run starts the controllers on spoke agent to register to the hub.
//
// The spoke agent uses four kubeconfigs for different concerns:
// - The 'agent' kubeconfig: used to communicate with the cluster where the agent is running.
// - The 'spoke' kubeconfig: used to communicate with the spoke/managed cluster which will
//   be registered to the hub.
// - The 'bootstrap' kubeconfig: used to communicate with the hub in order to
//   submit a CertificateSigningRequest, begin the join flow with the hub, and
//   to write the 'hub' kubeconfig.
// - The 'hub' kubeconfig: used to communicate with the hub using a signed
//   certificate from the hub.
//
// RunSpokeAgent handles the following scenarios:
//   #1. Bootstrap kubeconfig is valid and there is no valid hub kubeconfig in secret
//   #2. Both bootstrap kubeconfig and hub kubeconfig are valid
//   #3. Bootstrap kubeconfig is invalid (e.g. certificate expired) and hub kubeconfig is valid
//   #4. Neither bootstrap kubeconfig nor hub kubeconfig is valid
//
// A temporary ClientCertForHubController with bootstrap kubeconfig is created
// and started if the hub kubeconfig does not exist or is invalid and used to
// create a valid hub kubeconfig. Once the hub kubeconfig is valid, the
// temporary controller is stopped and the main controllers are started.
func (o *spokeAgent) Run(ctx context.Context, controllerContext *controllercmd.ControllerContext) error {
	// create agent kube client
	agentKubeClient, err := kubernetes.NewForConfig(controllerContext.KubeConfig)
	if err != nil {
		return err
	}

	// load spoke client config and create spoke clients,
	// the registration agent may running not in the spoke/managed cluster.
	if o.spokeKubeConfig == nil {
		if o.spokeKubeconfigFile != "" {
			o.spokeKubeConfig, err = clientcmd.BuildConfigFromFlags("" /* leave masterurl as empty */, o.spokeKubeconfigFile)
			if err != nil {
				return fmt.Errorf("unable to load spoke kubeconfig from file %q: %w", o.spokeKubeconfigFile, err)
			}
		} else {
			o.spokeKubeConfig = controllerContext.KubeConfig
		}
	}

	spokeKubeClient, err := kubernetes.NewForConfig(o.spokeKubeConfig)
	if err != nil {
		return err
	}

	if err := o.complete(spokeKubeClient.CoreV1(), ctx, controllerContext.EventRecorder); err != nil {
		klog.Fatal(err)
	}

	if err := o.Validate(); err != nil {
		klog.Fatal(err)
	}

	klog.Infof("Cluster name is %q and agent name is %q", o.clusterName, o.agentName)

	// create shared informer factory for spoke cluster
	spokeKubeInformerFactory := informers.NewSharedInformerFactory(spokeKubeClient, 10*time.Minute)
	namespacedSpokeKubeInformerFactory := informers.NewSharedInformerFactoryWithOptions(spokeKubeClient, 10*time.Minute, informers.WithNamespace(o.componentNamespace))

	// get spoke cluster CA bundle
	spokeClusterCABundle, err := o.getSpokeClusterCABundle(o.spokeKubeConfig)
	if err != nil {
		return err
	}

	// load bootstrap client config and create bootstrap clients
	bootstrapClientConfig, err := clientcmd.BuildConfigFromFlags("", o.bootstrapKubeconfig)
	if err != nil {
		return fmt.Errorf("unable to load bootstrap kubeconfig from file %q: %w", o.bootstrapKubeconfig, err)
	}
	bootstrapKubeClient, err := kubernetes.NewForConfig(bootstrapClientConfig)
	if err != nil {
		return err
	}
	bootstrapClusterClient, err := clusterv1client.NewForConfig(bootstrapClientConfig)
	if err != nil {
		return err
	}

	// start a SpokeClusterCreatingController to make sure there is a spoke cluster on hub cluster
	spokeClusterCreatingController := managedcluster.NewManagedClusterCreatingController(
		o.clusterName, o.spokeExternalServerURLs,
		spokeClusterCABundle,
		bootstrapClusterClient,
		controllerContext.EventRecorder,
	)
	go spokeClusterCreatingController.Run(ctx, 1)

	hubKubeconfigSecretController := managedcluster.NewHubKubeconfigSecretController(
		o.hubKubeconfigDir, o.componentNamespace, o.hubKubeconfigSecret,
		// the hub kubeconfig secret stored in the cluster where the agent pod runs
		agentKubeClient.CoreV1(),
		namespacedSpokeKubeInformerFactory.Core().V1().Secrets(),
		controllerContext.EventRecorder,
	)
	go hubKubeconfigSecretController.Run(ctx, 1)

	// check if there already exists a valid client config for hub
	ok, err := o.hasValidHubClientConfig()
	if err != nil {
		return err
	}

	// create and start a ClientCertForHubController for spoke agent bootstrap to deal with scenario #1 and #4.
	// Running the bootstrap ClientCertForHubController is optional. If always run it no matter if there already
	// exists a valid client config for hub or not, the controller will be started and then stopped immediately
	// in scenario #2 and #3, which results in an error message in log: 'Observed a panic: timeout waiting for
	// informer cache'
	if !ok {
		// create a ClientCertForHubController for spoke agent bootstrap
		bootstrapInformerFactory := informers.NewSharedInformerFactory(bootstrapKubeClient, 10*time.Minute)

		// create a kubeconfig with references to the key/cert files in the same secret
		kubeconfig := clientcert.BuildKubeconfig(bootstrapClientConfig, clientcert.TLSCertFile, clientcert.TLSKeyFile)
		kubeconfigData, err := clientcmd.Write(kubeconfig)
		if err != nil {
			return err
		}

		controllerName := fmt.Sprintf("BootstrapClientCertController@cluster:%s", o.clusterName)
		clientCertForHubController := managedcluster.NewClientCertForHubController(
			o.clusterName, o.agentName, o.componentNamespace, o.hubKubeconfigSecret,
			kubeconfigData,
			// store the secret in the cluster where the agent pod runs
			agentKubeClient.CoreV1(),
			bootstrapKubeClient.CertificatesV1().CertificateSigningRequests(),
			bootstrapInformerFactory.Certificates().V1().CertificateSigningRequests(),
			namespacedSpokeKubeInformerFactory.Core().V1().Secrets(),
			controllerContext.EventRecorder,
			controllerName,
		)

		bootstrapCtx, stopBootstrap := context.WithCancel(ctx)

		go bootstrapInformerFactory.Start(bootstrapCtx.Done())
		go namespacedSpokeKubeInformerFactory.Start(bootstrapCtx.Done())

		go clientCertForHubController.Run(bootstrapCtx, 1)

		// wait for the hub client config is ready.
		klog.Info("Waiting for hub client config and managed cluster to be ready")
		if err := wait.PollImmediateInfinite(1*time.Second, o.hasValidHubClientConfig); err != nil {
			// TODO need run the bootstrap CSR forever to re-establish the client-cert if it is ever lost.
			stopBootstrap()
			return err
		}

		// stop the clientCertForHubController for bootstrap once the hub client config is ready
		stopBootstrap()
	}

	// create hub clients and shared informer factories from hub kube config
	hubClientConfig, err := clientcmd.BuildConfigFromFlags("", path.Join(o.hubKubeconfigDir, clientcert.KubeconfigFile))
	if err != nil {
		return err
	}

	hubKubeClient, err := kubernetes.NewForConfig(hubClientConfig)
	if err != nil {
		return err
	}

	hubClusterClient, err := clusterv1client.NewForConfig(hubClientConfig)
	if err != nil {
		return err
	}

	addOnClient, err := addonclient.NewForConfig(hubClientConfig)
	if err != nil {
		return err
	}

	hubKubeInformerFactory := informers.NewSharedInformerFactory(hubKubeClient, 10*time.Minute)
	addOnInformerFactory := addoninformers.NewSharedInformerFactoryWithOptions(
		addOnClient, 10*time.Minute, addoninformers.WithNamespace(o.clusterName))
	// create a cluster informer factory with name field selector because we just need to handle the current spoke cluster
	hubClusterInformerFactory := clusterv1informers.NewSharedInformerFactoryWithOptions(
		hubClusterClient,
		10*time.Minute,
		clusterv1informers.WithTweakListOptions(func(listOptions *metav1.ListOptions) {
			listOptions.FieldSelector = fields.OneTermEqualSelector("metadata.name", o.clusterName).String()
		}),
	)

	controllerContext.EventRecorder.Event("HubClientConfigReady", "Client config for hub is ready.")

	// create a kubeconfig with references to the key/cert files in the same secret
	kubeconfig := clientcert.BuildKubeconfig(hubClientConfig, clientcert.TLSCertFile, clientcert.TLSKeyFile)
	kubeconfigData, err := clientcmd.Write(kubeconfig)
	if err != nil {
		return err
	}

	// create another ClientCertForHubController for client certificate rotation
	controllerName := fmt.Sprintf("ClientCertController@cluster:%s", o.clusterName)
	clientCertForHubController := managedcluster.NewClientCertForHubController(
		o.clusterName, o.agentName, o.componentNamespace, o.hubKubeconfigSecret,
		kubeconfigData,
		// store the secret in the cluster where the agent pod runs
		agentKubeClient.CoreV1(),
		hubKubeClient.CertificatesV1().CertificateSigningRequests(),
		hubKubeInformerFactory.Certificates().V1().CertificateSigningRequests(),
		namespacedSpokeKubeInformerFactory.Core().V1().Secrets(),
		controllerContext.EventRecorder,
		controllerName,
	)

	// create ManagedClusterJoiningController to reconcile instances of ManagedCluster on the managed cluster
	managedClusterJoiningController := managedcluster.NewManagedClusterJoiningController(
		o.clusterName,
		hubClusterClient,
		hubClusterInformerFactory.Cluster().V1().ManagedClusters(),
		controllerContext.EventRecorder,
	)

	// create ManagedClusterLeaseController to keep the spoke cluster heartbeat
	managedClusterLeaseController := managedcluster.NewManagedClusterLeaseController(
		o.clusterName,
		hubKubeClient,
		hubClusterInformerFactory.Cluster().V1().ManagedClusters(),
		controllerContext.EventRecorder,
	)

	// create NewManagedClusterStatusController to update the spoke cluster status
	managedClusterHealthCheckController := managedcluster.NewManagedClusterStatusController(
		o.clusterName,
		hubClusterClient,
		hubClusterInformerFactory.Cluster().V1().ManagedClusters(),
		spokeKubeClient.Discovery(),
		spokeKubeInformerFactory.Core().V1().Nodes(),
		o.clusterHealthCheckPeriod,
		controllerContext.EventRecorder,
	)
	spokeClusterClient, err := clusterv1client.NewForConfig(controllerContext.KubeConfig)
	if err != nil {
		return err
	}
	spokeClusterInformerFactory := clusterv1informers.NewSharedInformerFactory(spokeClusterClient, 10*time.Minute)

	var managedClusterClaimController factory.Controller
	if features.DefaultMutableFeatureGate.Enabled(features.ClusterClaim) {
		// create managedClusterClaimController to sync cluster claims
		managedClusterClaimController = managedcluster.NewManagedClusterClaimController(
			o.clusterName,
			o.maxCustomClusterClaims,
			hubClusterClient,
			hubClusterInformerFactory.Cluster().V1().ManagedClusters(),
			spokeClusterInformerFactory.Cluster().V1alpha1().ClusterClaims(),
			controllerContext.EventRecorder,
		)
	}

	var addOnLeaseController factory.Controller
	var addOnRegistrationController factory.Controller
	if features.DefaultMutableFeatureGate.Enabled(features.AddonManagement) {
		addOnLeaseController = addon.NewManagedClusterAddOnLeaseController(
			o.clusterName,
			addOnClient,
			addOnInformerFactory.Addon().V1alpha1().ManagedClusterAddOns(),
			hubKubeClient.CoordinationV1(),
			spokeKubeClient.CoordinationV1(),
			AddOnLeaseControllerSyncInterval, //TODO: this interval time should be allowed to change from outside
			controllerContext.EventRecorder,
		)

		addOnRegistrationController = addon.NewAddOnRegistrationController(
			o.clusterName,
			o.agentName,
			kubeconfigData,
			agentKubeClient,
			// addon runs in the cluster where the agent pod runs
			hubKubeInformerFactory.Certificates().V1().CertificateSigningRequests(),
			addOnInformerFactory.Addon().V1alpha1().ManagedClusterAddOns(),
			hubKubeClient.CertificatesV1().CertificateSigningRequests(),
			controllerContext.EventRecorder,
		)
	}

	go hubKubeInformerFactory.Start(ctx.Done())
	go hubClusterInformerFactory.Start(ctx.Done())
	go spokeKubeInformerFactory.Start(ctx.Done())
	go namespacedSpokeKubeInformerFactory.Start(ctx.Done())
	go spokeClusterInformerFactory.Start(ctx.Done())
	go addOnInformerFactory.Start(ctx.Done())

	go clientCertForHubController.Run(ctx, 1)
	go managedClusterJoiningController.Run(ctx, 1)
	go managedClusterLeaseController.Run(ctx, 1)
	go managedClusterHealthCheckController.Run(ctx, 1)
	if features.DefaultMutableFeatureGate.Enabled(features.ClusterClaim) {
		go managedClusterClaimController.Run(ctx, 1)
	}
	if features.DefaultMutableFeatureGate.Enabled(features.AddonManagement) {
		go addOnLeaseController.Run(ctx, 1)
		go addOnRegistrationController.Run(ctx, 1)
	}

	<-ctx.Done()
	return nil
}

// AddFlags registers flags for Agent
func (o *spokeAgent) AddFlags(fs *pflag.FlagSet) {
	features.DefaultMutableFeatureGate.AddFlag(fs)
	fs.StringVar(&o.clusterName, "cluster-name", o.clusterName,
		"If non-empty, will use as cluster name instead of generated random name.")
	fs.StringVar(&o.bootstrapKubeconfig, "bootstrap-kubeconfig", o.bootstrapKubeconfig,
		"The path of the kubeconfig file for agent bootstrap.")
	fs.StringVar(&o.hubKubeconfigSecret, "hub-kubeconfig-secret", o.hubKubeconfigSecret,
		"The name of secret in component namespace storing kubeconfig for hub.")
	fs.StringVar(&o.hubKubeconfigDir, "hub-kubeconfig-dir", o.hubKubeconfigDir,
		"The mount path of hub-kubeconfig-secret in the container.")
	fs.StringVar(&o.spokeKubeconfigFile, "spoke-kubeconfig", o.spokeKubeconfigFile,
		"The path of the kubeconfig file for spoke cluster.")
	fs.StringArrayVar(&o.spokeExternalServerURLs, "spoke-external-server-urls", o.spokeExternalServerURLs,
		"A list of reachable spoke cluster api server URLs for hub cluster.")
	fs.DurationVar(&o.clusterHealthCheckPeriod, "cluster-healthcheck-period", o.clusterHealthCheckPeriod,
		"The period to check managed cluster kube-apiserver health")
	fs.IntVar(&o.maxCustomClusterClaims, "max-custom-cluster-claims", o.maxCustomClusterClaims,
		"The max number of custom cluster claims to expose.")
}

// Validate verifies the inputs.
func (o *spokeAgent) Validate() error {
	if o.bootstrapKubeconfig == "" {
		return errors.New("bootstrap-kubeconfig is required")
	}

	if o.clusterName == "" {
		return errors.New("cluster name is empty")
	}

	if o.agentName == "" {
		return errors.New("agent name is empty")
	}

	// if SpokeExternalServerURLs is specified we validate every URL in it, we expect the spoke external server URL is https
	if len(o.spokeExternalServerURLs) != 0 {
		for _, serverURL := range o.spokeExternalServerURLs {
			if !helpers.IsValidHTTPSURL(serverURL) {
				return fmt.Errorf("%q is invalid", serverURL)
			}
		}
	}

	if o.clusterHealthCheckPeriod <= 0 {
		return errors.New("cluster healthcheck period must greater than zero")
	}

	return nil
}

// complete fills in missing values.
func (o *spokeAgent) complete(coreV1Client corev1client.CoreV1Interface, ctx context.Context, recorder events.Recorder) error {
	// get component namespace of spoke agent
	nsBytes, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		o.componentNamespace = defaultSpokeComponentNamespace
	} else {
		o.componentNamespace = string(nsBytes)
	}

	// dump data in hub kubeconfig secret into file system if it exists
	err = managedcluster.DumpSecret(coreV1Client, o.componentNamespace, o.hubKubeconfigSecret,
		o.hubKubeconfigDir, ctx, recorder)
	if err != nil {
		return err
	}

	// load or generate cluster/agent names
	o.clusterName, o.agentName = o.getOrGenerateClusterAgentNames()

	return nil
}

// generateClusterName generates a name for spoke cluster
func generateClusterName() string {
	return string(uuid.NewUUID())
}

// generateAgentName generates a random name for spoke cluster agent
func generateAgentName() string {
	return utilrand.String(spokeAgentNameLength)
}

// hasValidHubClientConfig returns ture if all the conditions below are met:
//   1. KubeconfigFile exists;
//   2. TLSKeyFile exists;
//   3. TLSCertFile exists;
//   4. Certificate in TLSCertFile is issued for the current cluster/agent;
//   5. Certificate in TLSCertFile is not expired;
// Normally, KubeconfigFile/TLSKeyFile/TLSCertFile will be created once the bootstrap process
// completes. Changing the name of the cluster will make the existing hub kubeconfig invalid,
// because certificate in TLSCertFile is issued to a specific cluster/agent.
func (o *spokeAgent) hasValidHubClientConfig() (bool, error) {
	kubeconfigPath := path.Join(o.hubKubeconfigDir, clientcert.KubeconfigFile)
	if _, err := os.Stat(kubeconfigPath); os.IsNotExist(err) {
		klog.V(4).Infof("Kubeconfig file %q not found", kubeconfigPath)
		return false, nil
	}

	keyPath := path.Join(o.hubKubeconfigDir, clientcert.TLSKeyFile)
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		klog.V(4).Infof("TLS key file %q not found", keyPath)
		return false, nil
	}

	certPath := path.Join(o.hubKubeconfigDir, clientcert.TLSCertFile)
	certData, err := ioutil.ReadFile(path.Clean(certPath))
	if err != nil {
		klog.V(4).Infof("Unable to load TLS cert file %q", certPath)
		return false, nil
	}

	// check if the tls certificate is issued for the current cluster/agent
	clusterName, agentName, err := managedcluster.GetClusterAgentNamesFromCertificate(certData)
	if err != nil {
		return false, nil
	}
	if clusterName != o.clusterName || agentName != o.agentName {
		klog.V(4).Infof("Certificate in file %q is issued for agent %q instead of %q",
			certPath, fmt.Sprintf("%s:%s", clusterName, agentName),
			fmt.Sprintf("%s:%s", o.clusterName, o.agentName))
		return false, nil
	}

	return clientcert.IsCertificateValid(certData, nil)
}

// getOrGenerateClusterAgentNames returns cluster name and agent name.
// Rules for picking up cluster name:
//   1. Use cluster name from input arguments if 'cluster-name' is specified;
//   2. Parse cluster name from the common name of the certification subject if the certification exists;
//   3. Fallback to cluster name in the mounted secret if it exists;
//   4. TODO: Read cluster name from openshift struct if the agent is running in an openshift cluster;
//   5. Generate a random cluster name then;

// Rules for picking up agent name:
//   1. Parse agent name from the common name of the certification subject if the certification exists;
//   2. Fallback to agent name in the mounted secret if it exists;
//   3. Generate a random agent name then;
func (o *spokeAgent) getOrGenerateClusterAgentNames() (string, string) {
	// try to load cluster/agent name from tls certification
	var clusterNameInCert, agentNameInCert string
	certPath := path.Join(o.hubKubeconfigDir, clientcert.TLSCertFile)
	certData, certErr := ioutil.ReadFile(path.Clean(certPath))
	if certErr == nil {
		clusterNameInCert, agentNameInCert, _ = managedcluster.GetClusterAgentNamesFromCertificate(certData)
	}

	clusterName := o.clusterName
	// if cluster name is not specified with input argument, try to load it from file
	if clusterName == "" {
		// TODO, read cluster name from openshift struct if the spoke agent is running in an openshift cluster

		// and then load the cluster name from the mounted secret
		clusterNameFilePath := path.Join(o.hubKubeconfigDir, clientcert.ClusterNameFile)
		clusterNameBytes, err := ioutil.ReadFile(path.Clean(clusterNameFilePath))
		switch {
		case len(clusterNameInCert) > 0:
			// use cluster name loaded from the tls certification
			clusterName = clusterNameInCert
			if clusterNameInCert != string(clusterNameBytes) {
				klog.Warningf("Use cluster name %q in certification instead of %q in the mounted secret", clusterNameInCert, string(clusterNameBytes))
			}
		case err == nil:
			// use cluster name load from the mounted secret
			clusterName = string(clusterNameBytes)
		default:
			// generate random cluster name
			clusterName = generateClusterName()
		}
	}

	// try to load agent name from the mounted secret
	agentNameFilePath := path.Join(o.hubKubeconfigDir, clientcert.AgentNameFile)
	agentNameBytes, err := ioutil.ReadFile(path.Clean(agentNameFilePath))
	var agentName string
	switch {
	case len(agentNameInCert) > 0:
		// use agent name loaded from the tls certification
		agentName = agentNameInCert
		if agentNameInCert != string(agentNameBytes) {
			klog.Warningf("Use agent name %q in certification instead of %q in the mounted secret", agentNameInCert, string(agentNameBytes))
		}
	case err == nil:
		// use agent name loaded from the mounted secret
		agentName = string(agentNameBytes)
	default:
		// generate random agent name
		agentName = generateAgentName()
	}

	return clusterName, agentName
}

// getSpokeClusterCABundle returns the spoke cluster Kubernetes client CA data when SpokeExternalServerURLs is specified
func (o *spokeAgent) getSpokeClusterCABundle(kubeConfig *rest.Config) ([]byte, error) {
	if len(o.spokeExternalServerURLs) == 0 {
		return nil, nil
	}
	if kubeConfig.CAData != nil {
		return kubeConfig.CAData, nil
	}
	data, err := ioutil.ReadFile(kubeConfig.CAFile)
	if err != nil {
		return nil, err
	}
	return data, nil
}
