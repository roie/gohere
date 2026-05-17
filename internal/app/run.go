package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/project"
	"github.com/roie/gohere/internal/runner"
)

type RunPlan struct {
	Command []string
	Env     []string
	Port    int
}

func PrepareRun(cmd cli.Command, cwd string) (RunPlan, error) {
	port := cmd.TargetPort
	if port == 0 {
		var err error
		port, err = runner.ChooseFreePort()
		if err != nil {
			return RunPlan{}, err
		}
	}

	env := runner.ChildEnv(os.Environ(), port)
	if cmd.Kind == cli.CommandRaw {
		return RunPlan{Command: append([]string(nil), cmd.Raw...), Env: env, Port: port}, nil
	}

	packagePath, found, err := project.FindNearestPackageJSON(cwd)
	if err != nil {
		return RunPlan{}, err
	}
	if !found {
		return RunPlan{}, errors.New("no package.json found; use gohere -- <command>")
	}

	pkg, err := project.ReadPackageJSON(packagePath)
	if err != nil {
		return RunPlan{}, err
	}
	scriptCommand, ok := pkg.Script(cmd.Script)
	if !ok {
		return RunPlan{}, fmt.Errorf("script %s not found; available scripts: %s", cmd.Script, strings.Join(pkg.AvailableScripts(), ", "))
	}

	pm, _, err := project.DetectPackageManager(projectDir(packagePath))
	if err != nil {
		return RunPlan{}, err
	}
	injected := runner.InjectPortArgs(scriptCommand, port, cmd.PortFlag)
	command := runner.BuildScriptCommand(pm, cmd.Script, injected)
	return RunPlan{Command: command, Env: env, Port: port}, nil
}

func Run(ctx context.Context, cmd cli.Command, cwd string, stdout, stderr io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	plan, err := PrepareRun(cmd, cwd)
	if err != nil {
		return err
	}

	if cmd.Verbose {
		fmt.Fprintf(stderr, "hidden port: %d\n", plan.Port)
		fmt.Fprintf(stderr, "command: %s\n", strings.Join(plan.Command, " "))
	}

	result, err := runner.Start(ctx, runner.Config{
		Command:        plan.Command,
		Env:            plan.Env,
		ChosenPort:     plan.Port,
		Stdout:         stdout,
		Stderr:         stderr,
		StartupTimeout: 15 * time.Second,
	})
	if err != nil {
		return err
	}
	defer result.Stop()

	if cmd.Verbose {
		fmt.Fprintf(stderr, "target: http://127.0.0.1:%d\n", result.Port)
	}
	return result.Wait()
}

func projectDir(packagePath string) string {
	return filepath.Dir(packagePath)
}
