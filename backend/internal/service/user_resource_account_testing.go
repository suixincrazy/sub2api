package service

import (
	"context"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminicli"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/gin-gonic/gin"
)

// UserAccountTestModel is the non-sensitive model metadata exposed to account owners.
type UserAccountTestModel struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

func (s *UserResourceService) GetAccountTestModels(ctx context.Context, ownerID, accountID int64) ([]UserAccountTestModel, error) {
	item, err := s.GetAccount(ctx, ownerID, accountID)
	if err != nil {
		return nil, err
	}
	account := &Account{
		ID:          accountID,
		Platform:    strings.TrimSpace(urAsString(item["platform"])),
		Type:        strings.TrimSpace(urAsString(item["type"])),
		Credentials: userResourceMap(item["credentials"]),
		Extra:       userResourceMap(item["extra"]),
	}
	return availableUserAccountTestModels(account), nil
}

// StreamAccountTest runs the same SSE test used by account management after
// enforcing ownership at the user-resource boundary.
func (s *UserResourceService) StreamAccountTest(c *gin.Context, ownerID, accountID int64, modelID, prompt, mode string, opts ...AccountTestOptions) error {
	if err := s.ensureOwned(c.Request.Context(), "accounts", ownerID, accountID); err != nil {
		return err
	}
	if s.accountTestService == nil {
		return infraerrors.ServiceUnavailable("ACCOUNT_TEST_UNAVAILABLE", "account test service is not available")
	}
	return s.accountTestService.TestAccountConnection(
		c,
		accountID,
		strings.TrimSpace(modelID),
		strings.TrimSpace(prompt),
		strings.TrimSpace(mode),
		firstAccountTestOptions(opts),
	)
}

func userResourceMap(value any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	return map[string]any{}
}

func availableUserAccountTestModels(account *Account) []UserAccountTestModel {
	if account == nil {
		return []UserAccountTestModel{}
	}

	if account.IsOpenAI() {
		defaults := make([]UserAccountTestModel, 0, len(openai.DefaultModels))
		for _, model := range openai.DefaultModels {
			defaults = append(defaults, UserAccountTestModel{
				ID: model.ID, Type: model.Type, DisplayName: model.DisplayName,
			})
		}
		if account.IsOpenAIPassthroughEnabled() {
			return defaults
		}
		return mappedUserAccountTestModels(account.GetModelMapping(), defaults)
	}

	if account.IsGemini() {
		defaults := make([]UserAccountTestModel, 0, len(geminicli.DefaultModels))
		for _, model := range geminicli.DefaultModels {
			defaults = append(defaults, UserAccountTestModel{
				ID: model.ID, Type: model.Type, DisplayName: model.DisplayName, CreatedAt: model.CreatedAt,
			})
		}
		if account.IsOAuth() {
			return defaults
		}
		return mappedUserAccountTestModels(account.GetModelMapping(), defaults)
	}

	if account.Platform == PlatformAntigravity {
		models := antigravity.DefaultModels()
		out := make([]UserAccountTestModel, 0, len(models))
		for _, model := range models {
			out = append(out, UserAccountTestModel{
				ID: model.ID, Type: model.Type, DisplayName: model.DisplayName, CreatedAt: model.CreatedAt,
			})
		}
		return out
	}

	if account.Platform == PlatformGrok {
		models := xai.DefaultModels()
		defaults := make([]UserAccountTestModel, 0, len(models))
		for _, model := range models {
			defaults = append(defaults, UserAccountTestModel{
				ID: model.ID, Type: "model", DisplayName: model.DisplayName,
			})
		}
		if !hasExplicitUserAccountModelMapping(account.Credentials) {
			return defaults
		}
		return mappedUserAccountTestModels(account.GetModelMapping(), defaults)
	}

	if account.IsCNProvider() {
		defaults := make([]UserAccountTestModel, 0)
		for _, id := range userResourceDefaultModelsListCandidateIDs(account.Platform) {
			defaults = append(defaults, UserAccountTestModel{ID: id, Type: "model", DisplayName: id})
		}
		return mappedUserAccountTestModels(account.GetModelMapping(), defaults)
	}

	defaults := make([]UserAccountTestModel, 0, len(claude.DefaultModels))
	for _, model := range claude.DefaultModels {
		defaults = append(defaults, UserAccountTestModel{
			ID: model.ID, Type: model.Type, DisplayName: model.DisplayName, CreatedAt: model.CreatedAt,
		})
	}
	if account.IsOAuth() {
		return defaults
	}
	return mappedUserAccountTestModels(account.GetModelMapping(), defaults)
}

func mappedUserAccountTestModels(mapping map[string]string, defaults []UserAccountTestModel) []UserAccountTestModel {
	if len(mapping) == 0 {
		return defaults
	}
	byID := make(map[string]UserAccountTestModel, len(defaults))
	for _, model := range defaults {
		byID[model.ID] = model
	}
	ids := make([]string, 0, len(mapping))
	for id := range mapping {
		id = strings.TrimSpace(id)
		if id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	out := make([]UserAccountTestModel, 0, len(ids))
	for _, id := range ids {
		if model, ok := byID[id]; ok {
			out = append(out, model)
			continue
		}
		out = append(out, UserAccountTestModel{ID: id, Type: "model", DisplayName: id})
	}
	return out
}

func hasExplicitUserAccountModelMapping(credentials map[string]any) bool {
	if credentials == nil {
		return false
	}
	switch mapping := credentials["model_mapping"].(type) {
	case map[string]any:
		return len(mapping) > 0
	case map[string]string:
		return len(mapping) > 0
	default:
		return false
	}
}
