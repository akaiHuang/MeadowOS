package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

var openConfigRoots = map[string]struct{}{
	"providers":                  {},
	"credential_pool_strategies": {},
	"mcp_servers":                {},
	"hooks":                      {},
	"quick_commands":             {},
	"personalities":              {},
	"command_allowlist":          {},
	"model_catalog":              {},
	"channel_prompts":            {},
	"server_actions":             {},
	"secrets":                    {},
	"goals":                      {},
}

var extensibleConfigRoots = map[string]struct{}{
	"discord":     {},
	"telegram":    {},
	"slack":       {},
	"whatsapp":    {},
	"signal":      {},
	"mattermost":  {},
	"matrix":      {},
	"feishu":      {},
	"wecom":       {},
	"weixin":      {},
	"bluebubbles": {},
	"qqbot":       {},
	"yuanbao":     {},
	"email":       {},
	"sms":         {},
	"dingtalk":    {},
	"sessions":    {},
	"checkpoints": {},
}

var dynamicConfigRoots = map[string]struct{}{
	"custom_providers": {},
}

func splitDottedPath(dotted string) ([]string, error) {
	dotted = strings.TrimSpace(dotted)
	if dotted == "" {
		return nil, fmt.Errorf("config path is required")
	}
	parts := strings.Split(dotted, ".")
	for _, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("invalid config path %q", dotted)
		}
	}
	return parts, nil
}

