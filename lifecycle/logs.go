package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/engine"
	"github.com/glguida/dcomp/proxy"
	"github.com/glguida/dcomp/state"
)

// LogRecord identifies one component line in a system-wide log stream.
type LogRecord struct {
	Component string
	Line      engine.LogLine
}

type logSource struct {
	component composition.ResolvedComponent
	resource  state.Resource
}

type logEvent struct {
	record *LogRecord
	err    error
}

const ProxyLogSource = "@proxy"

// Logs streams Docker stdout and stderr for selected currently recorded
// components, or every component when no names are supplied. Component names
// are exact and must occur in the recorded system. Logs is observational: it
// takes no lifecycle lock and never repairs state.
//
// The callback is invoked serially, even though component streams are read in
// parallel. A pending operation exposes its target containers once any exist;
// before target creation it exposes the previous deployment instead.
func (controller *Controller) Logs(
	ctx context.Context,
	name string,
	follow bool,
	emit func(LogRecord) error,
	components ...string,
) error {
	if err := controller.validate(); err != nil {
		return err
	}
	if emit == nil {
		return fmt.Errorf("log receiver is nil")
	}
	var logEngine engine.LogEngine
	proxyOnly := len(components) == 1 && components[0] == ProxyLogSource
	if !proxyOnly {
		var ok bool
		logEngine, ok = controller.Engine.(engine.LogEngine)
		if !ok {
			return fmt.Errorf("configured container engine does not support logs")
		}
	}
	spec, networks, resources, process, runtimeRoot, committed, err := controller.logState(name)
	if err != nil {
		return err
	}
	scope := controller.dockerScope(spec.Name)
	if err := controller.verifyEngineBinding(ctx); err != nil {
		return err
	}
	selected, includeProxy, err := selectLogComponents(spec, components)
	if err != nil {
		return fmt.Errorf("system %q: %w", name, err)
	}
	sources := make([]logSource, 0, len(spec.Components))
	runtimeDir := runtimeDirectory(runtimeRoot, spec.Name)
	if process != nil {
		runtimeDir = process.RuntimeDir
	}
	for _, component := range spec.Components {
		if selected != nil {
			if _, exists := selected[component.Name]; !exists {
				continue
			}
		}
		resource, exists := resources[component.Name]
		if !exists || resource.ID == "" {
			continue
		}
		container, inspectErr := controller.inspectContainer(ctx, resource.ID)
		if inspectErr != nil {
			return fmt.Errorf("inspect %s before reading logs: %w", component.Name, inspectErr)
		}
		// Logs remain available while diagnosing a component whose standard-I/O
		// policy has drifted. Identity, ownership, security, environment, and
		// networks are still verified below; only attach requires current stdio.
		if err := verifyContainerCore(scope, runtimeDir, component, resource, container); err != nil {
			return err
		}
		if err := verifyContainerEnvironment(spec, component, container); err != nil {
			return err
		}
		if committed {
			plans, topologyErr := resolvedTopology(scope, spec)
			if topologyErr != nil {
				return topologyErr
			}
			if err := verifyContainerNetworks(component, plans, networks, container); err != nil {
				return err
			}
		}
		sources = append(sources, logSource{component: component, resource: resource})
	}
	if len(sources) == 0 && (!includeProxy || process == nil) {
		return fmt.Errorf("system %q has no recorded component logs", name)
	}
	if len(sources) != 0 {
		if logEngine == nil {
			return fmt.Errorf("configured container engine does not support logs")
		}
	}

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan logEvent, 128)
	var readers sync.WaitGroup
	for _, source := range sources {
		source := source
		readers.Add(1)
		go func() {
			defer readers.Done()
			err := logEngine.ContainerLogs(
				streamCtx,
				source.resource.ID,
				engine.LogOptions{Follow: follow},
				func(line engine.LogLine) error {
					record := LogRecord{Component: source.component.Name, Line: line}
					select {
					case events <- logEvent{record: &record}:
						return nil
					case <-streamCtx.Done():
						return streamCtx.Err()
					}
				},
			)
			if err != nil {
				select {
				case events <- logEvent{err: err}:
				case <-streamCtx.Done():
				}
			}
		}()
	}
	if includeProxy && process != nil {
		process := *process
		readers.Add(1)
		go func() {
			defer readers.Done()
			err := controller.Proxy.Logs(
				streamCtx,
				process,
				follow,
				func(line proxy.LogLine) error {
					record := LogRecord{
						Component: ProxyLogSource,
						Line: engine.LogLine{
							Timestamp: line.Timestamp,
							Stream:    engine.LogStderr,
							Message:   line.Message,
						},
					}
					select {
					case events <- logEvent{record: &record}:
						return nil
					case <-streamCtx.Done():
						return streamCtx.Err()
					}
				},
			)
			if err != nil {
				select {
				case events <- logEvent{err: err}:
				case <-streamCtx.Done():
				}
			}
		}()
	}
	go func() {
		readers.Wait()
		close(events)
	}()

	var firstErr error
	for event := range events {
		if event.err != nil {
			if firstErr == nil && !(errors.Is(event.err, context.Canceled) && ctx.Err() == nil) {
				firstErr = event.err
				cancel()
			}
			continue
		}
		if event.record == nil || firstErr != nil {
			continue
		}
		if err := emit(*event.record); err != nil {
			firstErr = err
			cancel()
		}
	}
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func selectLogComponents(
	spec composition.ResolvedSpec,
	requested []string,
) (map[string]struct{}, bool, error) {
	if len(requested) == 0 {
		return nil, true, nil
	}
	available := make(map[string]struct{}, len(spec.Components))
	for _, component := range spec.Components {
		available[component.Name] = struct{}{}
	}
	selected := make(map[string]struct{}, len(requested))
	includeProxy := false
	for _, name := range requested {
		if name == ProxyLogSource {
			if includeProxy {
				return nil, false, fmt.Errorf("proxy logs were requested more than once")
			}
			includeProxy = true
			continue
		}
		if _, duplicate := selected[name]; duplicate {
			return nil, false, fmt.Errorf("component %q was requested more than once", name)
		}
		if _, exists := available[name]; !exists {
			return nil, false, fmt.Errorf("component %q is not present in the recorded system", name)
		}
		selected[name] = struct{}{}
	}
	return selected, includeProxy, nil
}

