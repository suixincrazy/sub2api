package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// RegisterUserRoutes 注册用户相关路由（需要认证）
func RegisterUserRoutes(
	v1 *gin.RouterGroup,
	h *handler.Handlers,
	jwtAuth middleware.JWTAuthMiddleware,
	auditLog middleware.AuditLogMiddleware,
	settingService *service.SettingService,
	panelRateLimiter *middleware.PanelRateLimiter,
) {
	authenticated := v1.Group("")
	authenticated.Use(gin.HandlerFunc(jwtAuth))
	authenticated.Use(middleware.BackendModeUserGuard(settingService))
	// 面板全局按用户限流：防止单个账号高频刷接口打爆数据库
	authenticated.Use(panelRateLimiter.Global())
	// 用户管理面变更类操作入审计（含 TOTP 启用/禁用、step-up 验证、密码修改等安全事件）
	authenticated.Use(gin.HandlerFunc(auditLog))
	{
		// 用户接口
		user := authenticated.Group("/user")
		{
			user.GET("/profile", h.User.GetProfile)
			user.PUT("/password", h.User.ChangePassword)
			user.PUT("", h.User.UpdateProfile)
			user.GET("/aff", h.User.GetAffiliate)
			user.POST("/aff/transfer", h.User.TransferAffiliateQuota)
			user.POST("/account-bindings/email/send-code", h.User.SendEmailBindingCode)
			user.POST("/account-bindings/email", h.User.BindEmailIdentity)
			user.DELETE("/account-bindings/:provider", h.User.UnbindIdentity)
			user.POST("/auth-identities/bind/start", h.User.StartIdentityBinding)
			user.GET("/api-keys/:id/usage/daily", panelRateLimiter.Heavy(), h.Usage.GetMyAPIKeyDailyUsage)
			user.GET("/platform-quotas", h.User.GetMyPlatformQuotas)

			// 通知邮箱管理
			notifyEmail := user.Group("/notify-email")
			{
				notifyEmail.POST("/send-code", h.User.SendNotifyEmailCode)
				notifyEmail.POST("/verify", h.User.VerifyNotifyEmail)
				notifyEmail.PUT("/toggle", h.User.ToggleNotifyEmail)
				notifyEmail.DELETE("", h.User.RemoveNotifyEmail)
			}

			// TOTP 双因素认证
			totp := user.Group("/totp")
			{
				totp.GET("/status", h.Totp.GetStatus)
				totp.GET("/verification-method", h.Totp.GetVerificationMethod)
				totp.POST("/send-code", h.Totp.SendVerifyCode)
				totp.POST("/setup", h.Totp.InitiateSetup)
				totp.POST("/enable", h.Totp.Enable)
				totp.POST("/disable", h.Totp.Disable)
				// 敏感操作二次验证：授予当前会话一段时间的 step-up 权限
				totp.POST("/step-up", h.Totp.StepUp)
			}

			passkeys := user.Group("/passkeys")
			{
				passkeys.GET("", h.Passkey.List)
				passkeys.POST("/register/begin", h.Passkey.BeginRegistration)
				passkeys.POST("/register/finish", h.Passkey.FinishRegistration)
				passkeys.PATCH("/:id", h.Passkey.Rename)
				passkeys.DELETE("/:id", h.Passkey.Delete)
			}
		}

		// API Key管理
		keys := authenticated.Group("/keys")
		{
			keys.GET("", h.APIKey.List)
			keys.GET("/:id", h.APIKey.GetByID)
			keys.POST("", h.APIKey.Create)
			keys.PUT("/:id", h.APIKey.Update)
			keys.DELETE("/:id", h.APIKey.Delete)
		}

		// 用户可用分组（非管理员接口）
		groups := authenticated.Group("/groups")
		{
			groups.GET("/available", h.APIKey.GetAvailableGroups)
			groups.GET("/rates", h.APIKey.GetUserGroupRates)
		}

		// 用户可用渠道（非管理员接口）
		channels := authenticated.Group("/channels")
		{
			channels.GET("/available", h.AvailableChannel.List)
		}

		// 使用记录（聚合统计属重查询，叠加更严格的按用户限流）
		usage := authenticated.Group("/usage")
		usage.Use(panelRateLimiter.Heavy())
		{
			usage.GET("", h.Usage.List)
			usage.GET("/errors", h.Usage.ListErrors)
			usage.GET("/errors/:id", h.Usage.GetErrorDetail)
			usage.GET("/:id", h.Usage.GetByID)
			usage.GET("/stats", h.Usage.Stats)
			// User dashboard endpoints
			usage.GET("/dashboard/stats", h.Usage.DashboardStats)
			usage.GET("/dashboard/trend", h.Usage.DashboardTrend)
			usage.GET("/dashboard/models", h.Usage.DashboardModels)
			usage.GET("/dashboard/snapshot-v2", h.Usage.DashboardSnapshotV2)
			usage.POST("/dashboard/api-keys-usage", h.Usage.DashboardAPIKeysUsage)
		}

		// 公告（用户可见）
		announcements := authenticated.Group("/announcements")
		{
			announcements.GET("", h.Announcement.List)
			announcements.POST("/:id/read", h.Announcement.MarkRead)
		}

		// 卡密兑换
		redeem := authenticated.Group("/redeem")
		{
			redeem.POST("", h.Redeem.Redeem)
			redeem.GET("/history", h.Redeem.GetHistory)
		}

		// 用户订阅
		subscriptions := authenticated.Group("/subscriptions")
		{
			subscriptions.GET("", h.Subscription.List)
			subscriptions.GET("/active", h.Subscription.GetActive)
			subscriptions.GET("/progress", h.Subscription.GetProgress)
			subscriptions.GET("/summary", h.Subscription.GetSummary)
			subscriptions.POST("/:id/unsubscribe", h.Subscription.Unsubscribe)
		}

		// 渠道监控（用户只读）
		my := authenticated.Group("/my")
		{
			my.GET("/feature-status", h.MyResources.FeatureStatus)

			groups := my.Group("/groups")
			{
				groups.GET("", h.MyResources.ListGroups)
				groups.POST("", h.MyResources.CreateGroup)
				groups.GET("/live-capability", h.MyResources.GetGroupLiveCapability)
				groups.GET("/usage-summary", h.MyResources.GetGroupUsageSummary)
				groups.GET("/capacity-summary", h.MyResources.GetGroupCapacitySummary)
				groups.GET("/:id", h.MyResources.GetGroup)
				groups.PUT("/:id", h.MyResources.UpdateGroup)
				groups.DELETE("/:id", h.MyResources.DeleteGroup)
				groups.GET("/:id/pool-health", h.MyResources.GetGroupPoolHealth)
				groups.GET("/:id/models-list-candidates", h.MyResources.GetGroupModelsListCandidates)
				groups.GET("/:id/user-overrides", h.MyResources.GetGroupUserOverrides)
				groups.PUT("/:id/rate-multipliers", h.MyResources.SetGroupRateMultipliers)
				groups.DELETE("/:id/rate-multipliers", h.MyResources.ClearGroupRateMultipliers)
				groups.PUT("/:id/rpm-overrides", h.MyResources.SetGroupRPMOverrides)
				groups.DELETE("/:id/rpm-overrides", h.MyResources.ClearGroupRPMOverrides)
			}

			accounts := my.Group("/accounts")
			{
				accounts.GET("", h.MyResources.ListAccounts)
				accounts.POST("", h.MyResources.CreateAccount)
				registerMyAccountModelCapabilityRoutes(accounts, h.MyResources)
				accounts.GET("/export", h.MyResources.ExportAccounts)
				accounts.POST("/import", h.MyResources.ImportAccounts)
				accounts.POST("/import/codex-session", h.MyResources.ImportCodexSessions)
				accounts.POST("/import/codex-pat", h.MyResources.ImportCodexPAT)
				accounts.POST("/batch-update", h.MyResources.BatchUpdateAccounts)
				accounts.POST("/oauth/auth-url", h.MyResources.GenerateAccountOAuthURL)
				accounts.POST("/oauth/exchange", h.MyResources.ExchangeAccountOAuthCode)
				accounts.POST("/oauth/cookie", h.MyResources.ExchangeAccountOAuthCookie)
				accounts.GET("/:id/models", h.MyResources.GetAccountTestModels)
				accounts.POST("/:id/test/stream", h.MyResources.StreamAccountTest)
				accounts.GET("/:id", h.MyResources.GetAccount)
				accounts.PUT("/:id", h.MyResources.UpdateAccount)
				accounts.DELETE("/:id", h.MyResources.DeleteAccount)
				accounts.POST("/:id/test", h.MyResources.TestAccount)
				accounts.POST("/:id/refresh", h.MyResources.RefreshAccount)
				accounts.POST("/:id/clear-error", h.MyResources.ClearAccountError)
				accounts.POST("/:id/schedulable", h.MyResources.SetAccountSchedulable)
			}

			proxies := my.Group("/proxies")
			{
				proxies.GET("", h.MyResources.ListProxies)
				proxies.POST("", h.MyResources.CreateProxy)
				proxies.GET("/export", h.MyResources.ExportProxies)
				proxies.POST("/import", h.MyResources.ImportProxyNodes)
				proxies.GET("/sources", h.MyResources.ListProxySources)
				proxies.POST("/sources", h.MyResources.CreateProxySource)
				proxies.POST("/sources/sync-all", h.MyResources.SyncAllProxySources)
				proxies.PUT("/sources/:id", h.MyResources.UpdateProxySource)
				proxies.DELETE("/sources/:id", h.MyResources.DeleteProxySource)
				proxies.POST("/sources/:id/sync", h.MyResources.SyncProxySource)
				proxies.GET("/:id", h.MyResources.GetProxy)
				proxies.PUT("/:id", h.MyResources.UpdateProxy)
				proxies.DELETE("/:id", h.MyResources.DeleteProxy)
				proxies.POST("/:id/test", h.MyResources.TestProxy)
				proxies.POST("/:id/quality-check", h.MyResources.QualityCheckProxy)
			}

			assigned := my.Group("/assigned-subscriptions")
			{
				assigned.GET("", h.MyResources.ListAssignedSubscriptions)
				assigned.POST("", h.MyResources.AssignSubscription)
				assigned.POST("/bulk", h.MyResources.BulkAssignSubscription)
				assigned.POST("/:id/extend", h.MyResources.ExtendAssignedSubscription)
				assigned.POST("/:id/revoke", h.MyResources.RevokeAssignedSubscription)
				assigned.POST("/:id/restore", h.MyResources.RestoreAssignedSubscription)
				assigned.POST("/:id/reset-usage", h.MyResources.ResetAssignedSubscriptionUsage)
			}

			redeemCodes := my.Group("/redeem-codes")
			{
				redeemCodes.GET("", h.MyResources.ListRedeemCodes)
				redeemCodes.POST("", h.MyResources.GenerateRedeemCodes)
				redeemCodes.GET("/stats", h.MyResources.RedeemCodeStats)
				redeemCodes.GET("/export", h.MyResources.ExportRedeemCodes)
				redeemCodes.POST("/batch-update", h.MyResources.BatchUpdateRedeemCodes)
				redeemCodes.DELETE("", h.MyResources.BatchDeleteRedeemCodes)
				redeemCodes.POST("/batch-expire", h.MyResources.BatchExpireRedeemCodes)
				redeemCodes.GET("/:id/usages", h.MyResources.ListRedeemCodeUsages)
				redeemCodes.DELETE("/:id", h.MyResources.DeleteRedeemCode)
				redeemCodes.POST("/:id/expire", h.MyResources.ExpireRedeemCode)
			}

			usage := my.Group("/usage")
			{
				usage.GET("/account-logs", h.MyResources.ListAccountUsageLogs)
				usage.GET("/account-logs/stats", h.MyResources.GetAccountUsageStats)
				usage.GET("/account-logs/export", h.MyResources.ExportAccountUsageLogs)
				usage.GET("/upstream-errors", h.MyResources.ListUpstreamErrors)
			}
		}

		monitors := authenticated.Group("/channel-monitors")
		{
			monitors.GET("", h.ChannelMonitor.List)
			monitors.GET("/:id/status", h.ChannelMonitor.GetStatus)
		}

		// V2 passive views require feature on + mode=v2.
		monitorV2 := authenticated.Group("/channel-monitor-v2")
		monitorV2.Use(panelRateLimiter.Heavy())
		monitorV2.Use(channelMonitorModeV2Guard(settingService))
		{
			monitorV2.GET("/dimensions", h.ChannelMonitorV2.Dimensions)
			monitorV2.GET("/snapshot", h.ChannelMonitorV2.Snapshot)
			monitorV2.GET("/models", h.ChannelMonitorV2.Models)
			monitorV2.GET("/matrix", h.ChannelMonitorV2.Matrix)
			monitorV2.GET("/errors", h.ChannelMonitorV2.Errors)
			monitorV2.GET("/users", h.ChannelMonitorV2.Users)
		}
	}
}

func registerMyAccountModelCapabilityRoutes(accounts *gin.RouterGroup, h *handler.MyResourceHandler) {
	accounts.POST("/models/sync-upstream-preview", h.SyncAccountUpstreamModelsPreview)
	accounts.GET("/oauth/gemini/capabilities", h.GetGeminiOAuthCapabilities)
	accounts.GET("/antigravity/default-model-mapping", h.GetAntigravityDefaultModelMapping)
	accounts.POST("/:id/models/sync-upstream", h.SyncAccountUpstreamModels)
}
