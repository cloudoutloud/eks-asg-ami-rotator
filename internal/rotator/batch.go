package rotator

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// reconcileBatch rolls an ASG in waves rather than one instance at a time.
//
// Each wave surges up to --batch-max-surge stale instances into Standby, waits
// for replacements to join and report Ready, then drains them --batch-size at a
// time. With --batch-terminate-last, node deletion and instance termination
// happen once after every wave has been drained; otherwise each wave is torn
// down before the next surges.
func (r *Rotator) reconcileBatch(ctx context.Context, p *rollPlan) error {
	name, targetAMI := p.group.Name, p.target
	rollCount := len(p.standby) + len(p.stale)

	surge, band := r.batchSurgeBand(p)
	maxRestoreTarget := r.maxRestoreTarget(p.group, surge, band)

	if rollCount == 0 {
		r.logf("asg %s: all InService instances already on target AMI %s; no Standby to recover", name, targetAMI)
		r.restoreMaxSizeWhenSettled(ctx, name, maxRestoreTarget)
		return nil
	}

	r.logf("asg %s: batch roll (batch-size=%d, max-surge=%d, terminate-last=%t); target AMI %s; %d stale, %d in Standby",
		name, r.cfg.BatchSize, r.cfg.BatchMaxSurge, r.cfg.BatchTerminateLast, targetAMI, len(p.stale), len(p.standby))

	restore, err := r.prepareGroup(ctx, p.group, surge)
	if err != nil {
		return err
	}
	defer func() {
		restore()
		r.restoreMaxSizeWhenSettled(context.Background(), name, maxRestoreTarget)
	}()

	outgoing := make([]string, 0, rollCount)
	outgoing = append(outgoing, p.standby...)
	outgoing = append(outgoing, p.stale...)

	if len(p.standby) > 0 {
		if r.cfg.BatchTerminateLast {
			r.logf("asg %s: resuming interrupted roll: draining %d instance(s) already in Standby", name, len(p.standby))
			if err := r.drainWave(ctx, name, p.standby, outgoing); err != nil {
				return err
			}
		} else {
			r.logf("asg %s: resuming interrupted wave: decommissioning %d instance(s) already in Standby", name, len(p.standby))
			if err := r.decommissionWave(ctx, name, p.standby, outgoing); err != nil {
				return err
			}
		}
	}

	remaining := p.stale
	for wave := 1; len(remaining) > 0; wave++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n := r.waveSize(len(remaining))
		instances := remaining[:n]
		remaining = remaining[n:]

		r.logf("asg %s: wave %d: rolling %d instance(s), %d still to go after this wave",
			name, wave, len(instances), len(remaining))
		if err := r.surgeWave(ctx, name, instances); err != nil {
			return fmt.Errorf("surge wave %v: %w", instances, err)
		}
		if r.cfg.BatchTerminateLast {
			if err := r.drainWave(ctx, name, instances, outgoing); err != nil {
				return fmt.Errorf("drain wave %v: %w", instances, err)
			}
		} else if err := r.decommissionWave(ctx, name, instances, outgoing); err != nil {
			return fmt.Errorf("decommission wave %v: %w", instances, err)
		}
	}

	if r.cfg.BatchTerminateLast {
		r.logf("asg %s: all waves drained; terminating %d instance(s)", name, len(outgoing))
		if err := r.terminateRoll(ctx, name, outgoing); err != nil {
			return err
		}
	}

	r.logf("asg %s: batch roll complete; all instances on AMI %s", name, targetAMI)
	return nil
}

// batchSurgeBand returns the MaxSize headroom and restore band for this pass.
// Terminate-last keeps every replaced instance in Standby until the roll
// finishes draining, so headroom is the full roll size. Per-wave termination
// only needs room for one wave (or orphaned Standby) at a time.
func (r *Rotator) batchSurgeBand(p *rollPlan) (surge, band int32) {
	if r.cfg.BatchTerminateLast {
		surge = int32(len(p.standby) + len(p.stale))
	} else {
		surge = int32(len(p.standby))
		if wave := int32(r.waveSize(len(p.stale))); wave > surge {
			surge = wave
		}
	}
	band = surge
	if limit := int32(r.cfg.BatchMaxSurge); limit > band {
		band = limit
	}
	return surge, band
}

// waveSize is how many of the remaining stale instances one wave surges: all of
// them unless --batch-max-surge caps it.
func (r *Rotator) waveSize(remaining int) int {
	if limit := r.cfg.BatchMaxSurge; limit > 0 && limit < remaining {
		return limit
	}
	return remaining
}

// surgeWave is phase 1: move the whole wave into Standby so the ASG launches a
// replacement for each, then wait for those replacements to become InService and
// for their nodes to join and report Ready. Nothing is drained here, so the wave
// keeps serving traffic until its replacements can take over.
func (r *Rotator) surgeWave(ctx context.Context, name string, instances []string) error {
	if err := r.aws.WaitForStable(ctx, name, r.cfg.StabilizeTimeout, r.cfg.StabilizePoll, r.logf); err != nil {
		return err
	}

	before, err := r.aws.InServiceInstanceIDs(ctx, name)
	if err != nil {
		return err
	}

	r.logf("asg %s: [phase 1] moving %d instance(s) into Standby: %v", name, len(instances), instances)
	if err := r.aws.EnterStandbyMany(ctx, name, instances); err != nil {
		return err
	}
	if err := r.aws.WaitForInstancesState(ctx, name, instances, "Standby", r.cfg.StabilizeTimeout, r.cfg.StabilizePoll, r.logf); err != nil {
		return err
	}

	r.logf("asg %s: [phase 1] waiting for %d replacement(s) to become InService", name, len(instances))
	replacements, err := r.aws.WaitForNewInServiceCount(ctx, name, before, len(instances), r.cfg.StabilizeTimeout, r.cfg.StabilizePoll, r.logf)
	if err != nil {
		return err
	}

	r.logf("asg %s: [phase 1] replacements %v are InService; waiting for their nodes to be Ready", name, replacements)
	return r.kube.WaitForNodesReady(ctx, replacements, r.cfg.StabilizeTimeout, r.cfg.StabilizePoll)
}

