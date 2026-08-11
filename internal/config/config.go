// Package config holds runtime configuration for the asg-ami-rotator controller.
package config

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config is the fully-resolved controller configuration.
type Config struct {
	// ASGNames is the explicit list of Auto Scaling Groups to manage.
	ASGNames []string

	// Region is the AWS region. Empty means the SDK default chain decides.
	Region string

	// AMIIDOverride, when set, is the target AMI ID to roll onto instead of
	// resolving from the ASG launch template. The launch template must still
	// launch new instances with this AMI or replacements will not become healthy
	// relative to the override.
	AMIIDOverride string
	// RequireLaunchTemplateMatch, when a pin (AMIIDOverride) is set, refuses to
	// roll if the ASG launch template resolves to a different AMI than the pin.
	// This turns the pin into a safety guard: it stops the controller acting on
	// an accidental launch-template change (and avoids churning against surge
	// replacements the ASG would launch on the wrong AMI). Ignored when no pin
	// is set.
	RequireLaunchTemplateMatch bool

	// PollInterval is how often the reconcile loop runs.
	PollInterval time.Duration
	// StabilizeTimeout bounds how long we wait for the ASG to become healthy.
	StabilizeTimeout time.Duration
	// StabilizePoll is the poll cadence while waiting for the ASG to stabilize.
	StabilizePoll time.Duration

	// Drain settings.
	DrainTimeout        time.Duration
	DrainGracePeriod    int
	DrainForce          bool
	IgnoreAllDaemonSets bool
	DeleteEmptyDirData  bool

	// SuspendAZRebalance suspends the AZRebalance process during a roll.
	SuspendAZRebalance bool
	// ManageMaxSize temporarily raises MaxSize to allow the surge instance.
	ManageMaxSize bool

	// BatchMode selects the batched roll strategy instead of one node at a time:
	// a wave of stale instances is surged together, then drained and terminated
	// in batches. Off by default so it must be opted into per environment.
	BatchMode bool
	// BatchSize is how many Standby instances a batched roll drains and
	// terminates together.
	BatchSize int
	// BatchMaxSurge caps how many instances a batched roll moves into Standby at
	// once, bounding the extra capacity the ASG launches. 0 means no cap (every
	// stale instance in a single wave).
	BatchMaxSurge int
	// BatchTerminateLast, when batch mode is on, deletes nodes and terminates
	// Standby instances once after every wave has been drained. When false,
	// each wave is terminated before the next wave surges (lower peak MaxSize).
	BatchTerminateLast bool

	// Leader election / probes.
	EnableLeaderElection bool
	LeaderElectionID     string
	HealthProbeAddr      string
	MetricsAddr          string
}

