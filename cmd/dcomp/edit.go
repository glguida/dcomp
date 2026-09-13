package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/lifecycle"
)

type repeatedStrings []string

func (items *repeatedStrings) String() string         { return strings.Join(*items, ",") }
func (items *repeatedStrings) Set(value string) error { *items = append(*items, value); return nil }

func runEdit(ctx context.Context, controller *lifecycle.Controller, command string, args []string) int {
	if command == "add-component" {
		return runAddComponent(ctx, controller, args)
	}
	if command == "rm-component" {
		if len(args) != 2 {
			return commandUsage("rm-component expects SYSTEM COMPONENT")
		}
		if err := controller.RemoveComponent(ctx, args[0], args[1]); err != nil {
			return commandError(err, ctx)
		}
		return 0
	}
	if command == "mod-wire" {
		if len(args) != 3 {
			return commandUsage("mod-wire expects SYSTEM COMPONENT.INPUT COMPONENT.OUTPUT|@GLOBAL|-")
		}
		input, err := composition.ParseEndpointRef(args[1])
		if err != nil {
			return commandUsage(err.Error())
		}
		var target composition.EndpointRef
		if args[2] != "-" {
			target, err = composition.ParseTarget(args[2])
		}
		if err != nil {
			return commandUsage(err.Error())
		}
		if err := controller.ModifyWire(ctx, args[0], input, target); err != nil {
			return commandError(err, ctx)
		}
		return 0
	}
	flags := flag.NewFlagSet("dcomp assign-global", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	service := flags.String("service", "", "service type, required when declaring an unbound name")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 3 {
		return commandUsage("assign-global [--service TYPE] expects SYSTEM NAME COMPONENT.OUTPUT|-")
	}
	var target composition.EndpointRef
	var err error
	if flags.Arg(2) != "-" {
		target, err = composition.ParseEndpointRef(flags.Arg(2))
	}
	if err != nil {
		return commandUsage(err.Error())
	}
	if err := controller.AssignGlobal(ctx, flags.Arg(0), flags.Arg(1), *service, target); err != nil {
		return commandError(err, ctx)
	}
	return 0
}

func runAddComponent(ctx context.Context, controller *lifecycle.Controller, args []string) int {
	flags := flag.NewFlagSet("dcomp add-component", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var links, binds, volumes, ports, arguments repeatedStrings
	flags.Var(&links, "link", "INPUT=COMPONENT.OUTPUT or INPUT=@GLOBAL (repeatable)")
	flags.Var(&binds, "bind", "SOURCE,TARGET,ro|rw (repeatable; source relative to current directory)")
	flags.Var(&volumes, "volume", "NAME,TARGET,ro|rw (repeatable)")
	flags.Var(&ports, "publish", "tcp|udp,HOST_IP,HOST_PORT,CONTAINER_PORT (repeatable)")
	flags.Var(&arguments, "arg", "literal command argument (repeatable)")
	egress := flags.Bool("egress", false, "enable external network access")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 3 {
		return commandUsage("add-component [OPTIONS] expects SYSTEM NAME PATH")
	}
	component, err := composition.LoadComponent(flags.Arg(2))
	if err != nil {
		return commandError(err, ctx)
	}
	instance := composition.Instance{Name: flags.Arg(1), Component: component,
		Runtime: composition.Runtime{Args: arguments, ExternalEgress: *egress}}
	for _, item := range binds {
		parts, err := mountFields(item)
		if err != nil {
			return commandUsage(err.Error())
		}
		source, err := filepath.Abs(parts[0])
		if err == nil {
			source, err = filepath.EvalSymlinks(source)
		}
		if err != nil {
			return commandError(err, ctx)
		}
		instance.Runtime.Binds = append(instance.Runtime.Binds, composition.BindMount{Source: source, Target: parts[1], ReadOnly: parts[2] == "ro"})
	}
	for _, item := range volumes {
		parts, err := mountFields(item)
		if err != nil {
			return commandUsage(err.Error())
		}
		instance.Runtime.Volumes = append(instance.Runtime.Volumes, composition.VolumeMount{Name: parts[0], Target: parts[1], ReadOnly: parts[2] == "ro"})
	}
	for _, item := range ports {
		parts := strings.Split(item, ",")
		if len(parts) != 4 {
			return commandUsage("publish expects PROTOCOL,HOST_IP,HOST_PORT,CONTAINER_PORT")
		}
		host, err := strconv.Atoi(parts[2])
		if err != nil {
			return commandUsage("invalid host port")
		}
		container, err := strconv.Atoi(parts[3])
		if err != nil {
			return commandUsage("invalid container port")
		}
		instance.Runtime.Ports = append(instance.Runtime.Ports, composition.PublishedPort{Protocol: parts[0], HostIP: parts[1], HostPort: host, ContainerPort: container})
	}
	var wiring []composition.Link
	for _, item := range links {
		input, output, ok := strings.Cut(item, "=")
		if !ok || !composition.ValidName(input) {
			return commandUsage("link expects INPUT=COMPONENT.OUTPUT or INPUT=@GLOBAL")
		}
		target, err := composition.ParseTarget(output)
		if err != nil {
			return commandUsage(err.Error())
		}
		wiring = append(wiring, composition.Link{Input: composition.EndpointRef{Component: instance.Name, Endpoint: input}, Output: target})
	}
	if err := controller.AddComponent(ctx, flags.Arg(0), instance, wiring); err != nil {
		return commandError(err, ctx)
	}
	return 0
}

func mountFields(value string) ([]string, error) {
	parts := strings.Split(value, ",")
	if len(parts) != 3 || (parts[2] != "ro" && parts[2] != "rw") {
		return nil, fmt.Errorf("mount expects SOURCE_OR_NAME,TARGET,ro|rw")
	}
	return parts, nil
}
