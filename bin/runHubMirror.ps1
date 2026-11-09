<#
.SYNOPSIS
    Runs a Hub Mirror: a Hub Proxy that eagerly keeps a complete copy of the
    upstream instead of only caching what's requested.

.DESCRIPTION
    Defaults (-Addr :8082, -Root data/mirror-repo, -Upstream
    http://localhost:8080) are chosen so runHub.ps1, runHubProxy.ps1, and
    runHubMirror.ps1 can all run at once on one machine without colliding
    on port or repo root -- pointing at a runHub.ps1 started on this same
    machine. On a dedicated machine, override -Addr/-Root/-Upstream freely.

    Any extra arguments are passed straight through to the server, e.g.:
        ./runHubMirror.ps1 -mirror-concurrency 16
#>
param(
    [string]$Addr = ":8082",
    [string]$Root = "data/mirror-repo",
    [string]$Upstream = "http://localhost:8080",
    [string]$UpstreamUser = "",
    [string]$UpstreamPass = "",
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$ExtraArgs
)

$repoDir = Resolve-Path "$PSScriptRoot/.."

$goArgs = @("run", ".", "-addr", $Addr, "-root", $Root, "-upstream", $Upstream, "-mirror")
if ($UpstreamUser) { $goArgs += @("-upstream-user", $UpstreamUser) }
if ($UpstreamPass) { $goArgs += @("-upstream-pass", $UpstreamPass) }
if ($ExtraArgs) { $goArgs += $ExtraArgs }

Push-Location $repoDir
try {
    & go @goArgs
} finally {
    Pop-Location
}
