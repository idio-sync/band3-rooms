# Copies the status page and its Caddyfile to the band3-rooms VM and reloads Caddy.
#
#   .\site\deploy.ps1            # page, songs.json and Caddyfile
#   .\site\deploy.ps1 -PageOnly  # page and songs.json only; Caddy isn't touched
#
# Needs the Google Cloud SDK, logged in with access to the project, and Caddy
# already installed on the VM (site/README.md). Remote commands go in --command:
# on Windows, anything after -- makes gcloud fall back to putty.exe, which fails.

[CmdletBinding()]
param(
    [switch]$PageOnly,
    [string]$Instance = "band3-rooms",
    [string]$Zone = "us-east1-c",
    [string]$Project = "kasm-auth-500514"
)

$ErrorActionPreference = "Stop"
$gcloud = "$env:LOCALAPPDATA\google-cloud-sdk-install\google-cloud-sdk\bin\gcloud.cmd"
if (-not (Test-Path $gcloud)) { throw "gcloud not found at $gcloud" }
$site = $PSScriptRoot
$common = @("--zone", $Zone, "--project", $Project, "--tunnel-through-iap")

function Invoke-Gcloud {
    & $gcloud @args
    if ($LASTEXITCODE -ne 0) { throw "gcloud failed ($LASTEXITCODE): $args" }
}

# check songs.json parses before shipping it
Get-Content -Raw -Encoding UTF8 "$site\songs.json" | ConvertFrom-Json | Out-Null

# into a staging folder first (scp can't write to root's directories), then into place
$stage = "/tmp/band3-site-deploy"
Invoke-Gcloud compute ssh $Instance @common --command "rm -rf $stage && mkdir -p $stage"

$files = @("$site\index.html", "$site\songs.json")
if (-not $PageOnly) { $files += "$site\Caddyfile" }
Invoke-Gcloud compute scp @files "${Instance}:$stage/" @common

$remote = @(
    "set -e",
    "sudo install -d -m 755 /var/www/band3-site",
    "sudo install -m 644 $stage/index.html $stage/songs.json /var/www/band3-site/"
)
if (-not $PageOnly) {
    $remote += @(
        "sudo caddy validate --config $stage/Caddyfile --adapter caddyfile",
        "sudo cp /etc/caddy/Caddyfile /etc/caddy/Caddyfile.bak 2>/dev/null || true",
        "sudo install -m 644 $stage/Caddyfile /etc/caddy/Caddyfile",
        "sudo systemctl reload caddy"
    )
}
$remote += "rm -rf $stage"
Invoke-Gcloud compute ssh $Instance @common --command ($remote -join " && ")

Write-Host "Deployed. Check https://rockband.idiosynchronic.com/ and /api/stats, /api/rooms."
