[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$versionFile = Join-Path (Split-Path -Parent $PSScriptRoot) 'VERSION'
if (-not (Test-Path -LiteralPath $versionFile -PathType Leaf)) {
    throw "VERSION must be a regular file: $versionFile"
}

[byte[]] $bytes = [System.IO.File]::ReadAllBytes($versionFile)
if ($bytes.Length -lt 2 -or $bytes[$bytes.Length - 1] -ne 10) {
    throw 'VERSION must end with exactly one LF or CRLF'
}

$lineLength = $bytes.Length - 1
if ($lineLength -gt 0 -and $bytes[$lineLength - 1] -eq 13) {
    $lineLength--
}
for ($index = 0; $index -lt $lineLength; $index++) {
    if ($bytes[$index] -eq 10 -or $bytes[$index] -eq 13) {
        throw 'VERSION must contain exactly one LF- or CRLF-terminated line'
    }
}

$version = [System.Text.Encoding]::ASCII.GetString($bytes, 0, $lineLength)
if ($version -cnotmatch '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$') {
    throw 'VERSION must be canonical X.Y.Z without whitespace or leading zeros'
}

$fields = @($version.Split('.'))
[uint32] $major = 0
[uint32] $minor = 0
[uint32] $build = 0
$style = [System.Globalization.NumberStyles]::None
$culture = [System.Globalization.CultureInfo]::InvariantCulture
if (-not [uint32]::TryParse($fields[0], $style, $culture, [ref] $major) -or
    -not [uint32]::TryParse($fields[1], $style, $culture, [ref] $minor) -or
    -not [uint32]::TryParse($fields[2], $style, $culture, [ref] $build)) {
    throw 'VERSION contains a field outside the unsigned integer domain'
}
if ($major -gt 255 -or $minor -gt 255 -or $build -gt 65535) {
    throw 'VERSION exceeds MSI limits (major/minor <= 255, build <= 65535)'
}

$version
