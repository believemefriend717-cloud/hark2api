$ErrorActionPreference = 'Stop'
$harkExecutable = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot 'hark2api.exe'))
$harkListeners = Get-NetTCPConnection -LocalPort 8789 -State Listen -ErrorAction SilentlyContinue
foreach ($harkListener in $harkListeners) {
    $harkProcess = Get-CimInstance Win32_Process -Filter "ProcessId = $($harkListener.OwningProcess)"
    if ($harkProcess.ExecutablePath -ne $harkExecutable) {
        throw 'The process on port 8789 does not match this Hark2API installation.'
    }
    Stop-Process -Id $harkListener.OwningProcess
    Write-Output 'Hark2API stopped.'
}
if (-not $harkListeners) { Write-Output 'Hark2API is not running.' }
