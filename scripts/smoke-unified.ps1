param([string]$CPAExe='D:\APP\AI\CLIProxyAPI_8.0.23_windows_amd64\cli-proxy-api.exe')
$ErrorActionPreference='Stop'
$root=Split-Path $PSScriptRoot -Parent
$smoke=Join-Path $env:TEMP ('cpa-unified-smoke-'+[guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path (Join-Path $smoke 'auths') -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $smoke 'plugins/windows/amd64') -Force | Out-Null
Copy-Item -LiteralPath (Join-Path $root 'workbuddy/dist/windows/amd64/workbuddy.dll') -Destination (Join-Path $smoke 'plugins/windows/amd64/workbuddy.dll')
$listener=[Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback,0);$listener.Start();$port=$listener.LocalEndpoint.Port;$listener.Stop()
$secret=[guid]::NewGuid().ToString('N')
$yaml=@"
config-version: 8
server:
  host: '127.0.0.1'
  port: $port
management:
  allow-remote: false
  secret-key: '$secret'
  disable-control-panel: true
access:
  api-keys: ['fixture-api-key']
oauth:
  auth-dir: '$($smoke.Replace('\','/'))/auths'
plugins:
  enabled: true
  dir: '$($smoke.Replace('\','/'))/plugins'
  configs:
    workbuddy:
      enabled: true
      checkin_auto: false
      token_keepalive: false
      lifecycle_auto: false
      daily_bonus: false
      trae_checkin_auto: false
      trae_token_keepalive: false
      trae_callback_port: 0
"@
$config=Join-Path $smoke 'config.yaml';[IO.File]::WriteAllText($config,$yaml,[Text.UTF8Encoding]::new($false))
$p=Start-Process -FilePath $CPAExe -ArgumentList @('-config',"`"$config`"",'-local-model') -WorkingDirectory $smoke -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $smoke 'stdout.log') -RedirectStandardError (Join-Path $smoke 'stderr.log')
try{
    $origin="http://127.0.0.1:$port";$headers=@{Authorization="Bearer $secret"};$ready=$false
    for($i=0;$i -lt 50;$i++){
        if($p.HasExited){throw "CPA exited. See $smoke"}
        try{$plugins=Invoke-RestMethod "$origin/v0/management/plugins" -Headers $headers -TimeoutSec 2;$ready=$true;break}catch{Start-Sleep -Milliseconds 250}
    }
    if(-not $ready){throw "CPA unavailable. See $smoke"}
    $wb=@($plugins.plugins | Where-Object {$_.id -eq 'workbuddy'})
    if($wb.Count -ne 1 -or -not $wb[0].registered -or -not $wb[0].effective_enabled){throw 'Unified plugin did not register.'}
    $settings=Invoke-RestMethod "$origin/v0/management/plugins/workbuddy/trae/settings" -Headers $headers -TimeoutSec 5
    if($settings.checkin_auto -or $settings.token_keepalive -or $settings.callback_port -ne 0){throw 'Trae namespaced config failed.'}
    $overview=Invoke-RestMethod "$origin/v0/management/plugins/workbuddy/combined/accounts" -Headers $headers -TimeoutSec 5
    if($overview.trae.accounts.Count -ne 0){throw 'Fixture auth directory is not empty.'}
    foreach($resource in @('panel','codebuddy')){$panel=Invoke-WebRequest "$origin/v0/resource/plugins/workbuddy/$resource" -UseBasicParsing -TimeoutSec 5;if($panel.StatusCode -ne 200){throw "Resource $resource failed."}}
    $unauthorized=$false
    try{Invoke-RestMethod "$origin/v0/management/plugins/workbuddy/trae/accounts" -TimeoutSec 5 | Out-Null}catch{$unauthorized=$_.Exception.Response.StatusCode.value__ -eq 401}
    if(-not $unauthorized){throw 'Management route accepted unauthenticated request.'}
    $models=Invoke-RestMethod "$origin/v1/models" -Headers @{Authorization='Bearer fixture-api-key'} -TimeoutSec 5
    "PASS: CPA 8.0.23 loaded one unified DLL; registration, config isolation, overview, both panels and management authentication. Logs: $smoke"
}finally{if(-not $p.HasExited){Stop-Process -Id $p.Id -Force}}
