/*
SPDX-License-Identifier: Apache-2.0
Copyright 2026 Gluesys Co., Ltd..

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
	"crypto/tls"
	"flag"
	"os"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	daosv1alpha1 "github.com/gluesys/daos-operator/api/v1alpha1"
	"github.com/gluesys/daos-operator/internal/controller"
	"github.com/gluesys/daos-operator/internal/dmg"
	"github.com/gluesys/daos-operator/internal/interception"
	"github.com/gluesys/daos-operator/internal/webhookcert"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(daosv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var webhookPort int
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	var icept interceptionFlags
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Port the webhook server listens on. "+
		"Defaults to 9443. Set -1 to disable the webhook server.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.BoolVar(&icept.enabled, "interception", false,
		"Serve the pod mutating webhook that preloads the DAOS interception library "+
			"(doc/plans/2026-11-19-client-interception.md). Needs --interception-system and the chart's webhook objects.")
	flag.StringVar(&icept.system, "interception-system", "", "DaosSystem whose CSI driver the webhook serves.")
	flag.StringVar(&icept.driver, "interception-csi-driver", "daos.csi.gluesys.com", "CSI driver name of DAOS volumes.")
	flag.StringVar(&icept.ibvDrivers, "interception-ibv-drivers", interception.DefaultIBVDrivers,
		"IBV_DRIVERS for injected containers (bundled rdma-core providers, without the -rdmav<N>.so suffix).")
	flag.StringVar(&icept.rdmaResources, "interception-rdma-resources", "",
		"Comma-separated extended resource names (a trailing / matches a prefix) that give a container an RDMA device.")
	flag.StringVar(&icept.service, "webhook-service", "daos-operator-webhook", "Service in front of the webhook server.")
	flag.StringVar(&icept.secret, "webhook-secret", "daos-operator-webhook-cert", "Secret holding the webhook serving certificate.")
	flag.StringVar(&icept.config, "webhook-config", "daos-operator-interception", "MutatingWebhookConfiguration to keep caBundle in.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

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
		Port:    webhookPort,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	var certOpts webhookcert.Options
	// uncached: the cert re-check reads one Secret and one webhook configuration,
	// and RBAC grants get/update, not the list/watch an informer would need
	var certClient client.Client
	if icept.enabled {
		if icept.system == "" {
			setupLog.Error(nil, "--interception needs --interception-system")
			os.Exit(1)
		}
		ns := os.Getenv("POD_NAMESPACE")
		if ns == "" {
			setupLog.Error(nil, "--interception needs POD_NAMESPACE (downward API)")
			os.Exit(1)
		}
		if webhookServerOptions.CertDir == "" {
			webhookServerOptions.CertDir = "/tmp/k8s-webhook-server/serving-certs"
		}
		certOpts = webhookcert.Options{
			Namespace: ns, SecretName: icept.secret, ServiceName: icept.service,
			WebhookConfig: icept.config, CertDir: webhookServerOptions.CertDir,
		}
		// before the manager: the webhook server reads the files when it starts
		var err error
		certClient, err = client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
		if err != nil {
			setupLog.Error(err, "Failed to create client for the webhook certificate")
			os.Exit(1)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		err = webhookcert.Ensure(ctx, certClient, certOpts, time.Now())
		cancel()
		if err != nil {
			setupLog.Error(err, "Failed to set up the webhook certificate")
			os.Exit(1)
		}
	}
	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/server
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
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
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

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "084bf26b.gluesys.com",
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
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	if v := os.Getenv("DAOS_HOSTPREP_DEFAULT_IMAGE"); v != "" {
		controller.DefaultHostPrepImage = v
	}
	kube, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "Failed to create kubernetes clientset")
		os.Exit(1)
	}
	if err := (&controller.DaosSystemReconciler{
		APIReader: mgr.GetAPIReader(),
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Dmg:       &dmg.JobRunner{Client: mgr.GetClient(), Kube: kube, Scheme: mgr.GetScheme()},
		Recorder:  mgr.GetEventRecorderFor("daossystem"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "daossystem")
		os.Exit(1)
	}
	runner := &dmg.JobRunner{Client: mgr.GetClient(), Kube: kube, Scheme: mgr.GetScheme()}
	if err := (&controller.DaosPoolReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Dmg:      runner,
		Recorder: mgr.GetEventRecorderFor("daospool"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "daospool")
		os.Exit(1)
	}
	if err := (&controller.DaosContainerReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Dmg:      runner,
		Recorder: mgr.GetEventRecorderFor("daoscontainer"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "daoscontainer")
		os.Exit(1)
	}
	if err := (&controller.DaosContainerSnapshotReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Dmg:      runner,
		Recorder: mgr.GetEventRecorderFor("daoscontainersnapshot"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "daoscontainersnapshot")
		os.Exit(1)
	}
	if err := (&controller.DaosSnapshotPolicyReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("daossnapshotpolicy"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "daossnapshotpolicy")
		os.Exit(1)
	}
	if err := (&controller.S3ServiceReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("s3service"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "s3service")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if icept.enabled {
		var res []string
		for _, r := range strings.Split(icept.rdmaResources, ",") {
			if r = strings.TrimSpace(r); r != "" {
				res = append(res, r)
			}
		}
		mgr.GetWebhookServer().Register(interception.Path, &webhook.Admission{Handler: &interception.Handler{
			Client:     mgr.GetClient(),
			Reader:     mgr.GetAPIReader(),
			Decoder:    admission.NewDecoder(mgr.GetScheme()),
			Config:     interception.Config{IBVDrivers: icept.ibvDrivers, RDMAResources: res},
			DriverName: icept.driver,
			SystemName: icept.system,
		}})
		// renew and re-assert caBundle (a chart upgrade may recreate the configuration)
		if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			t := time.NewTicker(10 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-t.C:
					if err := webhookcert.Ensure(ctx, certClient, certOpts, time.Now()); err != nil {
						setupLog.Error(err, "webhook certificate re-check failed")
					}
				}
			}
		})); err != nil {
			setupLog.Error(err, "Failed to add webhook certificate re-check")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	readyz := healthz.Ping
	if icept.enabled {
		readyz = mgr.GetWebhookServer().StartedChecker()
	}
	if err := mgr.AddReadyzCheck("readyz", readyz); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

type interceptionFlags struct {
	enabled                                   bool
	system, driver, ibvDrivers, rdmaResources string
	service, secret, config                   string
}
