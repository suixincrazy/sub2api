import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

// The pattern is local to the component; extract it from the source so this
// regression test exercises the same validation used by batch import.
const source = readFileSync(
  resolve(process.cwd(), 'src/views/admin/ProxiesView.vue'),
  'utf8'
)

function extractRegex(): RegExp {
  const match = source.match(/const MODERN_PROXY_URI_PATTERN = (\/[^\r\n]+\/i)/)
  expect(match, 'MODERN_PROXY_URI_PATTERN not found in ProxiesView.vue').toBeTruthy()

  const literal = (match as RegExpMatchArray)[1]
  const closingSlash = literal.lastIndexOf('/')
  return new RegExp(literal.slice(1, closingSlash), literal.slice(closingSlash + 1))
}

describe('proxy batch URI validation (IPv6 support)', () => {
  const regex = extractRegex()

  it.each([
    ['socks5://[2001:db8::1]:1080', true],
    ['socks5h://[2001:db8::1]:1080', true],
    ['http://[::1]:8080', true],
    ['socks5://user:pass@[2001:db8::1]:1080', true],
    ['socks5://proxy.example.com:1080', true],
    ['http://192.168.1.1:8080', true],
    ['socks5://user:pass@proxy.example.com:1080', true],
    ['vmess://encoded-payload', true],
    ['naive+https://proxy.example.com', true],
    ['wireguard://configuration', true],
    // Validation only accepts supported schemes and a non-space payload.
    ['ftp://example.com:21', false],
    ['socks5://', false],
    ['socks5://proxy.example.com:1080 extra', false]
  ])('%s => %s', (line, expected) => {
    expect(regex.test(line)).toBe(expected)
  })
})
