<#
.SYNOPSIS
    Runs a standalone Hub (Maven repo server).

.DESCRIPTION
    Defaults (-Addr :8080, -Root data/hub-repo, proxies allowed) are chosen
    so runHub.ps1, runHubProxy.ps1, and runHubMirror.ps1 can all run at once
    on one machine without colliding on port or repo root. On a dedicated
    machine, override -Addr/-Root freely -- there's nothing else on that
    host to collide with.

    Any extra arguments are passed straight through to the server, e.g.:
        ./runHub.ps1 -read-auth -max-upload-size 0
#>
param(
    [string]$Addr = ":8080",
    [string]$Root = "data/hub-repo",
    [string]$User = "",
    [string]$Pass = "",
    [switch]$NoAllowProxies,
    [switch]$ReadOnly,
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$ExtraArgs
)

$repoDir = Resolve-Path "$PSScriptRoot/.."

$goArgs = @("run", ".", "-addr", $Addr, "-root", $Root)
if (-not $NoAllowProxies) { $goArgs += "-allow-proxies" }
if ($ReadOnly) { $goArgs += "-read-only" }
if ($User) { $goArgs += @("-user", $User) }
if ($Pass) { $goArgs += @("-pass", $Pass) }
if ($ExtraArgs) { $goArgs += $ExtraArgs }

Push-Location $repoDir
try {
    & go @goArgs
} finally {
    Pop-Location
}
