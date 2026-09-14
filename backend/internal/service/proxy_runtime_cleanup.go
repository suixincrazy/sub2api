package service

import (
	"errors"
	"fmt"
	"time"
)

const proxyRuntimeStopAttempts = 3

func CloseProxyRuntimes() error {
	errs := []error{
		DefaultXrayRuntimeManager().Close(),
		DefaultSingBoxRuntimeManager().Close(),
	}
	if resolver := defaultProxyProbeRuntime; resolver != nil {
		for _, runtime := range []proxyProbeRuntime{resolver.xray, resolver.singBox} {
			if closer, ok := runtime.(interface{ Close() error }); ok {
				errs = append(errs, closer.Close())
			}
		}
	}
	return errors.Join(errs...)
}

func stopProxyRuntimesWithRetry(proxyID int64) error {
	if proxyID <= 0 {
		return nil
	}
	var lastErr error
	for attempt := 1; attempt <= proxyRuntimeStopAttempts; attempt++ {
		xrayErr := DefaultXrayRuntimeManager().Stop(proxyID)
		singBoxErr := DefaultSingBoxRuntimeManager().Stop(proxyID)
		if xrayErr == nil && singBoxErr == nil {
			return nil
		}
		lastErr = errors.Join(xrayErr, singBoxErr)
		if attempt < proxyRuntimeStopAttempts {
			time.Sleep(time.Duration(attempt) * 50 * time.Millisecond)
		}
	}
	return fmt.Errorf("stop proxy runtime %d after %d attempts: %w", proxyID, proxyRuntimeStopAttempts, lastErr)
}
