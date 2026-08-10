# eks-asg-ami-rotator

A Kubernetes controller that keeps a set of EC2 Auto Scaling Groups (ASGs)
rolled onto their current AMI, one node at a time, draining each node gracefully
through the Kubernetes API.

This is intended to be used with Amazon EKS https://docs.aws.amazon.com/eks/latest/userguide/what-is-eks.html

## What is solves

Karpenter may manage most of the fleet, but some **bootstrap** nodes still live in
a plain ASG (for example, the nodes Karpenter itself needs before it can act).
When that ASG's launch template points at a new AMI, those nodes must be rolled
without an outage. This controller does that continuously and unattended.

It **runs on Karpenter on-demand nodes** (via a `nodeSelector`) precisely because
it manages the ASG bootstrap nodes — it must not be scheduled onto a node it is
about to drain.

This is useful if your team build there own AMI for EKS worker nodes.

## What it does

On a timer, for each managed ASG:

1. **Resolve the target AMI** from the ASG's launch template / mixed-instances
   policy / launch configuration, dereferencing `resolve:ssm:` aliases.
2. **Recover any instances left in `Standby`** by an interrupted earlier roll
   (crashed leader, failed drain), decommissioning them before starting new
   work so they can't be orphaned.
3. **Find `InService` instances on an old AMI.** If there are none (and nothing
   to recover), it does nothing.
4. Prepare the group: temporarily **raise `MaxSize`** to make room for the surge
   and **suspend `AZRebalance`**.
