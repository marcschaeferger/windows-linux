package fingerprint

import (
	"encoding/json"
	"strings"
)

// The checks below decide whether the host filters inbound traffic. Docker,
// podman/netavark and libvirt add NAT and forward rules that say nothing about
// that, so only chains reached from the INPUT hook count: a drop policy on the
// hook, or a drop/reject verdict in a chain it jumps to. A host with just a
// container engine installed therefore reports no firewall.

// nftInputFilters reports whether the output of `nft -j list ruleset` shows
// inbound filtering. ok is false if the output can't be parsed.
func nftInputFilters(out string) (filters, ok bool) {
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return false, false
	}

	type chainID struct{ family, table, name string }
	type nftChain struct {
		Family string `json:"family"`
		Table  string `json:"table"`
		Name   string `json:"name"`
		Type   string `json:"type"`
		Hook   string `json:"hook"`
		Policy string `json:"policy"`
	}
	type nftRule struct {
		Family string                       `json:"family"`
		Table  string                       `json:"table"`
		Chain  string                       `json:"chain"`
		Expr   []map[string]json.RawMessage `json:"expr"`
	}

	var bases []chainID
	rules := map[chainID][]nftRule{}
	for _, el := range doc.Nftables {
		if raw, found := el["chain"]; found {
			var c nftChain
			if json.Unmarshal(raw, &c) != nil {
				continue
			}
			if c.Hook == "input" && (c.Type == "" || c.Type == "filter") {
				if strings.EqualFold(c.Policy, "drop") {
					return true, true
				}
				bases = append(bases, chainID{c.Family, c.Table, c.Name})
			}
		}
		if raw, found := el["rule"]; found {
			var r nftRule
			if json.Unmarshal(raw, &r) != nil {
				continue
			}
			id := chainID{r.Family, r.Table, r.Chain}
			rules[id] = append(rules[id], r)
		}
	}

	seen := map[chainID]bool{}
	queue := bases
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		for _, r := range rules[id] {
			for _, expr := range r.Expr {
				if _, drops := expr["drop"]; drops {
					return true, true
				}
				if _, rejects := expr["reject"]; rejects {
					return true, true
				}
				for _, verb := range []string{"jump", "goto"} {
					if raw, found := expr[verb]; found {
						var t struct {
							Target string `json:"target"`
						}
						if json.Unmarshal(raw, &t) == nil && t.Target != "" {
							queue = append(queue, chainID{id.family, id.table, t.Target})
						}
					}
				}
			}
		}
	}
	return false, true
}

// iptablesInputFilters reports whether the output of `iptables -S` shows
// inbound filtering: a DROP/REJECT policy on INPUT, or a DROP/REJECT rule in
// INPUT or a chain it jumps to.
func iptablesInputFilters(out string) bool {
	rules := map[string][][]string{}
	for _, line := range strings.Split(out, "\n") {
		args := splitShellArgs(line)
		if len(args) < 3 {
			continue
		}
		switch args[0] {
		case "-P":
			if args[1] == "INPUT" && isDropTarget(args[2]) {
				return true
			}
		case "-A":
			rules[args[1]] = append(rules[args[1]], args[2:])
		}
	}

	seen := map[string]bool{}
	queue := []string{"INPUT"}
	for len(queue) > 0 {
		chain := queue[0]
		queue = queue[1:]
		if seen[chain] {
			continue
		}
		seen[chain] = true
		for _, rule := range rules[chain] {
			for i := 0; i+1 < len(rule); i++ {
				if rule[i] != "-j" && rule[i] != "-g" && rule[i] != "--jump" && rule[i] != "--goto" {
					continue
				}
				if isDropTarget(rule[i+1]) {
					return true
				}
				queue = append(queue, rule[i+1])
			}
		}
	}
	return false
}

func isDropTarget(target string) bool {
	return target == "DROP" || target == "REJECT"
}

// splitShellArgs splits an `iptables -S` line, which quotes arguments with
// spaces (comments) the way a shell does.
func splitShellArgs(line string) []string {
	var args []string
	var cur strings.Builder
	inQuote := false
	started := false
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
			started = true
		case (r == ' ' || r == '\t') && !inQuote:
			if started {
				args = append(args, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if started {
		args = append(args, cur.String())
	}
	return args
}
