/*
Copyright 2022.

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
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/scale"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	kubevalidation "k8s.io/apimachinery/pkg/util/validation"
	cached "k8s.io/client-go/discovery/cached"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	controllermetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"
	"github.com/cslab-ntua/HPA-Plus/internal/controllers"
	"github.com/cslab-ntua/HPA-Plus/internal/hook/http"
	metricadapter "github.com/cslab-ntua/HPA-Plus/internal/metrics"
	"github.com/cslab-ntua/HPA-Plus/internal/observability"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction/arima"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction/holtwinters"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction/lightgbm"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction/linear"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction/xgboost"
	"github.com/cslab-ntua/HPA-Plus/internal/worker"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(hpaplusv1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var watchNamespace string
	var cpuHistorySource string
	var collectorNamespace, collectorSelector, trafficPrometheusURL string
	var collectorPort int
	flag.StringVar(&collectorNamespace, "cgroup-collector-namespace", "", "Namespace of the optional cgroup collectors.")
	flag.StringVar(&collectorSelector, "cgroup-collector-selector", "app.kubernetes.io/component=cgroup-collector", "Label selector for trusted cgroup collectors.")
	flag.IntVar(&collectorPort, "cgroup-collector-port", 19091, "Cgroup collector HTTP port accessed through Kubernetes pod proxy.")
	flag.StringVar(&trafficPrometheusURL, "traffic-prometheus-url", "", "Trusted Prometheus origin for workload traffic telemetry; empty disables request-demand mode.")
	flag.StringVar(&cpuHistorySource, "cpu-history-source", "metrics-server", "CPU measurements: metrics-server, kubelet, or optional cgroup collector. Cgroup applies to CPU training and reactive evaluation.")
	flag.StringVar(&watchNamespace, "watch-namespace", "", "Restrict the manager cache and reconciliation to one namespace; empty watches all namespaces.")
	var enableLeaderElection bool
	var probeAddr string
	var workerMemoryMiB, workerConcurrency, workerSessions int
	var workerSocket string
	flag.StringVar(&workerSocket, "worker-socket", "", "Use isolated Python service on this Unix socket; never fall back to local execution.")
	var hookAllowedOrigins, hookCredentialsDir string
	flag.StringVar(&hookCredentialsDir, "hook-credentials-dir", "", "Directory containing administrator-owned hook bindings and projected Secret tokens.")
	flag.StringVar(&hookAllowedOrigins, "hook-allowed-origins", "", "Comma-separated exact HTTP(S) origins allowed for tuning hooks; empty disables hooks.")
	var retryPolicy controllers.RetryPolicy
	flag.IntVar(&workerMemoryMiB, "worker-memory-mib", 1024, "Per Python worker virtual memory limit in MiB (64..65536; 0 disables).")
	flag.IntVar(&workerConcurrency, "worker-concurrency", 4, "Maximum concurrent one-shot Python processes.")
	flag.IntVar(&workerSessions, "worker-sessions", 16, "Maximum resident incremental Python sessions.")
	flag.DurationVar(&retryPolicy.Initial, "model-retry-initial", 0, "First failed-model retry delay (0 follows syncPeriod).")
	flag.DurationVar(&retryPolicy.Maximum, "model-retry-max", 5*time.Minute, "Maximum failed-model retry delay (1s..24h).")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	if watchNamespace != "" && len(kubevalidation.IsDNS1123Label(watchNamespace)) != 0 {
		setupLog.Error(fmt.Errorf("invalid namespace"), "invalid watch namespace")
		os.Exit(1)
	}
	if err := algorithm.ValidateMemoryLimit(workerMemoryMiB); err != nil {
		setupLog.Error(observability.PublicError(err), "invalid worker limits")
		os.Exit(1)
	}
	if workerConcurrency < 1 || workerConcurrency > 64 || workerSessions < 1 || workerSessions > 64 {
		setupLog.Error(fmt.Errorf("process limits must be between 1 and 64"), "invalid worker limits")
		os.Exit(1)
	}
	if err := retryPolicy.Validate(); err != nil {
		setupLog.Error(observability.PublicError(err), "invalid retry policy")
		os.Exit(1)
	}

	var origins []string
	if hookAllowedOrigins != "" {
		origins = strings.Split(hookAllowedOrigins, ",")
	}
	httpExec, err := http.NewWithCredentials(origins, hookCredentialsDir)
	if err != nil {
		setupLog.Error(observability.PublicError(err), "Invalid hook destination policy")
		os.Exit(1)
	}
	defer httpExec.Client.CloseIdleConnections()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Namespace:              watchNamespace,
		MetricsBindAddress:     metricsAddr,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "a07591f8.hpa.plus",
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
	})
	if err != nil {
		setupLog.Error(observability.PublicError(err), "unable to start manager")
		os.Exit(1)
	}

	clientSet, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(observability.PublicError(err), "unable to get clientset from config")
		os.Exit(1)
	}

	scaleClient, err := scale.NewForConfig(mgr.GetConfig(),
		restmapper.NewDeferredDiscoveryRESTMapper(cached.NewMemCacheClient(clientSet.Discovery())),
		dynamic.LegacyAPIPathResolverFunc,
		scale.NewDiscoveryScaleKindResolver(clientSet.Discovery()))
	if err != nil {
		setupLog.Error(observability.PublicError(err), "unable to set up scale client from config")
		os.Exit(1)
	}

	metricGatherer, err := metricadapter.New(mgr.GetConfig())
	if err != nil {
		setupLog.Error(observability.PublicError(err), "unable to set up metrics gatherer")
		os.Exit(1)
	}
	if cpuHistorySource == "cgroup" {
		if err := metricGatherer.ConfigureCgroup(metricadapter.CgroupConfig{Namespace: collectorNamespace, Selector: collectorSelector, Port: collectorPort}); err != nil {
			setupLog.Error(err, "invalid cgroup collector configuration")
			os.Exit(1)
		}
	}
	if err := metricGatherer.SetCPUHistorySource(cpuHistorySource); err != nil {
		setupLog.Error(err, "invalid CPU history source")
		os.Exit(1)
	}
	localRunner := algorithm.NewAlgorithmPython()
	localRunner.MaxMemoryMiB, localRunner.MaxConcurrent = workerMemoryMiB, workerConcurrency
	telemetry, err := observability.New(controllermetrics.Registry)
	if err != nil {
		setupLog.Error(observability.PublicError(err), "unable to register HPAPlus metrics")
		os.Exit(1)
	}
	localRunner.Telemetry = telemetry
	localPersistent := algorithm.NewPersistentAlgorithmPython()
	localPersistent.Telemetry = telemetry
	localPersistent.MaxMemoryMiB, localPersistent.MaxSessions = workerMemoryMiB, workerSessions
	var pyRunner algorithm.Runner = localRunner
	var persistentPyRunner interface {
		arima.IncrementalRunner
		Start(context.Context) error
		NeedLeaderElection() bool
		CloseAll() error
	} = localPersistent
	if workerSocket != "" {
		remote, err := worker.NewClient(workerSocket)
		if err != nil {
			setupLog.Error(observability.PublicError(err), "invalid worker socket")
			os.Exit(1)
		}
		pyRunner, persistentPyRunner = remote, remote
	}
	if workerSocket == "" {
		if err := mgr.Add(algorithm.NewFeatureDiagnosticJanitor()); err != nil {
			setupLog.Error(observability.PublicError(err), "unable to register feature diagnostic cleanup")
			os.Exit(1)
		}
	}
	if err := mgr.Add(persistentPyRunner); err != nil {
		setupLog.Error(observability.PublicError(err), "unable to register Python worker shutdown")
		os.Exit(1)
	}
	defer func() {
		_ = persistentPyRunner.CloseAll()
	}()

	reconciler := &controllers.HPAPlusReconciler{
		RetryPolicy:          retryPolicy,
		WorkloadRunner:       pyRunner,
		TrafficPrometheusURL: trafficPrometheusURL,
		Client:               mgr.GetClient(),
		APIReader:            mgr.GetAPIReader(),
		RESTMapper:           mgr.GetRESTMapper(),
		Scheme:               mgr.GetScheme(),
		ScaleClient:          scaleClient,
		Gatherer:             metricGatherer,
		Observations:         metricadapter.NewObservations(),
		Evaluator:            metricadapter.HPAEvaluator{},
		Telemetry:            telemetry,
		Events:               observability.NewEvents(mgr.GetEventRecorderFor("hpa-plus")),
		Predicter: &prediction.ModelPredict{
			Predicters: []prediction.Predicter{
				&linear.Predict{
					Runner: pyRunner,
				},
				&holtwinters.Predict{
					HookExecute: httpExec,
					Runner:      pyRunner,
				},
				&arima.Predict{
					Runner:            pyRunner,
					IncrementalRunner: persistentPyRunner,
				},
				&xgboost.Predict{
					Runner: pyRunner,
				},
				&lightgbm.Predict{
					Runner: pyRunner,
				},
			},
		},
	}

	reconciler.Runtime = prediction.Adapter{Predictor: reconciler.Predicter}
	reconciler.HistoryStore = &controllers.AllocatingStore{ConfigMapStore: controllers.ConfigMapStore{Reader: mgr.GetAPIReader(), Client: mgr.GetClient(), Telemetry: telemetry}}
	if err = reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(observability.PublicError(err), "unable to create controller", "controller", "HPAPlus")
		os.Exit(1)
	}
	if err = controllers.NewHistorySampler(reconciler).SetupWithManager(mgr); err != nil {
		setupLog.Error(observability.PublicError(err), "unable to add history sampler")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(observability.PublicError(err), "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(observability.PublicError(err), "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	err = mgr.Start(ctrl.SetupSignalHandler())
	_ = persistentPyRunner.CloseAll()
	if err != nil {
		setupLog.Error(observability.PublicError(err), "problem running manager")
		os.Exit(1)
	}
}
