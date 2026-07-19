# asg-ami-rotator

A Kubernetes controller that keeps a set of EC2 Auto Scaling Groups (ASGs)
rolled onto their current AMI, one node at a time, draining each node gracefully
through the Kubernetes API.

It is the automated, in-cluster successor to the `rotate-asg-standby.sh` and
`rotate-asg-terminate.sh` scripts in the parent directory.

## Why

Karpenter manages most of the fleet, but some **bootstrap** nodes still live in
a plain ASG (for example, the nodes Karpenter itself needs before it can act).
When that ASG's launch template points at a new AMI, those nodes must be rolled
without an outage. This controller does that continuously and unattended.

It **runs on Karpenter on-demand nodes** (via a `nodeSelector`) precisely because
it manages the ASG bootstrap nodes — it must not be scheduled onto a node it is
about to drain.

## What it does

On a timer, for each managed ASG:

1. **Resolve the target AMI** from the ASG's launch template / mixed-instances
   policy / launch configuration, dereferencing `resolve:ssm:` aliases.
2. **Recover any instances left in `Standby`** by an interrupted earlier roll
   (crashed leader, failed drain), decommissioning them before starting new
   work so they can't be orphaned.
3. **Find `InService` instances on an old AMI.** If there are none (and nothing
   to recover), it does nothing.
4. Prepare the group: temporarily **raise `MaxSize`** to allow the +1 surge and
   **suspend `AZRebalance`**.
5. For each stale instance, **one at a time**:
   1. `EnterStandby` (without decrementing desired) → the ASG launches a
      replacement on the new AMI.
   2. **Cordon and drain** the backing Kubernetes node (respects
      PodDisruptionBudgets; ignores DaemonSets).
   3. **Wait until the group is healthy** (the replacement is `InService`).
   4. **Delete the Node** object.
   5. **Terminate** the old instance through the ASG with
      `TerminateInstanceInAutoScalingGroup` **without decrementing desired**, so
      the healthy replacement is not scaled back down.
6. Restore `MaxSize` and resume `AZRebalance` (safe, because every instance is
   fully terminated before this runs).

Instance → Node mapping is done via the node's `spec.providerID`
(`aws:///<az>/<instance-id>`).

## Configuration

Flags (each has an env fallback, shown in parentheses):

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--asg-names` | `ASG_NAMES` | – | Comma-separated ASG names to manage. |
| `--asg-tag-key` / `--asg-tag-value` | `ASG_TAG_KEY` / `ASG_TAG_VALUE` | – | Discover ASGs by tag when `--asg-names` is empty. |
| `--region` | `AWS_REGION` | SDK default | AWS region. |
| `--poll-interval` | `POLL_INTERVAL` | `60s` | Reconcile cadence. |
| `--stabilize-timeout` | `STABILIZE_TIMEOUT` | `20m` | Max wait for the ASG to become healthy. |
| `--stabilize-poll` | `STABILIZE_POLL` | `15s` | Poll cadence while waiting. |
| `--drain-timeout` | `DRAIN_TIMEOUT` | `10m` | Max wait for a node to drain. |
| `--drain-grace-period` | `DRAIN_GRACE_PERIOD` | `-1` | Pod grace period (`-1` = pod's own). |
| `--drain-force` | `DRAIN_FORCE` | `true` | Evict unmanaged/standalone pods. |
| `--ignore-daemonsets` | `IGNORE_DAEMONSETS` | `true` | Ignore DaemonSet pods. |
| `--delete-emptydir-data` | `DELETE_EMPTYDIR_DATA` | `true` | Allow eviction of emptyDir pods. |
| `--suspend-azrebalance` | `SUSPEND_AZREBALANCE` | `true` | Suspend AZRebalance during a roll. |
| `--manage-max-size` | `MANAGE_MAX_SIZE` | `true` | Temporarily raise MaxSize for surge. |
| `--dry-run` | `DRY_RUN` | `false` | Log actions without mutating anything. |
| `--leader-elect` | `LEADER_ELECT` | `true` | Leader election (safe with >1 replica). |

## AWS permissions (IRSA)

Attach the policy in [`deploy/iam-policy.json`](deploy/iam-policy.json) to the
IAM role referenced by the ServiceAccount's `eks.amazonaws.com/role-arn`
annotation. It grants read-only describes plus the ASG/EC2 mutations the roll
needs. Scope the `Resource`/conditions to your bootstrap ASGs in production.

## Kubernetes permissions

See [`deploy/rbac.yaml`](deploy/rbac.yaml): read/patch/delete nodes, list pods,
create evictions, read workload controllers, and manage a leader-election lease.

## Build & deploy

```bash
make tidy          # resolve go.sum
make build         # build the binary
make docker IMAGE=<your-registry>/asg-ami-rotator:tag
# edit deploy/*.yaml: image, ASG_NAMES, AWS_REGION, IRSA role ARN
make deploy
```

## Local dry-run

```bash
make run ASG_NAMES=my-bootstrap-asg AWS_REGION=eu-west-2
```

This uses your local kubeconfig and AWS credentials, disables leader election,
and only logs what it *would* do.

## Notes & safety

- **One instance at a time**, and each step waits for the ASG to be healthy
  before proceeding — a failed drain or unhealthy replacement halts the roll.
- Cleanup (restore `MaxSize`, resume `AZRebalance`) runs even if the process is
  cancelled mid-roll.
- Because it drains via the Kube API directly, it does **not** require
  aws-node-termination-handler or ASG lifecycle hooks.
