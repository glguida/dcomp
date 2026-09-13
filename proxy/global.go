package proxy

import "fmt"

// Global binds a system-global name to a concrete output. Empty output fields
// retain the name while leaving it unbound.
type Global struct {
	Name            string `json:"name"`
	OutputComponent string `json:"output_component"`
	OutputEndpoint  string `json:"output_endpoint"`
}

// WireEdit replaces or disconnects one input. Link's input fields select the
// input; Disconnect ignores output fields (which must be empty).
type WireEdit struct {
	Link       Link `json:"link"`
	Disconnect bool `json:"disconnect,omitempty"`
}

// editWiring operates on an owned snapshot; callers serialize it with resync.
func editWiring(current Wiring, request ControlRequest) (Wiring, error) {
	wiring := Wiring{Endpoints: append([]EndpointIdentity(nil), current.Endpoints...), Links: append([]Link(nil), current.Links...), Globals: append([]Global(nil), current.Globals...)}
	switch request.Command {
	case "mod-wire":
		if request.Wire == nil {
			return Wiring{}, fmt.Errorf("mod-wire edit is missing")
		}
		edit := *request.Wire
		found := false
		for _, endpoint := range wiring.Endpoints {
			if endpointIdentityKey(endpoint) == linkInputKey(edit.Link) {
				found = true
			}
		}
		if !found {
			return Wiring{}, fmt.Errorf("mod-wire names unknown input")
		}
		if edit.Disconnect && (edit.Link.OutputComponent != "" || edit.Link.OutputEndpoint != "" || edit.Link.Global != "") {
			return Wiring{}, fmt.Errorf("disconnect must not specify a target")
		}
		links := wiring.Links[:0]
		for _, link := range wiring.Links {
			if linkInputKey(link) != linkInputKey(edit.Link) {
				links = append(links, link)
			}
		}
		wiring.Links = links
		if !edit.Disconnect {
			wiring.Links = append(wiring.Links, edit.Link)
		}
	case "assign-global":
		if request.Global == nil {
			return Wiring{}, fmt.Errorf("global assignment is missing")
		}
		found := false
		for i := range wiring.Globals {
			if wiring.Globals[i].Name == request.Global.Name {
				wiring.Globals[i] = *request.Global
				found = true
			}
		}
		if !found {
			wiring.Globals = append(wiring.Globals, *request.Global)
		}
	default:
		return Wiring{}, fmt.Errorf("unknown wiring edit")
	}
	return wiring, wiring.Validate()
}

func (server *server) handleEdit(request ControlRequest) Status {
	server.mu.Lock()
	current := server.wiring
	digest := server.digest
	server.mu.Unlock()
	if request.ExpectedDigest != "" && request.ExpectedDigest != digest {
		status := server.status()
		status.Error = "current wiring digest mismatch"
		return status
	}
	wiring, err := editWiring(current, request)
	if err == nil {
		var targetDigest string
		targetDigest, err = wiring.Digest()
		if err == nil {
			err = server.resync(wiring, targetDigest)
		}
	}
	status := server.status()
	if err != nil {
		status.Error = err.Error()
	}
	return status
}

// ResolvedLinks derives concrete stream routes without altering the symbolic
// wiring. Unbound names produce no route: input connections close immediately.
func (wiring Wiring) ResolvedLinks() []Link {
	globals := make(map[string]Global, len(wiring.Globals))
	for _, global := range wiring.Globals {
		globals[global.Name] = global
	}
	links := make([]Link, 0, len(wiring.Links))
	for _, link := range wiring.Links {
		if link.Global != "" {
			global := globals[link.Global]
			if global.OutputComponent == "" {
				continue
			}
			link.OutputComponent, link.OutputEndpoint = global.OutputComponent, global.OutputEndpoint
			link.Global = ""
		}
		links = append(links, link)
	}
	return links
}
