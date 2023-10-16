/*
Copyright 2023.

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

package main

import (
	"context"
	"crypto/rand"
	"flag"
	"os"
	"runtime"
	"time"

	"github.com/go-logr/logr"
	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	"github.com/operator-framework/helm-operator-plugins/pkg/annotation"
	"github.com/operator-framework/helm-operator-plugins/pkg/reconciler"
	"github.com/operator-framework/helm-operator-plugins/pkg/watches"
	"helm.sh/helm/v3/pkg/chartutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrlruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	//+kubebuilder:scaffold:imports
)

var (
	scheme                         = ctrlruntime.NewScheme()
	setupLog                       = ctrl.Log.WithName("setup")
	defaultMaxConcurrentReconciles = runtime.NumCPU()
	defaultReconcilePeriod         = time.Minute
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	//+kubebuilder:scaffold:scheme
}

func main() {
	var (
		metricsAddr          string
		leaderElectionID     string
		watchesPath          string
		probeAddr            string
		enableLeaderElection bool
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&watchesPath, "watches-file", "watches.yaml", "path to watches file")
	flag.StringVar(&leaderElectionID, "leader-election-id", "bdfd69d3.apache.org", "provide leader election")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		Port:                   9443,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       leaderElectionID,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ws, err := watches.Load(watchesPath)
	if err != nil {
		setupLog.Error(err, "Failed to create new manager factories")
		os.Exit(1)
	}

	// instantiate translator to populate defaults for OpenShift
	overridesYaml := defaultTranslator{}

	// instantiate pre hook to generate webserver secret
	prehookSecret := ensureAppSecret{}

	for _, w := range ws {
		// Register controller with the factory
		reconcilePeriod := defaultReconcilePeriod
		if w.ReconcilePeriod != nil {
			reconcilePeriod = w.ReconcilePeriod.Duration
		}

		maxConcurrentReconciles := defaultMaxConcurrentReconciles
		if w.MaxConcurrentReconciles != nil {
			maxConcurrentReconciles = *w.MaxConcurrentReconciles
		}

		r, err := reconciler.New(
			reconciler.WithChart(*w.Chart),
			reconciler.WithGroupVersionKind(w.GroupVersionKind),
			reconciler.WithOverrideValues(w.OverrideValues),
			reconciler.WithValueTranslator(&overridesYaml),
			reconciler.WithPreHook(&prehookSecret),
			reconciler.SkipDependentWatches(w.WatchDependentResources != nil && !*w.WatchDependentResources),
			reconciler.WithMaxConcurrentReconciles(maxConcurrentReconciles),
			reconciler.WithReconcilePeriod(reconcilePeriod),
			reconciler.WithInstallAnnotations(annotation.DefaultInstallAnnotations...),
			reconciler.WithUpgradeAnnotations(annotation.DefaultUpgradeAnnotations...),
			reconciler.WithUninstallAnnotations(annotation.DefaultUninstallAnnotations...),
		)
		if err != nil {
			setupLog.Error(err, "unable to create helm reconciler", "controller", "Helm")
			os.Exit(1)
		}
		if err := r.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "Helm")
			os.Exit(1)
		}
		setupLog.Info("configured watch", "gvk", w.GroupVersionKind, "chartPath", w.ChartPath, "maxConcurrentReconciles", maxConcurrentReconciles, "reconcilePeriod", reconcilePeriod)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// defaultTranslator implements the Value Translator Interface
type defaultTranslator struct{}

// Translate sets default values in the helm chart that cannot be set via the watches.yaml file
func (*defaultTranslator) Translate(ctx context.Context, overrideValues *unstructured.Unstructured) (chartutil.Values, error) {
	ctrl.Log.WithName("translate-values")
	// Read values from custom resource
	tmpClient, err := client.New(ctrl.GetConfigOrDie(), client.Options{})
	namespacedClient := client.NewNamespacedClient(tmpClient, "airflow-helm")
	if err != nil {
		ctrl.Log.Error(err, "Unable to instantiate runtime client to inject default values")
		os.Exit(1)
	}
	airflowName := client.ObjectKey{
		Name:      "airflow-helm",
		Namespace: "airflow-helm",
	}
	// Retrieve User defined spec
	AirFlowCROptions := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{}}}
	AirFlowCROptions.SetGroupVersionKind(schema.GroupVersionKind{Group: "workflow.apache.org", Version: "v1alpha1", Kind: "AirFlow"})
	err = namespacedClient.Get(context.TODO(), airflowName, AirFlowCROptions)
	if err != nil {
		ctrl.Log.Error(err, "Unable to retrieve user defined airflow-helm object")
		return nil, err
	}

	// Read override values that should always be applied by default in the context of this operator
	OverridesYaml, err := os.ReadFile("overrides.yaml")
	if err != nil {
		ctrl.Log.Error(err, "Error reading overrides.yaml")
		return nil, err
	}
	AirFlowOverrides := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{}}}
	err = yaml.Unmarshal(OverridesYaml, &AirFlowOverrides)
	if err != nil {
		ctrl.Log.Error(err, "Error unmarshalling overrides.yaml")
		return nil, err
	}

	//Match resourceVersion to the object that triggered the reconcile operation
	crMetadata := AirFlowCROptions.Object["metadata"].(map[string]interface{})
	AirFlowOverrides.Object["metadata"] = crMetadata

	// Merge userValues and overrideValues
	err = namespacedClient.Patch(ctx, AirFlowOverrides, client.MergeFrom(AirFlowCROptions))
	if err != nil {
		ctrl.Log.Error(err, "Error merging overrides into spec")
	}

	// Retrieve merged object
	err = namespacedClient.Get(ctx, airflowName, AirFlowCROptions)
	if err != nil {
		ctrl.Log.Error(err, "Unable to retrieve merged airflow-helm object")
		return nil, err
	}

	// Pass in merged object as chart values
	chartValues := AirFlowCROptions.Object["spec"].(map[string]interface{})
	return chartValues, err
}

type ensureAppSecret struct{}

func (*ensureAppSecret) Exec(unstructured *unstructured.Unstructured, values chartutil.Values, log logr.Logger) error {
	log.WithName("prehook-secret")
	tmpClient, err := client.New(ctrl.GetConfigOrDie(), client.Options{})
	namespacedClient := client.NewNamespacedClient(tmpClient, "airflow-helm")
	if err != nil {
		log.Error(err, "Unable to instantiate runtime client to create app secret")
		os.Exit(1)
	}

	secretName := client.ObjectKey{
		Name:      "webserver-secret",
		Namespace: "airflow-helm",
	}
	secretObj := &corev1.Secret{}
	err = namespacedClient.Get(context.TODO(), secretName, secretObj)
	if err != nil {
		if errors.IsNotFound(err) {
			return generateAppSecret(context.TODO(), namespacedClient, log)
		}
		log.Error(err, "Unable to retrieve app secret")
		return err
	}

	if secretObj.Name == "webserver-secret" {
		log.Info("It appears that the secret already exists")
		if secretObj.Data["webserver-secret-key"] != nil &&
			len(secretObj.Data["webserver-secret-key"]) == 16 {
			log.Info("Secret key length is as expected")
			return nil
		}
		log.Error(err, "Unexpected secret key value, consider deleting webserver-secret so it can be regenerated")
	}
	return nil
}

func generateAppSecret(ctx context.Context, client client.Client, log logr.Logger) error {
	var SecretLength = 16
	token := make([]byte, SecretLength)
	_, err := rand.Read(token)
	if err != nil {
		log.Error(err, "Error generating random token")
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "webserver-secret",
			Namespace: "airflow-helm",
		},
		Data: map[string][]byte{
			"webserver-secret-key": token,
		},
	}
	log.Info("Creating webserver secret in namespace airflow-helm")
	err = client.Create(ctx, secret)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			log.Info("webserver secret already exists in namespace airflow-helm")
			return nil
		}
		log.Error(err, "Error when creating webserver secret")
		return err
	}
	return nil
}
