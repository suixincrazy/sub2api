package service

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

type HTTPUpstreamNetworkPolicy struct {
	PublicOnly           bool
	AllowedDialAddresses []string
}

type httpUpstreamNetworkPolicyContextKey struct{}

func WithHTTPUpstreamNetworkPolicy(ctx context.Context, policy HTTPUpstreamNetworkPolicy) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if !policy.PublicOnly {
		return ctx
	}
	policy.AllowedDialAddresses = append([]string(nil), policy.AllowedDialAddresses...)
	return context.WithValue(ctx, httpUpstreamNetworkPolicyContextKey{}, policy)
}

func HTTPUpstreamNetworkPolicyFromContext(ctx context.Context) HTTPUpstreamNetworkPolicy {
	if ctx == nil {
		return HTTPUpstreamNetworkPolicy{}
	}
	policy, _ := ctx.Value(httpUpstreamNetworkPolicyContextKey{}).(HTTPUpstreamNetworkPolicy)
	return policy
}

func (p HTTPUpstreamNetworkPolicy) CacheKey() string {
	if !p.PublicOnly {
		return ""
	}
	allowed := append([]string(nil), p.AllowedDialAddresses...)
	for i := range allowed {
		allowed[i] = strings.ToLower(strings.TrimSpace(allowed[i]))
	}
	sort.Strings(allowed)
	return "public-only:" + strings.Join(allowed, ",")
}

// ProtectUserOwnedUpstreamRequest applies socket-level public-network enforcement
// to requests made with user-owned accounts. The exact local Xray SOCKS listener
// is allowed because its outbound node endpoint is separately validated.
func ProtectUserOwnedUpstreamRequest(req *http.Request, account *Account, proxyURL string) *http.Request {
	if req == nil {
		return req
	}
	policy := HTTPUpstreamNetworkPolicyForAccount(account, proxyURL)
	if !policy.PublicOnly {
		return req
	}
	return req.WithContext(WithHTTPUpstreamNetworkPolicy(req.Context(), policy))
}

func HTTPUpstreamNetworkPolicyForAccount(account *Account, proxyURL string) HTTPUpstreamNetworkPolicy {
	if account == nil || account.OwnerUserID == nil {
		return HTTPUpstreamNetworkPolicy{}
	}
	policy := HTTPUpstreamNetworkPolicy{PublicOnly: true}
	if account.Proxy != nil && strings.EqualFold(account.Proxy.Kind, "xray") {
		if parsed, err := url.Parse(strings.TrimSpace(proxyURL)); err == nil {
			host := parsed.Hostname()
			if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() && parsed.Port() != "" {
				policy.AllowedDialAddresses = []string{net.JoinHostPort(host, parsed.Port())}
			}
		}
	}
	return policy
}
