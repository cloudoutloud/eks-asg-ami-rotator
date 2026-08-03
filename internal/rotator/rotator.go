// Package rotator orchestrates rolling an ASG's instances onto the current AMI:
// standby -> replacement InService -> replacement Ready -> cordon/drain ->
// delete node -> terminate (via ASG, no decrement).
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
	for _, name := range r.cfg.ASGNames {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := r.reconcileASG(ctx, name); err != nil {
			r.logf("ERROR reconciling asg %s: %v", name, err)
		}
	}
	return nil
}

// rollPlan is a single pass's view of an ASG: the AMI its instances should be
// on, the instances stranded in Standby by an earlier pass, and the InService
// instances still on an old AMI. Both roll strategies work from this, and both
// derive it fresh each pass rather than tracking progress locally, so an
// interrupted roll resumes from whatever state AWS reports.
type rollPlan struct {
	group   *awsclient.GroupState
	target  string
	standby []string
	stale   []string
}

func (r *Rotator) reconcileASG(ctx context.Context, name string) error {
	plan, err := r.planRoll(ctx, name)
	if err != nil {
		return err
	}
	if r.cfg.BatchMode {
		return r.reconcileBatch(ctx, plan)
	}
	return r.reconcileSerial(ctx, plan)
}

func (r *Rotator) planRoll(ctx context.Context, name string) (*rollPlan, error) {
	targetAMI, err := r.aws.ResolveTargetAMI(ctx, name)
	if err != nil {
		return nil, err
	}

	gs, err := r.aws.DescribeGroup(ctx, name)
	if err != nil {
		return nil, err
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
		return nil, err
	}

	var stale []string
	for _, id := range inService {
		if amis[id] != targetAMI {
			stale = append(stale, id)
		}
	}

	return &rollPlan{group: gs, target: targetAMI, standby: standby, stale: stale}, nil
}

