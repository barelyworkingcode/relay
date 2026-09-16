package provider

// SettingField describes a single configurable parameter for a provider.
type SettingField struct {
	Key         string      `json:"key"`
	Label       string      `json:"label"`
	Type        string      `json:"type"` // "number", "boolean", "string", "string[]", "select"
	Default     interface{} `json:"default"`
	Min         *float64    `json:"min,omitempty"`
	Max         *float64    `json:"max,omitempty"`
	Step        *float64    `json:"step,omitempty"`
	Options     []string    `json:"options,omitempty"`
	Placeholder string      `json:"placeholder,omitempty"`
	Hint        string      `json:"hint,omitempty"`
}

// ProviderSettings returns the settings schema for each provider.
func ProviderSettings() map[string][]SettingField {
	piFields := []SettingField{
		{
			Key:     "thinkingLevel",
			Label:   "Thinking Level",
			Type:    "select",
			Default: "medium",
			Options: []string{"off", "minimal", "low", "medium", "high", "xhigh"},
			Hint:    "Reasoning depth for models that support it. xhigh is OpenAI codex-max only.",
		},
	}

	return map[string][]SettingField{
		"claude": {},
		"pi":     piFields,
	}
}
