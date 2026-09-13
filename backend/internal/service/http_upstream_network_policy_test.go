package service

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func TestProtectUserOwnedUpstreamRequest(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	ownerID := int64(7)
	protected := ProtectUserOwnedUpstreamRequest(req, &Account{OwnerUserID: &ownerID}, "")
	if policy := HTTPUpstreamNetworkPolicyFromContext(protected.Context()); !policy.PublicOnly {
		t.Fatal("user-owned account request was not protected")
	}

	system := ProtectUserOwnedUpstreamRequest(req, &Account{}, "")
	if policy := HTTPUpstreamNetworkPolicyFromContext(system.Context()); policy.PublicOnly {
		t.Fatal("system account request unexpectedly received user network policy")
	}
}

func TestAccountTestAllowsCustomBaseURLWhenGlobalAllowlistIsDisabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true

	svc := &AccountTestService{cfg: cfg}
	got, err := svc.validateUpstreamBaseURL("https://www.fastaitoken.com/")
	if err != nil {
		t.Fatalf("custom public base URL was rejected: %v", err)
	}
	if got != "https://www.fastaitoken.com" {
		t.Fatalf("normalized base URL = %q, want %q", got, "https://www.fastaitoken.com")
	}
}
