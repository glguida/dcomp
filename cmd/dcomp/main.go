package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/dockerengine"
	"github.com/glguida/dcomp/engine"
	"github.com/glguida/dcomp/lifecycle"
	"github.com/glguida/dcomp/state"
)

const usageText = `dcomp runs systems of Docker components linked by protobuf interfaces.

Usage:
  dcomp version [--json]
  dcomp [--state-root DIR] check FILE
  dcomp [--state-root DIR] up FILE
  dcomp [--state-root DIR] ps [-a|--all] [--json] [NAME]
  dcomp [--state-root DIR] status [--json] NAME
  dcomp [--state-root DIR] volume [--json] SYSTEM COMPONENT LOGICAL
  dcomp [--state-root DIR] logs [-f|--follow] NAME [COMPONENT...]
  dcomp [--state-root DIR] restart NAME [COMPONENT...]
  dcomp [--state-root DIR] down NAME
  dcomp [--state-root DIR] resume NAME
  dcomp [--state-root DIR] abort NAME
  dcomp [--state-root DIR] inspect-image IMAGE

Operations interrupted by Ctrl-C or host failure remain recorded. Use up FILE
to resume the same resolved target or supersede it with a different one. Resume
continues the exact recorded operation. Once no create result is unresolved,
abort may remove its verified in-progress resources.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(arguments []string) int {
	flags := flag.NewFlagSet("dcomp", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateRoot := flags.String("state-root", "", "durable dcomp state directory")
	flags.Usage = func() { fmt.Fprint(flags.Output(), usageText) }
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	args := flags.Args()
	if len(args) == 0 {
		flags.Usage()
		return 2
	}
	command := args[0]
	commandArgs := args[1:]
	if command == "version" {
		versionFlags := flag.NewFlagSet("dcomp version", flag.ContinueOnError)
		versionFlags.SetOutput(os.Stderr)
		jsonOutput := versionFlags.Bool("json", false, "emit stable machine-readable JSON")
		if err := versionFlags.Parse(commandArgs); err != nil {
			return 2
		}
		if versionFlags.NArg() != 0 {
			return commandUsage("version expects no arguments")
		}
		if err := writeVersion(os.Stdout, *jsonOutput); err != nil {
			return commandError(err, context.Background())
		}
		return 0
	}
	root := *stateRoot
	if root == "" {
		var err error
		root, err = state.DefaultRoot()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
	}
	if !filepath.IsAbs(root) {
		fmt.Fprintln(os.Stderr, "error: --state-root must be an absolute path")
		return 2
	}

	docker, err := dockerengine.NewFromEnvironment()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	controller := lifecycle.Controller{
		Engine: docker,
		State:  state.Store{Root: root},
		Report: func(message string) { fmt.Fprintln(os.Stderr, message) },
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch command {
	case "check":
		if len(commandArgs) != 1 {
			return commandUsage("check expects FILE")
		}
		spec, err := composition.Load(commandArgs[0])
		if err != nil {
			return commandError(err, ctx)
		}
		resolved, err := controller.Check(ctx, spec)
		if err != nil {
			return commandError(err, ctx)
		}
		fmt.Printf("%s %s\n", resolved.Name, resolved.Digest)
	case "up":
		if len(commandArgs) != 1 {
			return commandUsage("up expects FILE")
		}
		spec, err := composition.Load(commandArgs[0])
		if err != nil {
			return commandError(err, ctx)
		}
		if err := controller.Up(ctx, spec); err != nil {
			return commandError(err, ctx)
		}
	case "ps":
		psFlags := flag.NewFlagSet("dcomp ps", flag.ContinueOnError)
		psFlags.SetOutput(os.Stderr)
		all := false
		psFlags.BoolVar(&all, "a", false, "include non-running components")
		psFlags.BoolVar(&all, "all", false, "include non-running components")
		jsonOutput := psFlags.Bool("json", false, "emit stable machine-readable JSON")
		if err := psFlags.Parse(commandArgs); err != nil {
			return 2
		}
		if psFlags.NArg() > 1 {
			return commandUsage("ps expects at most one NAME")
		}
		name := ""
		if psFlags.NArg() == 1 {
			name = psFlags.Arg(0)
		}
		processes, err := controller.Processes(ctx, name, all)
		if err != nil {
			return commandError(err, ctx)
		}
		if *jsonOutput {
			if err := writeProcessesJSON(os.Stdout, processes); err != nil {
				return commandError(err, ctx)
			}
		} else {
			printProcesses(processes)
		}
	case "status":
		statusFlags := flag.NewFlagSet("dcomp status", flag.ContinueOnError)
		statusFlags.SetOutput(os.Stderr)
		jsonOutput := statusFlags.Bool("json", false, "emit stable machine-readable JSON")
		if err := statusFlags.Parse(commandArgs); err != nil {
			return 2
		}
		if statusFlags.NArg() != 1 {
			return commandUsage("status expects NAME")
		}
		status, err := controller.Status(ctx, statusFlags.Arg(0))
		if err != nil {
			return commandError(err, ctx)
		}
		if *jsonOutput {
			if err := writeStatusJSON(os.Stdout, status); err != nil {
				return commandError(err, ctx)
			}
		} else {
			printStatus(status)
		}
		if !status.Operational() {
			return 1
		}
	case "volume":
		volumeFlags := flag.NewFlagSet("dcomp volume", flag.ContinueOnError)
		volumeFlags.SetOutput(os.Stderr)
		jsonOutput := volumeFlags.Bool("json", false, "emit stable machine-readable JSON")
		if err := volumeFlags.Parse(commandArgs); err != nil {
			return 2
		}
		if volumeFlags.NArg() != 3 {
			return commandUsage("volume expects SYSTEM COMPONENT LOGICAL")
		}
		volume, err := controller.InspectPersistentVolume(
			ctx,
			volumeFlags.Arg(0),
			volumeFlags.Arg(1),
			volumeFlags.Arg(2),
		)
		if err != nil {
			return commandError(err, ctx)
		}
		if *jsonOutput {
			if err := writeVolumeJSON(os.Stdout, volume); err != nil {
				return commandError(err, ctx)
			}
		} else {
			fmt.Println(volume.Name)
		}
	case "logs":
		logFlags := flag.NewFlagSet("dcomp logs", flag.ContinueOnError)
		logFlags.SetOutput(os.Stderr)
		follow := false
		logFlags.BoolVar(&follow, "f", false, "follow new component output")
		logFlags.BoolVar(&follow, "follow", false, "follow new component output")
		if err := logFlags.Parse(commandArgs); err != nil {
			return 2
		}
		if logFlags.NArg() < 1 {
			return commandUsage("logs expects NAME [COMPONENT...]")
		}
		err := controller.Logs(ctx, logFlags.Arg(0), follow, func(record lifecycle.LogRecord) error {
			_, writeErr := fmt.Printf(
				"%s\t%s\t%s\t%s\n",
				record.Line.Timestamp.Format(time.RFC3339Nano),
				record.Component,
				record.Line.Stream,
				record.Line.Message,
			)
			return writeErr
		}, logFlags.Args()[1:]...)
		if err != nil {
			return commandError(err, ctx)
		}
	case "restart":
		if len(commandArgs) < 1 {
			return commandUsage("restart expects NAME [COMPONENT...]")
		}
		if err := controller.Restart(ctx, commandArgs[0], commandArgs[1:]...); err != nil {
			return commandError(err, ctx)
		}
	case "down":
		if len(commandArgs) != 1 {
			return commandUsage("down expects NAME")
		}
		if err := controller.Down(ctx, commandArgs[0]); err != nil {
			return commandError(err, ctx)
		}
	case "resume":
		if len(commandArgs) != 1 {
			return commandUsage("resume expects NAME")
		}
		if err := controller.Resume(ctx, commandArgs[0]); err != nil {
			return commandError(err, ctx)
		}
	case "abort":
		if len(commandArgs) != 1 {
			return commandUsage("abort expects NAME")
		}
		if err := controller.Abort(ctx, commandArgs[0]); err != nil {
			return commandError(err, ctx)
		}
	case "inspect-image":
		if len(commandArgs) != 1 {
			return commandUsage("inspect-image expects IMAGE")
		}
		inspectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		image, err := docker.ResolveImage(inspectCtx, commandArgs[0])
		cancel()
		if err != nil {
			return commandError(err, ctx)
		}
		fmt.Println("image", image.ID)
		fmt.Println("healthcheck", image.HasHealthcheck)
		for _, target := range image.DeclaredVolumes {
			fmt.Println("volume", target)
		}
	default:
		return commandUsage(fmt.Sprintf("unknown command %q", command))
	}
	return 0
}

func printStatus(status lifecycle.Status) {
	switch {
	case status.Operation != "":
		fmt.Printf("%s operation=%s phase=%s digest=%s\n", status.Name, status.Operation, status.Phase, status.Digest)
	case status.Desired:
		fmt.Printf("%s desired=running digest=%s\n", status.Name, status.Digest)
	default:
		fmt.Printf("%s desired=absent\n", status.Name)
	}
	if len(status.Networks) != 0 {
		fmt.Println("NETWORK\tPOLICY\tID\tPROBLEM")
		for _, network := range status.Networks {
			id := network.ID
			if len(id) > 12 {
				id = id[:12]
			}
			policy := "egress"
			if network.Internal {
				policy = "internal"
			}
			fmt.Printf(
				"%s\t%s\t%s\t%s\n",
				network.Key,
				policy,
				id,
				strings.ReplaceAll(network.Problem, "\n", " "),
			)
		}
	}
	if len(status.Components) == 0 {
		return
	}
	fmt.Println("COMPONENT\tSTATUS\tHEALTH\tEXIT\tCONTAINER\tPROBLEM")
	for _, component := range status.Components {
		containerID := component.ID
		if len(containerID) > 12 {
			containerID = containerID[:12]
		}
		fmt.Printf(
			"%s\t%s\t%s\t%d\t%s\t%s\n",
			component.Name,
			component.Status,
			component.Health,
			component.ExitCode,
			containerID,
			strings.ReplaceAll(component.Problem, "\n", " "),
		)
	}
}

func printProcesses(processes []lifecycle.ComponentProcess) {
	fmt.Println("SYSTEM\tCOMPONENT\tSTATUS\tHEALTH\tEXIT\tCONTAINER\tPORTS\tOPERATION\tPROBLEM")
	for _, process := range processes {
		containerID := process.ID
		if len(containerID) > 12 {
			containerID = containerID[:12]
		}
		operation := ""
		if process.Operation != "" {
			operation = process.Operation
			if process.Phase != "" {
				operation += "/" + process.Phase
			}
		}
		fmt.Printf(
			"%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			process.System,
			process.Name,
			process.Status,
			process.Health,
			process.ExitCode,
			containerID,
			formatPublishedPorts(process.PublishedPorts),
			operation,
			strings.ReplaceAll(process.Problem, "\n", " "),
		)
	}
}

func formatPublishedPorts(bindings []engine.PortBinding) string {
	values := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		values = append(values, fmt.Sprintf(
			"%s:%d->%d/%s",
			binding.HostIP,
			binding.HostPort,
			binding.ContainerPort,
			binding.Protocol,
		))
	}
	sort.Strings(values)
	return strings.Join(values, ",")
}

func commandUsage(message string) int {
	fmt.Fprintln(os.Stderr, "error:", message)
	fmt.Fprint(os.Stderr, usageText)
	return 2
}

func commandError(err error, ctx context.Context) int {
	if errors.Is(ctx.Err(), context.Canceled) {
		return 130
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}
