// Package kube handles the Kubernetes side of a roll: mapping EC2 instances to
// Nodes, cordoning, draining, and deleting them.
package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kubectl/pkg/drain"
)

// DrainOptions configures the cordon/drain behaviour.
type DrainOptions struct {
	Timeout             time.Duration
	GracePeriodSeconds  int
	Force               bool
	IgnoreAllDaemonSets bool
	DeleteEmptyDirData  bool
}

// Client wraps a Kubernetes clientset with node operations.
type Client struct {
	cs     kubernetes.Interface
	logf   func(string, ...any)
	drainO DrainOptions
}

// New builds a Client from in-cluster config, falling back to the default
// kubeconfig for local runs.
func New(logf func(string, ...any), o DrainOptions) (*Client, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{})
		cfg, err = loader.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("build kubernetes config: %w", err)
		}
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}
	return &Client{cs: cs, logf: logf, drainO: o}, nil
}

// NodeForInstance returns the Node whose providerID references instanceID, or
// nil if no such node exists (already gone / never joined).
func (c *Client) NodeForInstance(ctx context.Context, instanceID string) (*corev1.Node, error) {
	nodes, err := c.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		// providerID looks like: aws:///<az>/<instance-id>
		if strings.HasSuffix(n.Spec.ProviderID, "/"+instanceID) {
			return n, nil
		}
	}
	return nil, nil
}

// CordonAndDrain cordons then drains the node using the official kubectl drain
// helper (respects PDBs, evicts pods, filters DaemonSets).
func (c *Client) CordonAndDrain(ctx context.Context, node *corev1.Node) error {
	helper := &drain.Helper{
		Ctx:                 ctx,
		Client:              c.cs,
		Force:               c.drainO.Force,
		IgnoreAllDaemonSets: c.drainO.IgnoreAllDaemonSets,
		DeleteEmptyDirData:  c.drainO.DeleteEmptyDirData,
		GracePeriodSeconds:  c.drainO.GracePeriodSeconds,
		Timeout:             c.drainO.Timeout,
		Out:                 logWriter{c.logf, "drain"},
		ErrOut:              logWriter{c.logf, "drain-err"},
	}

	if err := drain.RunCordonOrUncordon(helper, node, true); err != nil {
		return fmt.Errorf("cordon node %s: %w", node.Name, err)
	}
	c.logf("cordoned node %s", node.Name)

	if err := drain.RunNodeDrain(helper, node.Name); err != nil {
		return fmt.Errorf("drain node %s: %w", node.Name, err)
	}
	c.logf("drained node %s", node.Name)
	return nil
}

// DeleteNode removes the Node object from the API server.
func (c *Client) DeleteNode(ctx context.Context, name string) error {
	err := c.cs.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete node %s: %w", name, err)
	}
	c.logf("deleted node %s", name)
	return nil
}

type logWriter struct {
	logf   func(string, ...any)
	prefix string
}

func (w logWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if msg != "" {
		w.logf("[%s] %s", w.prefix, msg)
	}
	return len(p), nil
}
