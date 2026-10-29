// A toy: GitHub schedules jobs; Buildkite hosts the official ephemeral runners.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
)

type config struct {
	GitHubURL, Name, Org, Pipeline, Queue, Branch, Commit, StatePath string
	MaxRunners, MaxBuilds                                            int
	Lifetime                                                         time.Duration
	githubToken, writeToken, readToken                               string
}

func configuration() (config, error) {
	env := func(key, fallback string) string {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			return fallback
		}
		return v
	}
	c := config{GitHubURL: env("GITHUB_CONFIG_URL", ""), Name: env("SCALE_SET_NAME", "buildkite"),
		Org: env("BUILDKITE_ORG", ""), Pipeline: env("BUILDKITE_PIPELINE", ""), Queue: env("BUILDKITE_QUEUE", ""),
		Branch: env("BUILDKITE_BRANCH", "main"), Commit: env("BUILDKITE_COMMIT", "HEAD"), StatePath: env("STATE_PATH", ".state/state.json"),
		githubToken: env("GITHUB_TOKEN", ""), writeToken: env("BUILDKITE_API_TOKEN", "")}
	c.readToken = env("BUILDKITE_READ_TOKEN", c.writeToken)
	for key, value := range map[string]string{"GITHUB_CONFIG_URL": c.GitHubURL, "GITHUB_TOKEN": c.githubToken, "BUILDKITE_API_TOKEN": c.writeToken, "BUILDKITE_ORG": c.Org, "BUILDKITE_PIPELINE": c.Pipeline, "BUILDKITE_QUEUE": c.Queue} {
		if value == "" {
			return c, fmt.Errorf("%s is required", key)
		}
	}
	for _, value := range []string{c.Org, c.Pipeline, c.Queue, c.Name} {
		if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(value) {
			return c, fmt.Errorf("org/pipeline/queue/scale-set names must be simple slugs")
		}
	}
	var err error
	c.MaxRunners, err = strconv.Atoi(env("MAX_RUNNERS", "2"))
	if err != nil || c.MaxRunners < 1 || c.MaxRunners > 10 {
		return c, fmt.Errorf("MAX_RUNNERS must be 1–10")
	}
	c.MaxBuilds, err = strconv.Atoi(env("MAX_BUILDS", "10"))
	if err != nil || c.MaxBuilds < 1 || c.MaxBuilds > 100 {
		return c, fmt.Errorf("MAX_BUILDS must be 1–100, counted across restarts")
	}
	c.Lifetime, err = time.ParseDuration(env("LIFETIME", "20m"))
	if err != nil || c.Lifetime <= 0 || c.Lifetime > time.Hour {
		return c, fmt.Errorf("LIFETIME must be positive and at most 1h")
	}
	return c, nil
}
func (c config) target() string {
	return strings.Join([]string{c.GitHubURL, c.Name, c.Org, c.Pipeline, c.Queue, c.Branch, c.Commit}, "|")
}

type runner struct {
	Name                                          string
	Ref                                           *scaleset.RunnerReference
	Stage, SecretKey, SecretID                    string
	SecretDeleted                                 bool
	DemandAt, ProvisionStarted, ProvisionFinished time.Time
	Build                                         build
	Started                                       *scaleset.JobStarted
	Completed                                     *scaleset.JobCompleted
}
type state struct {
	Target     string
	ScaleSetID int
	Creating   bool
	ExpiresAt  time.Time
	Runners    []*runner
}
type toy struct {
	config   config
	state    state
	github   *scaleset.Client
	session  listener.Client
	listener *listener.Listener
	bk       *buildkite
}

