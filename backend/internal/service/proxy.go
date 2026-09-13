package service

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	FallbackModeNone   = "none"
	FallbackModeProxy  = "proxy"
	FallbackModeDirect = "direct"
)

type Proxy struct {
	ID             int64
	Name           string
	OwnerUserID    *int64
	IsPublic       bool
	Kind           string
	Protocol       string
	Host           string
	Port           int
	Username       string
	Password       string
	Status         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ExpiresAt      *time.Time
	FallbackMode   string
	BackupProxyID  *int64
	ExpiryWarnDays int
	Extra          map[string]any
}

func (p *Proxy) IsActive() bool {
	return p.Status == StatusActive
}

// IsExpired reports whether the proxy has expired based on expires_at, independent of status.
func (p *Proxy) IsExpired(now time.Time) bool {
	return p.ExpiresAt != nil && !p.ExpiresAt.After(now)
}

func (p *Proxy) URL() string {
	resolved, err := p.ResolveURL(context.Background())
	if err == nil && resolved != "" {
		return resolved
	}
	if p != nil && strings.EqualFold(p.Kind, "xray") {
		if requiresSingBoxRuntime(p) {
			return "sing-box://unavailable/" + strconv.FormatInt(p.ID, 10)
		}
		return "xray://unavailable/" + strconv.FormatInt(p.ID, 10)
	}
	return ""
}

// ResolveURL returns the concrete HTTP-client-compatible proxy URL. Unlike
// URL, it preserves runtime startup errors so connectivity tests can exercise
// the same long-lived runtime used by account traffic.
func (p *Proxy) ResolveURL(ctx context.Context) (string, error) {
	if p == nil {
		return "", errors.New("proxy is nil")
	}
	if !strings.EqualFold(p.Kind, "xray") {
		return p.StandardURL(), nil
	}
	if requiresSingBoxRuntime(p) {
		return DefaultSingBoxRuntimeManager().ProxyURL(ctx, p)
	}
	return DefaultXrayRuntimeManager().ProxyURL(ctx, p)
}

func (p *Proxy) StandardURL() string {
	if p == nil {
		return ""
	}
	u := &url.URL{
		Scheme: canonicalStandardProxyProtocol(p.Protocol),
		Host:   net.JoinHostPort(p.Host, strconv.Itoa(p.Port)),
	}
	if p.Username != "" && p.Password != "" {
		u.User = url.UserPassword(p.Username, p.Password)
	}
	return u.String()
}

func canonicalStandardProxyProtocol(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "socks":
		return "socks5h"
	default:
		return strings.ToLower(strings.TrimSpace(protocol))
	}
}

type ProxyWithAccountCount struct {
	Proxy
	AccountCount   int64
	LatencyMs      *int64
	LatencyStatus  string
	LatencyMessage string
	IPAddress      string
	Country        string
	CountryCode    string
	Region         string
	City           string
	QualityStatus  string
	QualityScore   *int
	QualityGrade   string
	QualitySummary string
	QualityChecked *int64
}

type ProxyAccountSummary struct {
	ID       int64
	Name     string
	Platform string
	Type     string
	Notes    *string
}
