$ErrorActionPreference = 'Stop'
$harkRoot = $PSScriptRoot
$harkURL = 'http://127.0.0.1:8789'
$harkExecutable = Join-Path $harkRoot 'hark2api.exe'
try {
    $harkState = Invoke-RestMethod -Uri "$harkURL/admin/state" -TimeoutSec 2
    if ($harkState.provider.name -ne 'Hark' -or $null -eq $harkState.endpoint) {
        throw 'Port 8789 is occupied by another application.'
    }
} catch {
    $harkListener = Get-NetTCPConnection -LocalPort 8789 -State Listen -ErrorAction SilentlyContinue
    if ($harkListener) {
        throw 'Port 8789 is already occupied. Hark2API was not started.'
    }
    Start-Process -FilePath $harkExecutable -WorkingDirectory $harkRoot -ArgumentList '-addr','127.0.0.1:8789','-upstream-proxy','http://127.0.0.1:7897' -WindowStyle Hidden
    $harkReady = $false
    for ($harkAttempt = 0; $harkAttempt -lt 20; $harkAttempt++) {
        Start-Sleep -Milliseconds 250
        try {
            $harkState = Invoke-RestMethod -Uri "$harkURL/admin/state" -TimeoutSec 2
            if ($harkState.provider.name -eq 'Hark') { $harkReady = $true; break }
        } catch {}
    }
    if (-not $harkReady) { throw 'Hark2API did not become ready.' }
}
Start-Process $harkURL
Write-Output "Hark2API dashboard: $harkURL"
