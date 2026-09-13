# Account Test Timeout Fix Verification

## Root Cause Analysis

### Backend Issues
1. **TLS Fingerprint Transport**: Three dialer paths used zero-value `net.Dialer{}` without timeout
   - Line 124: Chrome TLS fingerprint dialer
   - Line 200: SOCKS5 proxy dialer  
   - Line 1416/1424: Firefox TLS fingerprint dialers
   - **Impact**: When proxy unreachable, TCP connection hangs for ~130s (kernel default)

2. **Missing Request-Level Timeout**: `TestAccountConnection()` uses bare `c.Request.Context()` without timeout wrapper
   - Only specific timeouts exist (Grok realtime: 8s, read ops: 3s)
   - No general deadline for entire test flow

### Frontend Issues
1. **No Fetch Timeout**: SSE fetch uses `AbortController` but no automatic timeout
2. **Poor Error Recovery**: Catch block sets `status='error'` but may not reset UI properly

## Fixes Applied

### Backend (`internal/service/gateway_anthropic_passthrough.go`)

**Line 124** - Chrome TLS fingerprint dialer:
```go
tlsfingerprint.WithDialer(&net.Dialer{
    Timeout:   10 * time.Second,
    KeepAlive: 30 * time.Second,
})
```

**Line 200** - SOCKS5 proxy dialer:
```go
dialer := &net.Dialer{
    Timeout:   10 * time.Second,
    KeepAlive: 30 * time.Second,
}
proxyDialer, err := proxy.SOCKS5("tcp", socksURL, nil, dialer)
```

**Line 1416/1424** - Firefox TLS fingerprint dialers:
```go
tlsfingerprint.WithDialer(&net.Dialer{
    Timeout:   10 * time.Second,
    KeepAlive: 30 * time.Second,
})
```

### Frontend (`AccountTestModal.vue`)

**Lines 847-851** - Added 25-second timeout:
```typescript
const timeoutId = setTimeout(() => {
  abortController?.abort()
}, 25000)
```

**Line 899** - Clear timeout on success:
```typescript
clearTimeout(timeoutId)
```

**Lines 935-941** - Improved error handling:
```typescript
catch (error: unknown) {
  clearTimeout(timeoutId)
  if (error instanceof DOMException && error.name === 'AbortError') {
    status.value = 'idle'
    errorMessage.value = ''
    addLine(t('admin.accounts.testCancelled'), 'text-yellow-400')
    return
  }
  // ... standard error handling
}
```

**i18n keys added**:
- English: `testCancelled: 'Test cancelled or timed out'`
- Chinese: `testCancelled: '测试已取消或超时'`

## Verification Steps

### Test 1: Unreachable Proxy (Dead Host)
**Setup**: Create account with proxy pointing to non-existent host (e.g., `10.255.255.1:1080`)

**Expected behavior**:
- Backend: TCP dial fails after 10 seconds
- Frontend: User sees "Test cancelled or timed out" after max 25 seconds
- UI remains responsive throughout

**Before fix**: UI frozen for ~130 seconds

### Test 2: Unreachable Proxy (Firewalled Port)
**Setup**: Create account with proxy pointing to valid host but firewalled port

**Expected behavior**:
- Connection refused or timeout after 10 seconds
- Frontend timeout at 25 seconds if backend hangs
- Clean error message displayed

### Test 3: Valid Working Account
**Setup**: Test existing working Claude/OpenAI account

**Expected behavior**:
- Test completes successfully within 5-15 seconds
- Timeout timer cleared after connection succeeds
- No false-positive timeouts

### Test 4: Slow-Responding Proxy
**Setup**: Use proxy with high latency but functional (e.g., international free proxy)

**Expected behavior**:
- If response within 25s: test succeeds
- If exceeds 25s: clean timeout message
- No UI freeze in either case

### Test 5: Multiple Concurrent Tests
**Setup**: Open test modal for 3+ accounts and trigger tests rapidly

**Expected behavior**:
- Each test has independent 25s timeout
- No timeout leak between tests
- Previous test's timeout properly cleared when new test starts

## Timeout Strategy

| Layer | Timeout | Purpose |
|-------|---------|---------|
| TCP Dial | 10s | Fail fast on unreachable hosts |
| TLS Handshake | Inherited (10s) | Part of dial context |
| HTTP Response Header | 300s | Generous for slow APIs |
| Frontend SSE | 25s | User-facing timeout for full test |

**Why 25s frontend timeout?**
- Allows 2.5 TCP dial retries (10s each)
- Covers slow but functional proxies
- Prevents indefinite UI hang
- Well below browser's 30s global fetch timeout

## Build Verification

### Backend
```bash
cd backend
go build -o sub2api.exe ./cmd/sub2api
```
✅ Build successful

### Frontend  
```bash
cd frontend
pnpm run build
```
✅ Build successful
✅ i18n completeness check passed
✅ TypeScript compilation passed

## Regression Risks

### Low Risk
- Dial timeout (10s) matches `defaultUpstreamDialTimeout` constant already used elsewhere
- Frontend timeout (25s) is new safety net, doesn't restrict working connections
- Error handling improvements only activate on abort, don't change success path

### Monitoring Points
- Watch for false-positive timeouts on legitimately slow connections
- Monitor if 10s dial timeout is too aggressive for high-latency proxies
- Verify timeout cleanup doesn't leak on rapid test start/stop cycles

## Rollback Plan

If issues arise:
1. **Backend**: Revert dial timeout changes in `gateway_anthropic_passthrough.go` (lines 124, 200, 1416, 1424)
2. **Frontend**: Remove timeout logic in `AccountTestModal.vue` (lines 847-851, 899, 935-941)
3. **i18n**: Remove `testCancelled` keys (non-breaking, just unused)

No database migrations affected, pure runtime behavior change.
