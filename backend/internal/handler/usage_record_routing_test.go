package handler

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUsageRecordTaskKeepsCompositeChannelPricing(t *testing.T) {
	const groupID = int64(42)
	const model = "shared-model"
	openAIPrice, anthropicPrice := 1e-6, 5e-6
	channels := service.NewChannelService(&openAIWSUsageHandlerChannelRepoStub{
		channels: []service.Channel{{ID: 1, Status: service.StatusActive, GroupIDs: []int64{groupID}, ModelPricing: []service.ChannelModelPricing{
			{Platform: service.PlatformOpenAI, Models: []string{model}, InputPrice: &openAIPrice},
			{Platform: service.PlatformAnthropic, Models: []string{model}, InputPrice: &anthropicPrice},
		}}},
		groupPlatforms: map[int64]string{groupID: service.PlatformComposite},
	}, nil, nil, nil, nil)
	for _, tc := range []struct {
		name, target, force string
		want                float64
	}{
		{"resolved target", service.PlatformOpenAI, "", openAIPrice},
		{"forced platform wins", service.PlatformAnthropic, service.PlatformOpenAI, openAIPrice},
		{"no route keeps fallback", "", "", anthropicPrice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			parent = service.WithResolvedTargetPlatform(parent, tc.target)
			if tc.force != "" {
				parent = context.WithValue(parent, ctxkey.ForcePlatform, tc.force)
			}
			checkPrice := func(ctx context.Context) {
				card := channels.GetChannelModelPricing(ctx, groupID, model)
				require.NotNil(t, card)
				require.NotNil(t, card.InputPrice)
				require.Equal(t, tc.want, *card.InputPrice)
			}
			checkPrice(parent)
			task, _ := wrapUsageRecordTaskContext(parent, func(ctx context.Context) {
				require.NoError(t, ctx.Err())
				checkPrice(ctx)
			})
			cancel()
			task(context.Background())
		})
	}
}