func (controller *Controller) logState(
	name string,
) (
	composition.ResolvedSpec,
	map[string]state.Resource,
	map[string]state.Resource,
	*proxy.Process,
	string,
	bool,
	error,
) {
	operation, operationExists, err := controller.State.ReadOperation(name)
	if err != nil {
		return composition.ResolvedSpec{}, nil, nil, nil, "", false, err
	}
	if operationExists && len(operation.Containers) != 0 {
		return operation.Target, operation.Networks, operation.Containers,
			operation.Proxy, operation.RuntimeRoot, false, nil
	}
	if operationExists && operation.Previous != nil {
		return operation.Previous.Spec,
			operation.Previous.Networks,
			operation.Previous.Containers,
			operation.Previous.Proxy,
			operation.Previous.RuntimeRoot,
			true,
			nil
	}
	desired, desiredExists, err := controller.State.ReadDesired(name)
	if err != nil {
		return composition.ResolvedSpec{}, nil, nil, nil, "", false, err
	}
	if desiredExists {
		return desired.Spec, desired.Networks, desired.Containers,
			desired.Proxy, desired.RuntimeRoot, true, nil
	}
	if operationExists {
		return operation.Target, operation.Networks, operation.Containers,
			operation.Proxy, operation.RuntimeRoot, false, nil
	}
	return composition.ResolvedSpec{}, nil, nil, nil, "", false,
		fmt.Errorf("system %q is absent", name)
}
