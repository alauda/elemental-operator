/*
Copyright © 2022 - 2026 SUSE LLC

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

package operator

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/rancher/steve/pkg/aggregation"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	runtimeconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/controllers"
	"github.com/rancher/elemental-operator/pkg/clients"
	"github.com/rancher/elemental-operator/pkg/log"
	"github.com/rancher/elemental-operator/pkg/server"
	"github.com/rancher/elemental-operator/pkg/version"
)

// IMPORTANT: The RBAC permissions below should be reviewed after old code is deprecated.
// +kubebuilder:rbac:groups="",resources=events,verbs=patch;create
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;create;delete;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=services,verbs=get;create;delete;list;watch

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

type rootConfig struct {
	debug                           bool
	enableLeaderElection            bool
	profilerAddress                 string
	metricsBindAddr                 string
	syncPeriod                      time.Duration
	leaderElectionLeaseDuration     time.Duration
	leaderElectionRenewDeadline     time.Duration
	leaderElectionRetryPeriod       time.Duration
	webhookPort                     int
	webhookCertDir                  string
	healthAddr                      string
	defaultRegistry                 string
	operatorImage                   string
	watchNamespace                  string
	seedimageImage                  string
	seedimageImagePullPolicy        string
	seedimageImagePullSecrets       []string
	seedimagePullImageTLSVerify     bool
	serverURL                       string
	httpBindAddr                    string
	caCertFile                      string
	caCert                          string
	agentTLSMode                    string
	systemAgentClusterName          string
	systemAgentServerURL            string
	systemAgentEndpointMode         string
	systemAgentAuthMode             string
	systemAgentServiceAccount       string
	systemAgentGlobalServiceAccount string
	systemAgentSplitAuthEnabled     bool
	systemAgentSharedAuthReadOnly   bool
}

func init() {
	klog.InitFlags(nil)

	utilruntime.Must(elementalv1.AddToScheme(scheme))
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func NewOperatorCommand() *cobra.Command {
	var config rootConfig

	cmd := &cobra.Command{
		Use:   "operator",
		Short: "Run the Kubernetes operator using kubebuilder.",
		Args: func(_ *cobra.Command, _ []string) error {
			if config.seedimageImagePullPolicy != string(corev1.PullAlways) &&
				config.seedimageImagePullPolicy != string(corev1.PullIfNotPresent) &&
				config.seedimageImagePullPolicy != string(corev1.PullNever) {
				return fmt.Errorf("invalid pull policy '%s', valid values: '%s', '%s', '%s'",
					config.seedimageImagePullPolicy,
					corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever)
			}
			serverURL, err := normalizeURL(config.serverURL, "server-url", true)
			if err != nil {
				return err
			}
			config.serverURL = serverURL
			systemAgentServerURL, err := normalizeURL(config.systemAgentServerURL, "system-agent-server-url", false)
			if err != nil {
				return err
			}
			config.systemAgentServerURL = systemAgentServerURL
			config.systemAgentClusterName = server.NormalizeSystemAgentClusterName(config.systemAgentClusterName)
			config.systemAgentEndpointMode = server.NormalizeSystemAgentEndpointMode(config.systemAgentEndpointMode)
			if err := server.ValidateSystemAgentEndpointMode(config.systemAgentEndpointMode); err != nil {
				return err
			}

			agentTLSMode := strings.TrimSpace(config.agentTLSMode)
			switch agentTLSMode {
			case "", server.AgentTLSModeStrict:
				config.agentTLSMode = server.AgentTLSModeStrict
			case server.AgentTLSModeSystemStore:
				config.agentTLSMode = agentTLSMode
			default:
				return fmt.Errorf("invalid agent-tls-mode %q, valid values: %q, %q", config.agentTLSMode, server.AgentTLSModeStrict, server.AgentTLSModeSystemStore)
			}

			if config.caCertFile != "" {
				caCert, err := os.ReadFile(config.caCertFile)
				if err != nil {
					return fmt.Errorf("failed to read ca-cert-file %q: %w", config.caCertFile, err)
				}
				config.caCert = string(caCert)
			}
			config.systemAgentAuthMode = controllers.NormalizeSystemAgentAuthMode(config.systemAgentAuthMode)
			switch config.systemAgentAuthMode {
			case controllers.SystemAgentAuthModeRegistration:
			case controllers.SystemAgentAuthModeShared:
				if strings.TrimSpace(config.systemAgentServiceAccount) == "" {
					return fmt.Errorf("system-agent-service-account is required when system-agent-auth-mode is shared")
				}
				if config.systemAgentSplitAuthEnabled {
					if strings.TrimSpace(config.systemAgentGlobalServiceAccount) == "" {
						return fmt.Errorf("system-agent-global-service-account is required when system-agent-split-auth-enabled is true")
					}
					if strings.TrimSpace(config.systemAgentServiceAccount) == strings.TrimSpace(config.systemAgentGlobalServiceAccount) {
						return fmt.Errorf("system-agent-service-account and system-agent-global-service-account must be different when system-agent-split-auth-enabled is true")
					}
				}
			default:
				return fmt.Errorf("invalid system-agent-auth-mode %q, valid values: %q, %q",
					config.systemAgentAuthMode,
					controllers.SystemAgentAuthModeRegistration,
					controllers.SystemAgentAuthModeShared)
			}
			config.systemAgentServiceAccount = strings.TrimSpace(config.systemAgentServiceAccount)
			config.systemAgentGlobalServiceAccount = strings.TrimSpace(config.systemAgentGlobalServiceAccount)
			return nil
		},
		Run: func(_ *cobra.Command, _ []string) {
			if config.debug {
				log.EnableDebugLogging()
			}

			log.Infof("Operator version %s, architecture %s, commit %s, commit date %s", version.Version, goruntime.GOARCH, version.Commit, version.CommitDate)
			operatorRun(&config)
		},
	}

	viper.AutomaticEnv()

	cmd.PersistentFlags().StringVar(&config.profilerAddress, "profiler-address", "",
		"Bind address to expose the pprof profiler (e.g. localhost:6060)")
	_ = viper.BindPFlag("profiler-address", cmd.PersistentFlags().Lookup("profiler-address"))

	cmd.PersistentFlags().StringVar(&config.metricsBindAddr, "metrics-bind-addr", ":8080",
		"The address the metric endpoint binds to.")
	_ = viper.BindPFlag("metrics-bind-addr", cmd.PersistentFlags().Lookup("metrics-bind-addr"))

	cmd.PersistentFlags().DurationVar(&config.leaderElectionLeaseDuration, "leader-elect-lease-duration", 15*time.Second,
		"Interval at which non-leader candidates will wait to force acquire leadership (duration string)")
	_ = viper.BindPFlag("leader-elect-lease-duration", cmd.PersistentFlags().Lookup("leader-elect-lease-duration"))

	cmd.PersistentFlags().DurationVar(&config.leaderElectionRenewDeadline, "leader-elect-renew-deadline", 10*time.Second,
		"Duration that the acting leader will retry refreshing leadership before giving up (duration string)")
	_ = viper.BindPFlag("leader-elect-renew-deadline", cmd.PersistentFlags().Lookup("leader-elect-renew-deadline"))

	cmd.PersistentFlags().DurationVar(&config.leaderElectionRetryPeriod, "leader-elect-retry-period", 2*time.Second,
		"Duration the LeaderElector clients should wait between tries of actions (duration string)")
	_ = viper.BindPFlag("leader-elect-retry-period", cmd.PersistentFlags().Lookup("leader-elect-retry-period"))

	cmd.PersistentFlags().BoolVar(&config.enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
	_ = viper.BindPFlag("leader-elect", cmd.PersistentFlags().Lookup("leader-elect"))

	cmd.PersistentFlags().IntVar(&config.webhookPort, "webhook-port", 9443,
		"Webhook Server port.")
	_ = viper.BindPFlag("webhook-port", cmd.PersistentFlags().Lookup("webhook-port"))

	cmd.PersistentFlags().StringVar(&config.webhookCertDir, "webhook-cert-dir", ":8080",
		"Webhook cert dir, only used when webhook-port is specified.")
	_ = viper.BindPFlag("webhook-cert-dir", cmd.PersistentFlags().Lookup("webhook-cert-dir"))

	cmd.PersistentFlags().StringVar(&config.healthAddr, "health-addr", ":9440",
		"The address the health endpoint binds to.")
	_ = viper.BindPFlag("health-addr", cmd.PersistentFlags().Lookup("health-addr"))

	cmd.PersistentFlags().StringVar(&config.defaultRegistry, "default-registry", "", "default registry to prepend to os images")
	_ = viper.BindPFlag("default-registry", cmd.PersistentFlags().Lookup("default-registry"))

	cmd.PersistentFlags().StringVar(&config.operatorImage, "operator-image", "",
		"Operator image. Used to gather the results from the syncer by running the 'display' command")
	_ = viper.BindPFlag("operator-image", cmd.PersistentFlags().Lookup("operator-image"))
	_ = cobra.MarkFlagRequired(cmd.PersistentFlags(), "operator-image")

	cmd.PersistentFlags().StringVar(&config.watchNamespace, "namespace", "", "Namespace that the controller watches to reconcile objects.")
	_ = viper.BindPFlag("namespace", cmd.PersistentFlags().Lookup("namespace"))

	cmd.PersistentFlags().BoolVar(&config.debug, "debug", false, "registration debug logging")
	_ = viper.BindPFlag("debug", cmd.PersistentFlags().Lookup("debug"))

	cmd.PersistentFlags().StringVar(&config.seedimageImage, "seedimage-image", "", "SeedImage builder image. Used to build a SeedImage ISO.")
	_ = viper.BindPFlag("seedimage-image", cmd.PersistentFlags().Lookup("seedimage-image"))

	cmd.PersistentFlags().StringVar(&config.seedimageImagePullPolicy, "seedimage-image-pullpolicy", "IfNotPresent", "PullPolicy for the SeedImage builder image.")
	_ = viper.BindPFlag("seedimage-image-pullpolicy", cmd.PersistentFlags().Lookup("seedimage-image-pullpolicy"))

	cmd.PersistentFlags().StringSliceVar(&config.seedimageImagePullSecrets, "seedimage-image-pull-secrets", nil, "Comma-separated list of imagePullSecret names used by SeedImage builder Pods.")
	_ = viper.BindPFlag("seedimage-image-pull-secrets", cmd.PersistentFlags().Lookup("seedimage-image-pull-secrets"))

	cmd.PersistentFlags().BoolVar(&config.seedimagePullImageTLSVerify, "seedimage-pull-image-tls-verify", true, "Require HTTPS and verify certificates when SeedImage builder runs elemental pull-image.")
	_ = viper.BindPFlag("seedimage-pull-image-tls-verify", cmd.PersistentFlags().Lookup("seedimage-pull-image-tls-verify"))

	cmd.PersistentFlags().StringVar(&config.serverURL, "server-url", "", "External URL used by machines to reach the Elemental operator HTTP server.")
	_ = viper.BindPFlag("server-url", cmd.PersistentFlags().Lookup("server-url"))
	_ = cobra.MarkFlagRequired(cmd.PersistentFlags(), "server-url")

	cmd.PersistentFlags().StringVar(&config.httpBindAddr, "http-bind-addr", ":8082", "The address the Elemental HTTP server binds to.")
	_ = viper.BindPFlag("http-bind-addr", cmd.PersistentFlags().Lookup("http-bind-addr"))

	cmd.PersistentFlags().StringVar(&config.caCertFile, "ca-cert-file", "", "Path to a PEM CA bundle used in Elemental registration configs.")
	_ = viper.BindPFlag("ca-cert-file", cmd.PersistentFlags().Lookup("ca-cert-file"))

	cmd.PersistentFlags().StringVar(&config.agentTLSMode, "agent-tls-mode", server.AgentTLSModeStrict, "System agent TLS mode. Valid values: strict, system-store.")
	_ = viper.BindPFlag("agent-tls-mode", cmd.PersistentFlags().Lookup("agent-tls-mode"))

	cmd.PersistentFlags().StringVar(&config.systemAgentClusterName, "system-agent-cluster-name", server.DefaultSystemAgentClusterName, "Cluster name used to build the Elemental system agent Kubernetes API URL.")
	_ = viper.BindPFlag("system-agent-cluster-name", cmd.PersistentFlags().Lookup("system-agent-cluster-name"))

	cmd.PersistentFlags().StringVar(&config.systemAgentServerURL, "system-agent-server-url", "", "Optional base URL used by elemental-system-agent to reach the platform Kubernetes API. Defaults to server-url.")
	_ = viper.BindPFlag("system-agent-server-url", cmd.PersistentFlags().Lookup("system-agent-server-url"))

	cmd.PersistentFlags().StringVar(&config.systemAgentEndpointMode, "system-agent-endpoint-mode", server.DefaultSystemAgentEndpointMode, "Default system-agent endpoint mode. Valid values: erebus, direct-apiserver.")
	_ = viper.BindPFlag("system-agent-endpoint-mode", cmd.PersistentFlags().Lookup("system-agent-endpoint-mode"))

	cmd.PersistentFlags().StringVar(&config.systemAgentAuthMode, "system-agent-auth-mode", controllers.SystemAgentAuthModeRegistration, "System-agent authentication mode. Valid values: registration, shared.")
	_ = viper.BindPFlag("system-agent-auth-mode", cmd.PersistentFlags().Lookup("system-agent-auth-mode"))

	cmd.PersistentFlags().StringVar(&config.systemAgentServiceAccount, "system-agent-service-account", controllers.DefaultSharedSystemAgentServiceAccountName, "ServiceAccount name used when system-agent-auth-mode is shared.")
	_ = viper.BindPFlag("system-agent-service-account", cmd.PersistentFlags().Lookup("system-agent-service-account"))

	cmd.PersistentFlags().StringVar(&config.systemAgentGlobalServiceAccount, "system-agent-global-service-account", controllers.DefaultGlobalSystemAgentServiceAccountName, "Cluster-local ServiceAccount name used by global-scoped MachineRegistrations when split auth is enabled.")
	_ = viper.BindPFlag("system-agent-global-service-account", cmd.PersistentFlags().Lookup("system-agent-global-service-account"))

	cmd.PersistentFlags().BoolVar(&config.systemAgentSplitAuthEnabled, "system-agent-split-auth-enabled", false, "Split shared system-agent authorization into DR-synchronized shared and cluster-local global scopes.")
	_ = viper.BindPFlag("system-agent-split-auth-enabled", cmd.PersistentFlags().Lookup("system-agent-split-auth-enabled"))

	cmd.PersistentFlags().BoolVar(&config.systemAgentSharedAuthReadOnly, "system-agent-shared-auth-read-only", false, "Treat shared system-agent ServiceAccount, token and RBAC as externally managed; with split auth enabled, global-scoped auth remains locally reconciled.")
	_ = viper.BindPFlag("system-agent-shared-auth-read-only", cmd.PersistentFlags().Lookup("system-agent-shared-auth-read-only"))

	cmd.PersistentFlags().AddGoFlagSet(flag.CommandLine)

	return cmd
}

func normalizeURL(value, flagName string, required bool) (string, error) {
	normalized := strings.TrimRight(strings.TrimSpace(value), "/")
	if normalized == "" {
		if required {
			return "", fmt.Errorf("%s is required", flagName)
		}
		return "", nil
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid %s %q", flagName, value)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("invalid %s scheme %q, expected http or https", flagName, parsed.Scheme)
	}
	return normalized, nil
}

func operatorRun(config *rootConfig) {
	if config.profilerAddress != "" {
		klog.Infof("Profiler listening for requests at %s", config.profilerAddress)
		go func() {
			klog.Info(http.ListenAndServe(config.profilerAddress, nil))
		}()
	}

	restCfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: config.metricsBindAddr,
		},
		LeaderElection:   config.enableLeaderElection,
		LeaderElectionID: "controller-leader-election-elemental-operator",
		LeaseDuration:    &config.leaderElectionLeaseDuration,
		RenewDeadline:    &config.leaderElectionRenewDeadline,
		RetryPeriod:      &config.leaderElectionRetryPeriod,
		Cache: cache.Options{
			SyncPeriod: &config.syncPeriod,
		},
		WebhookServer: webhook.NewServer(webhook.Options{
			Port:    config.webhookPort,
			CertDir: config.webhookCertDir,
		}),
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{
					&corev1.ConfigMap{},
					&corev1.Secret{},
				},
			},
		},
		HealthProbeBindAddress: config.healthAddr,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Setup the context that's going to be used in controllers and for the manager.
	ctx := ctrl.SetupSignalHandler()

	setupChecks(mgr)
	setupReconcilers(mgr, config)

	// +kubebuilder:scaffold:builder
	runRegistration(ctx, mgr, config.watchNamespace, config.httpBindAddr, config.serverURL, config.caCert, config.agentTLSMode, config.systemAgentClusterName, config.systemAgentServerURL, config.systemAgentEndpointMode, config.systemAgentSplitAuthEnabled)
	runManager(ctx, mgr)
}

func runManager(ctx context.Context, mgr ctrl.Manager) {
	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func runRegistration(ctx context.Context, mgr ctrl.Manager, namespace, httpBindAddr, serverURL, caCert, agentTLSMode, systemAgentClusterName, systemAgentServerURL, systemAgentEndpointMode string, systemAgentSplitAuthEnabled bool) {
	setupLog.Info("starting registration")
	handler := server.NewWithOptions(ctx, mgr.GetClient(), server.Options{
		ServerURL:                   serverURL,
		CACert:                      caCert,
		AgentTLSMode:                agentTLSMode,
		SystemAgentClusterName:      systemAgentClusterName,
		SystemAgentServerURL:        systemAgentServerURL,
		SystemAgentEndpointMode:     systemAgentEndpointMode,
		SystemAgentSplitAuthEnabled: systemAgentSplitAuthEnabled,
	})

	if httpBindAddr != "" {
		httpServer := &http.Server{
			Addr:    httpBindAddr,
			Handler: handler,
			BaseContext: func(_ net.Listener) context.Context {
				return ctx
			},
		}

		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := httpServer.Shutdown(shutdownCtx); err != nil {
				setupLog.Error(err, "problem shutting down elemental HTTP server")
			}
		}()

		go func() {
			setupLog.Info("starting elemental HTTP server", "addr", httpBindAddr)
			if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				setupLog.Error(err, "problem running elemental HTTP server")
				os.Exit(1)
			}
		}()
	}

	restConfig, err := runtimeconfig.GetConfig()
	if err != nil {
		setupLog.Error(err, "Failed to find kubeconfig")
		os.Exit(1)
	}

	cl, err := clients.NewFromConfig(restConfig)
	if err != nil {
		setupLog.Error(err, "Error building restconfig")
		os.Exit(1)
	}

	aggregation.Watch(ctx, cl.Core().Secret(), namespace, "elemental-operator", handler)

	if err := cl.Start(ctx); err != nil {
		setupLog.Error(err, "problem running registration")
		os.Exit(1)
	}
}

func setupChecks(mgr ctrl.Manager) {
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to create ready check")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to create health check")
		os.Exit(1)
	}
}

func setupReconcilers(mgr ctrl.Manager, config *rootConfig) {
	if err := (&controllers.MachineRegistrationReconciler{
		Client:                          mgr.GetClient(),
		ServerURL:                       config.serverURL,
		SystemAgentAuthMode:             config.systemAgentAuthMode,
		SystemAgentServiceAccount:       config.systemAgentServiceAccount,
		GlobalSystemAgentServiceAccount: config.systemAgentGlobalServiceAccount,
		SystemAgentSplitAuthEnabled:     config.systemAgentSplitAuthEnabled,
		SystemAgentSharedAuthReadOnly:   config.systemAgentSharedAuthReadOnly,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create reconciler", "controller", "MachineRegistration")
		os.Exit(1)
	}
	if err := (&controllers.MachineInventoryReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create reconciler", "controller", "MachineInventory")
		os.Exit(1)
	}
	if err := (&controllers.SeedImageReconciler{
		Client:                             mgr.GetClient(),
		SeedImageImage:                     config.seedimageImage,
		SeedImageImagePullPolicy:           corev1.PullPolicy(config.seedimageImagePullPolicy),
		SeedImageImagePullSecrets:          config.seedimageImagePullSecrets,
		DisableSeedImagePullImageTLSVerify: !config.seedimagePullImageTLSVerify,
		ServerURL:                          config.serverURL,
		CACert:                             config.caCert,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create reconciler", "controller", "SeedImage")
		os.Exit(1)
	}
}
