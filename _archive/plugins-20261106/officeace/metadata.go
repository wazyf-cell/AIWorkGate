package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// staticMeta is one entry of the model catalog that OfficeAce ships inside its
// Electron bundle. The bundle is the only local source for context/vision/
// thinking metadata; the loopback API does not expose them.
type staticMeta struct {
	Name   string
	Vendor string
	Labels []string
}

var (
	staticEntryPattern   = regexp.MustCompile(`"([A-Za-z0-9._:-]{2,128})":\{name:"([^"]*)"`)
	staticLabelsPattern  = regexp.MustCompile(`labels:\[([^\]]*)\]`)
	staticLabelPattern   = regexp.MustCompile(`"([^"]*)"`)
	staticVendorPattern  = regexp.MustCompile(`developer:"([^"]*)"`)
	staticContextPattern = regexp.MustCompile(`(?i)^([0-9]+)([KM])$`)
)

// loadStaticCatalog scans OfficeAce's app.asar for the bundled model metadata
// table. Missing or unreadable bundles yield an empty map; callers keep the
// fields unknown rather than inventing values.
func loadStaticCatalog(home string) map[string]staticMeta {
	if strings.TrimSpace(home) == "" {
		return nil
	}
	var data []byte
	for _, path := range []string{
		filepath.Join(home, "resources", "app.asar"),
		filepath.Join(home, "app.asar"),
	} {
		b, err := os.ReadFile(path)
		if err == nil && len(b) > 0 {
			data = b
			break
		}
	}
	if len(data) == 0 {
		return nil
	}
	out := make(map[string]staticMeta)
	for _, loc := range staticEntryPattern.FindAllSubmatchIndex(data, -1) {
		id := string(data[loc[2]:loc[3]])
		if _, seen := out[id]; seen {
			continue
		}
		end := loc[1] + 4096
		if end > len(data) {
			end = len(data)
		}
		window := data[loc[1]:end]
		labels := staticLabelsPattern.FindSubmatch(window)
		if labels == nil {
			continue
		}
		meta := staticMeta{Name: string(data[loc[4]:loc[5]])}
		if vendor := staticVendorPattern.FindSubmatch(window); vendor != nil {
			meta.Vendor = string(vendor[1])
		}
		for _, raw := range staticLabelPattern.FindAllSubmatch(labels[1], -1) {
			meta.Labels = append(meta.Labels, string(raw[1]))
		}
		out[id] = meta
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func contextFromLabel(label string) (int, bool) {
	m := staticContextPattern.FindStringSubmatch(strings.TrimSpace(label))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	switch strings.ToUpper(m[2]) {
	case "M":
		return n * 1024 * 1024, true
	default:
		return n * 1024, true
	}
}

// applyStaticMetadata fills fields the loopback API leaves unknown. It never
// overwrites live values.
func applyStaticMetadata(models []Model, catalog map[string]staticMeta) []Model {
	if len(catalog) == 0 {
		return models
	}
	for i := range models {
		meta, ok := catalog[models[i].ID]
		if !ok {
			continue
		}
		if (models[i].Name == "" || models[i].Name == models[i].ID) && meta.Name != "" {
			models[i].Name = meta.Name
		}
		if models[i].Vendor == "" {
			models[i].Vendor = meta.Vendor
		}
		for _, label := range meta.Labels {
			switch label {
			case "图像理解":
				if models[i].Vision == nil {
					models[i].Vision = boolPtr(true)
				}
			case "深度思考":
				if len(models[i].Thinking) == 0 {
					models[i].Thinking = []string{"off", "on"}
				}
			default:
				if models[i].ContextLength == 0 {
					if n, ok := contextFromLabel(label); ok {
						models[i].ContextLength = n
					}
				}
			}
		}
	}
	return models
}