// Load parses flags (with env fallbacks) into a Config.
func Load(args []string) (*Config, error) {
	fs := flag.NewFlagSet("asg-ami-rotator", flag.ContinueOnError)

	var asgNames string
	c := &Config{}

	fs.StringVar(&asgNames, "asg-names", env("ASG_NAMES", ""),
		"Comma-separated list of Auto Scaling Group names to manage (required).")
	fs.StringVar(&c.Region, "region", env("AWS_REGION", ""),
		"AWS region (defaults to the SDK credential/region chain).")

	fs.StringVar(&c.AMIIDOverride, "ami-id-override", env("AMI_ID_OVERRIDE", ""),
		"Target AMI ID to roll onto (ami-...). When set, skips launch-template/SSM resolution.")
	fs.BoolVar(&c.RequireLaunchTemplateMatch, "require-launch-template-match", envBool("REQUIRE_LAUNCH_TEMPLATE_MATCH", true),
		"When --ami-id-override is set, refuse to roll if the ASG launch template resolves to a different AMI (safety guard against accidental launch-template changes).")

	fs.DurationVar(&c.PollInterval, "poll-interval", envDuration("POLL_INTERVAL", 60*time.Second),
		"How often to reconcile the managed ASGs.")
	fs.DurationVar(&c.StabilizeTimeout, "stabilize-timeout", envDuration("STABILIZE_TIMEOUT", 20*time.Minute),
		"Max time to wait for an ASG to become healthy after a change.")
	fs.DurationVar(&c.StabilizePoll, "stabilize-poll", envDuration("STABILIZE_POLL", 15*time.Second),
		"Poll cadence while waiting for the ASG to stabilize.")

	fs.DurationVar(&c.DrainTimeout, "drain-timeout", envDuration("DRAIN_TIMEOUT", 10*time.Minute),
		"Max time to wait for a node to drain.")
	fs.IntVar(&c.DrainGracePeriod, "drain-grace-period", envInt("DRAIN_GRACE_PERIOD", -1),
		"Pod termination grace period during drain (-1 uses the pod's own value).")
	fs.BoolVar(&c.DrainForce, "drain-force", envBool("DRAIN_FORCE", true),
		"Drain pods not managed by a controller (bare/standalone pods).")
	fs.BoolVar(&c.IgnoreAllDaemonSets, "ignore-daemonsets", envBool("IGNORE_DAEMONSETS", true),
		"Ignore DaemonSet-managed pods during drain.")
	fs.BoolVar(&c.DeleteEmptyDirData, "delete-emptydir-data", envBool("DELETE_EMPTYDIR_DATA", true),
		"Allow eviction of pods using emptyDir volumes.")

	fs.BoolVar(&c.SuspendAZRebalance, "suspend-azrebalance", envBool("SUSPEND_AZREBALANCE", true),
		"Suspend the AZRebalance process during a roll.")
	fs.BoolVar(&c.ManageMaxSize, "manage-max-size", envBool("MANAGE_MAX_SIZE", true),
		"Temporarily raise MaxSize to allow the surge instance during a roll.")

	fs.BoolVar(&c.BatchMode, "batch-mode", envBool("BATCH_MODE", false),
		"Roll a wave of instances at once (surge, cordon, then drain/terminate in batches) instead of one node at a time.")
	fs.IntVar(&c.BatchSize, "batch-size", envInt("BATCH_SIZE", 5),
		"Batch mode: how many Standby instances to drain and terminate together.")
	fs.IntVar(&c.BatchMaxSurge, "batch-max-surge", envInt("BATCH_MAX_SURGE", 10),
		"Batch mode: max instances moved into Standby at once (0 = every stale instance in one wave).")
	fs.BoolVar(&c.BatchTerminateLast, "batch-terminate-last", envBool("BATCH_TERMINATE_LAST", false),
		"Batch mode: delete nodes and terminate instances once after all waves are drained (requires MaxSize headroom for the full roll). When false, each wave is terminated before the next surges.")

	fs.BoolVar(&c.EnableLeaderElection, "leader-elect", envBool("LEADER_ELECT", true),
		"Enable leader election so only one replica acts at a time.")
	fs.StringVar(&c.LeaderElectionID, "leader-election-id", env("LEADER_ELECTION_ID", "asg-ami-rotator"),
		"Leader election lock name.")
	fs.StringVar(&c.HealthProbeAddr, "health-probe-addr", env("HEALTH_PROBE_ADDR", ":8081"),
		"Address for health/readiness probes.")
	fs.StringVar(&c.MetricsAddr, "metrics-addr", env("METRICS_ADDR", ":8080"),
		"Address for the metrics endpoint (\"0\" disables).")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	for _, n := range strings.Split(asgNames, ",") {
		if s := strings.TrimSpace(n); s != "" {
			c.ASGNames = append(c.ASGNames, s)
		}
	}

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	if len(c.ASGNames) == 0 {
		return fmt.Errorf("--asg-names must be set")
	}
	if c.PollInterval <= 0 {
		return fmt.Errorf("--poll-interval must be positive")
	}
	if c.BatchMode {
		if c.BatchSize < 1 {
			return fmt.Errorf("--batch-size must be at least 1 when --batch-mode is set")
		}
		if c.BatchMaxSurge < 0 {
			return fmt.Errorf("--batch-max-surge cannot be negative")
		}
	}
	if o := strings.TrimSpace(c.AMIIDOverride); o != "" {
		if !strings.HasPrefix(o, "ami-") {
			return fmt.Errorf("--ami-id-override must be a valid AMI ID (ami-...)")
		}
		c.AMIIDOverride = o
	}
	return nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return def
	}
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n); err != nil {
		return def
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return d
}
