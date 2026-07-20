# escape=`

# Nano Server does not include Windows PowerShell or msi.dll.  The WiX DTF
# validation in this repository requires both, so Server Core is the smallest
# viable Windows base for the complete installer build.
ARG BASE_IMAGE
FROM ${BASE_IMAGE}

SHELL ["powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", "$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue';"]

ARG TOOLCHAIN_ARCH
ARG GO_URL
ARG GO_SHA256
ARG ZIG_URL
ARG ZIG_SHA256
ARG RUSTUP_URL
ARG RUSTUP_SHA256
ARG DOTNET_URL
ARG DOTNET_SHA512
ARG MINGIT_URL
ARG MINGIT_SHA256

ENV RUSTUP_HOME="C:\toolchains\rustup" `
    CARGO_HOME="C:\toolchains\cargo" `
    RUSTUP_TOOLCHAIN="1.97.0" `
    DOTNET_ROOT="C:\toolchains\dotnet" `
    DOTNET_CLI_TELEMETRY_OPTOUT="1" `
    DOTNET_NOLOGO="1" `
    GOTOOLCHAIN="local" `
    GOPATH="C:\build-cache\go" `
    GOCACHE="C:\build-cache\go-build" `
    NUGET_PACKAGES="C:\build-cache\nuget"

RUN New-Item -ItemType Directory -Force C:\downloads,C:\toolchains,C:\build-cache | Out-Null; `
    $items = @( `
        @{ Name = 'go.zip'; Url = $env:GO_URL; Algorithm = 'SHA256'; Hash = $env:GO_SHA256 }, `
        @{ Name = 'zig.zip'; Url = $env:ZIG_URL; Algorithm = 'SHA256'; Hash = $env:ZIG_SHA256 }, `
        @{ Name = 'rustup-init.exe'; Url = $env:RUSTUP_URL; Algorithm = 'SHA256'; Hash = $env:RUSTUP_SHA256 }, `
        @{ Name = 'dotnet.zip'; Url = $env:DOTNET_URL; Algorithm = 'SHA512'; Hash = $env:DOTNET_SHA512 }, `
        @{ Name = 'mingit.zip'; Url = $env:MINGIT_URL; Algorithm = 'SHA256'; Hash = $env:MINGIT_SHA256 } `
    ); `
    foreach ($item in $items) { `
        $path = Join-Path C:\downloads $item.Name; `
        Write-Host ('Downloading ' + $item.Name); `
        Invoke-WebRequest -UseBasicParsing -Uri $item.Url -OutFile $path; `
        $actual = (Get-FileHash -LiteralPath $path -Algorithm $item.Algorithm).Hash.ToLowerInvariant(); `
        if ($actual -cne $item.Hash.ToLowerInvariant()) { throw ('checksum mismatch for ' + $item.Name + ': expected ' + $item.Hash + ', got ' + $actual) } `
    }

RUN Expand-Archive -LiteralPath C:\downloads\go.zip -DestinationPath C:\toolchains; `
    Expand-Archive -LiteralPath C:\downloads\dotnet.zip -DestinationPath C:\toolchains\dotnet; `
    Expand-Archive -LiteralPath C:\downloads\mingit.zip -DestinationPath C:\toolchains\git; `
    New-Item -ItemType Directory -Force C:\toolchains\zig,C:\zig-extract | Out-Null; `
    Expand-Archive -LiteralPath C:\downloads\zig.zip -DestinationPath C:\zig-extract; `
    $zigRoot = @(Get-ChildItem -LiteralPath C:\zig-extract -Directory); `
    if ($zigRoot.Count -ne 1) { throw ('expected one Zig archive root, got ' + $zigRoot.Count) } `
    Move-Item -Path (Join-Path $zigRoot[0].FullName '*') -Destination C:\toolchains\zig

ENV PATH="C:\toolchains\go\bin;C:\toolchains\zig;C:\toolchains\dotnet;C:\toolchains\cargo\bin;C:\toolchains\git\cmd;C:\toolchains\git\usr\bin;C:\Windows\System32\WindowsPowerShell\v1.0;C:\Windows\system32;C:\Windows"

RUN if (Test-Path -LiteralPath $env:RUSTUP_HOME) { Remove-Item -LiteralPath $env:RUSTUP_HOME -Recurse -Force }; `
    if (Test-Path -LiteralPath $env:CARGO_HOME) { Remove-Item -LiteralPath $env:CARGO_HOME -Recurse -Force }; `
    & C:\downloads\rustup-init.exe -y --no-modify-path --profile minimal --default-host x86_64-pc-windows-gnu --default-toolchain 1.97.0; `
    if ($LASTEXITCODE -ne 0) { throw ('rustup-init failed with exit ' + $LASTEXITCODE) }; `
    rustup target add --toolchain 1.97.0-x86_64-pc-windows-gnu x86_64-pc-windows-gnu aarch64-pc-windows-gnullvm; `
    if ($LASTEXITCODE -ne 0) { throw ('rustup target add failed with exit ' + $LASTEXITCODE) }; `
    git config --system --add safe.directory C:/workspace

