package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

const firstProxyProbeRuntimeID int64 = 1 << 62

type proxyProbeRuntime interface {
	ProxyURL(context.Context, *Proxy) (string, error)
	Stop(int64) error
}

type ProxyProbeURLResolver interface {
	Resolve(context.Context, *Proxy) (string, func(), error)
}

// ProxyProbeRuntimeResolver isolates short-lived connectivity checks from the
// long-lived runtimes used by account traffic.
type ProxyProbeRuntimeResolver struct {
	xray    proxyProbeRuntime
	singBox proxyProbeRuntime
	nextID  atomic.Int64
}

var (
	defaultProxyProbeRuntimeOnce sync.Once
	defaultProxyProbeRuntime     *ProxyProbeRuntimeResolver
)

func DefaultProxyProbeRuntimeResolver() *ProxyProbeRuntimeResolver {
	defaultProxyProbeRuntimeOnce.Do(func() {
		xray := NewXrayRuntimeManager(
			os.Getenv("XRAY_BIN"),
			proxyProbeWorkDir(os.Getenv("XRAY_WORK_DIR"), "sub2api-xray"),
		)
		xray.maxInstances = parseXrayMaxInstances(os.Getenv("XRAY_PROBE_MAX_INSTANCES"))
		xray.maxInstancesPerUser = parseXrayMaxInstancesPerUser(os.Getenv("XRAY_PROBE_MAX_INSTANCES_PER_USER"))
		if xray.maxInstancesPerUser > xray.maxInstances {
			xray.maxInstancesPerUser = xray.maxInstances
		}

		singBox := NewSingBoxRuntimeManager(
			os.Getenv("SING_BOX_BIN"),
			proxyProbeWorkDir(os.Getenv("SING_BOX_WORK_DIR"), "sub2api-sing-box"),
		)
		singBox.maxInstances = parseSingBoxLimit(os.Getenv("SING_BOX_PROBE_MAX_INSTANCES"), defaultSingBoxMaxInstances)
		singBox.maxInstancesPerUser = parseSingBoxLimit(os.Getenv("SING_BOX_PROBE_MAX_INSTANCES_PER_USER"), defaultSingBoxMaxInstancesUser)
		if singBox.maxInstancesPerUser > singBox.maxInstances {
			singBox.maxInstancesPerUser = singBox.maxInstances
		}

		defaultProxyProbeRuntime = NewProxyProbeRuntimeResolver(xray, singBox)
	})
	return defaultProxyProbeRuntime
}

func NewProxyProbeRuntimeResolver(xray, singBox proxyProbeRuntime) *ProxyProbeRuntimeResolver {
	resolver := &ProxyProbeRuntimeResolver{xray: xray, singBox: singBox}
	resolver.nextID.Store(firstProxyProbeRuntimeID)
	return resolver
}

func proxyProbeWorkDir(configured, fallback string) string {
	base := strings.TrimSpace(configured)
	if base == "" {
		base = filepath.Join(os.TempDir(), fallback)
	}
	return filepath.Join(base, "probes")
}

func (r *ProxyProbeRuntimeResolver) Resolve(ctx context.Context, proxy *Proxy) (string, func(), error) {
	noop := func() {}
	if proxy == nil {
		return "", noop, errors.New("proxy is nil")
	}
	if !strings.EqualFold(proxy.Kind, "xray") {
		return proxy.StandardURL(), noop, nil
	}
	if r == nil {
		return "", noop, errors.New("proxy probe runtime resolver is nil")
	}

	runtimeName := "xray"
	runtime := r.xray
	if requiresSingBoxRuntime(proxy) {
		runtimeName = "sing-box"
		runtime = r.singBox
	}
	if runtime == nil {
		return "", noop, fmt.Errorf("%s probe runtime is unavailable", runtimeName)
	}

	temporaryID := r.nextID.Add(1)
	probeProxy := cloneProxyForProbe(proxy, temporaryID)
	var stopOnce sync.Once
	cleanup := func() {
		stopOnce.Do(func() {
			_ = runtime.Stop(temporaryID)
		})
	}

	proxyURL, err := runtime.ProxyURL(ctx, probeProxy)
	if err != nil {
		cleanup()
		return "", noop, fmt.Errorf("%s probe runtime failed: %w", runtimeName, err)
	}
	if strings.TrimSpace(proxyURL) == "" {
		cleanup()
		return "", noop, fmt.Errorf("%s probe runtime returned an empty URL", runtimeName)
	}
	return proxyURL, cleanup, nil
}

func resolveProxyProbeURL(ctx context.Context, resolver ProxyProbeURLResolver, proxy *Proxy) (string, func(), error) {
	if resolver == nil {
		resolver = DefaultProxyProbeRuntimeResolver()
	}
	return resolver.Resolve(ctx, proxy)
}

func cloneProxyForProbe(proxy *Proxy, temporaryID int64) *Proxy {
	clone := *proxy
	clone.ID = temporaryID
	if proxy.OwnerUserID != nil {
		ownerID := *proxy.OwnerUserID
		clone.OwnerUserID = &ownerID
	}
	if proxy.BackupProxyID != nil {
		backupID := *proxy.BackupProxyID
		clone.BackupProxyID = &backupID
	}
	if proxy.ExpiresAt != nil {
		expiresAt := *proxy.ExpiresAt
		clone.ExpiresAt = &expiresAt
	}
	clone.Extra = cloneProxyProbeMap(proxy.Extra)
	return &clone
}

func cloneProxyProbeMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = cloneProxyProbeValue(value)
	}
	return clone
}

func cloneProxyProbeValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneProxyProbeMap(typed)
	case []any:
		clone := make([]any, len(typed))
		for index := range typed {
			clone[index] = cloneProxyProbeValue(typed[index])
		}
		return clone
	case []map[string]any:
		clone := make([]map[string]any, len(typed))
		for index := range typed {
			clone[index] = cloneProxyProbeMap(typed[index])
		}
		return clone
	default:
		return value
	}
}
