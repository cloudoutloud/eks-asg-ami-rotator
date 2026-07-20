// Package awsclient wraps the AWS Auto Scaling, EC2 and SSM APIs used by the rotator.
package awsclient

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	asgtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// Client bundles the AWS service clients the rotator needs.
type Client struct {
	asg *autoscaling.Client
	ec2 *ec2.Client
	ssm *ssm.Client
}

// Instance is a compact view of an ASG member instance.
type Instance struct {
	ID             string
	LifecycleState string
	HealthStatus   string
	AMI            string // populated via EC2 DescribeInstances
}

// GroupState is a snapshot of an ASG relevant to a roll.
type GroupState struct {
	Name            string
	DesiredCapacity int32
	MinSize         int32
	MaxSize         int32
	Instances       []Instance
}

// New builds a Client using the default AWS credential/region chain.
func New(ctx context.Context, region string) (*Client, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return &Client{
		asg: autoscaling.NewFromConfig(cfg),
		ec2: ec2.NewFromConfig(cfg),
		ssm: ssm.NewFromConfig(cfg),
	}, nil
}

// DiscoverByTag returns ASG names carrying the given tag key/value.
func (c *Client) DiscoverByTag(ctx context.Context, key, value string) ([]string, error) {
	var names []string
	p := autoscaling.NewDescribeAutoScalingGroupsPaginator(c.asg, &autoscaling.DescribeAutoScalingGroupsInput{
		Filters: []asgtypes.Filter{
			{Name: aws.String("tag-key"), Values: []string{key}},
			{Name: aws.String("tag-value"), Values: []string{value}},
		},
	})
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("discover asgs by tag: %w", err)
		}
		for _, g := range out.AutoScalingGroups {
			names = append(names, aws.ToString(g.AutoScalingGroupName))
		}
	}
	return names, nil
}

// DescribeGroup returns a snapshot of the named ASG.
func (c *Client) DescribeGroup(ctx context.Context, name string) (*GroupState, error) {
	out, err := c.asg.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{name},
	})
	if err != nil {
		return nil, fmt.Errorf("describe asg %q: %w", name, err)
	}
	if len(out.AutoScalingGroups) == 0 {
		return nil, fmt.Errorf("auto scaling group %q not found", name)
	}
	g := out.AutoScalingGroups[0]
	gs := &GroupState{
		Name:            aws.ToString(g.AutoScalingGroupName),
		DesiredCapacity: aws.ToInt32(g.DesiredCapacity),
		MinSize:         aws.ToInt32(g.MinSize),
		MaxSize:         aws.ToInt32(g.MaxSize),
	}
	for _, in := range g.Instances {
		gs.Instances = append(gs.Instances, Instance{
			ID:             aws.ToString(in.InstanceId),
			LifecycleState: string(in.LifecycleState),
			HealthStatus:   aws.ToString(in.HealthStatus),
		})
	}
	return gs, nil
}

// ResolveTargetAMI resolves the AMI the ASG would launch new instances with,
// following LaunchTemplate, MixedInstancesPolicy, or LaunchConfiguration, and
// dereferencing "resolve:ssm:" AMI aliases. Mirrors resolve_target_ami() in the
// bash scripts.
func (c *Client) ResolveTargetAMI(ctx context.Context, name string) (string, error) {
	out, err := c.asg.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{name},
	})
	if err != nil {
		return "", fmt.Errorf("describe asg %q: %w", name, err)
	}
	if len(out.AutoScalingGroups) == 0 {
		return "", fmt.Errorf("auto scaling group %q not found", name)
	}
	g := out.AutoScalingGroups[0]

	ltID, ltName, ltVer := launchTemplateRef(g)

	var imageID string
	switch {
	case ltID != "" || ltName != "":
		in := &ec2.DescribeLaunchTemplateVersionsInput{Versions: []string{ltVer}}
		if ltID != "" {
			in.LaunchTemplateId = aws.String(ltID)
		} else {
			in.LaunchTemplateName = aws.String(ltName)
		}
		lt, err := c.ec2.DescribeLaunchTemplateVersions(ctx, in)
		if err != nil {
			return "", fmt.Errorf("describe launch template versions: %w", err)
		}
		if len(lt.LaunchTemplateVersions) == 0 || lt.LaunchTemplateVersions[0].LaunchTemplateData == nil {
			return "", fmt.Errorf("no launch template version data for asg %q", name)
		}
		imageID = aws.ToString(lt.LaunchTemplateVersions[0].LaunchTemplateData.ImageId)
	case g.LaunchConfigurationName != nil:
		lc, err := c.asg.DescribeLaunchConfigurations(ctx, &autoscaling.DescribeLaunchConfigurationsInput{
			LaunchConfigurationNames: []string{aws.ToString(g.LaunchConfigurationName)},
		})
		if err != nil {
			return "", fmt.Errorf("describe launch configuration: %w", err)
		}
		if len(lc.LaunchConfigurations) == 0 {
			return "", fmt.Errorf("launch configuration for asg %q not found", name)
		}
		imageID = aws.ToString(lc.LaunchConfigurations[0].ImageId)
	default:
		return "", fmt.Errorf("could not determine a launch template or launch configuration for asg %q", name)
	}

	if strings.HasPrefix(imageID, "resolve:ssm:") {
		path := strings.TrimPrefix(imageID, "resolve:ssm:")
		p, err := c.ssm.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(path)})
		if err != nil {
			return "", fmt.Errorf("resolve ssm ami %q: %w", path, err)
		}
		imageID = aws.ToString(p.Parameter.Value)
	}

	if imageID == "" {
		return "", fmt.Errorf("failed to resolve target AMI for asg %q", name)
	}
	return imageID, nil
}

