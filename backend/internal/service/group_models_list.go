package service

import "strings"

func normalizeGroupModelsListConfig(cfg GroupModelsListConfig) GroupModelsListConfig {
	normalized := GroupModelsListConfig{
		ShowAllModels: cfg.ShowAllModels,
	}

	if len(cfg.HiddenModels) > 0 {
		normalized.HiddenModels = make([]string, 0, len(cfg.HiddenModels))
		for _, model := range cfg.HiddenModels {
			model = strings.TrimSpace(model)
			if model != "" {
				normalized.HiddenModels = append(normalized.HiddenModels, model)
			}
		}
		if len(normalized.HiddenModels) == 0 {
			normalized.HiddenModels = nil
		}
	}

	if len(cfg.VisibleModels) > 0 {
		normalized.VisibleModels = make([]string, 0, len(cfg.VisibleModels))
		for _, model := range cfg.VisibleModels {
			model = strings.TrimSpace(model)
			if model != "" {
				normalized.VisibleModels = append(normalized.VisibleModels, model)
			}
		}
		if len(normalized.VisibleModels) == 0 {
			normalized.VisibleModels = nil
		}
	}

	return normalized
}
