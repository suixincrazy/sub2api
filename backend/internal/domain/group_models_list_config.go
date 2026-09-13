package domain

type GroupModelsListConfig struct {
	ShowAllModels  bool     `json:"show_all_models"`
	HiddenModels   []string `json:"hidden_models,omitempty"`
	VisibleModels  []string `json:"visible_models,omitempty"`
}