func lookupDotted(root any, dotted string) (any, bool) {
	parts, err := splitDottedPath(dotted)
	if err != nil {
		return nil, false
	}
	current := root
	for _, part := range parts {
		switch node := current.(type) {
		case map[string]any:
			var found bool
			current, found = node[part]
			if !found {
				return nil, false
			}
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(node) {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}

// SetDotted updates one user-owned map/list path without replacing siblings.
// Numeric path segments navigate existing list entries; lists are never grown.
func (d *Document) SetDotted(dotted string, value any) error {
	parts, err := splitDottedPath(dotted)
	if err != nil {
		return err
	}
	if d == nil {
		return fmt.Errorf("config document is unavailable")
	}
	if d.isManagedPath(parts) || d.hasManagedAncestor(parts) {
		return fmt.Errorf(
			"config key %s is managed by the administrator scope %s",
			dotted,
			d.managedDir,
		)
	}
	if d.Raw == nil {
		d.Raw = map[string]any{}
	}

	var current any = d.Raw
	for _, part := range parts[:len(parts)-1] {
		switch node := current.(type) {
		case map[string]any:
			child := node[part]
			switch child.(type) {
			case map[string]any, []any:
			default:
				child = map[string]any{}
				node[part] = child
			}
			current = child
		case []any:
			index, indexErr := strconv.Atoi(part)
			if indexErr != nil || index < 0 || index >= len(node) {
				return fmt.Errorf(
					"cannot navigate config list at %q: invalid index %q",
					dotted,
					part,
				)
			}
			current = node[index]
		default:
			return fmt.Errorf(
				"cannot navigate scalar while setting config key %q",
				dotted,
			)
		}
	}

	last := parts[len(parts)-1]
	switch node := current.(type) {
	case map[string]any:
		node[last] = cloneValue(value)
	case []any:
		index, indexErr := strconv.Atoi(last)
		if indexErr != nil || index < 0 || index >= len(node) {
			return fmt.Errorf(
				"cannot set config list at %q: invalid index %q",
				dotted,
				last,
			)
		}
		node[index] = cloneValue(value)
	default:
		return fmt.Errorf("cannot set config key %q below a scalar", dotted)
	}
	return nil
}

func (d *Document) hasManagedAncestor(parts []string) bool {
	if d == nil {
		return false
	}
	for index := 1; index < len(parts); index++ {
		if _, found := d.managedKeys[strings.Join(parts[:index], ".")]; found {
			return true
		}
	}
	return false
}

// UnsetDotted removes one user-owned map/list path. Empty map parents are
// pruned while empty lists are preserved, matching the Python CLI contract.
func (d *Document) UnsetDotted(dotted string) (bool, error) {
	parts, err := splitDottedPath(dotted)
	if err != nil {
		return false, err
	}
	if d == nil {
		return false, fmt.Errorf("config document is unavailable")
	}
	if d.isManagedPath(parts) || d.hasManagedAncestor(parts) {
		return false, fmt.Errorf(
			"config key %s is managed by the administrator scope %s",
			dotted,
			d.managedDir,
		)
	}
	updated, removed, _ := unsetDottedValue(d.Raw, parts)
	if root, ok := updated.(map[string]any); ok {
		d.Raw = root
	}
	return removed, nil
}

func unsetDottedValue(
	current any,
	parts []string,
) (updated any, removed bool, emptyMap bool) {
	if len(parts) == 0 {
		return current, false, false
	}
	part := parts[0]
	switch node := current.(type) {
	case map[string]any:
		child, found := node[part]
		if !found {
			return node, false, len(node) == 0
		}
		if len(parts) == 1 {
			delete(node, part)
			return node, true, len(node) == 0
		}
		child, removed, childEmpty := unsetDottedValue(child, parts[1:])
		if !removed {
			return node, false, len(node) == 0
		}
		if childEmpty {
			delete(node, part)
		} else {
			node[part] = child
		}
		return node, true, len(node) == 0
	case []any:
		index, err := strconv.Atoi(part)
		if err != nil || index < 0 || index >= len(node) {
			return node, false, false
		}
		if len(parts) == 1 {
			node = append(node[:index], node[index+1:]...)
			return node, true, false
		}
		child, removed, childEmpty := unsetDottedValue(node[index], parts[1:])
		if !removed {
			return node, false, false
		}
		if childEmpty {
			node = append(node[:index], node[index+1:]...)
		} else {
			node[index] = child
		}
		return node, true, false
	default:
		return current, false, false
	}
}

// ValidateDottedKey checks the complete native defaults plus documented open
// containers. Unknown keys remain writable, but callers can display a notice.
func ValidateDottedKey(dotted string) (known bool, suggestion string, err error) {
	parts, err := splitDottedPath(dotted)
	if err != nil {
		return false, "", err
	}
	if strings.HasPrefix(parts[0], "_") {
		return true, "", nil
	}
	defaults, err := loadDefaultConfig()
	if err != nil {
		return false, "", err
	}

	knownRoots := make(map[string]struct{}, len(defaults)+len(openConfigRoots))
	for key := range defaults {
		knownRoots[key] = struct{}{}
	}
	for _, source := range []map[string]struct{}{
		openConfigRoots,
		extensibleConfigRoots,
		dynamicConfigRoots,
	} {
		for key := range source {
			knownRoots[key] = struct{}{}
		}
	}
	knownRoots["platforms"] = struct{}{}

	top := parts[0]
	if _, found := knownRoots[top]; !found {
		return false, closestConfigKey(top, knownRoots), nil
	}
	if top == "platforms" ||
		containsConfigRoot(openConfigRoots, top) ||
		containsConfigRoot(extensibleConfigRoots, top) ||
		containsConfigRoot(dynamicConfigRoots, top) {
		return true, "", nil
	}

	var node any = defaults[top]
	consumed := []string{top}
	for _, part := range parts[1:] {
		if part == "platforms" {
			return true, "", nil
		}
		object, ok := node.(map[string]any)
		if !ok {
			return true, "", nil
		}
		child, found := object[part]
		if !found {
			candidates := make(map[string]struct{}, len(object))
			for candidate := range object {
				candidates[candidate] = struct{}{}
			}
			closest := closestConfigKey(part, candidates)
			if closest != "" {
				return false, strings.Join(
					append(consumed, closest),
					".",
				), nil
			}
			return false, "", nil
		}
		consumed = append(consumed, part)
		node = child
	}
	return true, "", nil
}

func containsConfigRoot(values map[string]struct{}, key string) bool {
	_, found := values[key]
	return found
}

func closestConfigKey(value string, candidates map[string]struct{}) string {
	ordered := make([]string, 0, len(candidates))
	for candidate := range candidates {
		ordered = append(ordered, candidate)
	}
	sort.Strings(ordered)
	best := ""
	bestScore := 0.6
	for _, candidate := range ordered {
		maxLength := max(len([]rune(value)), len([]rune(candidate)))
		if maxLength == 0 {
			continue
		}
		score := 1 - float64(levenshtein(value, candidate))/float64(maxLength)
		if score > bestScore {
			best = candidate
			bestScore = score
		}
	}
	return best
}

func levenshtein(left, right string) int {
	a := []rune(left)
	b := []rune(right)
	previous := make([]int, len(b)+1)
	for index := range previous {
		previous[index] = index
	}
	for row, leftRune := range a {
		current := make([]int, len(b)+1)
		current[0] = row + 1
		for column, rightRune := range b {
			cost := 0
			if leftRune != rightRune {
				cost = 1
			}
			current[column+1] = min(
				current[column]+1,
				previous[column+1]+1,
				previous[column]+cost,
			)
		}
		previous = current
	}
	return previous[len(b)]
}
