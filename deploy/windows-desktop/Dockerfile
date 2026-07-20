# escape=`

# Nano Server does not include Windows PowerShell or msi.dll. The WiX DTF
# validation in this repository requires both, so Server Core is the smallest
# viable Windows base for the complete installer build. BuildKit supplies
# TARGETARCH automatically; the legacy Windows builder receives the same ARG
# from scripts/build-windows-gui-builder-image.ps1.
ARG TARGETARCH
FROM mcr.microsoft.com/windows/servercore:ltsc2025-${TARGETARCH}
ARG TARGETARCH

SHELL ["powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", "$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue';"]

ENV GO_VERSION="1.23.12" `
    ZIG_VERSION="0.16.0" `
    RUSTUP_VERSION="1.29.0" `
    RUST_VERSION="1.97.0" `
    DOTNET_VERSION="8.0.423" `
    MINGIT_VERSION="2.55.0" `
    MINGIT_RELEASE="3" `
    LLVM_MINGW_VERSION="20260616" `
    RUSTUP_HOME="C:\toolchains\rustup" `
    CARGO_HOME="C:\toolchains\cargo" `
    RUSTUP_TOOLCHAIN="1.97.0" `
    DOTNET_ROOT="C:\toolchains\dotnet" `
    DOTNET_CLI_TELEMETRY_OPTOUT="1" `
    DOTNET_NOLOGO="1" `
    GOTOOLCHAIN="local" `
    GOPATH="C:\build-cache\go" `
    GOCACHE="C:\build-cache\go-build" `
    NUGET_PACKAGES="C:\build-cache\nuget" `
    PATH="C:\toolchains\go\bin;C:\toolchains\zig;C:\toolchains\dotnet;C:\toolchains\cargo\bin;C:\toolchains\git\cmd;C:\toolchains\git\usr\bin;C:\Windows\System32\WindowsPowerShell\v1.0;C:\Windows\system32;C:\Windows"

RUN $architecture = $env:TARGETARCH.ToLowerInvariant(); `
    if ($architecture -cne 'amd64' -and $architecture -cne 'arm64') { throw ('unsupported TARGETARCH: ' + $env:TARGETARCH) }; `
    $runtimeArchitecture = switch ($env:PROCESSOR_ARCHITECTURE.ToUpperInvariant()) { 'AMD64' { 'amd64' } 'ARM64' { 'arm64' } default { throw ('unsupported container architecture: ' + $env:PROCESSOR_ARCHITECTURE) } }; `
    if ($architecture -cne $runtimeArchitecture) { throw ('TARGETARCH ' + $architecture + ' does not match container architecture ' + $runtimeArchitecture) }; `
    $archiveArchitecture = if ($architecture -ceq 'arm64') { 'aarch64' } else { 'x86_64' }; `
    $dotnetArchitecture = if ($architecture -ceq 'arm64') { 'arm64' } else { 'x64' }; `
    $minGitArchitecture = if ($architecture -ceq 'arm64') { 'arm64' } else { '64-bit' }; `
    $rustupHost = if ($architecture -ceq 'arm64') { 'aarch64-pc-windows-msvc' } else { 'x86_64-pc-windows-msvc' }; `
    $downloads = @( `
        @{ Name = 'go.zip'; Url = ('https://go.dev/dl/go{0}.windows-{1}.zip' -f $env:GO_VERSION,$architecture) }, `
        @{ Name = 'zig.zip'; Url = ('https://ziglang.org/download/{0}/zig-{1}-windows-{0}.zip' -f $env:ZIG_VERSION,$archiveArchitecture) }, `
        @{ Name = 'rustup-init.exe'; Url = ('https://static.rust-lang.org/rustup/archive/{0}/{1}/rustup-init.exe' -f $env:RUSTUP_VERSION,$rustupHost) }, `
        @{ Name = 'dotnet.zip'; Url = ('https://builds.dotnet.microsoft.com/dotnet/Sdk/{0}/dotnet-sdk-{0}-win-{1}.zip' -f $env:DOTNET_VERSION,$dotnetArchitecture) }, `
        @{ Name = 'mingit.zip'; Url = ('https://github.com/git-for-windows/git/releases/download/v{0}.windows.{1}/MinGit-{0}.{1}-{2}.zip' -f $env:MINGIT_VERSION,$env:MINGIT_RELEASE,$minGitArchitecture) }, `
        @{ Name = 'llvm-mingw.zip'; Url = ('https://github.com/mstorsjo/llvm-mingw/releases/download/{0}/llvm-mingw-{0}-ucrt-{1}.zip' -f $env:LLVM_MINGW_VERSION,$archiveArchitecture) } `
    ); `
    New-Item -ItemType Directory -Force C:\downloads,C:\toolchains,C:\build-cache | Out-Null; `
    foreach ($download in $downloads) { `
        Write-Host ('Downloading ' + $download.Url); `
        Invoke-WebRequest -UseBasicParsing -Uri $download.Url -OutFile (Join-Path C:\downloads $download.Name) `
    }