func launchTemplateRef(g asgtypes.AutoScalingGroup) (id, name, version string) {
	version = "$Default"
	var spec *asgtypes.LaunchTemplateSpecification
	switch {
	case g.LaunchTemplate != nil:
		spec = g.LaunchTemplate
	case g.MixedInstancesPolicy != nil &&
		g.MixedInstancesPolicy.LaunchTemplate != nil &&
		g.MixedInstancesPolicy.LaunchTemplate.LaunchTemplateSpecification != nil:
		spec = g.MixedInstancesPolicy.LaunchTemplate.LaunchTemplateSpecification
	}
	if spec == nil {
		return "", "", version
	}
	id = aws.ToString(spec.LaunchTemplateId)
	name = aws.ToString(spec.LaunchTemplateName)
	if v := aws.ToString(spec.Version); v != "" {
		version = v
	}
	return id, name, version
}

// InstanceAMIs returns instance-id -> AMI for the given instances.
func (c *Client) InstanceAMIs(ctx context.Context, ids []string) (map[string]string, error) {
	result := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	p := ec2.NewDescribeInstancesPaginator(c.ec2, &ec2.DescribeInstancesInput{InstanceIds: ids})
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("describe instances: %w", err)
		}
		for _, r := range out.Reservations {
			for _, in := range r.Instances {
				result[aws.ToString(in.InstanceId)] = aws.ToString(in.ImageId)
			}
		}
	}
	return result, nil
}

// InServiceInstanceIDs returns the IDs of instances currently InService in the
// group. Used to snapshot the group before a surge so the newly launched
// replacement can be identified afterwards.
func (c *Client) InServiceInstanceIDs(ctx context.Context, name string) ([]string, error) {
	gs, err := c.DescribeGroup(ctx, name)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, in := range gs.Instances {
		if in.LifecycleState == "InService" {
			ids = append(ids, in.ID)
		}
	}
	return ids, nil
}