ARG LLVM_MINGW_URL
ARG LLVM_MINGW_SHA256
RUN $archive = 'C:\llvm-mingw.zip'; `
    Invoke-WebRequest -UseBasicParsing -Uri $env:LLVM_MINGW_URL -OutFile $archive; `
    $actual = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant(); `
    if ($actual -cne $env:LLVM_MINGW_SHA256.ToLowerInvariant()) { throw ('LLVM-MinGW checksum mismatch: ' + $actual) }; `
    New-Item -ItemType Directory -Force C:\llvm-mingw-extract | Out-Null; `
    Expand-Archive -LiteralPath $archive -DestinationPath C:\llvm-mingw-extract; `
    $llvmRoot = @(Get-ChildItem -LiteralPath C:\llvm-mingw-extract -Directory); `
    if ($llvmRoot.Count -ne 1) { throw ('expected one LLVM-MinGW archive root, got ' + $llvmRoot.Count) }; `
    Move-Item -LiteralPath $llvmRoot[0].FullName -Destination C:\toolchains\llvm-mingw; `
    Remove-Item -LiteralPath $archive,C:\llvm-mingw-extract -Recurse -Force

RUN $expectedGoArch = $env:TOOLCHAIN_ARCH; `
    $expectedRustHost = if ($expectedGoArch -ceq 'arm64' -or $expectedGoArch -ceq 'amd64') { 'x86_64-pc-windows-gnu' } else { throw ('unsupported TOOLCHAIN_ARCH: ' + $expectedGoArch) }; `
    if ((go version) -cnotmatch '^go version go1\.23\.12 windows/') { throw ('unexpected Go version: ' + (go version)) }; `
    if ((go env GOHOSTARCH).Trim() -cne $expectedGoArch) { throw ('Go host architecture does not match image: ' + (go env GOHOSTARCH)) }; `
    if ((rustc --version) -cnotmatch '^rustc 1\.97\.0 ') { throw ('unexpected Rust version: ' + (rustc --version)) }; `
    if ((rustc -vV | Select-String -SimpleMatch ('host: ' + $expectedRustHost)).Count -ne 1) { throw ('Rust host architecture does not match image: ' + (rustc -vV)) }; `
    if ((zig version).Trim() -cne '0.16.0') { throw ('unexpected Zig version: ' + (zig version)) }; `
    if ((dotnet --version).Trim() -cne '8.0.423') { throw ('unexpected .NET SDK version: ' + (dotnet --version)) }; `
    if ((git --version).Trim() -cne 'git version 2.55.0.windows.3') { throw ('unexpected Git version: ' + (git --version)) }; `
    if (-not (Test-Path -LiteralPath C:\toolchains\llvm-mingw\bin\aarch64-w64-mingw32-clang.exe -PathType Leaf)) { throw 'LLVM-MinGW is missing its ARM64 compiler' }; `
    if (-not (Test-Path -LiteralPath C:\toolchains\llvm-mingw\bin\x86_64-w64-mingw32-clang.exe -PathType Leaf)) { throw 'LLVM-MinGW is missing its AMD64 compiler' }; `
    if (-not (Test-Path -LiteralPath C:\Windows\System32\msi.dll -PathType Leaf)) { throw 'Server Core image is missing msi.dll' }; `
    if (-not (Test-Path -LiteralPath C:\toolchains\git\usr\bin\sh.exe -PathType Leaf)) { throw 'MinGit image is missing sh.exe' }; `
    Remove-Item -LiteralPath C:\downloads,C:\zig-extract -Recurse -Force

# Populate the persistent image-level NuGet cache with the exact WiX SDK used
# by the installer project.  Release containers can then restore from this
# cache instead of downloading the packaging toolchain on every invocation.
COPY ["Codesk.wixproj", "C:/toolchain-restore/Codesk.wixproj"]
RUN dotnet restore C:\toolchain-restore\Codesk.wixproj --packages $env:NUGET_PACKAGES; `
    if ($LASTEXITCODE -ne 0) { throw ('WiX SDK restore failed with exit ' + $LASTEXITCODE) }; `
    if (-not (Test-Path -LiteralPath (Join-Path $env:NUGET_PACKAGES 'wixtoolset.sdk\4.0.5') -PathType Container)) { throw 'WiX SDK 4.0.5 is missing from the image cache' }; `
    Remove-Item -LiteralPath C:\toolchain-restore -Recurse -Force

WORKDIR C:\workspace
ENTRYPOINT ["powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass"]
