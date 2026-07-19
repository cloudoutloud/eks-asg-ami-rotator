// Package rotator orchestrates rolling an ASG's instances onto the current AMI:
// standby -> cordon/drain -> (healthy) -> delete node -> detach -> terminate.
package rotator

import (
	"context"
	"fmt"
	"time"

	"github.com/example/asg-ami-rotator/internal/awsclient"
	"github.com/example/asg-ami-rotator/internal/config"
	"github.com/example/asg-ami-rotator/internal/kube"
)

// Rotator reconciles a set of ASGs against their target AMI.
type Rotator struct {
	cfg  *config.Config
	aws  *awsclient.Client
	kube *kube.Client
	logf func(string, ...any)
}

// New builds a Rotator.
func New(cfg *config.Config, awsC *awsclient.Client, kubeC *kube.Client, logf func(string, ...any)) *Rotator {
	return &Rotator{cfg: cfg, aws: awsC, kube: kubeC, logf: logf}
}

// ReconcileAll resolves the managed ASG list and reconciles each one. Errors on
// individual ASGs are logged and do not abort the others.
func (r *Rotator) ReconcileAll(ctx context.Context) error {
	names := r.cfg.ASGNames
	if len(names) == 0 {
		discovered, err := r.aws.DiscoverByTag(ctx, r.cfg.ASGTagKey, r.cfg.ASGTagValue)
		if err != nil {
			return err
		}
		names = discovered
	}
	if len(names) == 0 {
		r.logf("no ASGs to manage")
		return nil
	}
	for _, name := range names {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := r.reconcileASG(ctx, name); err != nil {
			r.logf("ERROR reconciling asg %s: %v", name, err)
		}
	}
	return nil
}

func (r *Rotator) reconcileASG(ctx context.Context, name string) error {
	targetAMI, err := r.aws.ResolveTargetAMI(ctx, name)
	if err != nil {
		return err
	}

	gs, err := r.aws.DescribeGroup(ctx, name)
	if err != nil {
		return err
	}

	// Instances left in Standby indicate a roll that a previous pass (or a crashed
	// leader) started but never finished. Recover them first so they cannot be
	// orphaned.
	var standby, inService []string
	for _, in := range gs.Instances {
		switch in.LifecycleState {
		case "Standby":
			standby = append(standby, in.ID)
		case "InService":
			inService = append(inService, in.ID)
		}
	}

	amis, err := r.aws.InstanceAMIs(ctx, inService)
	if err != nil {
		return err
	}

	var stale []string
	for _, id := range inService {
		if amis[id] != targetAMI {
			stale = append(stale, id)
		}
	}

	if len(standby) == 0 && len(stale) == 0 {
		r.logf("asg %s: all InService instances already on target AMI %s; no Standby to recover", name, targetAMI)
		return nil
	}

	if len(standby) > 0 {
		r.logf("asg %s: recovering %d instance(s) left in Standby: %v", name, len(standby), standby)
	}
	if len(stale) > 0 {
		r.logf("asg %s: target AMI %s; %d stale instance(s) to roll: %v", name, targetAMI, len(stale), stale)
	}

	if r.cfg.DryRun {
		r.logf("[DRY-RUN] asg %s: would recover %d Standby and roll %d stale instance(s) one at a time",
			name, len(standby), len(stale))
		return nil
	}

	// Prepare the group for the roll (surge headroom + suspend rebalancing) and
	// arrange to restore it afterwards. Because every instance is fully
	// decommissioned (terminated) before this returns, restoring MaxSize is safe.
	restore, err := r.prepareGroup(ctx, gs)
	if err != nil {
		return err
	}
	defer restore()

	// Ensure a healthy starting point before touching anything.
	if err := r.aws.WaitForStable(ctx, name, r.cfg.StabilizeTimeout, r.cfg.StabilizePoll, r.logf); err != nil {
		return err
	}

	// 1) Finish recovering any pre-existing Standby instances.
	for i, id := range standby {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.logf("asg %s: [recover %d/%d] decommissioning Standby instance %s", name, i+1, len(standby), id)
		if err := r.decommissionStandby(ctx, name, id); err != nil {
			return fmt.Errorf("recover standby instance %s: %w", id, err)
		}
	}

	// 2) Roll the stale InService instances.
	for i, id := range stale {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.logf("asg %s: [%d/%d] rolling instance %s", name, i+1, len(stale), id)
		if err := r.rollInstance(ctx, name, id); err != nil {
			return fmt.Errorf("roll instance %s: %w", id, err)
		}
		r.logf("asg %s: [%d/%d] instance %s rolled", name, i+1, len(stale), id)
	}

	r.logf("asg %s: roll complete; all instances on AMI %s", name, targetAMI)
	return nil
}

