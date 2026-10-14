# matrix/hardened: launch only the explicitly verified native build, without changing Codex settings.
[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$BinaryPath,
    [Parameter(Mandatory=$true)][ValidatePattern('^[0-9a-fA-F]{64}$')][string]$ExpectedSHA256,
    [Parameter(Mandatory=$true)][ValidatePattern('^[0-9a-fA-F]{64}$')][string]$ExpectedLauncherSHA256,
    [Parameter(Mandatory=$true)][string]$StateDirectory,
    [Parameter(Mandatory=$true)][string]$WorkingDirectory,
    [switch]$Console
)

$ErrorActionPreference='Stop'
$ProgressPreference='SilentlyContinue'

function Assert-PlainPath([string]$Path,[bool]$Directory) {
    $fullPath=[IO.Path]::GetFullPath($Path)
    $item=Get-Item -LiteralPath $fullPath -Force
    if($item.PSIsContainer -ne $Directory){throw '启动路径的类型已改变，请重新安装。'}
    $cursor=$fullPath
    while($cursor){
        $ancestor=Get-Item -LiteralPath $cursor -Force
        if(($ancestor.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0){throw '启动路径包含重定向，已停止启动。'}
        $parent=[IO.Directory]::GetParent($cursor)
        if($null -eq $parent){break}
        $cursor=$parent.FullName
    }
    return $fullPath
}

function ConvertTo-PSLiteral([string]$Value) {return "'"+$Value.Replace("'","''")+"'"}

try {
    $launcher=Assert-PlainPath $PSCommandPath $false
    if((Get-FileHash -LiteralPath $launcher -Algorithm SHA256).Hash -ine $ExpectedLauncherSHA256){
        throw 'Agent Deck 启动脚本校验失败，请重新安装。'
    }
    $BinaryPath=Assert-PlainPath $BinaryPath $false
    $StateDirectory=Assert-PlainPath $StateDirectory $true
    $WorkingDirectory=Assert-PlainPath $WorkingDirectory $true
    if((Get-FileHash -LiteralPath $BinaryPath -Algorithm SHA256).Hash -ine $ExpectedSHA256){
        throw 'Agent Deck 文件校验失败，请重新安装。'
    }

    # matrix/hardened: only fixed options and Base64 reach wt's command parser;
    # arbitrary paths, spaces, apostrophes and semicolons stay inside PS literals.
    $terminal=Get-Command wt.exe -CommandType Application -ErrorAction SilentlyContinue|Select-Object -First 1
    if(-not $Console -and $terminal){
        # This independent verifier executes in the new terminal before the
        # launcher is loaded, so a changed script cannot validate itself.
        $payload="`$ErrorActionPreference='Stop';if((Get-FileHash -LiteralPath "+(ConvertTo-PSLiteral $launcher)+
            ' -Algorithm SHA256).Hash -ine '+(ConvertTo-PSLiteral $ExpectedLauncherSHA256)+
            "){throw 'Agent Deck 启动脚本校验失败，请重新安装。'};& "+(ConvertTo-PSLiteral $launcher)+
            ' -BinaryPath '+(ConvertTo-PSLiteral $BinaryPath)+
            ' -ExpectedSHA256 '+(ConvertTo-PSLiteral $ExpectedSHA256)+
            ' -ExpectedLauncherSHA256 '+(ConvertTo-PSLiteral $ExpectedLauncherSHA256)+
            ' -StateDirectory '+(ConvertTo-PSLiteral $StateDirectory)+
            ' -WorkingDirectory '+(ConvertTo-PSLiteral $WorkingDirectory)+' -Console'
        $encoded=[Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($payload))
        $powerShellPath=Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
        # The child rechecks the binary before starting the UI. No shell profile,
        # global execution-policy change, scheduled task or resident service.
        try {
            & $terminal.Source '-w' 'new' 'new-tab' '--title' 'Agent Deck 原生版' $powerShellPath '-NoLogo' '-NoProfile' '-ExecutionPolicy' 'Bypass' '-EncodedCommand' $encoded
            if($LASTEXITCODE -eq 0){exit 0}
        } catch {
            # A missing/disabled app execution alias should retain the ordinary
            # console fallback instead of blocking the already verified binary.
        }
        Write-Host 'Windows Terminal 未能打开，正在使用普通控制台。'
    }

    [Console]::OutputEncoding=New-Object Text.UTF8Encoding($false)
    $Host.UI.RawUI.WindowTitle='Agent Deck 原生版'
    Set-Location -LiteralPath $WorkingDirectory
    & $BinaryPath 'ui' '--store' 'statedb' '--tool' 'codex' '--state-dir' $StateDirectory '--cwd' $WorkingDirectory
    $nativeExitCode=$LASTEXITCODE
    if($nativeExitCode -ne 0){throw 'Agent Deck 未能正常启动或退出，请查看上方提示。'}
    exit 0
} catch {
    Write-Host $_.Exception.Message -ForegroundColor Red
    if(-not [Console]::IsInputRedirected){Read-Host '按 Enter 关闭窗口'|Out-Null}
    exit 1
}
