package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	servingv1alpha1 "github.com/MuratAlkan06/ml-observability-system/operator/api/v1alpha1"
	"github.com/MuratAlkan06/ml-observability-system/operator/internal/controller"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(servingv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// getWatchNamespace returns the namespace(s) the manager should watch for changes.
// It reads the value from the WATCH_NAMESPACE environment variable.
// - If WATCH_NAMESPACE is not set, an error is returned
// - If WATCH_NAMESPACE contains a single namespace, the manager watches that namespace
// - If WATCH_NAMESPACE contains comma-separated namespaces, the manager watches those namespaces
func getWatchNamespace() (string, error) {
	watchNamespaceEnvVar := "WATCH_NAMESPACE"
	ns, found := os.LookupEnv(watchNamespaceEnvVar)
	if !found {
		return "", fmt.Errorf("%s must be set", watchNamespaceEnvVar)
	}
	return ns, nil
}

// setupCacheNamespaces configures the cache to watch specific namespace(s).
// It supports both single namespace ("ns1") and multi-namespace ("ns1,ns2,ns3") formats.
func setupCacheNamespaces(namespaces string) cache.Options {
	defaultNamespaces := make(map[string]cache.Config)
	for ns := range strings.SplitSeq(namespaces, ",") {
		defaultNamespaces[strings.TrimSpace(ns)] = cache.Config{}
	}
	return cache.Options{
		DefaultNamespaces: defaultNamespaces,
	}
}

const (
	// leaderElectionID names the Lease the operator's replicas contend for.
	leaderElectionID = "5bb4b9ad.mlobs.dev"
	// leaderElectionNamespace holds that Lease: mlobs, the one namespace the
	// operator's Role grants leases in (docs/PLAN.md D33). Naming it also
	// lets the manager run out of cluster, where it cannot be inferred.
	leaderElectionNamespace = "mlobs"
)

// validateImagePrefix rejects an --image-prefix that cannot head an image
// reference of the form <prefix>/mlobs-api:<tag>.
func validateImagePrefix(prefix string) error {
	switch {
	case prefix == "":
		return errors.New("--image-prefix must not be empty")
	case strings.ContainsAny(prefix, " \t\n@"):
		return fmt.Errorf("--image-prefix %q must not contain whitespace or a digest", prefix)
	case strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/"):
		return fmt.Errorf("--image-prefix %q must not begin or end with a slash", prefix)
	}
	return nil
}

// setLeaderElection applies kubebuilder's default leader election to opts:
// a coordination.k8s.io Lease, named leaderElectionID, in mlobs (D35).
func setLeaderElection(opts *ctrl.Options, enabled bool) {
	opts.LeaderElection = enabled
	opts.LeaderElectionID = leaderElectionID
	opts.LeaderElectionNamespace = leaderElectionNamespace
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var imagePrefix string
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager, through a Lease in the mlobs namespace. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&imagePrefix, "image-prefix", controller.DefaultImagePrefix,
		"Registry and owner of the api image the operator writes into deployment/api and deployment/api-canary, "+
			"as <prefix>/mlobs-api:<tag>. deploy/k3s/apply.sh renders its IMAGE_PREFIX here.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if err := validateImagePrefix(imagePrefix); err != nil {
		setupLog.Error(err, "Invalid flag")
		os.Exit(1)
	}

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	// Get the namespace(s) for namespace-scoped mode from WATCH_NAMESPACE environment variable.
	// The manager will only watch and manage resources in the specified namespace(s).
	watchNamespace, err := getWatchNamespace()
	if err != nil {
		setupLog.Error(err, "Unable to get WATCH_NAMESPACE, "+
			"the manager will watch and manage resources in all namespaces")
		os.Exit(1)
	}

	// Configure manager options for namespace-scoped mode
	mgrOptions := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	}
	setLeaderElection(&mgrOptions, enableLeaderElection)

	// Configure cache to watch namespace(s) specified in WATCH_NAMESPACE
	mgrOptions.Cache = setupCacheNamespaces(watchNamespace)
	setupLog.Info("Watching namespace(s)", "namespaces", watchNamespace)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOptions)
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	if err := (&controller.ServingDeploymentReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		// GetEventRecorderFor writes core/v1 Events, which the Role grants
		// (docs/PLAN.md D33). Its replacement, GetEventRecorder, writes
		// events.k8s.io/v1 Events, which the Role does not grant.
		Recorder:    mgr.GetEventRecorderFor("servingdeployment-controller"), //nolint:staticcheck // SA1019, see above
		ImagePrefix: imagePrefix,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "servingdeployment")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