RUN Expand-Archive -LiteralPath C:\downloads\go.zip -DestinationPath C:\toolchains; `
    Expand-Archive -LiteralPath C:\downloads\dotnet.zip -DestinationPath C:\toolchains\dotnet; `
    Expand-Archive -LiteralPath C:\downloads\mingit.zip -DestinationPath C:\toolchains\git; `
    New-Item -ItemType Directory -Force C:\toolchains\zig,C:\zig-extract | Out-Null; `
    Expand-Archive -LiteralPath C:\downloads\zig.zip -DestinationPath C:\zig-extract; `
    $zigRoot = @(Get-ChildItem -LiteralPath C:\zig-extract -Directory); `
    if ($zigRoot.Count -ne 1) { throw ('expected one Zig archive root, got ' + $zigRoot.Count) }; `
    Move-Item -Path (Join-Path $zigRoot[0].FullName '*') -Destination C:\toolchains\zig

RUN if (Test-Path -LiteralPath $env:RUSTUP_HOME) { Remove-Item -LiteralPath $env:RUSTUP_HOME -Recurse -Force }; `
    if (Test-Path -LiteralPath $env:CARGO_HOME) { Remove-Item -LiteralPath $env:CARGO_HOME -Recurse -Force }; `
    & C:\downloads\rustup-init.exe -y --no-modify-path --profile minimal --default-host x86_64-pc-windows-gnu --default-toolchain $env:RUST_VERSION; `
    if ($LASTEXITCODE -ne 0) { throw ('rustup-init failed with exit ' + $LASTEXITCODE) }; `
    $rustToolchain = $env:RUST_VERSION + '-x86_64-pc-windows-gnu'; `
    rustup target add --toolchain $rustToolchain x86_64-pc-windows-gnu aarch64-pc-windows-gnullvm; `
    if ($LASTEXITCODE -ne 0) { throw ('rustup target add failed with exit ' + $LASTEXITCODE) }; `
    git config --system --add safe.directory C:/workspace

RUN New-Item -ItemType Directory -Force C:\llvm-mingw-extract | Out-Null; `
    Expand-Archive -LiteralPath C:\downloads\llvm-mingw.zip -DestinationPath C:\llvm-mingw-extract; `
    $llvmRoot = @(Get-ChildItem -LiteralPath C:\llvm-mingw-extract -Directory); `
    if ($llvmRoot.Count -ne 1) { throw ('expected one LLVM-MinGW archive root, got ' + $llvmRoot.Count) }; `
    Move-Item -LiteralPath $llvmRoot[0].FullName -Destination C:\toolchains\llvm-mingw

RUN $expectedGoArch = $env:TARGETARCH.ToLowerInvariant(); `
    if ($expectedGoArch -cne 'arm64' -and $expectedGoArch -cne 'amd64') { throw ('unsupported TARGETARCH: ' + $env:TARGETARCH) }; `
    if ((go version) -cnotmatch ('^go version go' + [regex]::Escape($env:GO_VERSION) + ' windows/')) { throw ('unexpected Go version: ' + (go version)) }; `
    if ((go env GOHOSTARCH).Trim() -cne $expectedGoArch) { throw ('Go host architecture does not match image: ' + (go env GOHOSTARCH)) }; `
    if ((rustc --version) -cnotmatch ('^rustc ' + [regex]::Escape($env:RUST_VERSION) + ' ')) { throw ('unexpected Rust version: ' + (rustc --version)) }; `
    if ((rustc -vV | Select-String -SimpleMatch 'host: x86_64-pc-windows-gnu').Count -ne 1) { throw ('unexpected Rust host: ' + (rustc -vV)) }; `
    if ((zig version).Trim() -cne $env:ZIG_VERSION) { throw ('unexpected Zig version: ' + (zig version)) }; `
    if ((dotnet --version).Trim() -cne $env:DOTNET_VERSION) { throw ('unexpected .NET SDK version: ' + (dotnet --version)) }; `
    if ((git --version).Trim() -cne ('git version {0}.windows.{1}' -f $env:MINGIT_VERSION,$env:MINGIT_RELEASE)) { throw ('unexpected Git version: ' + (git --version)) }; `
    if (-not (Test-Path -LiteralPath C:\toolchains\llvm-mingw\bin\aarch64-w64-mingw32-clang.exe -PathType Leaf)) { throw 'LLVM-MinGW is missing its ARM64 compiler' }; `
    if (-not (Test-Path -LiteralPath C:\toolchains\llvm-mingw\bin\x86_64-w64-mingw32-clang.exe -PathType Leaf)) { throw 'LLVM-MinGW is missing its AMD64 compiler' }; `
    if (-not (Test-Path -LiteralPath C:\Windows\System32\msi.dll -PathType Leaf)) { throw 'Server Core image is missing msi.dll' }; `
    if (-not (Test-Path -LiteralPath C:\toolchains\git\usr\bin\sh.exe -PathType Leaf)) { throw 'MinGit image is missing sh.exe' }; `
    Remove-Item -LiteralPath C:\downloads,C:\zig-extract,C:\llvm-mingw-extract -Recurse -Force

# Populate the persistent image-level NuGet cache with the exact WiX SDK used
# by the installer project. Release containers restore from this cache instead
# of downloading the packaging toolchain on every invocation.
COPY ["Codesk.wixproj", "C:/toolchain-restore/Codesk.wixproj"]
RUN dotnet restore C:\toolchain-restore\Codesk.wixproj --packages $env:NUGET_PACKAGES; `
    if ($LASTEXITCODE -ne 0) { throw ('WiX SDK restore failed with exit ' + $LASTEXITCODE) }; `
    if (-not (Test-Path -LiteralPath (Join-Path $env:NUGET_PACKAGES 'wixtoolset.sdk\4.0.5') -PathType Container)) { throw 'WiX SDK 4.0.5 is missing from the image cache' }; `
    Remove-Item -LiteralPath C:\toolchain-restore -Recurse -Force

WORKDIR C:\workspace
ENTRYPOINT ["powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass"]
