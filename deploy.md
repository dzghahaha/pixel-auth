# Pixel Auth 生产环境发布文档

项目提供了自动化的 PowerShell 发布脚本 `deploy.ps1`，支持自动编译、备份、上传、修改权限并重启服务。

## 环境配置与发布步骤

### 一、 快捷发布 (PowerShell)
在项目根目录下打开 PowerShell 终端，直接运行：
```powershell
.\deploy.ps1
```

### 二、 手动发布步骤
如需手动发布，请按以下步骤执行：

1. **本地编译 Linux 目标二进制程序**：
   ```powershell
   # 设置交叉编译环境变量并编译
   $env:GOOS="linux"
   $env:GOARCH="amd64"
   go build -ldflags="-s -w" -o pixel-auth-linux main.go
   ```

2. **备份并移动远程服务器上的当前运行程序**（避免 text file busy 占用错误）：
   使用 SSH 连接到远程服务器，将原二进制重命名备份：
   ```powershell
   ssh -i C:\Users\15562907296\.ssh\id_rsa_46 root@212.129.244.194 "if [ -f /home/taozi/pixel_auth/pixel-auth-linux ]; then mv /home/taozi/pixel_auth/pixel-auth-linux /home/taozi/pixel_auth/pixel-auth-linux.bak.$(date +%Y%m%d%H%M%S); fi"
   ```

3. **上传新编译的二进制文件**：
   使用 SCP 将本地 Linux 二进制文件上传替换服务器上的目标路径：
   ```powershell
   scp -i C:\Users\15562907296\.ssh\id_rsa_46 pixel-auth-linux root@212.129.244.194:/home/taozi/pixel_auth/pixel-auth-linux
   ```

4. **更新程序执行权限并重启 systemd 服务**：
   连接远程服务器更新二进制程序的可执行权限，并通过 systemctl 重启应用程序服务：
   ```powershell
   ssh -i C:\Users\15562907296\.ssh\id_rsa_46 root@212.129.244.194 "chmod +x /home/taozi/pixel_auth/pixel-auth-linux && systemctl restart pixel-auth"
   ```

5. **清理本地临时 Linux 编译文件**：
   ```powershell
   Remove-Item pixel-auth-linux -ErrorAction SilentlyContinue
   ```

## 回滚方案
如果发布后服务异常，可以 SSH 登录服务器，将备份的二进制文件（如 `pixel-auth-linux.bak.20260703120000`）重命名替换回 `/home/taozi/pixel_auth/pixel-auth-linux`，并执行 `systemctl restart pixel-auth` 重启服务进行快速回滚。
