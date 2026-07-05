# Pixel Auth Production Deployment Script
# Usage: .\deploy.ps1

$ServerIP = "212.129.244.194"
$User = "root"
$SSHKey = "C:\Users\15562907296\.ssh\id_rsa_46"
if (-not (Test-Path $SSHKey)) {
    $SSHKey = "C:\Users\ding\.ssh\id_rsa_194"
}
$RemoteDir = "/home/taozi/pixel_auth"
$RemotePath = "/home/taozi/pixel_auth/pixel-auth-linux"
$ServiceName = "pixel-auth"

Write-Host "==============================================" -ForegroundColor Cyan
Write-Host "Starting Pixel Auth Production Deployment..." -ForegroundColor Cyan
Write-Host "==============================================" -ForegroundColor Cyan

# Step 1: Compiling Go binary for Linux
Write-Host "[1/5] Compiling Go binary for Linux (amd64)..." -ForegroundColor Yellow
$oldGoos = $env:GOOS
$oldGoarch = $env:GOARCH
$env:GOOS = "linux"
$env:GOARCH = "amd64"

$GoCmd = "go"
if (-not (Get-Command $GoCmd -ErrorAction SilentlyContinue)) {
    $GoCmd = "C:\Users\ding\go\pkg\mod\golang.org\toolchain@v0.0.1-go1.25.0.windows-amd64\bin\go.exe"
}

& $GoCmd build -ldflags="-s -w" -o pixel-auth-linux .
if ($LASTEXITCODE -ne 0) {
    Write-Host "Error: Compilation failed!" -ForegroundColor Red
    $env:GOOS = $oldGoos
    $env:GOARCH = $oldGoarch
    exit 1
}

# Restore environment variables
$env:GOOS = $oldGoos
$env:GOARCH = $oldGoarch
Write-Host "Compilation successful!" -ForegroundColor Green

# Step 2: Backup and Move remote binary (avoids text file busy errors)
Write-Host "[2/5] Backing up and moving remote binary..." -ForegroundColor Yellow
$timestamp = Get-Date -Format "yyyyMMddHHmmss"
$backupCmd = "if [ -f $RemotePath ]; then mv $RemotePath ${RemotePath}.bak.${timestamp}; fi"
ssh -i $SSHKey "${User}@${ServerIP}" $backupCmd
if ($LASTEXITCODE -ne 0) {
    Write-Host "Warning: Remote backup/move failed. Proceeding..." -ForegroundColor Magenta
} else {
    Write-Host "Remote backup created successfully." -ForegroundColor Green
}

# Step 3: Upload compiled binary to server
Write-Host "[3/5] Uploading new binary to remote server..." -ForegroundColor Yellow
scp -i $SSHKey pixel-auth-linux "${User}@${ServerIP}:${RemotePath}"
if ($LASTEXITCODE -ne 0) {
    Write-Host "Error: Upload failed!" -ForegroundColor Red
    Remove-Item pixel-auth-linux -ErrorAction SilentlyContinue
    exit 1
}
Write-Host "Upload completed successfully!" -ForegroundColor Green

# Step 4: Apply permissions and restart service
Write-Host "[4/5] Chmod and restarting systemd service ($ServiceName)..." -ForegroundColor Yellow
$postDeployCmd = "chmod +x $RemotePath && systemctl restart $ServiceName"
ssh -i $SSHKey "${User}@${ServerIP}" $postDeployCmd
if ($LASTEXITCODE -ne 0) {
    Write-Host "Error: Permission update or service restart failed!" -ForegroundColor Red
    Remove-Item pixel-auth-linux -ErrorAction SilentlyContinue
    exit 1
}
Write-Host "Service $ServiceName restarted successfully!" -ForegroundColor Green

# Step 5: Clean up local binary
Write-Host "[5/5] Cleaning up local compiled binary..." -ForegroundColor Yellow
Remove-Item pixel-auth-linux -ErrorAction SilentlyContinue
Write-Host "Cleanup done!" -ForegroundColor Green

Write-Host "==============================================" -ForegroundColor Green
Write-Host "Deployment completed successfully!" -ForegroundColor Green
Write-Host "==============================================" -ForegroundColor Green
