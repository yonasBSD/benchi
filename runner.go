// Copyright © 2025 Meroxa, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package benchi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/conduitio/benchi/config"
	"github.com/conduitio/benchi/dockerutil"
	"github.com/docker/docker/client"
	"github.com/sourcegraph/conc/pool"
)

type RunOptions struct {
	// Dir is the working directory where the test is run. All relative paths
	// are resolved relative to this directory.
	OutPath      string
	FilterTests  []string
	DockerClient client.APIClient
}

func Run(ctx context.Context, cfg config.Config, opt RunOptions) error {
	// Create output directory if it does not exist.
	err := os.MkdirAll(opt.OutPath, 0o755)
	if err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	testRuns := buildTestRuns(cfg, opt)
	slog.Info("Identified tests", "count", len(testRuns))

	for i, tr := range testRuns {
		fmt.Println()
		err = tr.Run(ctx)
		if err != nil {
			return fmt.Errorf("failed to run test %d (%s): %w", i, tr.Tool, err)
		}
	}

	return nil
}

func buildTestRuns(cfg config.Config, opt RunOptions) []*testRun {
	now := time.Now()
	runs := make([]*testRun, 0, len(cfg.Tests)*len(cfg.Tools))

	for _, t := range cfg.Tests {
		// TODO filter
		infra := make([]config.ServiceConfig, 0, len(cfg.Infrastructure)+len(t.Infrastructure))
		for _, v := range cfg.Infrastructure {
			infra = append(infra, v)
		}
		for _, v := range t.Infrastructure {
			infra = append(infra, v)
		}

		metrics := make([]config.MetricsCollector, 0, len(cfg.Metrics)+len(t.Metrics))
		metrics = append(metrics, cfg.Metrics...)
		metrics = append(metrics, t.Metrics...)

		toolNames := slices.Collect(maps.Keys(cfg.Tools))
		for k := range t.Tools {
			if _, ok := cfg.Tools[k]; !ok {
				toolNames = append(toolNames, k)
			}
		}

		for _, tool := range toolNames {
			tools := make([]config.ServiceConfig, 0)
			if cfg.Tools != nil {
				toolCfg, ok := cfg.Tools[tool]
				if ok {
					tools = append(tools, toolCfg)
				}
			}
			if t.Tools != nil {
				toolCfg, ok := t.Tools[tool]
				if ok {
					tools = append(tools, toolCfg)
				}
			}

			runs = append(runs, &testRun{
				Infrastructure: infra,
				Tools:          tools,
				Metrics:        metrics,

				Name:     t.Name,
				Duration: t.Duration,
				Steps:    t.Steps,

				Tool:         tool,
				OutPath:      filepath.Join(opt.OutPath, fmt.Sprintf("%s_%s_%s", now.Format("20060102150405"), t.Name, tool)),
				DockerClient: opt.DockerClient,
			})
		}
	}

	return runs
}

// testRun is a single test run for a single tool.
type testRun struct {
	Infrastructure []config.ServiceConfig
	Tools          []config.ServiceConfig
	Metrics        []config.MetricsCollector

	Name     string
	Duration time.Duration
	Steps    config.TestSteps

	Tool         string // Tool is the name of the tool to run the test with
	OutPath      string // OutPath is the directory where the test results are stored
	DockerClient client.APIClient

	cleanupFns        []func(context.Context) error
	goroutinePool     *pool.ContextPool
	goroutinePoolDead *atomic.Bool
}

func (r *testRun) Run(ctx context.Context) (err error) {
	logger := slog.Default().With("name", r.Name, "tool", r.Tool)
	logger.Info("Running test", "output", r.OutPath)

	if _, err := os.Stat(r.OutPath); err == nil {
		return fmt.Errorf("output folder %q already exists", r.OutPath)
	}
	if err := os.MkdirAll(r.OutPath, 0o755); err != nil {
		return fmt.Errorf("failed to create output folder %q: %w", r.OutPath, err)
	}

	r.goroutinePool = pool.New().WithContext(ctx).WithCancelOnError()
	r.goroutinePoolDead = &atomic.Bool{}

	// Always run cleanup, regardless of errors
	defer func() {
		cleanupSteps := []func(context.Context) error{
			r.preCleanup,
			r.cleanup,
			r.postCleanup,
		}
		var errs []error
		for _, step := range cleanupSteps {
			if err := step(ctx); err != nil {
				// Store the error but continue with cleanup
				errs = append(errs, err)
			}
		}
		if err == nil {
			// return cleanup error
			err = errors.Join(errs...)
		} else if len(errs) > 0 {
			// log cleanup error
			slog.Error("cleanup failed", "error", errors.Join(errs...))
		}
	}()

	steps := []func(context.Context) error{
		r.preInfrastructure,
		r.infrastructure,
		r.postInfrastructure,
		r.preTool,
		r.tool,
		r.postTool,
		r.preTest,
		r.test,
		r.postTest,
	}

	// Run each step
	for _, step := range steps {
		if err := step(ctx); err != nil {
			logger.Error("Test stopped because of an error", "error", err)
			return err
		}
	}

	slog.Info("Test successful", "name", r.Name, "tool", r.Tool)
	return nil
}

