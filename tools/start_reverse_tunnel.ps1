$ErrorActionPreference = 'Continue'

$sshPath = 'C:\Program Files\Git\usr\bin\ssh.exe'
$keyPath = 'C:\ProgramData\Sub2APIRelay\sub2api_relay_ed25519'
$knownHostsPath = 'C:\ProgramData\Sub2APIRelay\known_hosts'
$logPath = 'C:\ProgramData\Sub2APIRelay\reverse_tunnel.log'

# 中转目标不写死在脚本里：由环境变量提供，避免把服务器标识提交进仓库。
#   SUB2API_RELAY_TARGET  形如 user@host（必填）
#   SUB2API_RELAY_BIND    远端回连绑定地址，默认 127.0.0.1
$relayTarget = $env:SUB2API_RELAY_TARGET
if ([string]::IsNullOrWhiteSpace($relayTarget)) {
    throw 'SUB2API_RELAY_TARGET is required (format: user@host)'
}
$relayBind = if ([string]::IsNullOrWhiteSpace($env:SUB2API_RELAY_BIND)) { '127.0.0.1' } else { $env:SUB2API_RELAY_BIND }

$sshArguments = @(
    '-N',
    '-T',
    '-i', $keyPath,
    '-o', 'BatchMode=yes',
    '-o', 'StrictHostKeyChecking=accept-new',
    '-o', "UserKnownHostsFile=$knownHostsPath",
    '-o', 'ConnectTimeout=10',
    '-o', 'ConnectionAttempts=1',
    '-o', 'ServerAliveInterval=15',
    '-o', 'ServerAliveCountMax=3',
    '-o', 'ExitOnForwardFailure=yes',
    '-R', "${relayBind}:18080:127.0.0.1:18080",
    $relayTarget
)

function Write-TunnelLog {
    param([string]$Message)

    try {
        if ((Test-Path -LiteralPath $logPath) -and (Get-Item -LiteralPath $logPath).Length -gt 1048576) {
            Set-Content -LiteralPath $logPath -Value '' -Encoding utf8
        }
        Add-Content -LiteralPath $logPath -Value ("{0} {1}" -f (Get-Date -Format 'yyyy-MM-dd HH:mm:ssK'), $Message) -Encoding utf8
    } catch {
        # Logging must never stop the reconnect loop.
    }
}

Write-TunnelLog 'tunnel supervisor started'

while ($true) {
    & $sshPath @sshArguments
    $exitCode = $LASTEXITCODE
    Write-TunnelLog ("ssh exited with code {0}; retrying in 5 seconds" -f $exitCode)
    Start-Sleep -Seconds 5
}
