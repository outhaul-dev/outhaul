package compose

import (
	"encoding/json"
	"fmt"
)

// LeftAlone is one service that keeps the restart behaviour its file gives it.
type LeftAlone struct {
	Service string
	Reason  string // LeftAloneOwnPolicy or LeftAloneWaitedOn
}

const (
	// LeftAloneOwnPolicy: the service sets restart or deploy.restart_policy,
	// which the override must not change.
	LeftAloneOwnPolicy = "it sets its own restart policy"
	// LeftAloneWaitedOn: another service waits for it to finish, so restarted
	// it would never complete and the deploy would time out.
	LeftAloneWaitedOn = "another service waits for it to finish"
)

// RestartExtras returns an Extra carrying policy for every service that states
// no restart policy of its own and that no service waits on to complete, plus
// every service it left alone with the reason. Both are in Model order
// (service name). An empty policy returns nothing, so an install that does
// not set OUTHAUL_COMPOSE_RESTART changes no file.
func RestartExtras(m Model, policy string) (map[string]Extra, []LeftAlone, error) {
	if policy == "" {
		return nil, nil, nil
	}
	waitedOn, err := waitedOnToComplete(m)
	if err != nil {
		return nil, nil, err
	}
	var extras map[string]Extra
	var left []LeftAlone
	for _, s := range m.Services {
		switch {
		case s.HasRestart():
			left = append(left, LeftAlone{Service: s.Name, Reason: LeftAloneOwnPolicy})
		case waitedOn[s.Name]:
			left = append(left, LeftAlone{Service: s.Name, Reason: LeftAloneWaitedOn})
		default:
			if extras == nil {
				extras = map[string]Extra{}
			}
			extras[s.Name] = Extra{Restart: policy}
		}
	}
	return extras, left, nil
}

// waitedOnToComplete collects the services another service waits on with
// condition: service_completed_successfully. Compose prints the short form
// (depends_on: [web]) as the same object with condition: service_started, so
// one shape covers both; a value that is neither is refused rather than
// guessed at. The error names the service and the option, never a value.
func waitedOnToComplete(m Model) (map[string]bool, error) {
	out := map[string]bool{}
	for _, s := range m.Services {
		raw, ok := s.Raw["depends_on"]
		if !ok {
			continue
		}
		var deps map[string]struct{ Condition string }
		if err := json.Unmarshal(raw, &deps); err != nil {
			return nil, fmt.Errorf("service %q: depends_on: this option could not be read", s.Name)
		}
		for name, d := range deps {
			if d.Condition == "service_completed_successfully" {
				out[name] = true
			}
		}
	}
	return out, nil
}

// MergeExtras unions two per-service extras maps: each field comes from the map
// that has it. Two empty maps return nil, so "no extras" stays nil for the
// pipeline's write test.
func MergeExtras(a, b map[string]Extra) map[string]Extra {
	out := make(map[string]Extra, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		cur := out[k]
		cur.Limits = cur.Limits.Or(v.Limits)
		cur.InDeploy = cur.InDeploy || v.InDeploy
		if cur.Restart == "" {
			cur.Restart = v.Restart
		}
		out[k] = cur
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
