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

// NodesForInstances maps each instance ID to its Node using a single List call,
// which matters when a batched roll needs nodes for many instances at once.
// Instances with no matching Node are absent from the result.
func (c *Client) NodesForInstances(ctx context.Context, instanceIDs []string) (map[string]*corev1.Node, error) {
	result := make(map[string]*corev1.Node, len(instanceIDs))
	if len(instanceIDs) == 0 {
		return result, nil
	}
	nodes, err := c.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	byInstance := make(map[string]*corev1.Node, len(nodes.Items))
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if idx := strings.LastIndex(n.Spec.ProviderID, "/"); idx >= 0 {
			byInstance[n.Spec.ProviderID[idx+1:]] = n
		}
	}
	for _, id := range instanceIDs {
		if n, ok := byInstance[id]; ok {
			result[id] = n
		}
	}
	return result, nil
}

// WaitForNodesReady blocks until every listed instance has a Node that has
// joined the cluster, reports Ready and is schedulable, or the timeout elapses.
func (c *Client) WaitForNodesReady(ctx context.Context, instanceIDs []string, timeout, poll time.Duration) error {
	if len(instanceIDs) == 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		nodes, err := c.NodesForInstances(ctx, instanceIDs)
		if err != nil {
			return err
		}
		var pending []string
		for _, id := range instanceIDs {
			node, ok := nodes[id]
			if !ok || !nodeReady(node) || node.Spec.Unschedulable {
				pending = append(pending, id)
			}
		}
		if len(pending) == 0 {
			c.logf("all %d replacement node(s) are Ready", len(instanceIDs))
			return nil
		}
		c.logf("waiting for %d/%d replacement node(s) to be Ready: %s",
			len(pending), len(instanceIDs), strings.Join(pending, " "))
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for replacement node(s) to become Ready: %s",
				timeout, strings.Join(pending, " "))
		}
		if err := sleep(ctx, poll); err != nil {
			return err
		}
	}
}

// WaitForNodeReady blocks until the Node backing instanceID has joined the
// cluster and reports Ready=True (and is not cordoned/unschedulable), or the
// timeout elapses. This ensures the surge replacement can actually accept the
// pods evicted from the node we are about to drain.
func (c *Client) WaitForNodeReady(ctx context.Context, instanceID string, timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	logged := false
	for {
		node, err := c.NodeForInstance(ctx, instanceID)
		if err != nil {
			return err
		}
		if node != nil {
			if !logged {
				c.logf("instance %s: replacement node %s joined; waiting for Ready", instanceID, node.Name)
				logged = true
			}
			if nodeReady(node) && !node.Spec.Unschedulable {
				c.logf("replacement node %s is Ready", node.Name)
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for replacement node of instance %s to become Ready", timeout, instanceID)
		}
		if err := sleep(ctx, poll); err != nil {
			return err
		}
	}
}

func nodeReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) drainHelper(ctx context.Context) *drain.Helper {
	return &drain.Helper{
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
}

// Cordon marks the node unschedulable so no further pods are placed on it. Safe
// to call on an already-cordoned node.
func (c *Client) Cordon(ctx context.Context, node *corev1.Node) error {
	if err := drain.RunCordonOrUncordon(c.drainHelper(ctx), node, true); err != nil {
		return fmt.Errorf("cordon node %s: %w", node.Name, err)
	}
	c.logf("cordoned node %s", node.Name)
	return nil
}

// Drain evicts the node's pods using the official kubectl drain helper
// (respects PDBs, filters DaemonSets).
func (c *Client) Drain(ctx context.Context, node *corev1.Node) error {
	c.logf("draining node %s", node.Name)
	if err := drain.RunNodeDrain(c.drainHelper(ctx), node.Name); err != nil {
		return fmt.Errorf("drain node %s: %w", node.Name, err)
	}
	c.logf("drained node %s", node.Name)
	return nil
}

// CordonAndDrain cordons then drains the node.
func (c *Client) CordonAndDrain(ctx context.Context, node *corev1.Node) error {
	if err := c.Cordon(ctx, node); err != nil {
		return err
	}
	return c.Drain(ctx, node)
}

// VerifyNodeDrainClean checks that no evictable workloads remain on the node.
// DaemonSet and mirror pods left by a successful drain are allowed.
func (c *Client) VerifyNodeDrainClean(ctx context.Context, node *corev1.Node) error {
	list, errs := c.drainHelper(ctx).GetPodsForDeletion(node.Name)
	if len(errs) > 0 {
		return fmt.Errorf("verify node %s drain clean: %v", node.Name, errs)
	}
	remaining := list.Pods()
	if len(remaining) == 0 {
		c.logf("node %s: verified clean (no evictable workloads remain)", node.Name)
		return nil
	}
	names := make([]string, len(remaining))
	for i, p := range remaining {
		names[i] = fmt.Sprintf("%s/%s", p.Namespace, p.Name)
	}
	return fmt.Errorf("node %s still has %d workload pod(s) after drain: %s",
		node.Name, len(remaining), strings.Join(names, ", "))
}

// DeleteNode removes the Node object from the API server after verifying that
// only DaemonSet/mirror pods remain.
func (c *Client) DeleteNode(ctx context.Context, node *corev1.Node) error {
	if err := c.VerifyNodeDrainClean(ctx, node); err != nil {
		return err
	}
	return c.deleteNode(ctx, node.Name)
}

// deleteNode removes the Node object from the API server.
func (c *Client) deleteNode(ctx context.Context, name string) error {
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
