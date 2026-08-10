package rotator

import (
	"context"
	"fmt"
)

// reconcileBatch rolls an ASG in waves rather than one instance at a time.
//
// Each wave has two phases. Phase 1 moves up to --batch-max-surge stale
// instances into Standby together so the ASG launches all their replacements at
// once, then waits for those replacements to join the cluster and report Ready.
// Phase 2 cordons the outgoing nodes and drains and terminates them
// --batch-size at a time.
//
// The win over the serial strategy is that a wave pays the node-boot wait once
// instead of once per instance; the cost is that more capacity is in flight at
// once, which is why the surge is capped and the mode is opt-in.
func (r *Rotator) reconcileBatch(ctx context.Context, p *rollPlan) error {
	if r.cfg.BatchStandbyBuffer > 0 {
		return r.reconcileBatchBuffered(ctx, p)
	}
	return r.reconcileBatchUnbuffered(ctx, p)
}

// reconcileBatchUnbuffered rolls an ASG in waves rather than one instance at a time.
//
// Each wave has two phases. Phase 1 moves up to --batch-max-surge stale
// instances into Standby together so the ASG launches all their replacements at
// once, then waits for those replacements to join the cluster and report Ready.
// Phase 2 cordons the outgoing nodes and drains and terminates them
// --batch-size at a time.
//
// The win over the serial strategy is that a wave pays the node-boot wait once
// instead of once per instance; the cost is that more capacity is in flight at
// once, which is why the surge is capped and the mode is opt-in.
func (r *Rotator) reconcileBatchUnbuffered(ctx context.Context, p *rollPlan) error {
	name, targetAMI := p.group.Name, p.target

	// Peak extra capacity is whichever is larger: instances a previous pass
	// already surged into Standby, or the wave this pass is about to surge.
	surge := int32(len(p.standby))
	if wave := int32(r.waveSize(len(p.stale))); wave > surge {
		surge = wave
	}
	band := surge
	if limit := int32(r.cfg.BatchMaxSurge); limit > band {
		band = limit
	}
	maxRestoreTarget := r.maxRestoreTarget(p.group, surge, band)

	if len(p.standby) == 0 && len(p.stale) == 0 {
		r.logf("asg %s: all InService instances already on target AMI %s; no Standby to recover", name, targetAMI)
		r.restoreMaxSizeWhenSettled(ctx, name, maxRestoreTarget)
		return nil
	}

	r.logf("asg %s: batch roll (batch-size=%d, max-surge=%d); target AMI %s; %d stale, %d in Standby",
		name, r.cfg.BatchSize, r.cfg.BatchMaxSurge, targetAMI, len(p.stale), len(p.standby))

	restore, err := r.prepareGroup(ctx, p.group, surge)
	if err != nil {
		return err
	}
	defer func() {
		restore()
		r.restoreMaxSizeWhenSettled(context.Background(), name, maxRestoreTarget)
	}()

	// Every instance still due for replacement, whether already in Standby from
	// an interrupted pass or still InService. The whole set is cordoned as soon as
	// there is somewhere else for pods to go, so a pod evicted by one wave is
	// never rescheduled onto a node a later wave is going to drain.
	outgoing := make([]string, 0, len(p.standby)+len(p.stale))
	outgoing = append(outgoing, p.standby...)
	outgoing = append(outgoing, p.stale...)

	// Finish a wave a previous pass left mid-flight before surging another one,
	// so the number of extra instances stays within the surge cap.
	if len(p.standby) > 0 {
		r.logf("asg %s: resuming interrupted wave: %d instance(s) already in Standby", name, len(p.standby))
		if err := r.decommissionWave(ctx, name, p.standby, outgoing); err != nil {
			return err
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
		if err := r.decommissionWave(ctx, name, instances, outgoing); err != nil {
			return fmt.Errorf("decommission wave %v: %w", instances, err)
		}
	}

	r.logf("asg %s: batch roll complete; all instances on AMI %s", name, targetAMI)
	return nil
}

// reconcileBatchBuffered is like reconcileBatchUnbuffered but keeps
// --batch-standby-buffer instances in Standby after each wave (with their
// replacements up) so the next drain batch always has extra schedulable capacity.
//
// Example with max-surge=4, batch-size=3, buffer=1: surge 4 into Standby,
// drain 3, leave 1 buffered; next wave surges 3 more (pool back to 4), drains 3,
// leaves 1 buffered; repeat until stale is exhausted, then drain the buffer.
func (r *Rotator) reconcileBatchBuffered(ctx context.Context, p *rollPlan) error {
	name, targetAMI := p.group.Name, p.target
	buffer := r.cfg.BatchStandbyBuffer
	maxSurge := r.cfg.BatchMaxSurge

	surge := int32(maxSurge)
	maxRestoreTarget := r.maxRestoreTarget(p.group, surge, surge)

	if len(p.standby) == 0 && len(p.stale) == 0 {
		r.logf("asg %s: all InService instances already on target AMI %s; no Standby to recover", name, targetAMI)
		r.restoreMaxSizeWhenSettled(ctx, name, maxRestoreTarget)
		return nil
	}

	r.logf("asg %s: batch roll (batch-size=%d, max-surge=%d, standby-buffer=%d); target AMI %s; %d stale, %d in Standby",
		name, r.cfg.BatchSize, maxSurge, buffer, targetAMI, len(p.stale), len(p.standby))

	restore, err := r.prepareGroup(ctx, p.group, surge)
	if err != nil {
		return err
	}
	defer func() {
		restore()
		r.restoreMaxSizeWhenSettled(context.Background(), name, maxRestoreTarget)
	}()

	standby := append([]string(nil), p.standby...)
	remaining := append([]string(nil), p.stale...)

	// Resume interrupted decommission: more than buffer instances in Standby.
	if len(standby) > buffer {
		r.logf("asg %s: resuming interrupted wave: %d instance(s) in Standby (%d reserved as buffer)", name, len(standby), buffer)
		toDrain := append([]string(nil), standby[buffer:]...)
		if err := r.decommissionWave(ctx, name, toDrain, buildOutgoing(standby, remaining)); err != nil {
			return err
		}
		standby = standby[:buffer]
	}

	wave := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if len(remaining) == 0 {
			if len(standby) == 0 {
				break
			}
			r.logf("asg %s: final wave: draining %d buffered instance(s)", name, len(standby))
			if err := r.decommissionWave(ctx, name, standby, buildOutgoing(standby, nil)); err != nil {
				return err
			}
			break
		}

		if len(standby) < maxSurge {
			n := surgeSizeBuffered(len(standby), len(remaining), maxSurge)
			instances := remaining[:n]
			remaining = remaining[n:]

			wave++
			r.logf("asg %s: wave %d: surging %d instance(s) (%d in Standby incl. buffer, %d stale after)",
				name, wave, len(instances), len(standby), len(remaining))
			if err := r.surgeWave(ctx, name, instances); err != nil {
				return fmt.Errorf("surge wave %v: %w", instances, err)
			}
			standby = append(standby, instances...)
		}

		if len(standby) <= buffer {
			continue
		}
		if len(standby) < maxSurge && len(remaining) > 0 {
			continue
		}

		toDrain := append([]string(nil), standby[buffer:]...)
		r.logf("asg %s: wave %d: draining %d instance(s); keeping %d in Standby as buffer",
			name, wave, len(toDrain), buffer)
		if err := r.decommissionWave(ctx, name, toDrain, buildOutgoing(standby, remaining)); err != nil {
			return fmt.Errorf("decommission wave %v: %w", toDrain, err)
		}
		standby = standby[:buffer]
	}

	r.logf("asg %s: batch roll complete; all instances on AMI %s", name, targetAMI)
	return nil
}