// rollInstance performs the full lifecycle for a single stale instance:
// standby (surge a replacement on the new AMI) then decommission the old one.
func (r *Rotator) rollInstance(ctx context.Context, asgName, instanceID string) error {
	// Standby (no decrement) -> ASG launches a replacement on the new AMI.
	r.logf("instance %s: entering standby (replacement will launch on new AMI)", instanceID)
	if err := r.aws.EnterStandby(ctx, asgName, instanceID); err != nil {
		return err
	}
	if err := r.aws.WaitForInstanceState(ctx, asgName, instanceID, "Standby", r.cfg.StabilizeTimeout, r.cfg.StabilizePoll, r.logf); err != nil {
		return err
	}
	return r.decommissionStandby(ctx, asgName, instanceID)
}

// decommissionStandby drains, deletes, and terminates an instance that is
// already in Standby. It is idempotent enough to also recover an instance left
// in Standby by an interrupted earlier roll (cordon/drain on an already-drained
// node is a no-op).
func (r *Rotator) decommissionStandby(ctx context.Context, asgName, instanceID string) error {
	// Cordon + drain the node backing this instance.
	node, err := r.kube.NodeForInstance(ctx, instanceID)
	if err != nil {
		return err
	}
	if node == nil {
		r.logf("instance %s: no matching Kubernetes node found; skipping drain", instanceID)
	} else {
		if err := r.kube.CordonAndDrain(ctx, node); err != nil {
			return err
		}
	}

	// Wait until the group is healthy (replacement in service).
	if err := r.aws.WaitForStable(ctx, asgName, r.cfg.StabilizeTimeout, r.cfg.StabilizePoll, r.logf); err != nil {
		return err
	}

	// Delete the Node object.
	if node != nil {
		if err := r.kube.DeleteNode(ctx, node.Name); err != nil {
			return err
		}
	}

	// Terminate through the ASG WITHOUT decrementing desired capacity: desired
	// stays satisfied by the in-service replacement, so no healthy instance is
	// scaled down and no new instance is launched.
	r.logf("instance %s: terminating via ASG (no decrement)", instanceID)
	if err := r.aws.TerminateInASG(ctx, asgName, instanceID); err != nil {
		return err
	}
	if err := r.aws.WaitForInstanceState(ctx, asgName, instanceID, "Gone", r.cfg.StabilizeTimeout, r.cfg.StabilizePoll, r.logf); err != nil {
		return err
	}

	// Settle before moving to the next instance.
	return r.aws.WaitForStable(ctx, asgName, r.cfg.StabilizeTimeout, r.cfg.StabilizePoll, r.logf)
}

// prepareGroup raises MaxSize for the +1 surge and suspends AZRebalance,
// returning a function that restores the original settings.
func (r *Rotator) prepareGroup(ctx context.Context, gs *awsclient.GroupState) (func(), error) {
	name := gs.Name
	origMax := gs.MaxSize
	bumped := false
	suspended := false

	if r.cfg.ManageMaxSize {
		needed := gs.DesiredCapacity + 1
		if origMax < needed {
			r.logf("asg %s: raising MaxSize %d -> %d for surge", name, origMax, needed)
			if err := r.aws.SetMaxSize(ctx, name, needed); err != nil {
				return nil, err
			}
			bumped = true
		}
	}

	if r.cfg.SuspendAZRebalance {
		r.logf("asg %s: suspending AZRebalance", name)
		if err := r.aws.SuspendAZRebalance(ctx, name); err != nil {
			if bumped {
				_ = r.aws.SetMaxSize(ctx, name, origMax)
			}
			return nil, err
		}
		suspended = true
	}

	return func() {
		// Use a fresh short-lived context so cleanup runs even if the parent
		// context was cancelled.
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if suspended {
			r.logf("asg %s: resuming AZRebalance", name)
			if err := r.aws.ResumeAZRebalance(cctx, name); err != nil {
				r.logf("asg %s: WARN failed to resume AZRebalance: %v", name, err)
			}
		}
		if bumped {
			r.logf("asg %s: restoring MaxSize -> %d", name, origMax)
			if err := r.aws.SetMaxSize(cctx, name, origMax); err != nil {
				r.logf("asg %s: WARN failed to restore MaxSize: %v", name, err)
			}
		}
	}, nil
}