// -- STEPS --------------------------------------------------------------------

func (r *testRun) preInfrastructure(ctx context.Context) (err error) {
	logger, lastLog := r.loggerForStep("pre-infrastructure")
	defer func() { lastLog(err) }()

	_ = logger

	return nil
}

func (r *testRun) infrastructure(ctx context.Context) (err error) {
	logger, lastLog := r.loggerForStep("infrastructure")
	defer func() { lastLog(err) }()

	paths := r.collectDockerComposeFiles(r.Infrastructure)
	if len(paths) == 0 {
		logger.Info("No infrastructure to start")
		return nil
	}

	logPath := filepath.Join(r.OutPath, "infrastructure.log")

	err = r.dockerComposeUpWait(ctx, logger, paths, logPath)
	if err != nil {
		return fmt.Errorf("failed to start infrastructure: %w", err)
	}

	logger.Info("Infrastructure started")
	return nil
}

func (r *testRun) postInfrastructure(ctx context.Context) (err error) {
	logger, lastLog := r.loggerForStep("post-infrastructure")
	defer func() { lastLog(err) }()

	_ = logger

	return nil
}

func (r *testRun) preTool(ctx context.Context) (err error) {
	logger, lastLog := r.loggerForStep("pre-tool")
	defer func() { lastLog(err) }()

	_ = logger

	return nil
}

func (r *testRun) tool(ctx context.Context) (err error) {
	logger, lastLog := r.loggerForStep("tool")
	defer func() { lastLog(err) }()

	_ = logger

	paths := r.collectDockerComposeFiles(r.Tools)
	if len(paths) == 0 {
		logger.Info("No tools to start")
		return nil
	}

	logPath := filepath.Join(r.OutPath, "tools.log")

	err = r.dockerComposeUpWait(ctx, logger, paths, logPath)
	if err != nil {
		return fmt.Errorf("failed to start tools: %w", err)
	}

	logger.Info("Tools started")
	return nil
}

func (r *testRun) postTool(ctx context.Context) (err error) {
	logger, lastLog := r.loggerForStep("post-tool")
	defer func() { lastLog(err) }()

	_ = logger

	return nil
}

func (r *testRun) preTest(ctx context.Context) (err error) {
	logger, lastLog := r.loggerForStep("pre-test")
	defer func() { lastLog(err) }()

	_ = logger

	return nil
}

func (r *testRun) test(ctx context.Context) (err error) {
	logger, lastLog := r.loggerForStep("test")
	defer func() { lastLog(err) }()

	const timeBetweenLogs = 5 * time.Second

	endTestAt := time.Now().Add(r.Duration + 500*time.Millisecond) // Add 500ms to account for time drift and nicer log output
	testCompleted := time.After(r.Duration)
	logTicker := time.NewTicker(timeBetweenLogs)
	defer logTicker.Stop()

	// TODO during

	for {
		logger.Info("Test in progress", "time-left", endTestAt.Sub(time.Now()).Truncate(time.Second))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-logTicker.C:
			continue
		case <-testCompleted:
		}
		break
	}

	return nil
}

func (r *testRun) postTest(ctx context.Context) (err error) {
	logger, lastLog := r.loggerForStep("post-test")
	defer func() { lastLog(err) }()

	_ = logger

	return nil
}

func (r *testRun) preCleanup(ctx context.Context) (err error) {
	logger, lastLog := r.loggerForStep("pre-cleanup")
	defer func() { lastLog(err) }()

	_ = logger

	return nil
}

