package main

func (p *Plugin) modelByName(id string) (Model, bool) {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	for _, m := range p.models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// modelVision reports (supported, known).
func (p *Plugin) modelVision(id string) (bool, bool) {
	if m, ok := p.modelByName(id); ok && m.Vision != nil {
		return *m.Vision, true
	}
	return false, false
}

func modelWireObjects(models []Model, created int64) []map[string]any {
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		obj := map[string]any{
			"id":       m.ID,
			"object":   "model",
			"created":  created,
			"owned_by": "officeace",
			"name":     firstNonEmpty(m.Name, m.ID),
		}
		if m.ContextLength > 0 {
			obj["context_length"] = m.ContextLength
		}
		if m.Tools != nil {
			obj["tools"] = *m.Tools
		}
		if m.Vision != nil {
			obj["vision"] = *m.Vision
		}
		if m.Multiplier > 0 {
			obj["multiplier"] = m.Multiplier
		}
		if m.Vendor != "" {
			obj["vendor"] = m.Vendor
		}
		if len(m.Thinking) > 0 {
			obj["thinking"] = m.Thinking
		}
		if m.Free {
			obj["free"] = true
		}
		out = append(out, obj)
	}
	return out
}