5. **Roll the stale instances** using one of two strategies — see
   [Roll strategies](#roll-strategies).
6. Restore `MaxSize` (once no instances remain in `Standby`) and resume
   `AZRebalance`.

Instance → Node mapping is done via the node's `spec.providerID`
(`aws:///<az>/<instance-id>`).

## Roll strategies

The controller has two modes. **Serial is the default**; batch is opt-in via
`--batch-mode` / `BATCH_MODE` so it can be enabled per environment.

Both modes derive all their state from AWS and Kubernetes on every pass and
store nothing locally, so an interrupted roll (crashed leader, lost leader
election, failed drain) resumes from whatever state the ASG reports.

### Serial — one instance at a time (default)

For each stale instance, in turn:
   1. `EnterStandby` (without decrementing desired) → the ASG launches a
      replacement on the new AMI.
   2. **Wait for the replacement to be usable** before touching the old node:
      first until it is `InService`/`Healthy` at the ASG level, then until its
      Kubernetes Node has joined the cluster and reports `Ready` (and is
      schedulable). This guarantees evicted pods have somewhere to land.
   3. **Cordon and drain** the backing Kubernetes node (respects
      PodDisruptionBudgets; ignores DaemonSets).
   4. **Verify** no evictable workloads remain (DaemonSet/mirror pods allowed),
      then **delete the Node** object.
   5. **Terminate** the old instance through the ASG with
      `TerminateInstanceInAutoScalingGroup` **without decrementing desired**, so
      the healthy replacement is not scaled back down.

Safest and slowest: only ever one extra instance, and only one node draining at
a time. Rolling *n* nodes costs *n* node-boot waits.

### Batch — waves of instances (`--batch-mode`)

Stale instances are rolled in **waves** of up to `--batch-max-surge`. Each wave
runs two phases.

**Phase 1 — surge:**

1. `EnterStandby` for **every instance in the wave at once** (without
   decrementing desired) → the ASG launches a replacement for each.
2. Wait for all replacements to become `InService`/`Healthy`.
3. Wait for all their Kubernetes Nodes to join and report `Ready`.

**Phase 2 — decommission:**

4. **Cordon every node still on an old AMI** — not just this wave's, but every
   remaining stale node and any node already in `Standby`. See
   [why the cordon is fleet-wide](#why-the-cordon-is-fleet-wide-but-late).
5. Then, `--batch-size` nodes at a time: **drain** (sequentially within the
   batch, respecting PodDisruptionBudgets), **verify** no evictable workloads
   remain (DaemonSet/mirror pods allowed), **delete the Node** objects, and
   **terminate** the instances.
6. Wait for the group to settle, then move to the next batch.

The saving is that a wave pays the node-boot wait **once** instead of once per
instance. The cost is more capacity in flight, which is what `--batch-max-surge`
bounds.

#### Why the cordon is fleet-wide but late

Two separate decisions, both about where evicted pods end up.

**Fleet-wide, not per wave.** If only the current wave were cordoned, a pod
evicted in wave 1 could be scheduled onto a stale node in wave 3 — and evicted
again when wave 3 comes round. On a 70-instance ASG rolled 10 at a time that is
up to seven moves for the same pod. Cordoning every outgoing node means each pod
moves **once**, onto a node on the new AMI.

**After phase 1, not before.** Cordoning stale nodes before their replacements
are `Ready` would leave the group with nothing schedulable for the several
minutes a node takes to boot and join. Waiting until the first wave's
replacements are up means there is always somewhere for pods to land.

Cordoning is idempotent and re-checked each wave, so nodes stay cordoned across
controller restarts. Note that a roll which halts permanently leaves the
remaining stale nodes cordoned — they keep running their existing pods, but take
no new ones until the roll completes or an operator uncordons them.

#### Sizing the two numbers

| Flag | Bounds |
|------|--------|
| `--batch-max-surge` | How many **extra** instances exist at peak, and so how much `MaxSize` headroom and EC2 quota a roll needs |
| `--batch-size` | How many nodes are drained and terminated together, and so how much disruption pods see at once |

`--batch-max-surge` may be smaller than the number of stale instances; the roll
just takes more waves. Setting it (rather than leaving it `0` for unlimited) also
lets a restarted controller recognise and lower a `MaxSize` ceiling it raised in
an earlier pass.

For a 70-instance ASG, `--batch-max-surge=10` with `--batch-size=5` gives seven
waves, each surging 10 replacements and then draining them in two batches of
five — so at most 10 extra instances and 5 nodes draining at any moment, and
seven node-boot waits instead of 70.

#### Standby buffer (`--batch-standby-buffer`)

When set to a positive value, the controller keeps that many instances in
`Standby` (with their replacements already up) after each wave instead of
draining them. The next wave then surges only enough stale instances to refill
the Standby pool to `--batch-max-surge`, and drains the rest — so there is
always one (or more) extra uncordoned replacement node ahead of the nodes being
drained.

Example for a 46-instance ASG with `--batch-max-surge=4`, `--batch-size=3`,
`--batch-standby-buffer=1`:

1. Surge 4 into Standby, wait for 4 replacements.
2. Cordon all outgoing nodes.
3. Drain, verify clean, and terminate 3; leave 1 in Standby (its replacement is the headroom).
4. Surge 3 more (pool back to 4), drain, verify, terminate 3, leave 1 buffered — repeat.
5. When no stale instances remain, drain, verify, and terminate the final buffered instance(s).

Requires `--batch-max-surge` to be greater than `--batch-standby-buffer`.

## How AMI detection works

The controller does not watch for AMI release events. On each reconcile it
compares **two AMI IDs**:

| | Source |
|---|--------|
| **Target AMI** | Launch template / launch configuration on the ASG, **or** `--ami-id-override` / `AMI_ID_OVERRIDE` when set |
| **Actual AMI** | Each `InService` instance's AMI (from EC2 `DescribeInstances`) |

If **actual ≠ target**, that instance is stale and gets rolled (serially or in a
batch, depending on the mode). If every `InService` instance already matches the
target, it does nothing.

Updating the launch template (or the value behind an SSM alias) is what triggers
a roll — the controller simply notices the ID mismatch on the next poll.

Alternatively, set **`--ami-id-override`** / **`AMI_ID_OVERRIDE`** to pin a
specific `ami-...` as the target for all managed ASGs (skips launch-template
resolution). The ASG launch template must still launch **new** instances with that
same AMI ID, or surge replacements will never match the override and the roll
will not complete.

**Pin as a safety guard.** A common workflow is: (1) bump the launch template to
the new AMI and let the controller roll onto it, then (2) pin
`--ami-id-override` to that same AMI. Once pinned, an *accidental* later change
to the launch template is ignored — the controller's target stays the pinned
AMI, so nodes already on it are left alone. To make this robust, the controller
verifies the pin against the launch template each reconcile: if
**`--require-launch-template-match`** is enabled (the default when a pin is set)
and the launch template resolves to a **different** AMI than the pin, the
controller **refuses to roll** and logs a warning + reconcile error (rather than
churning). Set `--require-launch-template-match=false` to disable this check.

### Resolving the target AMI

The launch template `imageId` can be set in two common ways:

**Direct AMI ID** (typical for custom AMIs you build):

```text
imageId: ami-0abc123def4567890
```

The controller uses that ID as-is. When you publish a new custom AMI, update the
launch template to the new `ami-...` and the controller rolls instances still on
the old ID.

**SSM alias** (typical for official EKS optimized AMIs):

```text
imageId: resolve:ssm:/aws/service/eks/optimized-ami/1.34/amazon-linux-2023/x86_64/standard/recommended/image_id
```

- **`resolve:ssm:`** — AWS/EKS convention meaning “look up the AMI ID from SSM
  Parameter Store, not a literal string.”
- **SSM** — **AWS Systems Manager**; specifically **Parameter Store**, a
  key/value config service.
- The path after the prefix is the parameter name; its **value** is the current
  `ami-...` (AWS updates this when a new recommended EKS AMI is published).

The controller strips `resolve:ssm:`, calls `ssm:GetParameter`, and uses the
returned AMI ID as the target. IAM must allow `ssm:GetParameter` on those paths
(see [`deploy/iam-policy.json`](deploy/iam-policy.json)).

You can also point `resolve:ssm:` at your own parameter (e.g.
`/my-company/eks-node-ami/latest`) if you maintain a custom AMI ID there —
same lookup mechanism.

## Notes & safety

- **Node delete is gated on a clean drain** — before removing the Node object,
  the controller lists pods on the node and refuses to proceed if any
  non-DaemonSet workloads remain. Termination only happens after delete succeeds.
- **Replacements are always proven usable before anything is drained** — the
  controller waits for them to be `InService`/`Healthy` at the ASG level *and*
  for their Nodes to report `Ready` and schedulable. A failed drain or unhealthy
  replacement halts the roll rather than pressing on.
- A halted roll is **retried on the next poll**, picking up from the live ASG
  state. Cordon is idempotent and draining an already-drained node is a no-op, so
  retries are safe.
- Cleanup (resume `AZRebalance`) runs even if the process is cancelled mid-roll.
  **`MaxSize` is only restored once the roll has settled** (no instances left in
  `Standby`), so a failed or interrupted roll keeps surge headroom for the next
  pass.
- Because it drains via the Kube API directly, it does **not** require
  aws-node-termination-handler or ASG lifecycle hooks.

## Configuration

### Required

The controller **refuses to start** unless `--asg-names` / `ASG_NAMES` is set
(enforced in `config.validate()`):

| Option | Flag / env | Example |
|--------|------------|---------|
| **ASG names** | `--asg-names` or `ASG_NAMES` | `ASG_NAMES=eks-nodes-dev,eks-nodes-prod` |

Everything else in the table below is **optional** and has a default. The only
other validated setting is `--poll-interval` / `POLL_INTERVAL`, which must be
positive (default `60s` satisfies this).

**Recommended in production** (not enforced by the binary, but required for a
working in-cluster deploy):

| Setting | Where | Why |
|---------|--------|-----|
| `AWS_REGION` / `--region` | Deployment env or args | Avoid ambiguous region; must match the ASG |
| `POD_NAMESPACE` | Deployment env (downward API) | Leader-election lease namespace; set in [`deploy/deployment.yaml`](deploy/deployment.yaml) |
| Pod Identity association | EKS `PodIdentityAssociation` binding the IAM role to this ServiceAccount + namespace | AWS API access |
| Container image | Deployment | Which controller version to run |

By default the target AMI comes from the ASG launch template in AWS (see
[How AMI detection works](#how-ami-detection-works)). Optionally pin it with
`--ami-id-override` / `AMI_ID_OVERRIDE` instead.

### All flags

Flags (each has an env fallback, shown in parentheses):

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--asg-names` | `ASG_NAMES` | – | Comma-separated ASG names to manage (**required**). |
| `--ami-id-override` | `AMI_ID_OVERRIDE` | – | Pin target AMI ID (`ami-...`); skips launch-template/SSM resolution. Launch template must still launch this AMI. |
| `--require-launch-template-match` | `REQUIRE_LAUNCH_TEMPLATE_MATCH` | `true` | With a pin set, refuse to roll (warn + error) if the launch template resolves to a different AMI. Safety guard against accidental launch-template changes. |
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
| `--batch-mode` | `BATCH_MODE` | `false` | Roll in waves instead of one node at a time. See [Roll strategies](#roll-strategies). |
| `--batch-size` | `BATCH_SIZE` | `5` | Batch mode: nodes drained and terminated together. |
| `--batch-max-surge` | `BATCH_MAX_SURGE` | `10` | Batch mode: max instances moved into `Standby` at once (`0` = every stale instance in one wave). |
| `--batch-standby-buffer` | `BATCH_STANDBY_BUFFER` | `0` | Batch mode: instances to keep in `Standby` after each wave as schedulable headroom (`0` = drain all surged instances each wave). Requires `--batch-max-surge` > buffer. |
| `--leader-elect` | `LEADER_ELECT` | `true` | Leader election (safe with >1 replica). If more than one replica for HA only one pod will run controller loop to avoid clash |

## AWS permissions (IRSA)

Pre req your need to create an IAM role for the controller.

Attach the policy in [`deploy/iam-policy.json`](deploy/iam-policy.json) to the
IAM role referenced by the ServiceAccount's `eks.amazonaws.com/role-arn`
annotation. It grants read-only describes plus the ASG/EC2 mutations the roll
needs. Scope the `Resource`/conditions to your bootstrap ASGs in production.

## Kubernetes permissions

See [`deploy/rbac.yaml`](deploy/rbac.yaml): read/patch/delete nodes, list pods,
create evictions, read workload controllers, and manage a leader-election lease.

## Build & deploy

### Build and push the image to Amazon (ECR)

First set the tag/image and log in to ECR:

```bash
export TAG=v0.2.0
export IMAGE=ACCOUNT_ID.dkr.ecr.REGION.amazonaws.com/images/asg-ami-rotater:$TAG

aws ecr get-login-password --region REGION \
  | docker login --username AWS --password-stdin ACCOUNT_ID.dkr.ecr.REGION.amazonaws.com
```

EKS nodes are typically `amd64` — swap `linux/amd64` for `linux/arm64` on
Graviton nodes.

#### Recommended: compile on the host, then package the binary

The dependency tree (`client-go` + `kubectl` + `controller-runtime` + AWS SDK)
is large and its Go linker needs several GB of RAM. Compiling it **inside** a
resource-limited BuildKit VM (e.g. Rancher/Docker Desktop, especially when a
local `kind` cluster is also running) can thrash and appear to hang. Compiling
on the host is fast and avoids that. [`Dockerfile.prebuilt`](Dockerfile.prebuilt)
just copies the binary into a `distroless` image.

```bash
# 1) Cross-compile for the cluster's arch on the host
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" \
  -o controller ./cmd/controller

# 2) Package the binary (loads the image into the local Docker engine)
docker buildx build --platform linux/amd64 -f Dockerfile.prebuilt -t "$IMAGE" --load .

# 3) Push
docker push "$IMAGE"
```

The `controller` binary is gitignored, so it is never committed.

#### Alternative: build everything inside Docker

Uses the multi-stage [`Dockerfile`](Dockerfile) (with build-cache mounts).
Simpler, but the first cold build can take several minutes and needs a BuildKit
VM with enough memory (bump Docker/Rancher Desktop to 6–8 GB).

```bash
# 1) Build (loads the image into the local Docker engine)
docker buildx build --platform linux/amd64 -t "$IMAGE" --load .

# 2) Push
docker push "$IMAGE"
```

### Deploy

```bash
make tidy          # resolve go.sum
make build         # build the binary locally (optional)
# edit deploy/*.yaml: image, ASG_NAMES, AWS_REGION, IRSA role ARN
# apply k8s manifests
make deploy
```

Or roll out a new tag against a running deployment:

```bash
kubectl -n <namespace> set image deployment/asg-ami-rotator controller="$IMAGE"
```