func (t *toy) save() error {
	b, err := json.MarshalIndent(t.state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(t.config.StatePath+".tmp", b, 0600); err != nil {
		return err
	}
	return os.Rename(t.config.StatePath+".tmp", t.config.StatePath)
}
func (t *toy) active() int {
	n := 0
	for _, r := range t.state.Runners {
		if r.Completed == nil {
			n++
		}
	}
	return n
}
func (t *toy) cleanup(ctx context.Context) error {
	for _, r := range t.state.Runners {
		if r.Completed != nil && !r.SecretDeleted {
			if err := t.bk.deleteSecret(ctx, r); err != nil {
				return err
			}
			r.SecretDeleted = true
			if err := t.save(); err != nil {
				return err
			}
			fmt.Printf("Secret removed for %s\n", r.Name)
		}
	}
	return nil
}
func (t *toy) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	received := time.Now().UTC()
	if msg == nil {
		return nil
	}
	for _, r := range t.state.Runners {
		if r.Stage != "running" {
			return fmt.Errorf("uncertain %s for %s; inspect before restart", r.Stage, r.Name)
		}
	}
	if len(msg.JobAvailableMessages) > 0 {
		ids := []int64{}
		for _, j := range msg.JobAvailableMessages {
			ids = append(ids, j.RunnerRequestID)
		}
		if _, err := t.session.AcquireJobs(ctx, ids); err != nil {
			return fmt.Errorf("GitHub job acquisition failed")
		}
	}
	for _, r := range t.state.Runners {
		for _, j := range msg.JobStartedMessages {
			if j.RunnerName == r.Name {
				r.Started = j
			}
		}
		for _, j := range msg.JobCompletedMessages {
			if j.RunnerName == r.Name {
				r.Completed = j
			}
		}
	}
	if err := t.save(); err != nil {
		return err
	}
	if err := t.cleanup(ctx); err != nil {
		return err
	}
	if msg.Statistics == nil {
		return nil
	}
	fmt.Printf("%s message=%d assigned=%d active=%d\n", received.Format(time.RFC3339Nano), msg.MessageID, msg.Statistics.TotalAssignedJobs, t.active())
	for t.active() < min(t.config.MaxRunners, msg.Statistics.TotalAssignedJobs) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(t.state.Runners) >= t.config.MaxBuilds {
			return fmt.Errorf("total build budget exhausted")
		}
		id := uuid.NewString()
		r := &runner{Name: t.config.Name + "-" + id, Stage: "creating-jit", SecretKey: "gha_jit_" + strings.ReplaceAll(id, "-", ""), DemandAt: received, ProvisionStarted: time.Now().UTC()}
		t.state.Runners = append(t.state.Runners, r)
		if err := t.save(); err != nil {
			return err
		} // Durable claim before any remote mutation.
		jit, err := t.github.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: r.Name, WorkFolder: "_work"}, t.state.ScaleSetID)
		if err != nil {
			return fmt.Errorf("JIT creation failed; inspect reserved runner")
		}
		r.Ref = jit.Runner
		r.Stage = "creating-secret"
		if err := t.save(); err != nil {
			return err
		}
		r.SecretID, err = t.bk.createSecret(ctx, r.SecretKey, jit.EncodedJITConfig)
		if err != nil {
			return err
		}
		r.Stage = "creating-build"
		if err := t.save(); err != nil {
			return err
		}
		r.Build, err = t.bk.createBuild(ctx, r)
		if err != nil {
			return err
		}
		r.Stage = "running"
		r.ProvisionFinished = time.Now().UTC()
		if err := t.save(); err != nil {
			return err
		}
		fmt.Printf("Runner %s → %s\n", r.Name, r.Build.URL)
	}
	t.listener.SetMaxRunners(min(t.config.MaxRunners, t.active()+t.config.MaxBuilds-len(t.state.Runners)))
	return nil
}
func run() error {
	c, err := configuration()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.StatePath), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(c.StatePath+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("this state file is already in use")
	}
	t := &toy{config: c}
	raw, err := os.ReadFile(c.StatePath)
	if err == nil {
		if err := json.Unmarshal(raw, &t.state); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if t.state.Target != "" && t.state.Target != c.target() {
		return fmt.Errorf("state belongs to a different configuration; use a separate STATE_PATH")
	}
	t.state.Target = c.target()
	if len(t.state.Runners) > c.MaxBuilds {
		return fmt.Errorf("MAX_BUILDS is below the saved number of attempts")
	}
	if t.state.Creating {
		return fmt.Errorf("scale-set creation was uncertain; inspect before retrying")
	}
	for _, r := range t.state.Runners {
		if r.Stage != "running" {
			return fmt.Errorf("uncertain %s; inspect before restarting", r.Stage)
		}
	}
	if t.state.ExpiresAt.IsZero() {
		t.state.ExpiresAt = time.Now().UTC().Add(c.Lifetime)
	}
	if !time.Now().Before(t.state.ExpiresAt) {
		return fmt.Errorf("saved deadline expired; inspect then explicitly clear ExpiresAt to start a new bounded session")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithDeadline(ctx, t.state.ExpiresAt)
	defer cancel()
	t.bk = &buildkite{config: c, base: "https://api.buildkite.com/v2", http: &http.Client{Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if err := t.bk.preflight(ctx); err != nil {
		return err
	}
	if err := t.cleanup(ctx); err != nil {
		return err
	}
	for _, r := range t.state.Runners {
		if r.Completed == nil {
			b, err := t.bk.getBuild(ctx, r.Build.Number)
			if err != nil {
				return err
			}
			if b.State != "scheduled" && b.State != "running" {
				return fmt.Errorf("hosted build finished without recorded native completion; inspect, do not replace")
			}
		}
	}
	t.github, err = scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL: c.GitHubURL, PersonalAccessToken: c.githubToken}, scaleset.WithRetry(scaleset.RetryConfig{}))
	if err != nil {
		return fmt.Errorf("invalid GitHub configuration")
	}
	if t.state.ScaleSetID == 0 {
		ss, err := t.github.GetRunnerScaleSet(ctx, 1, c.Name)
		if err != nil {
			return fmt.Errorf("scale-set lookup failed")
		}
		if ss == nil {
			t.state.Creating = true
			if err := t.save(); err != nil {
				return err
			}
			ss, err = t.github.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{Name: c.Name, RunnerGroupID: 1,
				Labels: []scaleset.Label{{Name: c.Name, Type: "System"}}, RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true}})
			if err != nil {
				return fmt.Errorf("scale-set creation uncertain; inspect before retrying")
			}
		}
		t.state.ScaleSetID = ss.ID
		t.state.Creating = false
	}
	if err := t.save(); err != nil {
		return err
	}
	session, err := t.github.MessageSessionClient(ctx, t.state.ScaleSetID, "github-actions-buildkite-runner")
	if err != nil {
		return fmt.Errorf("message session failed; is another listener running?")
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if session.Close(closeCtx) != nil {
			fmt.Fprintln(os.Stderr, "Session close failed; inspect before restart")
		} else {
			fmt.Println("Session closed")
		}
	}()
	t.session = session
	t.listener, err = listener.New(session, listener.Config{ScaleSetID: t.state.ScaleSetID, MaxRunners: min(c.MaxRunners, t.active()+c.MaxBuilds-len(t.state.Runners))})
	if err != nil {
		return err
	}
	fmt.Printf("Listening: scope=%s scale_set=%s id=%d expires=%s\n", c.GitHubURL, c.Name, t.state.ScaleSetID, t.state.ExpiresAt.Format(time.RFC3339))
	err = t.listener.Run(ctx, t)
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("listener stopped; inspect state (SDK error details withheld)")
	}
	return nil
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