// decommissionWave drains a wave in --batch-size chunks, then deletes nodes and
// terminates those instances before the next wave surges.
func (r *Rotator) decommissionWave(ctx context.Context, name string, wave, outgoing []string) error {
	if err := r.drainWave(ctx, name, wave, outgoing); err != nil {
		return err
	}
	return r.terminateRoll(ctx, name, wave)
}

// drainWave cordons every outgoing node, then drains this wave --batch-size at
// a time. When --batch-terminate-last is set, termination is deferred to the
// end of the roll via terminateRoll.
func (r *Rotator) drainWave(ctx context.Context, name string, wave, outgoing []string) error {
	if err := r.aws.WaitForStable(ctx, name, r.cfg.StabilizeTimeout, r.cfg.StabilizePoll, r.logf); err != nil {
		return err
	}

	if err := r.cordonAll(ctx, name, outgoing); err != nil {
		return err
	}

	batches := (len(wave) + r.cfg.BatchSize - 1) / r.cfg.BatchSize
	for i, start := 1, 0; start < len(wave); i, start = i+1, start+r.cfg.BatchSize {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		end := start + r.cfg.BatchSize
		if end > len(wave) {
			end = len(wave)
		}
		batch := wave[start:end]

		nodes, err := r.kube.NodesForInstances(ctx, batch)
		if err != nil {
			return err
		}
		r.logf("asg %s: [drain] batch %d/%d: draining %d node(s): %s",
			name, i, batches, len(batch), formatDrainTargets(batch, nodes))
		if err := r.drainBatch(ctx, batch, nodes); err != nil {
			return err
		}
		r.logf("asg %s: [drain] batch %d/%d complete", name, i, batches)
	}

	return nil
}

// terminateRoll deletes every drained node and terminates every Standby instance
// in instances, then waits for the group to settle once.
func (r *Rotator) terminateRoll(ctx context.Context, name string, instances []string) error {
	if len(instances) == 0 {
		return nil
	}

	nodes, err := r.kube.NodesForInstances(ctx, instances)
	if err != nil {
		return err
	}

	r.logf("asg %s: [terminate] deleting %d node object(s)", name, len(instances))
	for _, id := range instances {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		node, ok := nodes[id]
		if !ok {
			r.logf("instance %s: no matching Kubernetes node found; skipping delete", id)
			continue
		}
		if err := r.kube.DeleteNode(ctx, node); err != nil {
			return err
		}
	}

	r.logf("asg %s: [terminate] terminating %d instance(s): %v", name, len(instances), instances)
	for _, id := range instances {
		if err := r.aws.TerminateInASG(ctx, name, id); err != nil {
			return err
		}
	}
	if err := r.aws.WaitForInstancesState(ctx, name, instances, "Gone", r.cfg.StabilizeTimeout, r.cfg.StabilizePoll, r.logf); err != nil {
		return err
	}
	return r.aws.WaitForStable(ctx, name, r.cfg.StabilizeTimeout, r.cfg.StabilizePoll, r.logf)
}

// cordonAll marks every node backing the given instances unschedulable. Called
// once per wave with the full outgoing set, so nodes already cordoned by an
// earlier wave are skipped.
func (r *Rotator) cordonAll(ctx context.Context, name string, instances []string) error {
	nodes, err := r.kube.NodesForInstances(ctx, instances)
	if err != nil {
		return err
	}
	cordoned := 0
	for _, id := range instances {
		node, ok := nodes[id]
		if !ok || node.Spec.Unschedulable {
			continue
		}
		if err := r.kube.Cordon(ctx, node); err != nil {
			return err
		}
		cordoned++
	}
	if cordoned > 0 {
		r.logf("asg %s: cordoned %d outgoing node(s); %d already cordoned", name, cordoned, len(nodes)-cordoned)
	}
	return nil
}

// drainBatch drains the batch's nodes one at a time. Draining sequentially
// keeps evictions from piling up against PodDisruptionBudgets.
func (r *Rotator) drainBatch(ctx context.Context, batch []string, nodes map[string]*corev1.Node) error {
	if nodes == nil {
		var err error
		nodes, err = r.kube.NodesForInstances(ctx, batch)
		if err != nil {
			return err
		}
	}
	for _, id := range batch {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		node, ok := nodes[id]
		if !ok {
			r.logf("instance %s: no matching Kubernetes node found; skipping drain", id)
			continue
		}
		if err := r.kube.Drain(ctx, node); err != nil {
			return err
		}
	}
	return nil
}

func formatDrainTargets(ids []string, nodes map[string]*corev1.Node) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		if n, ok := nodes[id]; ok {
			parts = append(parts, fmt.Sprintf("%s (%s)", id, n.Name))
		} else {
			parts = append(parts, id)
		}
	}
	return strings.Join(parts, ", ")
}
