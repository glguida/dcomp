package lifecycle

import (
	"context"
	"fmt"

	"github.com/glguida/dcomp/composition"
)

// Edit changes a committed system under its exclusive lifecycle lock. The
// callback receives only this invocation's editable copy, with existing image
// references pinned to their committed immutable IDs. It must not call another
// lifecycle operation. A failed callback leaves the system untouched.
//
// Interrupted edits use the existing apply journal and resume/abort commands.
// A new edit refuses a pending operation rather than overwriting its intent.
func (controller *Controller) Edit(ctx context.Context, name string, create bool, change func(*composition.Spec) error) error {
	if err := controller.validate(); err != nil {
		return err
	}
	if !composition.ValidName(name) {
		return fmt.Errorf("invalid system name %q", name)
	}
	if change == nil {
		return fmt.Errorf("edit callback is missing")
	}
	lock, err := controller.State.AcquireContext(ctx, name)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := controller.bindEngine(ctx); err != nil {
		return err
	}
	if pending, exists, err := controller.State.ReadOperation(name); err != nil {
		return err
	} else if exists {
		return pendingError(pending)
	}
	previous, exists, err := controller.State.ReadDesired(name)
	if err != nil {
		return err
	}
	if !exists && !create {
		return fmt.Errorf("system %q is not deployed", name)
	}
	spec := composition.Spec{Name: name}
	if exists {
		if previous.RuntimeRoot != controller.RuntimeRoot {
			return fmt.Errorf("incremental edit must use the system's existing runtime root %s", previous.RuntimeRoot)
		}
		spec = previous.Spec.Authored()
	}
	if err := change(&spec); err != nil {
		return err
	}
	if spec.Name != name {
		return fmt.Errorf("edit cannot rename the system")
	}
	resolved, err := controller.resolve(ctx, spec)
	if err != nil {
		return err
	}
	// Preserve human-readable image references without resolving their mutable
	// tags again or changing unrelated components during an incremental edit.
	for i := range resolved.Components {
		item := &resolved.Components[i]
		for _, old := range previous.Spec.Components {
			if item.Name == old.Name && item.ImageRef == old.ImageID {
				item.ImageRef = old.ImageRef
			}
		}
	}
	return controller.applyResolved(ctx, resolved)
}

// AddComponent also creates a previously absent system. Unconnected inputs
// are allowed and reject connections until wired.
func (controller *Controller) AddComponent(ctx context.Context, system string, instance composition.Instance, links []composition.Link) error {
	return controller.Edit(ctx, system, true, func(spec *composition.Spec) error {
		for _, item := range spec.Components {
			if item.Name == instance.Name {
				return fmt.Errorf("component %q already exists", instance.Name)
			}
		}
		for _, link := range links {
			if link.Input.Component != instance.Name {
				return fmt.Errorf("add-component may only wire its own inputs")
			}
		}
		spec.Components = append(spec.Components, instance)
		spec.Links = append(spec.Links, links...)
		return nil
	})
}

func (controller *Controller) RemoveComponent(ctx context.Context, system, name string) error {
	return controller.Edit(ctx, system, false, func(spec *composition.Spec) error {
		found := false
		components := spec.Components[:0]
		for _, item := range spec.Components {
			if item.Name == name {
				found = true
			} else {
				components = append(components, item)
			}
		}
		if !found {
			return fmt.Errorf("component %q does not exist", name)
		}
		spec.Components = components
		links := spec.Links[:0]
		for _, link := range spec.Links {
			if link.Input.Component != name && link.Output.Component != name {
				links = append(links, link)
			}
		}
		spec.Links = links
		for i := range spec.Globals {
			if spec.Globals[i].Target.Component == name {
				spec.Globals[i].Target = composition.EndpointRef{}
			}
		}
		return nil
	})
}

// ModifyWire replaces or removes one input's target. A zero target disconnects
// the input. A global reference is kept symbolic all the way into the proxy.
func (controller *Controller) ModifyWire(ctx context.Context, system string, input, target composition.EndpointRef) error {
	return controller.Edit(ctx, system, false, func(spec *composition.Spec) error {
		found := false
		for _, item := range spec.Components {
			if item.Name == input.Component {
				_, found = item.Component.Definition.Input(input.Endpoint)
			}
		}
		if !found || input.Global != "" {
			return fmt.Errorf("unknown input %s", input)
		}
		links := spec.Links[:0]
		for _, link := range spec.Links {
			if link.Input != input {
				links = append(links, link)
			}
		}
		spec.Links = links
		if target != (composition.EndpointRef{}) {
			spec.Links = append(spec.Links, composition.Link{Input: input, Output: target})
		}
		return nil
	})
}

// AssignGlobal creates a name (inferring its service from the output) or
// reassigns an existing one. Existing names retain their service type.
// For an unbound declaration supply a zero target and an explicit service.
func (controller *Controller) AssignGlobal(ctx context.Context, system, name, service string, target composition.EndpointRef) error {
	return controller.Edit(ctx, system, false, func(spec *composition.Spec) error {
		if target.Global != "" {
			return fmt.Errorf("a global name must target a specific component output")
		}
		for i := range spec.Globals {
			if spec.Globals[i].Name == name {
				if service != "" && service != spec.Globals[i].Service {
					return fmt.Errorf("cannot change service type of global %s", name)
				}
				spec.Globals[i].Target = target
				return nil
			}
		}
		if service == "" {
			for _, item := range spec.Components {
				if item.Name == target.Component {
					if output, ok := item.Component.Definition.Output(target.Endpoint); ok {
						service = output.Service
					}
				}
			}
		}
		spec.Globals = append(spec.Globals, composition.Global{Name: name, Service: service, Target: target})
		return nil
	})
}
