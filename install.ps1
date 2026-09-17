# install.ps1 - Install Vectrify Agent Runner as a Windows Service
#
# One-liner (run from an Administrator PowerShell):
#   iwr -useb https://github.com/kevin4885/vectrify-agent-runner/releases/latest/download/install.ps1 | iex
#
# To install a second instance for a different Vectrify account:
#   .\install.ps1 -InstanceName Account2
#
# To update an existing install's key and restart the service (fallback for
# when the runner is offline during key rotation — if it's online, rotating
# a key in the Vectrify UI applies live with no action needed here):
#   .\install.ps1 -SetKey vrun_...

param(
    [string]$InstanceName = "",
    [string]$SetKey = ""
)

[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

function Install-VectrifyRunner {

    $GITHUB_REPO    = "kevin4885/vectrify-agent-runner"

    # When InstanceName is provided, all names and paths get a suffix so multiple
    # instances can coexist as separate Windows services with separate configs.
    $suffix         = if ($InstanceName) { "-$InstanceName" } else { "" }
    $displaySuffix  = if ($InstanceName) { " ($InstanceName)" } else { "" }

    $ServiceName    = "VectrifyRunner$suffix"
    $ServiceDisplay = "Vectrify Agent Runner$displaySuffix"
    $InstallDir     = "C:\Program Files\VectrifyRunner$suffix"
    $ConfigDir      = "C:\ProgramData\VectrifyRunner$suffix"
    $ConfigFile     = "$ConfigDir\config.yaml"
    $LogFile        = "$ConfigDir\vectrify-runner.log"
    $ExeDest        = "$InstallDir\vectrify-runner.exe"

    # ── Admin check ───────────────────────────────────────────────────────────
    $isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    if (-not $isAdmin) {
        Write-Host ""
        Write-Host "  ERROR: Run from an Administrator PowerShell." -ForegroundColor Red
        Write-Host "  Right-click PowerShell -> Run as administrator, then try again." -ForegroundColor Yellow
        Write-Host ""
        return
    }

    $ErrorActionPreference = "Stop"

    # ── -SetKey: update an existing install's key and restart, then exit ──────
    if ($SetKey) {
        if ($SetKey -notmatch '^vrun_.+') {
            Write-Host "  ERROR: key must start with 'vrun_'" -ForegroundColor Red
            return
        }
        if (-not (Test-Path $ConfigFile)) {
            Write-Host "  ERROR: no existing install found at $ConfigFile - run a normal install first." -ForegroundColor Red
            return
        }
        Write-Host ""
        Write-Host "  Updating runner_key..." -NoNewline
        (Get-Content $ConfigFile) -replace '^\s*runner_key:.*', "runner_key:            $SetKey" |
            Set-Content -Encoding UTF8 $ConfigFile
        Write-Host " done" -ForegroundColor Green

        Write-Host "  Restarting service..." -NoNewline
        Restart-Service $ServiceName -Force
        Write-Host " done" -ForegroundColor Green
        Write-Host ""
        Write-Host "  Key updated and service restarted." -ForegroundColor Cyan
        Write-Host ""
        return
    }

    Write-Host ""
    Write-Host "  Vectrify Agent Runner - Installer" -ForegroundColor Cyan
    Write-Host ""

    # ── Locate or download binary ─────────────────────────────────────────────
    $src = $null; $downloaded = $false
    foreach ($c in @(".\vectrify-runner.exe", ".\dist\vectrify-runner-windows-amd64.exe")) {
        if (Test-Path $c) { $src = (Resolve-Path $c).Path; break }
    }
    if (-not $src) {
        $asset = "vectrify-runner-windows-amd64.exe"
        $url   = "https://github.com/$GITHUB_REPO/releases/latest/download/$asset"
        $tmp   = "$env:TEMP\vectrify-runner-$(Get-Random).exe"
        Write-Host "  Downloading $asset..." -NoNewline
        try {
            Invoke-WebRequest -Uri $url -OutFile $tmp -UseBasicParsing
            $src = $tmp; $downloaded = $true
            Write-Host " done" -ForegroundColor Green
        } catch {
            Write-Host " FAILED" -ForegroundColor Red
            Write-Host "  $_" -ForegroundColor Red
            return
        }
    }
    Write-Host "  Binary : $src" -ForegroundColor DarkGray
    Write-Host ""

    # ── Update path: existing install detected ────────────────────────────────
    if (Test-Path $ConfigFile) {
        Write-Host "  Existing install detected - updating binary..." -NoNewline
        $svc = Get-Service $ServiceName -EA SilentlyContinue
        if ($svc -and $svc.Status -eq "Running") {
            sc.exe stop $ServiceName | Out-Null
            Start-Sleep 2
        }
        New-Item -ItemType Directory -Force $InstallDir | Out-Null
        Copy-Item -Force $src $ExeDest
        Start-Service $ServiceName
        Write-Host " done" -ForegroundColor Green
        Write-Host ""
        $st = (Get-Service $ServiceName).Status
        Write-Host "  $ServiceName : $st" -ForegroundColor $(if ($st -eq "Running") { "Green" } else { "Yellow" })

        # This update path only swaps the binary and restarts -- it never
        # re-registers the service identity. A service already running as
        # LocalSystem (installed before this account-based install existed,
        # or from an even older release) stays LocalSystem forever unless
        # someone runs a full re-install with credentials. Flag that rather
        # than silently leaving the operator to assume this update brought
        # them the new "runs as your account" behaviour.
        try {
            $wmiSvc = Get-CimInstance Win32_Service -Filter "Name='$ServiceName'" -EA SilentlyContinue
            if ($wmiSvc -and $wmiSvc.StartName -match 'LocalSystem') {
                Write-Host ""
                Write-Host "  Note: this service still runs as LocalSystem (from an older install)." -ForegroundColor Yellow
                Write-Host "  To switch it to run as a specific account instead, remove the service" -ForegroundColor Yellow
                Write-Host "  (sc.exe delete $ServiceName) and run this installer fresh." -ForegroundColor Yellow
            }
        } catch {
            # Best-effort notice only -- never fail the update over this.
        }

        Write-Host ""
        if ($downloaded) { Remove-Item $src -EA 0 }
        return
    }

    # -- LSA helper: grant "Log on as a service" to a local account ------------
    # New-Service -Credential creates the service fine even without this right,
    # but the service then fails to actually start (logon failure) until the
    # account has SeServiceLogonRight -- regular user accounts (including the
    # one running this installer) do NOT have it by default; only interactive
    # logon rights are implied by normal account creation. There is no
    # PowerShell cmdlet for this -- it requires the LSA policy API directly.
    function Grant-ServiceLogonRight([string]$AccountName) {
        $sig = @'
using System;
using System.Runtime.InteropServices;

public class VectrifyLsaHelper {
    [DllImport("advapi32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    public static extern bool LookupAccountName(string lpSystemName, string lpAccountName,
        byte[] Sid, ref int cbSid, byte[] ReferencedDomainName, ref int cchReferencedDomainName, out int peUse);

    [DllImport("advapi32.dll", SetLastError = true, PreserveSig = true)]
    public static extern uint LsaOpenPolicy(ref LSA_UNICODE_STRING SystemName, ref LSA_OBJECT_ATTRIBUTES Attributes, int AccessMask, out IntPtr PolicyHandle);

    [DllImport("advapi32.dll", SetLastError = true, PreserveSig = true)]
    public static extern uint LsaAddAccountRights(IntPtr PolicyHandle, byte[] AccountSid, LSA_UNICODE_STRING[] UserRights, int CountOfRights);

    [DllImport("advapi32.dll")]
    public static extern int LsaClose(IntPtr ObjectHandle);

    [StructLayout(LayoutKind.Sequential)]
    public struct LSA_UNICODE_STRING {
        public ushort Length;
        public ushort MaximumLength;
        public IntPtr Buffer;
    }

    [StructLayout(LayoutKind.Sequential)]
    public struct LSA_OBJECT_ATTRIBUTES {
        public int Length;
        public IntPtr RootDirectory;
        public IntPtr ObjectName;
        public int Attributes;
        public IntPtr SecurityDescriptor;
        public IntPtr SecurityQualityOfService;
    }

    // POLICY_CREATE_ACCOUNT | POLICY_LOOKUP_NAMES -- the minimum access mask
    // that permits LsaAddAccountRights; POLICY_ALL_ACCESS is not required and
    // (perhaps counter-intuitively) is more likely to be denied.
    const int POLICY_CREATE_ACCOUNT = 0x00000010;
    const int POLICY_LOOKUP_NAMES   = 0x00000800;

    public static void AddRight(string accountName, string right) {
        int sidSize = 0, domainSize = 0, use;
        LookupAccountName(null, accountName, null, ref sidSize, null, ref domainSize, out use);
        byte[] sid = new byte[sidSize];
        byte[] domain = new byte[domainSize * 2];
        if (!LookupAccountName(null, accountName, sid, ref sidSize, domain, ref domainSize, out use))
            throw new Exception("LookupAccountName failed for '" + accountName + "': " + Marshal.GetLastWin32Error());

        LSA_UNICODE_STRING system = new LSA_UNICODE_STRING();
        LSA_OBJECT_ATTRIBUTES attrs = new LSA_OBJECT_ATTRIBUTES();
        IntPtr policyHandle;
        uint res = LsaOpenPolicy(ref system, ref attrs, POLICY_CREATE_ACCOUNT | POLICY_LOOKUP_NAMES, out policyHandle);
        if (res != 0) throw new Exception("LsaOpenPolicy failed: " + res);

        try {
            LSA_UNICODE_STRING rightStr = new LSA_UNICODE_STRING();
            rightStr.Buffer = Marshal.StringToHGlobalUni(right);
            rightStr.Length = (ushort)(right.Length * 2);
            rightStr.MaximumLength = (ushort)((right.Length + 1) * 2);

            LSA_UNICODE_STRING[] rights = new LSA_UNICODE_STRING[] { rightStr };
            res = LsaAddAccountRights(policyHandle, sid, rights, 1);
            if (res != 0) throw new Exception("LsaAddAccountRights failed: " + res);
        } finally {
            LsaClose(policyHandle);
        }
    }
}
'@
        if (-not ("VectrifyLsaHelper" -as [type])) {
            Add-Type -TypeDefinition $sig -Language CSharp
        }
        [VectrifyLsaHelper]::AddRight($AccountName, "SeServiceLogonRight")
    }


    # ── Prompt helpers ────────────────────────────────────────────────────────
    function Ask-Required([string]$Label) {
        while ($true) {
            $v = (Read-Host "  $Label").Trim()
            if ($v) { return $v }
            Write-Host "  Required." -ForegroundColor Yellow
        }
    }
    function Ask-Default([string]$Label, [string]$Def) {
        $v = (Read-Host "  $Label [$Def]").Trim()
        return $(if ($v) { $v } else { $Def })
    }
    function Ask-YesNo([string]$Label, [bool]$Def = $false) {
        $hint = if ($Def) { "Y/n" } else { "y/N" }
        while ($true) {
            $v = (Read-Host "  $Label [$hint]").Trim().ToLower()
            if ($v -eq "")  { return $Def }
            if ($v -eq "y") { return $true }
            if ($v -eq "n") { return $false }
            Write-Host "  Enter Y or N." -ForegroundColor Yellow
        }
    }
    function Ask-Choice([string]$Label, [string[]]$Choices, [string]$Def) {
        $hint = $Choices -join " | "
        while ($true) {
            $v = (Read-Host "  $Label ($hint) [$Def]").Trim().ToLower()
            if ($v -eq "") { return $Def }
            if ($Choices -contains $v) { return $v }
            Write-Host "  Choose: $hint" -ForegroundColor Yellow
        }
    }

    # ── Collect config ────────────────────────────────────────────────────────
    Write-Host "  Configure the runner:" -ForegroundColor White
    Write-Host ""

    $currentUser = "$env:USERDOMAIN\$env:USERNAME"
    while ($true) {
        $svcAccount = Ask-Default "Run the service as which account?" $currentUser
        if ($svcAccount -match '\\') { break }
        # Bare "name" (no domain\ prefix) -- assume local machine, matching
        # how Get-CimInstance/sc.exe report and accept local accounts.
        $svcAccount = "$env:COMPUTERNAME\$svcAccount"
        break
    }
    $svcPasswordSecure = Read-Host "  Password for $svcAccount" -AsSecureString
    while ($true) {
        $workspaceRoot = Ask-Required "Workspace root folder"
        if (Test-Path $workspaceRoot -PathType Container) { break }
        Write-Host "  Not found." -ForegroundColor Yellow
        if (Ask-YesNo "Create it?" $false) { New-Item -ItemType Directory -Force $workspaceRoot | Out-Null; break }
    }
    while ($true) {
        $runnerKey = Ask-Required "Runner key (vrun_...)"
        if ($runnerKey -match '^vrun_.+') { break }
        Write-Host "  Must start with vrun_" -ForegroundColor Yellow
    }
    $allowShell = Ask-YesNo  "Allow shell commands?" $false
    $preInstallBrowsers = $false
    if ($allowShell) {
        $preInstallBrowsers = Ask-YesNo "  Also pre-install browser automation now? (downloads ~300MB Chromium; optional -- it auto-installs on first use otherwise)" $false
    }
    $logLevel   = Ask-Choice "Log level" @("info","debug","warn","error") "info"
    while ($true) {
        $bs = Ask-Default "Max reconnect backoff seconds" "60"
        if ($bs -match '^\d+$' -and [int]$bs -gt 0) { $backoff = [int]$bs; break }
        Write-Host "  Must be a positive integer." -ForegroundColor Yellow
    }

    $allowShellYaml = if ($allowShell) { "true" } else { "false" }

    # ── Summary + confirm ─────────────────────────────────────────────────────
    Write-Host ""
    Write-Host "  service   : runs as $svcAccount"
    Write-Host "  workspace : $workspaceRoot"
    Write-Host "  key       : $($runnerKey.Substring(0,[Math]::Min(8,$runnerKey.Length)))..."
    Write-Host "  shell     : $allowShellYaml  |  log: $logLevel  |  backoff: ${backoff}s"
    if ($preInstallBrowsers) { Write-Host "  browser   : pre-installing now (~300MB)" }
    elseif ($allowShell)     { Write-Host "  browser   : auto-installs on first use (no action needed)" }
    Write-Host ""
    Write-Host "  NOTE: if $svcAccount's Windows password ever changes, the service" -ForegroundColor DarkGray
    Write-Host "  will fail to start on next reboot until you re-enter the new password" -ForegroundColor DarkGray
    Write-Host "  (re-run this installer, or update it via services.msc -> Log On As)." -ForegroundColor DarkGray
    Write-Host ""
    if (-not (Ask-YesNo "Proceed?" $true)) { if ($downloaded) { Remove-Item $src -EA 0 }; return }
    Write-Host ""

    # ── Install ───────────────────────────────────────────────────────────────
    Write-Host "  [1/6] Installing binary..."   -NoNewline
    New-Item -ItemType Directory -Force $InstallDir | Out-Null
    Copy-Item -Force $src $ExeDest
    Write-Host " done" -ForegroundColor Green

    $svcCred = New-Object System.Management.Automation.PSCredential($svcAccount, $svcPasswordSecure)

    Write-Host "  [2/6] Writing config..."      -NoNewline
    New-Item -ItemType Directory -Force $ConfigDir | Out-Null
    @"
api_url:               wss://api.vectrify.ai/api/v1/runner/ws
runner_key:            $runnerKey
workspace_root:        $workspaceRoot
allow_shell:           $allowShellYaml
log_level:             $logLevel
reconnect_max_backoff: $backoff
log_file:              $LogFile
"@ | Set-Content -Encoding UTF8 $ConfigFile
    Write-Host " done" -ForegroundColor Green

    Write-Host "  [3/6] Granting Log on as a service..." -NoNewline
    try {
        Grant-ServiceLogonRight -AccountName $svcAccount
        Write-Host " done" -ForegroundColor Green
    } catch {
        Write-Host " FAILED" -ForegroundColor Red
        Write-Host "  $_" -ForegroundColor Red
        Write-Host "  The service will likely fail to start. Grant 'Log on as a service' to" -ForegroundColor Yellow
        Write-Host "  $svcAccount manually via secpol.msc and re-run, or re-run this installer." -ForegroundColor Yellow
    }

    Write-Host "  [4/6] Setting folder permissions..." -NoNewline
    # $InstallDir and $ConfigDir are created under Program Files / ProgramData,
    # both owned by Administrators by default -- $svcAccount needs explicit
    # read access to the binary/config and write access to the log file.
    # Mirrors what install.sh does with chown for the Linux/macOS service user.
    icacls $InstallDir /grant "${svcAccount}:(OI)(CI)RX" | Out-Null
    icacls $ConfigDir  /grant "${svcAccount}:(OI)(CI)M"  | Out-Null
    # config.yaml holds runner_key -- install.sh deliberately chmod 700/600s
    # its equivalent for exactly that reason. The two /grant calls above only
    # *add* permissions on top of ProgramData's normal inherited ACL, which
    # commonly still leaves other local accounts able to read the file; break
    # inheritance and grant only SYSTEM, Administrators, and svcAccount, to
    # reach the same real-world protection Linux/macOS already has.
    icacls $ConfigFile /inheritance:r `
        /grant "SYSTEM:F" /grant "Administrators:F" /grant "${svcAccount}:R" | Out-Null
    Write-Host " done" -ForegroundColor Green

    if ($preInstallBrowsers) {
        Write-Host "  [4b/6] Installing browser automation (Chromium, ~300MB)..."
        # Deliberately run this AFTER granting Log on as a service and
        # setting folder permissions (steps 3-4), not before -- Start-Process
        # -Credential needs the account to be able to authenticate and,
        # separately, $InstallDir's RX grant to actually execute the binary;
        # running this earlier worked by relying on Program Files' inherited
        # Users:(RX) ACL, which isn't guaranteed on every machine, and any
        # failure was silently swallowed by -ErrorAction SilentlyContinue.
        # Running it here, after both grants are confirmed in place, makes
        # it deterministic instead of incidentally working.
        #
        # Run as $svcAccount, not the interactive installer's account --
        # Playwright's browser cache lives under the *running user's* home
        # directory, and the service will run as $svcAccount, so installing
        # under any other account would silently go to waste (the service
        # would just auto-install again itself on first real use).
        $installProc = Start-Process -FilePath $ExeDest -ArgumentList "-install-browsers" `
            -Credential $svcCred -Wait -PassThru -WindowStyle Hidden -ErrorAction SilentlyContinue
        if (-not $installProc -or $installProc.ExitCode -ne 0) {
            Write-Host "  Browser install failed -- browser commands will auto-install on first use instead." -ForegroundColor Yellow
        }
    }

    Write-Host "  [5/6] Registering service..." -NoNewline
    $svc = Get-Service $ServiceName -EA SilentlyContinue
    if ($svc) {
        if ($svc.Status -eq "Running") { Stop-Service $ServiceName -Force -EA 0; Start-Sleep 2 }
        sc.exe delete $ServiceName | Out-Null; Start-Sleep 1
    }
    New-Service -Name $ServiceName -DisplayName $ServiceDisplay -StartupType Automatic `
        -BinaryPathName "`"$ExeDest`" --config `"$ConfigFile`"" -Credential $svcCred | Out-Null
    sc.exe description $ServiceName "Connects to Vectrify Cloud and executes agent commands on this machine." | Out-Null
    Write-Host " done" -ForegroundColor Green

    Write-Host "  [6/6] Restart-on-failure + start..." -NoNewline
    sc.exe failure $ServiceName reset= 3600 actions= restart/5000/restart/10000/restart/30000 | Out-Null
    Start-Service $ServiceName
    Write-Host " done" -ForegroundColor Green

    # ── Done ──────────────────────────────────────────────────────────────────
    Write-Host ""
    $st = (Get-Service $ServiceName).Status
    Write-Host "  $ServiceName : $st" -ForegroundColor $(if ($st -eq "Running") { "Green" } else { "Yellow" })
    Write-Host ""
    Write-Host "  Done!" -ForegroundColor Cyan
    Write-Host ""
    Write-Host "  Logs : $LogFile"
    Write-Host "  Runs as : $svcAccount"
    Write-Host ""
    if ($st -ne "Running") {
        Write-Host "  Service did not start. This is usually a wrong password or a logon-right" -ForegroundColor Yellow
        Write-Host "  problem for $svcAccount. Re-run this installer to re-enter the password," -ForegroundColor Yellow
        Write-Host "  or check services.msc -> $ServiceDisplay -> Log On As." -ForegroundColor Yellow
        Write-Host ""
    }

    if ($downloaded) { Remove-Item $src -EA 0 }
}

Install-VectrifyRunner
