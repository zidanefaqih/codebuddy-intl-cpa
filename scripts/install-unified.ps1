param(
    [Parameter(Mandatory=$true)][string]$CPAHome,
    [Parameter(Mandatory=$true)][string]$AuthDir,
    [string]$DLL = (Join-Path $PSScriptRoot '../workbuddy/dist/windows/amd64/workbuddy.dll'),
    [string]$Migrator = (Join-Path $PSScriptRoot '../workbuddy/dist/windows/amd64/migrate-trae.exe'),
    [switch]$StartCPA
)
$ErrorActionPreference = 'Stop'
$CPAHome = (Resolve-Path -LiteralPath $CPAHome).Path
$AuthDir = (Resolve-Path -LiteralPath $AuthDir).Path
$DLL = (Resolve-Path -LiteralPath $DLL).Path
$Migrator = (Resolve-Path -LiteralPath $Migrator).Path
$exe = Join-Path $CPAHome 'cli-proxy-api.exe'
$config = Join-Path $CPAHome 'config.yaml'
$pluginDir = Join-Path $CPAHome 'plugins/windows/amd64'
$targets = @(Get-ChildItem -LiteralPath $pluginDir -Filter 'workbuddy*.dll')
if ($targets.Count -ne 1) { throw 'Expected exactly one installed workbuddy DLL.' }
$target = $targets[0].FullName
$traeDLL = Join-Path $pluginDir 'trae.dll'
$backup = Join-Path $CPAHome ('backups/unified-' + (Get-Date -Format 'yyyyMMdd-HHmmss') + '-' + [guid]::NewGuid().ToString('N').Substring(0,8))
New-Item -ItemType Directory -Path $backup | Out-Null
$oldProcesses = @(Get-Process -Name 'cli-proxy-api' -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $exe })
foreach ($process in $oldProcesses) { Stop-Process -Id $process.Id -Force; Wait-Process -Id $process.Id -ErrorAction SilentlyContinue }
$restart = $StartCPA -or $oldProcesses.Count -gt 0
$traePresent = Test-Path -LiteralPath $traeDLL
$newProcess = $null
try {
    Copy-Item -LiteralPath $target -Destination (Join-Path $backup 'workbuddy.dll')
    if ($traePresent) { Copy-Item -LiteralPath $traeDLL -Destination (Join-Path $backup 'trae.dll') }
    & $Migrator -auth-dir $AuthDir -config $config -backup-dir (Join-Path $backup 'migration') -apply | Out-File (Join-Path $backup 'migration-report.json') -Encoding utf8
    if ($LASTEXITCODE -ne 0) { throw 'Credential/config migration failed.' }
    Copy-Item -LiteralPath $DLL -Destination $target -Force
    # Move the old library outside the plugin loader's directory. Backups stay recoverable.
    if ($traePresent) { Move-Item -LiteralPath $traeDLL -Destination (Join-Path $backup 'trae-original.dll') }
    if ($restart) {
        $newProcess = Start-Process -FilePath $exe -ArgumentList @('-config', "`"$config`"", '-local-model') -WorkingDirectory $CPAHome -WindowStyle Hidden -PassThru
        Start-Sleep -Seconds 3
        if ($newProcess.HasExited) { throw 'CPA exited after installation; rolling back.' }
    }
    [pscustomobject]@{installed=$target;sha256=(Get-FileHash -LiteralPath $target -Algorithm SHA256).Hash;backup=$backup;started=$restart} | ConvertTo-Json
} catch {
    $failure = $_
    if ($newProcess -and -not $newProcess.HasExited) { Stop-Process -Id $newProcess.Id -Force; Wait-Process -Id $newProcess.Id -ErrorAction SilentlyContinue }
    $manifest = Join-Path $backup 'migration/manifest.json'
    if (Test-Path -LiteralPath $manifest) {
        $saved = Get-Content -LiteralPath $manifest -Raw | ConvertFrom-Json
        foreach ($change in $saved.changes) {
            $absoluteTarget = [System.IO.Path]::GetFullPath($change.target)
            $absoluteBackup = [System.IO.Path]::GetFullPath($change.backup)
            if ($absoluteTarget -ne $config -and -not $absoluteTarget.StartsWith($AuthDir + [System.IO.Path]::DirectorySeparatorChar,[System.StringComparison]::OrdinalIgnoreCase)) { throw 'Unsafe rollback target in manifest.' }
            if (-not $absoluteBackup.StartsWith($backup + [System.IO.Path]::DirectorySeparatorChar,[System.StringComparison]::OrdinalIgnoreCase)) { throw 'Unsafe rollback backup in manifest.' }
            Copy-Item -LiteralPath $absoluteBackup -Destination $absoluteTarget -Force
        }
    }
    if (Test-Path -LiteralPath (Join-Path $backup 'workbuddy.dll')) { Copy-Item -LiteralPath (Join-Path $backup 'workbuddy.dll') -Destination $target -Force }
    if ($traePresent -and (Test-Path -LiteralPath (Join-Path $backup 'trae.dll'))) { Copy-Item -LiteralPath (Join-Path $backup 'trae.dll') -Destination $traeDLL -Force }
    if ($oldProcesses.Count -gt 0) { Start-Process -FilePath $exe -ArgumentList @('-config', "`"$config`"", '-local-model') -WorkingDirectory $CPAHome -WindowStyle Hidden | Out-Null }
    throw $failure
}