func buildOutgoing(standby, stale []string) []string {
	outgoing := make([]string, 0, len(standby)+len(stale))
	outgoing = append(outgoing, standby...)
	outgoing = append(outgoing, stale...)
	return outgoing
}

// surgeSizeBuffered is how many stale instances to move into Standby so the
// Standby pool reaches --batch-max-surge without exceeding it.
func surgeSizeBuffered(currentStandby, remainingStale, maxSurge int) int {
	capacity := maxSurge - currentStandby
	if capacity <= 0 || remainingStale <= 0 {
		return 0
	}
	if remainingStale < capacity {
		return remainingStale
	}
	return capacity
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
	// Healthy starting point before taking capacity out of service.
	if err := r.aws.WaitForStable(ctx, name, r.cfg.StabilizeTimeout, r.cfg.StabilizePoll, r.logf); err != nil {
		return err
	}

	// Snapshot InService membership so the replacements can be told apart from
	// the instances leaving service.
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

// decommissionWave is phase 2: cordon every outgoing node, then drain and
// terminate this wave --batch-size at a time. outgoing is the full set of
// instances still due for replacement, which may span later waves.
func (r *Rotator) decommissionWave(ctx context.Context, name string, wave, outgoing []string) error {
	// Never drain until the group is back to full strength, so evicted pods have
	// somewhere to land. Normally a no-op straight after surgeWave, but it is
	// what makes a wave interrupted mid-phase-1 safe to pick up: those instances
	// are in Standby with their replacements still booting.
	if err := r.aws.WaitForStable(ctx, name, r.cfg.StabilizeTimeout, r.cfg.StabilizePoll, r.logf); err != nil {
		return err
	}

	// Cordon every outgoing node, not just this wave's, so pods evicted now are
	// not placed on a node a later wave will drain — otherwise a pod can be moved
	// once per wave. Deferring this until replacements are in service is what
	// keeps the group from having nowhere to schedule while they boot.
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

		r.logf("asg %s: [phase 2] batch %d/%d: draining %d node(s): %v", name, i, batches, len(batch), batch)
		if err := r.drainBatch(ctx, batch); err != nil {
			return err
		}

		// Terminate without decrementing desired capacity: desired is already
		// satisfied by the replacements, so no healthy instance is scaled down
		// and no further instance is launched.
		r.logf("asg %s: [phase 2] batch %d/%d: terminating %d instance(s)", name, i, batches, len(batch))
		for _, id := range batch {
			if err := r.aws.TerminateInASG(ctx, name, id); err != nil {
				return err
			}
		}
		if err := r.aws.WaitForInstancesState(ctx, name, batch, "Gone", r.cfg.StabilizeTimeout, r.cfg.StabilizePoll, r.logf); err != nil {
			return err
		}
		if err := r.aws.WaitForStable(ctx, name, r.cfg.StabilizeTimeout, r.cfg.StabilizePoll, r.logf); err != nil {
			return err
		}
		r.logf("asg %s: [phase 2] batch %d/%d complete", name, i, batches)
	}
	return nil
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

// drainBatch drains and deletes the batch's nodes one at a time. Draining
// sequentially keeps evictions from piling up against PodDisruptionBudgets; the
// batching win comes from sharing one surge wait per wave, not from parallel
// drains.
func (r *Rotator) drainBatch(ctx context.Context, batch []string) error {
	nodes, err := r.kube.NodesForInstances(ctx, batch)
	if err != nil {
		return err
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
		if err := r.kube.DeleteNode(ctx, node); err != nil {
			return err
		}
	}
	return nil
}
