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
4. Prepare the group: temporarily **raise `MaxSize`** to allow the +1 surge and
   **suspend `AZRebalance`**.
5. For each stale instance, **one at a time**:
   1. `EnterStandby` (without decrementing desired) → the ASG launches a
      replacement on the new AMI.
   2. **Wait for the replacement to be usable** before touching the old node:
      first until it is `InService`/`Healthy` at the ASG level, then until its
      Kubernetes Node has joined the cluster and reports `Ready` (and is
      schedulable). This guarantees evicted pods have somewhere to land.
   3. **Cordon and drain** the backing Kubernetes node (respects
      PodDisruptionBudgets; ignores DaemonSets).
   4. **Delete the Node** object.
   5. **Terminate** the old instance through the ASG with
      `TerminateInstanceInAutoScalingGroup` **without decrementing desired**, so
      the healthy replacement is not scaled back down.
6. Restore `MaxSize` and resume `AZRebalance` (safe, because every instance is
   fully terminated before this runs).

Instance → Node mapping is done via the node's `spec.providerID`
(`aws:///<az>/<instance-id>`).

## How AMI detection works

The controller does not watch for AMI release events. On each reconcile it
compares **two AMI IDs**:

| | Source |
|---|--------|
| **Target AMI** | Launch template / launch configuration on the ASG, **or** `--ami-id-override` / `AMI_ID_OVERRIDE` when set |
| **Actual AMI** | Each `InService` instance's AMI (from EC2 `DescribeInstances`) |

If **actual ≠ target**, that instance is stale and gets rolled one at a time.
If every `InService` instance already matches the target, it does nothing.

Updating the launch template (or the value behind an SSM alias) is what triggers
a roll — the controller simply notices the ID mismatch on the next poll.

Alternatively, set **`--ami-id-override`** / **`AMI_ID_OVERRIDE`** to pin a
specific `ami-...` as the target for all managed ASGs (skips launch-template
resolution). The ASG launch template must still launch **new** instances with that
same AMI ID, or surge replacements will never match the override and the roll
will not complete.

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

- **One instance at a time**, and each step waits for the ASG to be healthy
  before proceeding — a failed drain or unhealthy replacement halts the roll.
- Cleanup (restore `MaxSize`, resume `AZRebalance`) runs even if the process is
  cancelled mid-roll.
- Because it drains via the Kube API directly, it does **not** require
  aws-node-termination-handler or ASG lifecycle hooks.

## Configuration

### Required

The controller **refuses to start** unless you provide **one** of these ASG
selection options (enforced in `config.validate()`):

| Option | Flag / env | Example |
|--------|------------|---------|
| **Explicit ASG names** | `--asg-names` or `ASG_NAMES` | `ASG_NAMES=eks-nodes-dev,eks-nodes-prod` |
| **Tag discovery** | `--asg-tag-key` + `--asg-tag-value` (or `ASG_TAG_KEY` + `ASG_TAG_VALUE`) | `ASG_TAG_KEY=ami-rotator` and `ASG_TAG_VALUE=managed` |

You must supply **either** a non-empty ASG name list **or** **both** tag key and
value. Tag discovery is only used when `--asg-names` / `ASG_NAMES` is empty.

Everything else in the table below is **optional** and has a default. The only
other validated setting is `--poll-interval` / `POLL_INTERVAL`, which must be
positive (default `60s` satisfies this).

**Recommended in production** (not enforced by the binary, but required for a
working in-cluster deploy):

| Setting | Where | Why |
|---------|--------|-----|
| `AWS_REGION` / `--region` | Deployment env or args | Avoid ambiguous region with IRSA; must match the ASG |
| `POD_NAMESPACE` | Deployment env (downward API) | Leader-election lease namespace; set in [`deploy/deployment.yaml`](deploy/deployment.yaml) |
| IRSA role ARN | ServiceAccount annotation in [`deploy/rbac.yaml`](deploy/rbac.yaml) | AWS API access |
| Container image | Deployment | Which controller version to run |

By default the target AMI comes from the ASG launch template in AWS (see
[How AMI detection works](#how-ami-detection-works)). Optionally pin it with
`--ami-id-override` / `AMI_ID_OVERRIDE` instead.

### All flags

Flags (each has an env fallback, shown in parentheses):

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--asg-names` | `ASG_NAMES` | – | Comma-separated ASG names to manage. **Required** unless using tag discovery. |
| `--asg-tag-key` / `--asg-tag-value` | `ASG_TAG_KEY` / `ASG_TAG_VALUE` | – | Discover ASGs by tag when `--asg-names` is empty. **Both required** if used instead of names. |
| `--ami-id-override` | `AMI_ID_OVERRIDE` | – | Pin target AMI ID (`ami-...`); skips launch-template/SSM resolution. Launch template must still launch this AMI. |
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