// WaitForNewInService blocks until a new InService+Healthy instance (one not in
// the provided known set) appears, returning its ID. This is how the rotator
// identifies the surge replacement the ASG launches after an instance enters
// Standby.
func (c *Client) WaitForNewInService(ctx context.Context, name string, known []string, timeout, poll time.Duration, log func(string, ...any)) (string, error) {
	knownSet := make(map[string]struct{}, len(known))
	for _, id := range known {
		knownSet[id] = struct{}{}
	}
	deadline := time.Now().Add(timeout)
	for {
		gs, err := c.DescribeGroup(ctx, name)
		if err != nil {
			return "", err
		}
		for _, in := range gs.Instances {
			if in.LifecycleState != "InService" || in.HealthStatus != "Healthy" {
				continue
			}
			if _, ok := knownSet[in.ID]; !ok {
				return in.ID, nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out after %s waiting for a replacement instance in asg %q", timeout, name)
		}
		if err := sleep(ctx, poll); err != nil {
			return "", err
		}
	}
}

// EnterStandby moves an instance to Standby without decrementing desired
// capacity, so the ASG launches a replacement on the current AMI.
func (c *Client) EnterStandby(ctx context.Context, asgName, instanceID string) error {
	_, err := c.asg.EnterStandby(ctx, &autoscaling.EnterStandbyInput{
		AutoScalingGroupName:           aws.String(asgName),
		InstanceIds:                    []string{instanceID},
		ShouldDecrementDesiredCapacity: aws.Bool(false),
	})
	if err != nil {
		return fmt.Errorf("enter-standby %s: %w", instanceID, err)
	}
	return nil
}

// TerminateInASG terminates an instance through the Auto Scaling group WITHOUT
// decrementing desired capacity. This is the correct way to remove a surged
// Standby instance: desired stays the same, so the ASG does not scale a healthy
// replacement back down, and because desired is already satisfied by the
// in-service instances no new replacement is launched. Mirrors the cleanup
// guidance in rotate-asg-standby.sh.
func (c *Client) TerminateInASG(ctx context.Context, asgName, instanceID string) error {
	_, err := c.asg.TerminateInstanceInAutoScalingGroup(ctx, &autoscaling.TerminateInstanceInAutoScalingGroupInput{
		InstanceId:                     aws.String(instanceID),
		ShouldDecrementDesiredCapacity: aws.Bool(false),
	})
	if err != nil {
		return fmt.Errorf("terminate-in-asg %s: %w", instanceID, err)
	}
	return nil
}

// SetMaxSize updates the ASG MaxSize.
func (c *Client) SetMaxSize(ctx context.Context, asgName string, maxSize int32) error {
	_, err := c.asg.UpdateAutoScalingGroup(ctx, &autoscaling.UpdateAutoScalingGroupInput{
		AutoScalingGroupName: aws.String(asgName),
		MaxSize:              aws.Int32(maxSize),
	})
	if err != nil {
		return fmt.Errorf("update max-size for %s: %w", asgName, err)
	}
	return nil
}

// SuspendAZRebalance suspends the AZRebalance process.
func (c *Client) SuspendAZRebalance(ctx context.Context, asgName string) error {
	_, err := c.asg.SuspendProcesses(ctx, &autoscaling.SuspendProcessesInput{
		AutoScalingGroupName: aws.String(asgName),
		ScalingProcesses:     []string{"AZRebalance"},
	})
	if err != nil {
		return fmt.Errorf("suspend AZRebalance for %s: %w", asgName, err)
	}
	return nil
}

// ResumeAZRebalance resumes the AZRebalance process.
func (c *Client) ResumeAZRebalance(ctx context.Context, asgName string) error {
	_, err := c.asg.ResumeProcesses(ctx, &autoscaling.ResumeProcessesInput{
		AutoScalingGroupName: aws.String(asgName),
		ScalingProcesses:     []string{"AZRebalance"},
	})
	if err != nil {
		return fmt.Errorf("resume AZRebalance for %s: %w", asgName, err)
	}
	return nil
}

// WaitForStable blocks until healthy+InService instances >= desired and no
// instance is in a transitional state, or the timeout elapses.
func (c *Client) WaitForStable(ctx context.Context, name string, timeout, poll time.Duration, log func(string, ...any)) error {
	deadline := time.Now().Add(timeout)
	for {
		gs, err := c.DescribeGroup(ctx, name)
		if err != nil {
			return err
		}
		var healthy, transitional int32
		for _, in := range gs.Instances {
			if in.LifecycleState == "InService" && in.HealthStatus == "Healthy" {
				healthy++
			}
			if isTransitional(in.LifecycleState) {
				transitional++
			}
		}
		if log != nil {
			log("asg %s: desired=%d healthy_inservice=%d transitional=%d", name, gs.DesiredCapacity, healthy, transitional)
		}
		if healthy >= gs.DesiredCapacity && transitional == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for asg %q to stabilize", timeout, name)
		}
		if err := sleep(ctx, poll); err != nil {
			return err
		}
	}
}

// WaitForInstanceState blocks until the instance reaches wantState (or "Gone"
// if it has left the ASG), or the timeout elapses.
func (c *Client) WaitForInstanceState(ctx context.Context, name, instanceID, wantState string, timeout, poll time.Duration, log func(string, ...any)) error {
	deadline := time.Now().Add(timeout)
	for {
		gs, err := c.DescribeGroup(ctx, name)
		if err != nil {
			return err
		}
		state := "Gone"
		for _, in := range gs.Instances {
			if in.ID == instanceID {
				state = in.LifecycleState
				break
			}
		}
		if log != nil {
			log("instance %s state=%s (want %s)", instanceID, state, wantState)
		}
		if state == wantState {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for instance %s to reach %s", instanceID, wantState)
		}
		if err := sleep(ctx, poll); err != nil {
			return err
		}
	}
}

func isTransitional(state string) bool {
	for _, p := range []string{"Pending", "Terminating", "EnteringStandby", "Detaching"} {
		if strings.HasPrefix(state, p) {
			return true
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