// reconcileSerial rolls one instance at a time: each stale instance is surged,
// waited on, drained and terminated before the next is touched.
func (r *Rotator) reconcileSerial(ctx context.Context, p *rollPlan) error {
	name, gs, targetAMI := p.group.Name, p.group, p.target
	standby, stale := p.standby, p.stale

	// One-at-a-time rolls only ever add a single surge instance.
	maxRestoreTarget := r.maxRestoreTarget(gs, 1, 1)

	if len(standby) == 0 && len(stale) == 0 {
		r.logf("asg %s: all InService instances already on target AMI %s; no Standby to recover", name, targetAMI)
		// A prior interrupted roll may have left MaxSize at Desired+1 even though
		// the group is now fully rolled and settled. restoreMaxSizeWhenSettled
		// is a no-op when MaxSize is already at target.
		r.restoreMaxSizeWhenSettled(ctx, name, maxRestoreTarget)
		return nil
	}

	if len(standby) > 0 {
		r.logf("asg %s: recovering %d instance(s) left in Standby: %v", name, len(standby), standby)
	}
	if len(stale) > 0 {
		r.logf("asg %s: target AMI %s; %d stale instance(s) to roll: %v", name, targetAMI, len(stale), stale)
	}

	// Prepare the group for the roll (surge headroom + suspend rebalancing).
	// MaxSize is restored only once the roll has settled (no Standby left).
	restore, err := r.prepareGroup(ctx, gs, 1)
	if err != nil {
		return err
	}
	defer func() {
		restore()
		r.restoreMaxSizeWhenSettled(context.Background(), name, maxRestoreTarget)
	}()

	// Do not wait for full stability before recovering orphaned Standby
	// instances: a prior interrupted roll can leave healthy_inservice <
	// desired until the replacement finishes launching (or until we
	// decommission the Standby node). decommissionStandby waits for a healthy
	// replacement before draining.
	// Only run WaitForStable if there are no standby instances
	if len(standby) == 0 {
		if err := r.aws.WaitForStable(ctx, name, r.cfg.StabilizeTimeout, r.cfg.StabilizePoll, r.logf); err != nil {
			return err
		}
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
	if len(stale) > 0 {
		if err := r.aws.WaitForStable(ctx, name, r.cfg.StabilizeTimeout, r.cfg.StabilizePoll, r.logf); err != nil {
			return err
		}
	}

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
// standby (surge a replacement on the new AMI), wait for that replacement to be
// InService and Kubernetes-Ready, then decommission the old one.
func (r *Rotator) rollInstance(ctx context.Context, asgName, instanceID string) error {
	// Snapshot the InService instances so we can identify the replacement the
	// ASG launches for the surge.
	before, err := r.aws.InServiceInstanceIDs(ctx, asgName)
	if err != nil {
		return err
	}

	// Standby (no decrement) -> ASG launches a replacement on the new AMI.
	r.logf("instance %s: entering standby (replacement will launch on new AMI)", instanceID)
	if err := r.aws.EnterStandby(ctx, asgName, instanceID); err != nil {
		return err
	}
	if err := r.aws.WaitForInstanceState(ctx, asgName, instanceID, "Standby", r.cfg.StabilizeTimeout, r.cfg.StabilizePoll, r.logf); err != nil {
		return err
	}

	// Wait for the surge replacement to become InService+Healthy at the ASG
	// level, then for its Kubernetes Node to actually join and report Ready.
	// Only once the replacement can accept workloads do we drain the old node.
	newID, err := r.aws.WaitForNewInService(ctx, asgName, before, r.cfg.StabilizeTimeout, r.cfg.StabilizePoll, r.logf)
	if err != nil {
		return err
	}
	r.logf("instance %s: replacement %s is InService; waiting for its node to be Ready", instanceID, newID)
	if err := r.kube.WaitForNodeReady(ctx, newID, r.cfg.StabilizeTimeout, r.cfg.StabilizePoll); err != nil {
		return err
	}

	return r.decommissionStandby(ctx, asgName, instanceID)
}

// decommissionStandby drains, deletes, and terminates an instance that is
// already in Standby. It is idempotent enough to also recover an instance left
// in Standby by an interrupted earlier roll (cordon/drain on an already-drained
// node is a no-op).
func (r *Rotator) decommissionStandby(ctx context.Context, asgName, instanceID string) error {
	// Wait until the group is healthy (replacement in service) BEFORE draining,
	// so the old node's pods have somewhere to land. This also guards the
	// recovery path, where this method is called directly for an orphaned
	// Standby instance.
	if err := r.aws.WaitForStable(ctx, asgName, r.cfg.StabilizeTimeout, r.cfg.StabilizePoll, r.logf); err != nil {
		return err
	}

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

// prepareGroup raises MaxSize by surge to make room for the replacement
// instances the ASG will launch, suspends AZRebalance, and returns a function
// that restores the original settings.
func (r *Rotator) prepareGroup(ctx context.Context, gs *awsclient.GroupState, surge int32) (func(), error) {
	name := gs.Name
	origMax := gs.MaxSize
	bumped := false
	suspended := false

	if r.cfg.ManageMaxSize {
		needed := gs.DesiredCapacity + surge
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
	}, nil
}

// maxRestoreTarget returns the MaxSize to restore after a settled roll, or -1
// when ManageMaxSize is off or the current MaxSize is higher than any ceiling
// this controller would have raised (in which case it is the operator's value
// and must be left alone).
//
// surge is the headroom this pass needs. band is the largest headroom the
// controller could have added in an earlier pass; anything up to Desired+band is
// treated as a ceiling of our own, which is what lets a restarted controller
// still lower a ceiling it no longer has the in-memory context for.
func (r *Rotator) maxRestoreTarget(gs *awsclient.GroupState, surge, band int32) int32 {
	if !r.cfg.ManageMaxSize {
		return -1
	}
	if band < surge {
		band = surge
	}
	switch {
	case gs.MaxSize < gs.DesiredCapacity+surge:
		// Below what this pass needs: prepareGroup will raise it, so put back
		// what we found.
		return gs.MaxSize
	case gs.MaxSize <= gs.DesiredCapacity+band:
		return gs.DesiredCapacity
	default:
		return -1
	}
}

// restoreMaxSizeWhenSettled lowers MaxSize back to target once no instances
// remain in Standby. If the roll is still in progress (Standby present), it
// skips restoration so the surge replacement keeps its headroom.
func (r *Rotator) restoreMaxSizeWhenSettled(ctx context.Context, name string, target int32) {
	if target < 0 {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	gs, err := r.aws.DescribeGroup(cctx, name)
	if err != nil {
		r.logf("asg %s: WARN failed to describe ASG before restoring MaxSize: %v", name, err)
		return
	}
	var standbyCount int
	for _, in := range gs.Instances {
		if in.LifecycleState == "Standby" {
			standbyCount++
		}
	}
	if standbyCount > 0 {
		r.logf("asg %s: deferring MaxSize restore -> %d (%d instance(s) still in Standby)", name, target, standbyCount)
		return
	}
	if gs.MaxSize <= target {
		return
	}
	r.logf("asg %s: restoring MaxSize %d -> %d", name, gs.MaxSize, target)
	if err := r.aws.SetMaxSize(cctx, name, target); err != nil {
		r.logf("asg %s: WARN failed to restore MaxSize: %v", name, err)
	}
}
