<#
GenericAgent Desktop — portable uninstall (Windows).

Removes everything THIS portable bundle put on the machine, then deletes the
bundle folder itself:
  1. Stop the bundle's backend processes (bridge 14168 / conductor 8900 / scheduler)
     — only processes whose executable lives inside this bundle, so other bundles
     on the same machine are left alone.
  2. Remove the desktop shortcut (GenericAgent.lnk) — only when it points into
     this bundle.
  3. Remove ~/.ga_desktop_settings.json (shared settings; other bundles rebuild it
     automatically on next launch).
  4. Schedule deletion of the bundle folder after this script exits (a folder
     cannot delete itself while code runs inside it).

Invoked by uninstall.bat (which confirms with the user first). Not meant to be
run directly without -BundleDir.
#>

param(
    [Parameter(Mandatory = $true)]
    [string]$BundleDir
)

$ErrorActionPreference = "SilentlyContinue"

function Write-Step([string]$m) { Write-Host "==> $m" -ForegroundColor Cyan }
function Write-Ok([string]$m)   { Write-Host "[OK] $m" -ForegroundColor Green }
function Write-Info([string]$m) { Write-Host "     $m" -ForegroundColor Gray }

# Normalize the bundle dir to an absolute path with a trailing separator for prefix checks.
try { $bundle = (Resolve-Path -LiteralPath $BundleDir).Path } catch { $bundle = $BundleDir }
$bundlePrefix = ($bundle.TrimEnd('\') + '\').ToLowerInvariant()

function Path-IsInsideBundle([string]$p) {
    if (-not $p) { return $false }
    try { $rp = (Resolve-Path -LiteralPath $p -ErrorAction Stop).Path } catch { $rp = $p }
    return $rp.ToLowerInvariant().StartsWith($bundlePrefix)
}

# ── 1. Graceful backend shutdown, then force-kill bundle-owned processes ──────
Write-Step "Stopping GenericAgent backend services"

# Best-effort graceful exit: tell the bridge to stop its managed extras and quit.
try {
    Invoke-WebRequest -Uri "http://127.0.0.1:14168/services/bridge/exit" -Method Post `
        -TimeoutSec 3 -UseBasicParsing | Out-Null
    Start-Sleep -Milliseconds 800
} catch { }

# Force-kill anything still listening on our ports, but ONLY if the owning process
# executable lives inside this bundle (don't disturb a second installed copy).
foreach ($port in 14168, 8900) {
    foreach ($conn in (Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue)) {
        $proc = Get-Process -Id $conn.OwningProcess -ErrorAction SilentlyContinue
        if ($proc -and (Path-IsInsideBundle $proc.Path)) {
            Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
            Write-Info "killed PID $($proc.Id) ($($proc.ProcessName)) on port $port"
        } elseif ($proc) {
            Write-Info "port $port held by a process outside this bundle (PID $($proc.Id)); left running"
        }
    }
}

# Kill any remaining GenericAgent.exe / python.exe whose image is inside this bundle
# (e.g. the desktop shell itself, or a child python not bound to the two ports).
foreach ($p in (Get-Process -ErrorAction SilentlyContinue | Where-Object {
            $_.Name -in @('GenericAgent', 'python', 'pythonw') })) {
    if (Path-IsInsideBundle $p.Path) {
        Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
        Write-Info "killed PID $($p.Id) ($($p.ProcessName))"
    }
}
Write-Ok "backend stopped"

# ── 2. Desktop shortcut (only if it targets this bundle) ─────────────────────
Write-Step "Removing desktop shortcut"
$desktop = [Environment]::GetFolderPath('Desktop')
$lnk = Join-Path $desktop 'GenericAgent.lnk'
if (Test-Path -LiteralPath $lnk) {
    $target = $null
    try {
        $ws = New-Object -ComObject WScript.Shell
        $target = $ws.CreateShortcut($lnk).TargetPath
    } catch { }
    if ((-not $target) -or (Path-IsInsideBundle $target)) {
        Remove-Item -LiteralPath $lnk -Force -ErrorAction SilentlyContinue
        Write-Ok "removed $lnk"
    } else {
        Write-Info "desktop shortcut points to another bundle; left in place"
    }
} else {
    Write-Info "no desktop shortcut found"
}

# ── 3. Shared settings file ──────────────────────────────────────────────────
Write-Step "Removing settings file"
$settings = Join-Path $env:USERPROFILE '.ga_desktop_settings.json'
if (Test-Path -LiteralPath $settings) {
    Remove-Item -LiteralPath $settings -Force -ErrorAction SilentlyContinue
    Write-Ok "removed $settings"
} else {
    Write-Info "no settings file found"
}

# ── 4. Schedule deletion of the bundle folder ────────────────────────────────
# The folder cannot remove itself while this script runs from inside it. Spawn a
# detached cmd that waits for our process tree to exit, then deletes the folder.
Write-Step "Scheduling removal of the bundle folder"
$cmd = "cd /d `"$env:TEMP`" & ping 127.0.0.1 -n 3 >nul & rd /s /q `"$bundle`""
Start-Process -FilePath "cmd.exe" -ArgumentList "/c", $cmd -WindowStyle Hidden | Out-Null
Write-Ok "bundle folder will be deleted after exit: $bundle"

Write-Host ""
Write-Host "GenericAgent has been uninstalled." -ForegroundColor Green
