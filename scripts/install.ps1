$ErrorActionPreference = "Stop"

$AppDir = if ($env:APP_DIR) { $env:APP_DIR } else { "C:\package-down" }
$Repo = if ($env:REPO) { $env:REPO } else { "senhao-xu/package-down" }
$Port = if ($env:PORT) { $env:PORT } else { "3000" }
$Asset = "package-down-windows-x86_64.zip"
$Url = "https://github.com/$Repo/releases/latest/download/$Asset"
$TempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("package-down-" + [System.Guid]::NewGuid().ToString("N"))

New-Item -ItemType Directory -Force -Path $AppDir | Out-Null
New-Item -ItemType Directory -Force -Path $TempDir | Out-Null

try {
    $Archive = Join-Path $TempDir $Asset
    Invoke-WebRequest -Uri $Url -OutFile $Archive
    Expand-Archive -Force $Archive $TempDir
    Copy-Item -Force (Join-Path $TempDir "package-down.exe") (Join-Path $AppDir "package-down.exe")

    $env:PORT = $Port
    $OutLogPath = Join-Path $AppDir "package-down.out.log"
    $ErrLogPath = Join-Path $AppDir "package-down.err.log"
    Start-Process -FilePath (Join-Path $AppDir "package-down.exe") -WorkingDirectory $AppDir -WindowStyle Hidden -RedirectStandardOutput $OutLogPath -RedirectStandardError $ErrLogPath

    Write-Host "Package Proxy 已安装并启动: http://localhost:$Port"
    Write-Host "安装目录: $AppDir"
    Write-Host "日志文件: $ErrLogPath"
} finally {
    Remove-Item -Recurse -Force $TempDir -ErrorAction SilentlyContinue
}
