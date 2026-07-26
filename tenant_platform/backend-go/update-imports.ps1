$ErrorActionPreference = "Stop"
$backendRoot = "c:\sudy\github\GenericAgent\tenant_platform\backend-go"
$backupRoot = "c:\sudy\github\GenericAgent\tenant_platform\backend-go.bak"

# Restore all .go files from backup to fix encoding corruption
$backupFiles = Get-ChildItem -Recurse -File -Include *.go -Path $backupRoot
$restored = 0
foreach ($bf in $backupFiles) {
    $rel = $bf.FullName.Substring($backupRoot.Length + 1)
    $dest = Join-Path $backendRoot $rel
    Copy-Item -Force $bf.FullName $dest
    $restored++
}
Write-Output "Restored $restored .go files from backup"

# Now re-apply import replacements with UTF-8 encoding preserved
$pkgs = @("postgres","worker","workerclient","transport","ilink","poller","llmproxy","checkpoint","secret","policy","logging","systemd")
$utf8NoBom = New-Object System.Text.UTF8Encoding $false
$files = Get-ChildItem -Recurse -File -Include *.go -Path $backendRoot
$totalReplacements = 0
foreach ($f in $files) {
    $bytes = [System.IO.File]::ReadAllBytes($f.FullName)
    $content = $utf8NoBom.GetString($bytes)
    $original = $content
    foreach ($p in $pkgs) {
        $pattern = "/internal/$p`""
        $replacement = "/internal/infrastructure/$p`""
        $content = $content.Replace($pattern, $replacement)
    }
    if ($content -ne $original) {
        $outBytes = $utf8NoBom.GetBytes($content)
        [System.IO.File]::WriteAllBytes($f.FullName, $outBytes)
        $totalReplacements++
    }
}
Write-Output "Re-modified $totalReplacements files with UTF-8 encoding"
