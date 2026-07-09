package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC)
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/krishau99/docs/api/v1alpha1"
	"github.com/krishau99/docs/internal/controller"
	"github.com/krishau99/docs/internal/crdinstall"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr          string
		enableLeaderElection bool
		probeAddr            string
		caCertFile           string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for the operator manager. "+
			"Enabling this will ensure there is only one active operator instance.")
	flag.StringVar(&caCertFile, "ca-cert-file", "",
		"Path to a custom CA certificate file (PEM) for TLS verification when talking to Gitea or registries.")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	restConfig := ctrl.GetConfigOrDie()

	// Load the optional custom CA certificate for external HTTP calls (Gitea, registries).
	// It is intentionally NOT applied to restConfig so the cluster's own CA is preserved.
	var customCAPEM []byte
	if caCertFile == "" {
		caCertFile = os.Getenv("CA_CERT_FILE")
	}
	if caCertFile != "" {
		pem, err := os.ReadFile(caCertFile)
		if err != nil {
			setupLog.Error(err, "failed to read custom CA certificate file")
			os.Exit(1)
		}
		customCAPEM = pem
	}

	// Install CRDs before starting the manager
	if err := installCRDs(restConfig); err != nil {
		setupLog.Error(err, "failed to install CRDs")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "docspage-operator.docspage.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.DocsPageReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		DefaultCACert: customCAPEM,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DocsPage")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// installCRDs ensures the DocsPage CRD is installed in the cluster.
// It embeds the CRD YAML into the binary and applies it on startup.
func installCRDs(cfg *rest.Config) error {
	setupLog.Info("installing CRDs")

	crdYAML := crdinstall.DocsPageCRD()

	// Decode the CRD from YAML
	crdScheme := runtime.NewScheme()
	utilruntime.Must(apiextensionsv1.AddToScheme(crdScheme))

	decode := serializer.NewCodecFactory(crdScheme).UniversalDeserializer().Decode
	obj, _, err := decode(crdYAML, nil, nil)
	if err != nil {
		return err
	}

	crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
	if !ok {
		return fmt.Errorf("embedded YAML is not a CustomResourceDefinition, got %T", obj)
	}

	// Create the apiextensions client
	apiextClient, err := apiextensionsclient.NewForConfig(cfg)
	if err != nil {
		return err
	}

	ctx := context.Background()
	existing, err := apiextClient.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crd.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		setupLog.Info("creating CRD", "name", crd.Name)
		_, err = apiextClient.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}

	// Update the existing CRD to ensure it's up to date
	setupLog.Info("updating CRD", "name", crd.Name)
	crd.ResourceVersion = existing.ResourceVersion
	_, err = apiextClient.ApiextensionsV1().CustomResourceDefinitions().Update(ctx, crd, metav1.UpdateOptions{})
	return err
}
