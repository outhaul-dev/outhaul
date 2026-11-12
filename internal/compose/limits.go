package compose

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// Extra is what the override adds to one service: the limits Outhaul applies
// for resources the service does not set itself, and the restart policy it
// gives a service that states none.
type Extra struct {
	Limits   core.Limits // only the fields to write
	InDeploy bool        // write them under deploy.resources.limits
	Restart  string      // the restart policy to write; "" for none
}

// LimitExtras returns the additions for every service that needs one. A
// service keeps any resource it sets itself; Outhaul fills only the rest. A
// service that sets no memory limit and reserves more memory than Outhaul
// would allow fails here, naming both figures: Docker would refuse the
// container at `up` with a less clear message.
func LimitExtras(m Model, limit core.Limits) (map[string]Extra, error) {
	if limit == (core.Limits{}) {
		return nil, nil
	}
	out := map[string]Extra{}
	for _, s := range m.Services {
		res := serviceResources(s.Raw)
		e := Extra{Limits: core.Limits{
			MilliCPUs: pick(res.cpu, limit.MilliCPUs),
			Memory:    pick(res.mem, limit.Memory),
			Pids:      pick(res.pids, limit.Pids),
		}}
		e.InDeploy = res.inDeploy
		if e.Limits == (core.Limits{}) {
			continue
		}
		if !res.mem && limit.Memory > 0 {
			if r, ok := reservation(s.Raw); ok && r > limit.Memory {
				return nil, fmt.Errorf("service %q: mem_reservation %d is above the memory limit Outhaul would add, %d; raise the app's limit or lower the reservation",
					s.Name, r, limit.Memory)
			}
		}
		out[s.Name] = e
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// pick returns 0 for a resource the service set itself.
func pick(set bool, limit int64) int64 {
	if set {
		return 0
	}
	return limit
}

// resources is what a service says about its own CPU, memory and processes.
type resources struct {
	cpu, mem, pids bool
	inDeploy       bool // it has a deploy.resources.limits block
}

// serviceResources reads Service.Raw. Compose leaves a zero out of the model,
// so `mem_limit: 0` has no key and counts as not set.
func serviceResources(raw map[string]json.RawMessage) resources {
	var r resources
	_, r.cpu = raw["cpus"]
	if _, ok := raw["cpu_quota"]; ok {
		r.cpu = true
	}
	if _, ok := raw["cpu_period"]; ok {
		r.cpu = true
	}
	_, r.mem = raw["mem_limit"]
	_, r.pids = raw["pids_limit"]
	if deploy, ok := object(raw["deploy"]); ok {
		if res, ok := object(deploy["resources"]); ok {
			if limits, ok := object(res["limits"]); ok {
				r.inDeploy = true
				if _, ok := limits["cpus"]; ok {
					r.cpu = true
				}
				if _, ok := limits["memory"]; ok {
					r.mem = true
				}
				if _, ok := limits["pids"]; ok {
					r.pids = true
				}
			}
		}
	}
	return r
}

// reservation returns the service's memory reservation, from either spelling.
func reservation(raw map[string]json.RawMessage) (int64, bool) {
	if v, ok := raw["mem_reservation"]; ok {
		if n, ok := byteValue(v); ok {
			return n, true
		}
	}
	if deploy, ok := object(raw["deploy"]); ok {
		if res, ok := object(deploy["resources"]); ok {
			if v, ok := res["reservations"]; ok {
				if ress, ok := object(v); ok {
					if mem, ok := ress["memory"]; ok {
						return byteValue(mem)
					}
				}
			}
		}
	}
	return 0, false
}

// object decodes a raw JSON object, reporting false for absent or other values.
func object(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if raw == nil {
		return nil, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil, false
	}
	return m, true
}

// byteValue reads a memory figure Compose prints as a string of bytes, or a
// bare number.
func byteValue(raw json.RawMessage) (int64, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n, true
	}
	return 0, false
}
