package config

import (
	"encoding/json"
	"maps"
	"slices"

	"github.com/mrlhansen/idrac_exporter/internal/log"
)

type DiscoverItem struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels,omitempty"`
}

func GetDiscover() string {
	var list []DiscoverItem
	for t := range Config.Hosts {
		if t == "default" {
			continue
		}
		list = append(list, DiscoverItem{
			Targets: []string{t},
		})
	}

	// Targets of groups are returned with the group as label and as "group"
	// query parameter (via the special __param_ label), which selects the
	// credentials of the group when the target is scraped.
	for _, name := range slices.Sorted(maps.Keys(Config.Groups)) {
		g := Config.Groups[name]
		if len(g.TargetList) == 0 {
			continue
		}
		list = append(list, DiscoverItem{
			Targets: g.TargetList,
			Labels: map[string]string{
				"group":         name,
				"__param_group": name,
			},
		})
	}

	if len(list) == 0 {
		return "[]"
	}

	b, err := json.Marshal(list)
	if err != nil {
		log.Error("Failed to marshal json: %v", err)
		return "[]"
	}

	return string(b)
}
