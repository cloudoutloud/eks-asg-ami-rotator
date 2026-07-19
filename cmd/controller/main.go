// Command controller runs the asg-ami-rotator: a controller that keeps a set of
// Auto Scaling Groups rolled onto their current AMI, one node at a time.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/example/asg-ami-rotator/internal/awsclient"
	"github.com/example/asg-ami-rotator/internal/config"
	"github.com/example/asg-ami-rotator/internal/kube"
	"github.com/example/asg-ami-rotator/internal/rotator"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		return err
	}

	logger := zap.New(zap.UseDevMode(false))
	log.SetLogger(logger)
	logf := func(format string, args ...any) { logger.Info(fmt.Sprintf(format, args...)) }

	ctx := signals.SetupSignalHandler()

	restCfg, err := restConfig()
	if err != nil {
		return err
	}

	mgr, err := manager.New(restCfg, manager.Options{
		Metrics:                 metricsserver.Options{BindAddress: cfg.MetricsAddr},
		HealthProbeBindAddress:  cfg.HealthProbeAddr,
		LeaderElection:          cfg.EnableLeaderElection,
		LeaderElectionID:        cfg.LeaderElectionID,
		LeaderElectionNamespace: leaderElectionNamespace(),
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}

	awsC, err := awsclient.New(ctx, cfg.Region)
	if err != nil {
		return err
	}
	kubeC, err := kube.New(logf, kube.DrainOptions{
		Timeout:             cfg.DrainTimeout,
		GracePeriodSeconds:  cfg.DrainGracePeriod,
		Force:               cfg.DrainForce,
		IgnoreAllDaemonSets: cfg.IgnoreAllDaemonSets,
		DeleteEmptyDirData:  cfg.DeleteEmptyDirData,
	})
	if err != nil {
		return err
	}

	rot := rotator.New(cfg, awsC, kubeC, logf)

	// The poll loop only runs on the elected leader.
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		logf("starting rotator: interval=%s dry-run=%t", cfg.PollInterval, cfg.DryRun)
		ticker := time.NewTicker(cfg.PollInterval)
		defer ticker.Stop()
		// Run once immediately, then on each tick.
		for {
			if err := rot.ReconcileAll(ctx); err != nil {
				logf("reconcile error: %v", err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
		}
	})); err != nil {
		return err
	}

	logf("manager starting")
	return mgr.Start(ctx)
}

func restConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{})
	return loader.ClientConfig()
}

func leaderElectionNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	// Fall back to the mounted service account namespace when in-cluster.
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		return string(b)
	}
	return "kube-system"
}
