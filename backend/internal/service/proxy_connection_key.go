package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// ProxyConnectionKey ignores labels and subscription bookkeeping while tracking
// every setting used to establish the proxy connection.
func ProxyConnectionKey(p *Proxy) string {
	if p == nil {
		return ""
	}
	var config any
	var err error
	if strings.EqualFold(p.Kind, "xray") {
		if requiresSingBoxRuntime(p) {
			config, err = buildSingBoxRuntimeSpec(xrayRawNode(p), p)
		} else {
			config, err = buildXrayOutbound(xrayRawNode(p), p)
		}
	} else {
		config = p.StandardURL()
	}
	if err != nil {
		config = p.Extra
	}
	encoded, _ := json.Marshal(struct {
		Config any
		Status string
		Owner  *int64
	}{config, p.Status, p.OwnerUserID})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