func (r *testRun) cleanup(ctx context.Context) (err error) {
	logger, lastLog := r.loggerForStep("cleanup")
	defer func() { lastLog(err) }()

	_ = logger

	var errs []error
	for _, fn := range slices.Backward(r.cleanupFns) {
		errs = append(errs, fn(ctx))
	}
	return errors.Join(errs...)
}

func (r *testRun) postCleanup(ctx context.Context) (err error) {
	logger, lastLog := r.loggerForStep("post-cleanup")
	defer func() { lastLog(err) }()

	_ = logger

	return nil
}

// -- UTILS --------------------------------------------------------------------

func (r *testRun) collectDockerComposeFiles(cfgs []config.ServiceConfig) []string {
	paths := make([]string, len(cfgs))
	for i, cfg := range cfgs {
		paths[i] = cfg.DockerCompose
	}
	return paths
}

// Go spawns a goroutine in the goroutine pool that runs the given function.
// If the function returns an error, the goroutine pool is marked dead.
func (r *testRun) Go(f func(ctx context.Context) error) {
	r.goroutinePool.Go(func(ctx context.Context) error {
		err := f(ctx)
		if err != nil {
			r.goroutinePoolDead.Store(true)
		}
		return err
	})
}

// loggerForStep returns a logger for the given step name and a function that
// logs the result of the step. The function should be deferred.
func (r *testRun) loggerForStep(name string) (*slog.Logger, func(error)) {
	logger := slog.Default().
		With("test", r.Name).
		With("tool", r.Tool).
		With("step", name)

	logger.Info("Running step")
	return logger, func(err error) {
		if err != nil {
			logger.Error("Step failed", "error", err)
		} else {
			logger.Info("Step successful")
		}
	}
}

func (r *testRun) dockerComposeUpWait(
	ctx context.Context,
	logger *slog.Logger,
	dockerComposeFiles []string,
	logPath string,
) error {
	f, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("failed to create log file: %w", err)
	}
	// Close file in cleanup
	r.cleanupFns = append(r.cleanupFns, func(ctx context.Context) error {
		return f.Close()
	})

	r.Go(func(ctx context.Context) error {
		return dockerutil.ComposeUp(
			ctx,
			dockerutil.ComposeOptions{
				File:   dockerComposeFiles,
				Stdout: f,
				Stderr: f,
			},
			dockerutil.ComposeUpOptions{},
		)
	})

	r.cleanupFns = append(r.cleanupFns, func(ctx context.Context) error {
		logger.Info("Stopping containers")
		return dockerutil.ComposeDown(
			ctx,
			dockerutil.ComposeOptions{
				File: dockerComposeFiles,
			},
			dockerutil.ComposeDownOptions{},
		)
	})

	logger.Info("Waiting for containers to start")
	var containers []string

	for range 30 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}

		var buf bytes.Buffer
		err = dockerutil.ComposePs(
			ctx,
			dockerutil.ComposeOptions{
				File:   dockerComposeFiles,
				Stdout: &buf,
			},
			dockerutil.ComposePsOptions{
				Quiet: ptr(true),
			},
		)
		if err != nil {
			return fmt.Errorf("failed to list containers: %w", err)
		}
		containers = strings.Fields(buf.String())
		if len(containers) > 0 {
			break
		}
	}

	logger.Info(fmt.Sprintf("Identified %d containers", len(containers)))
	if r.goroutinePoolDead.Load() {
		return errors.New("failed to start containers")
	}

	wg := pool.New().WithErrors()
	for _, c := range containers {
		wg.Go(func() error {
			for {
				resp, err := r.DockerClient.ContainerInspect(ctx, c)
				if err != nil {
					return err
				}
				switch {
				case resp.State.Dead:
					return fmt.Errorf("container %s is dead", resp.Name)
				case resp.State.Health != nil && strings.EqualFold(resp.State.Health.Status, "healthy"):
					logger.Info("Container is healthy", "container", resp.Name)
					return nil
				case resp.State.Health == nil && resp.State.Running:
					logger.Info("Container is running (consider adding a health check!)", "container", resp.Name)
					return nil
				}

				logger.Info("Waiting for container status to be healthy", "container", resp.Name, "status", resp.State.Health.Status)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Second):
				}
				continue
			}
		})
	}

	err = wg.Wait()
	if err != nil {
		return fmt.Errorf("failed to start containers: %w", err)
	}

	return nil
}

func ptr[T any](v T) *T {
	return &v
}
