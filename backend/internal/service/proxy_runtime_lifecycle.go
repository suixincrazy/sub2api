package service

import (
	"strconv"
	"strings"
	"time"
)

const defaultProxyRuntimeIdleTTL = 15 * time.Minute

func parseProxyRuntimeIdleTTL(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultProxyRuntimeIdleTTL
	}
	if raw == "0" {
		return 0
	}
	if duration, err := time.ParseDuration(raw); err == nil && duration > 0 {
		return duration
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return defaultProxyRuntimeIdleTTL
}

func proxyRuntimeIsIdle(lastUsed, now time.Time, idleTTL time.Duration) bool {
	return idleTTL > 0 && !lastUsed.IsZero() && now.Sub(lastUsed) >= idleTTL
}
